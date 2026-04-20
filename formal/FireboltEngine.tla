---- MODULE FireboltEngine ----
\* TLA+ specification of the FireboltEngine reconciler state machine.
\*
\* Models the five-phase blue-green lifecycle:
\*   stable -> creating -> switching -> draining -> cleaning -> stable
\*
\* Verified properties:
\*   Safety  - invariants that must hold in every reachable state
\*   Liveness - engine always eventually reaches stable (with fairness)
\*
\* To check with TLC:
\*   1. Open FireboltEngine.cfg alongside this file
\*   2. Run: java -jar tla2tools.jar -config FireboltEngine.cfg FireboltEngine.tla
\*   3. Or use the TLA+ Toolbox / VS Code extension
\*
\* Design decisions captured here:
\*   - The instance gate is a SCHEDULING guard (outer Reconcile), not a
\*     precondition on compute* functions. When instanceReady is false and
\*     phase in {stable, creating}, the state machine does not tick -- only
\*     conditions are updated. Switching/Draining/Cleaning bypass the gate.
\*   - Each reconcile call is modeled as one atomic step. This is conservative
\*     (the real code makes multiple K8s writes per reconcile) but correct:
\*     safety violations found here are real; absence of violations holds in
\*     the coarser implementation too.
\*   - podsReady is a boolean abstraction of "all pods in currentGen are ready".
\*     It is reset to FALSE whenever currentGen is bumped.
\*   - podsDrained is a boolean abstraction of "draining gen has zero queries".
\*     It is reset to FALSE whenever drainingGen is set.
\*   - stsSpecVer[g] = -1 means no STS for generation g exists.
\*     stsSpecVer[g] >= 0 means the STS exists and was built from spec version g.

EXTENDS Integers, TLC

CONSTANTS
    MaxGen,     \* upper bound on generation numbers (e.g. 2)
    MaxSpec     \* upper bound on spec versions (e.g. 2)

Gens     == 0..MaxGen
SpecVers == 0..MaxSpec

Phases == {"uninitialized", "stable", "creating", "switching", "draining", "cleaning"}

VARIABLES
    phase,          \* current reconciler phase
    currentGen,     \* generation being created / most recently created
    activeGen,      \* generation currently serving traffic  (-1 = none)
    drainingGen,    \* generation being drained              (-1 = none)
    specVer,        \* current spec version (env-controlled; drives rollouts)
    stsSpecVer,     \* stsSpecVer[g]: spec version STS-g was built from, -1 if absent
    svcTargetGen,   \* generation the cluster Service selector points to (-1 = no service)
    podsReady,      \* TRUE when all pods in currentGen are Running+Ready
    podsDrained,    \* TRUE when draining gen has zero running/suspended queries
    instanceReady   \* TRUE when the referenced FireboltInstance is Ready (env-controlled)

vars == <<phase, currentGen, activeGen, drainingGen, specVer,
          stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady>>

\* ---------------------------------------------------------------------------
\* Helpers
\* ---------------------------------------------------------------------------

StsExists(g)       == stsSpecVer[g] # -1
StsMatchesSpec(g)  == StsExists(g) /\ stsSpecVer[g] = specVer

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

Init ==
    /\ phase         = "uninitialized"
    /\ currentGen    = 0
    /\ activeGen     = -1
    /\ drainingGen   = -1
    /\ specVer       = 0
    /\ stsSpecVer    = [g \in Gens |-> -1]
    /\ svcTargetGen  = -1
    /\ podsReady     = FALSE
    /\ podsDrained   = TRUE
    /\ instanceReady = TRUE

\* ---------------------------------------------------------------------------
\* Environment actions  (non-deterministic; can fire at any time)
\* ---------------------------------------------------------------------------

\* User changes the engine spec (e.g. scales replicas, changes image)
EnvChangeSpec ==
    /\ specVer < MaxSpec
    /\ specVer' = specVer + 1
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady>>

\* Pods in currentGen become all-ready
EnvPodsReady ==
    /\ ~podsReady
    /\ podsReady' = TRUE
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   stsSpecVer, svcTargetGen, podsDrained, instanceReady>>

\* Pods in drainingGen finish draining (zero running/suspended queries)
EnvPodsDrained ==
    /\ ~podsDrained
    /\ podsDrained' = TRUE
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   stsSpecVer, svcTargetGen, podsReady, instanceReady>>

\* Instance becomes ready or not-ready
EnvSetInstanceReady(v) ==
    /\ instanceReady # v
    /\ instanceReady' = v
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained>>

\* ---------------------------------------------------------------------------
\* Reconciler actions
\* ---------------------------------------------------------------------------

\* ------ Phase: uninitialized ------
\* First sight of a new FireboltEngine: seed phase=creating, activeGen=-1.
\* Mirrors the phase=="" early-return in engine_controller.go:Reconcile.

ReconcileInit ==
    /\ phase = "uninitialized"
    /\ instanceReady                          \* gate applies
    /\ phase'      = "creating"
    /\ currentGen' = 0
    /\ activeGen'  = -1
    /\ podsReady'  = FALSE
    /\ UNCHANGED <<drainingGen, specVer, stsSpecVer, svcTargetGen, podsDrained, instanceReady>>

\* ------ Phase: stable ------
\* Detect spec drift or missing STS; start a new generation if needed.
\* When everything is consistent, the reconciler does nothing (stutters).

ReconcileStable_Drift ==
    \* Spec changed or STS missing -> bump currentGen, go to creating.
    \* This is the only path out of stable.
    /\ phase = "stable"
    /\ instanceReady
    /\ ~StsMatchesSpec(currentGen)
    /\ currentGen < MaxGen
    /\ currentGen' = currentGen + 1
    /\ phase'      = "creating"
    /\ podsReady'  = FALSE
    /\ UNCHANGED <<activeGen, drainingGen, specVer, stsSpecVer, svcTargetGen, podsDrained, instanceReady>>

\* GC: delete STSes that belong neither to currentGen nor drainingGen.
\* Runs opportunistically in stable phase; safe to repeat.
\* Models gcOrphanedResources() in engine_gc.go.
GCOrphans ==
    /\ phase = "stable"
    /\ \E g \in Gens :
           /\ StsExists(g)
           /\ g # currentGen
           /\ g # drainingGen   \* drainingGen=-1 never equals any gen in Gens
           /\ stsSpecVer' = [stsSpecVer EXCEPT ![g] = -1]
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   svcTargetGen, podsReady, podsDrained, instanceReady>>

\* ------ Phase: creating ------
\* Four mutually-exclusive sub-cases (checked in order in the real code):
\*   1a. Spec drift, currentGen < MaxGen -> delete STS, bump gen, stay in creating.
\*   1b. Spec drift, currentGen = MaxGen -> delete STS in place; the real operator
\*                                          would bump to MaxGen+1 etc.; aliasing to
\*                                          MaxGen keeps the state space finite while
\*                                          preserving the liveness path (EnsureSTS
\*                                          recreates the STS at the new specVer).
\*   2.  STS absent                      -> create it (at current specVer).
\*   3.  STS present and matches spec    -> ensure service exists; when pods are
\*                                          ready transition to switching.

ReconcileCreating_SpecDrift ==
    \* Mirrors the early-return spec-drift check in computeCreating.
    /\ phase = "creating"
    /\ instanceReady
    /\ StsExists(currentGen) /\ ~StsMatchesSpec(currentGen)
    /\ currentGen < MaxGen
    /\ currentGen'  = currentGen + 1
    /\ stsSpecVer'  = [stsSpecVer EXCEPT ![currentGen] = -1]
    /\ podsReady'   = FALSE
    /\ UNCHANGED <<phase, activeGen, drainingGen, specVer, svcTargetGen, podsDrained, instanceReady>>

ReconcileCreating_SpecDrift_AtMax ==
    \* Boundary case: spec drifted but currentGen is already at the model ceiling.
    \* Delete the stale STS so EnsureSTS can rebuild it at the new specVer.
    \* podsReady is reset to FALSE: the old pods are gone with the deleted STS.
    /\ phase = "creating"
    /\ instanceReady
    /\ StsExists(currentGen) /\ ~StsMatchesSpec(currentGen)
    /\ currentGen = MaxGen
    /\ stsSpecVer'  = [stsSpecVer EXCEPT ![currentGen] = -1]
    /\ podsReady'   = FALSE
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   svcTargetGen, podsDrained, instanceReady>>

ReconcileCreating_EnsureSTS ==
    \* Create the StatefulSet for currentGen (also creates ConfigMap + headless Service
    \* in the real code; omitted here as they don't affect the phase state machine).
    /\ phase = "creating"
    /\ instanceReady
    /\ ~StsExists(currentGen)                                   \* STS absent
    /\ ~(StsExists(currentGen) /\ ~StsMatchesSpec(currentGen)) \* no spec drift
    /\ stsSpecVer' = [stsSpecVer EXCEPT ![currentGen] = specVer]
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   svcTargetGen, podsReady, podsDrained, instanceReady>>

ReconcileCreating_EnsureService ==
    \* Create the cluster Service when it does not yet exist (first deployment only;
    \* on subsequent rollouts the service already exists from the previous generation).
    \* The service initially points to currentGen and is updated in switching.
    /\ phase = "creating"
    /\ instanceReady
    /\ StsMatchesSpec(currentGen)
    /\ svcTargetGen = -1
    /\ svcTargetGen' = currentGen
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   stsSpecVer, podsReady, podsDrained, instanceReady>>

ReconcileCreating_Advance ==
    \* STS exists, service exists, pods ready -> transition to switching.
    /\ phase = "creating"
    /\ instanceReady
    /\ StsMatchesSpec(currentGen)
    /\ svcTargetGen # -1
    /\ podsReady
    /\ phase' = "switching"
    /\ UNCHANGED <<currentGen, activeGen, drainingGen, specVer,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady>>

\* ------ Phase: switching ------
\* Two sub-steps (matches computeSwitching):
\*   1. Flip the cluster Service selector to currentGen (if not already there).
\*   2. Once selector is confirmed, update activeGen and decide next phase.

ReconcileSwitching_UpdateService ==
    \* Flip the service selector to point at the new generation.
    /\ phase = "switching"
    /\ svcTargetGen # currentGen
    /\ svcTargetGen' = currentGen
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer,
                   stsSpecVer, podsReady, podsDrained, instanceReady>>

ReconcileSwitching_Complete ==
    \* Service already points to currentGen: finalise the switch.
    \* If there is an old generation to drain, go to draining; otherwise
    \* (first deployment, activeGen = -1) go directly to stable.
    /\ phase = "switching"
    /\ svcTargetGen = currentGen
    /\ activeGen' = currentGen
    /\ \/ \* First deployment: no old generation to drain.
          /\ activeGen = -1
          /\ phase'       = "stable"
          /\ drainingGen' = drainingGen   \* unchanged (-1)
          /\ UNCHANGED podsDrained
       \/ \* Rollout: old generation must drain before cleanup.
          /\ activeGen >= 0 /\ activeGen # currentGen
          /\ phase'       = "draining"
          /\ drainingGen' = activeGen
          /\ podsDrained' = FALSE         \* reset; new draining target
    /\ UNCHANGED <<currentGen, specVer, stsSpecVer, svcTargetGen, podsReady, instanceReady>>

\* ------ Phase: draining ------
\* Wait for drain completion, then go to cleaning.
\* The "not yet drained" case is handled by TLA+ stuttering (no explicit action needed).

ReconcileDraining_Complete ==
    /\ phase = "draining"
    /\ drainingGen # -1
    /\ podsDrained
    /\ phase' = "cleaning"
    /\ UNCHANGED <<currentGen, activeGen, drainingGen, specVer,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady>>

\* ------ Phase: cleaning ------
\* Delete old-generation resources and return to stable.

ReconcileCleaning ==
    /\ phase = "cleaning"
    /\ drainingGen # -1
    /\ stsSpecVer'  = [stsSpecVer EXCEPT ![drainingGen] = -1]
    /\ drainingGen' = -1
    /\ phase'       = "stable"
    /\ UNCHANGED <<currentGen, activeGen, specVer,
                   svcTargetGen, podsReady, podsDrained, instanceReady>>

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

Next ==
    \/ EnvChangeSpec
    \/ EnvPodsReady
    \/ EnvPodsDrained
    \/ EnvSetInstanceReady(TRUE)
    \/ EnvSetInstanceReady(FALSE)
    \/ ReconcileInit
    \/ ReconcileStable_Drift
    \/ GCOrphans
    \/ ReconcileCreating_SpecDrift
    \/ ReconcileCreating_SpecDrift_AtMax
    \/ ReconcileCreating_EnsureSTS
    \/ ReconcileCreating_EnsureService
    \/ ReconcileCreating_Advance
    \/ ReconcileSwitching_UpdateService
    \/ ReconcileSwitching_Complete
    \/ ReconcileDraining_Complete
    \/ ReconcileCleaning

\* ---------------------------------------------------------------------------
\* Safety invariants
\* ---------------------------------------------------------------------------

TypeOK ==
    /\ phase        \in Phases
    /\ currentGen   \in Gens
    /\ activeGen    \in {-1} \cup Gens
    /\ drainingGen  \in {-1} \cup Gens
    /\ specVer      \in SpecVers
    /\ stsSpecVer   \in [Gens -> {-1} \cup SpecVers]
    /\ svcTargetGen \in {-1} \cup Gens
    /\ podsReady    \in BOOLEAN
    /\ podsDrained  \in BOOLEAN
    /\ instanceReady \in BOOLEAN

\* Matches user-confirmed invariant from code review:
\* "Any persistent CurrentGeneration != ActiveGeneration while Phase=Stable
\*  would indicate a state-machine bug."
Inv_StableConsistency ==
    phase = "stable" => currentGen = activeGen

\* The cluster Service always points to a generation whose STS exists,
\* once traffic has been switched (activeGen != -1).
\* During the first deployment the service is pre-populated while still in
\* creating phase (activeGen=-1) so that a spec-drift bump does not require
\* re-creating the service; no real traffic flows until activeGen is set.
\* After the first switch this guard is always enforced.
Inv_ServiceValid ==
    activeGen # -1 => StsExists(svcTargetGen)

\* In stable phase the current generation's STS must exist.
Inv_StableHasSTS ==
    phase = "stable" => StsExists(currentGen)

\* The active generation's STS must always exist (once set).
\* Violation would mean serving traffic to a deleted StatefulSet.
Inv_ActiveHasSTS ==
    activeGen # -1 => StsExists(activeGen)

\* The service selector only points to activeGen or currentGen, once traffic has
\* been switched (activeGen != -1).
\* Before the first switch (activeGen=-1) spec-drift bumps can leave svcTargetGen
\* pointing to a stale gen; no real traffic flows and the selector is corrected in
\* switching phase. After the first switch this always holds.
Inv_ServiceKnownGen ==
    activeGen # -1 => svcTargetGen \in {activeGen, currentGen}

\* DrainingGeneration is only set while in draining or cleaning phase.
\* A non-nil drainingGen while in stable/creating/switching indicates a leak.
Inv_DrainingPhase ==
    drainingGen # -1 => phase \in {"draining", "cleaning"}

\* In stable phase there is no draining generation.
Inv_StableNoDraining ==
    phase = "stable" => drainingGen = -1

\* The draining generation is always strictly older than the current generation.
\* Violation would mean the operator is draining something it is also creating.
Inv_DrainingOlderThanCurrent ==
    drainingGen # -1 => drainingGen < currentGen

\* Active generation is never newer than current generation.
Inv_GenOrder ==
    activeGen # -1 => activeGen =< currentGen

\* Combined safety predicate checked by TLC.
Safety ==
    /\ TypeOK
    /\ Inv_StableConsistency
    /\ Inv_ServiceValid
    /\ Inv_StableHasSTS
    /\ Inv_ActiveHasSTS
    /\ Inv_ServiceKnownGen
    /\ Inv_DrainingPhase
    /\ Inv_StableNoDraining
    /\ Inv_DrainingOlderThanCurrent
    /\ Inv_GenOrder

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* The engine eventually reaches stable phase.
\*
\* Requires:
\*   - SF on instance-gated reconcile actions (ReconcileInit, ReconcileStable_Drift,
\*     all ReconcileCreating_*): SF is required rather than WF because
\*     EnvSetInstanceReady(FALSE) has no fairness constraint and can toggle
\*     instanceReady back to FALSE immediately after every TRUE. With WF the
\*     gate-disabled state satisfies "not continuously enabled", letting WF
\*     fire vacuously forever. SF: if a gated action is enabled infinitely
\*     often (because instanceReady becomes TRUE infinitely often), it fires
\*     infinitely often -- progress is guaranteed.
\*   - WF on non-gated reconcile actions (Switching/Draining/Cleaning): these
\*     do not depend on instanceReady so WF is sufficient.
\*   - WF on environment actions that unblock progress:
\*       EnvSetInstanceReady(TRUE) -- instance will eventually become ready
\*       EnvPodsReady              -- pods will eventually become ready
\*       EnvPodsDrained            -- drain will eventually complete
\*
\* Without the environment fairness the engine can be stuck forever on a
\* permanently unready instance or pods that never start -- correct behavior.

EventuallyStable == <>(phase = "stable")

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

Spec ==
    /\ Init
    /\ [][Next]_vars
    \* Instance-gated actions: SF because instanceReady can toggle adversarially.
    /\ SF_vars(ReconcileInit)
    /\ SF_vars(ReconcileStable_Drift)
    /\ SF_vars(ReconcileCreating_SpecDrift)
    /\ SF_vars(ReconcileCreating_SpecDrift_AtMax)
    /\ SF_vars(ReconcileCreating_EnsureSTS)
    /\ SF_vars(ReconcileCreating_EnsureService)
    /\ SF_vars(ReconcileCreating_Advance)
    \* Non-gated actions: WF is sufficient.
    /\ WF_vars(ReconcileSwitching_UpdateService)
    /\ WF_vars(ReconcileSwitching_Complete)
    /\ WF_vars(ReconcileDraining_Complete)
    /\ WF_vars(ReconcileCleaning)
    /\ WF_vars(EnvPodsReady)
    /\ WF_vars(EnvPodsDrained)
    /\ WF_vars(EnvSetInstanceReady(TRUE))

\* Theorems (checked by TLC, provable by TLAPS for the infinite-state version)
THEOREM Spec => []Safety
THEOREM Spec => EventuallyStable

====
