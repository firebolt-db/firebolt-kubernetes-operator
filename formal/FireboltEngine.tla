---- MODULE FireboltEngine ----
\* TLA+ specification of the FireboltEngine reconciler state machine.
\*
\* Models the six-phase blue-green lifecycle:
\*   stable/stopped -> creating -> switching -> draining -> cleaning -> stable/stopped
\*
\* Verified properties:
\*   Safety  - invariants that must hold in every reachable state
\*   Liveness - engine always eventually reaches a terminal phase (with fairness)
\*
\* To check with TLC:
\*   1. Open FireboltEngine.cfg alongside this file
\*   2. Run: java -jar tla2tools.jar -config FireboltEngine.cfg FireboltEngine.tla
\*   3. Or use the TLA+ Toolbox / VS Code extension
\*
\* Design decisions captured here:
\*   - The instance gate is a SCHEDULING guard (outer Reconcile), not a
\*     precondition on compute* functions. When instanceReady is false and
\*     phase in {stable, stopped, creating}, the state machine does not tick --
\*     only conditions are updated. Switching/Draining/Cleaning bypass the gate.
\*   - Each reconcile call is modeled as one atomic step. This is conservative
\*     (the real code makes multiple K8s writes per reconcile) but correct:
\*     safety violations found here are real; absence of violations holds in
\*     the coarser implementation too. The Phase 6 rapid harness goes finer-
\*     grained: it crashes the simulated apply at each of the 9 MaybeCrash
\*     points enumerated in crash_points.go, which form prefixes
\*     k in {1..5} of the production order
\*     [ConfigMap, HeadlessSvc, STS, ClusterSvc, Deletes, Status]. Each
\*     intermediate prefix is already a reachable model state — the spec
\*     allows resources to be in any subset present — so no spec extension
\*     is required; the harness narrows to recovery from concrete prefixes.
\*   - podsReady is a boolean abstraction of "all pods in currentGen are ready".
\*     It is reset to FALSE whenever currentGen is bumped. For spec.replicas=0
\*     the real code returns allReady=true vacuously; the model still requires
\*     EnvPodsReady to fire, which is sound (a superset of real behaviors).
\*   - podsDrained is a boolean abstraction of "draining gen has zero queries".
\*     It is reset to FALSE whenever drainingGen is set.
\*   - stsSpecVer[g] = -1 means no STS for generation g exists.
\*     stsSpecVer[g] >= 0 means the STS exists and was built from spec version g.
\*   - specWantsStop is a boolean abstraction of "current spec.replicas == 0".
\*     It can toggle atomically with EnvChangeSpec (the user edits replicas).
\*     The reconciler consults it only at terminal-phase writes, via
\*     TerminalPhase: zero-replica specs land in "stopped", non-zero in
\*     "stable". Drift detection and re-materialization treat "stopped"
\*     identically to "stable".
\*   - FireboltEngineClass and FireboltEnginePreset edits are modeled
\*     implicitly by specVer increments. The reconciler's class and Preset
\*     watches plus stsMatchesSpec hash comparison make a class or Preset
\*     spec edit observationally identical to a FireboltEngine spec edit:
\*     both flip StsMatchesSpec(g) to FALSE and the next reconcile bumps
\*     currentGeneration through the same code paths. The Go fidelity check
\*     uses ServiceAccountName as the version carrier in the rapid harness
\*     (engine_property_test.go) and exercises class- and Preset-hash
\*     drift in the merge unit tests. No per-source TLA+ variable is
\*     introduced: from the model's perspective both overlays are inputs
\*     to the spec-content hash specVer abstracts over.
\*   - The Preset fail-closed gate is NOT the same as a spec edit. When
\*     Preset is required-and-missing or Ready=False for an
\*     operator-owned template path, the outer Reconcile refuses to call
\*     computeEngineReconcile — the same scheduling window as instanceReady
\*     and classReady. That is presetReady. Merge content stays UNMODELLED.
\*     (A second Preset per namespace is impossible: the CRD CEL rule pins
\*     metadata.name, so there is no ambiguity state to model.)

EXTENDS Integers, TLC

CONSTANTS
    MaxGen,     \* upper bound on generation numbers (e.g. 2)
    MaxSpec,    \* upper bound on spec versions (e.g. 2)
    \* The six flags below each remove ONE shipped guard. All are FALSE in
    \* FireboltEngine.cfg -- that config checks the design as it ships. Each has a
    \* FireboltEngineNaive*.cfg that flips exactly one of them and names the
    \* invariant TLC must then report; `make formal-check-counterexample` runs
    \* them and fails if any stops failing. Same shape as AnchorAtDemotion in
    \* SigningKeyRotation.tla.
    \*
    \* They exist because a passing invariant is only evidence if it could have
    \* failed. Four of the twelve invariants in this spec are falsifiable by
    \* removing a guard, as is the no-delete-of-the-current-generation action
    \* property, and those five are what the flags here cover.
    \*
    \* Inv_TerminalHasSTS was falsifiable by a GCCurrentGeneration flag that
    \* dropped GCOrphans' `g # gcView` exclusion, until the generation floor
    \* subsumed that exclusion: with the floor in place, dropping it changes no
    \* reachable state, and TLC said so by no longer reporting the violation. The
    \* exclusion stays in the action as defense in depth and the flag is gone.
    \* Inv_TerminalHasSTS therefore has no independent falsifier any more: in a
    \* terminal phase activeGen = currentGen, so the active-generation exclusion
    \* alone keeps the current generation's StatefulSet alive whichever GC flag is
    \* flipped. NoDeleteOfCurrentGeneration covers the same hazard more strongly,
    \* forbidding the delete in every phase rather than only in a terminal one,
    \* but it is an action property and not that invariant's control.
    \*
    \* The other invariants are not falsifiable by a dropped guard, for reasons worth writing down rather than leaving as a gap:
    \* Inv_GenOrder and Inv_DrainingOlderThanCurrent hold structurally (activeGen
    \* and drainingGen are only ever assigned currentGen or a smaller gen, so no
    \* guard removal reaches them -- only a mutated WRITE would);
    \* Inv_TerminalNoDraining is implied by Inv_DrainingPhase, since
    \* TerminalPhases and {draining, cleaning} are disjoint;
    \* Inv_TerminalConsistency and Inv_QuiescedPhaseMatchesSpec likewise need a
    \* changed assignment rather than a dropped conjunct; and TypeOK cannot be
    \* violated by any guard change. The four needing a mutated write are NOT
    \* pinned anywhere today: formal/mutants/manifest.tsv mutates the Go
    \* reconciler, not this spec, and none of its rows targets them. Covering
    \* them takes the AnchorAtDemotion flag shape (change a write, not drop a
    \* guard) and is a deliberate follow-up, not coverage that already exists.
    SwitchWithoutService,
      \* TRUE removes the cutover gate in ReconcileSwitching_Complete: the
      \* switch finalises even though the cluster Service still points at an
      \* older generation. Violates Inv_ServiceKnownGen.
    GCIgnoresActiveGen,
      \* TRUE removes GCOrphans' `g # activeGen` exclusion, which in the
      \* implementation is the ActiveGeneration entry in gcOrphanedResources'
      \* keepGens set. The sweep runs in every phase, and mid-rollout the
      \* still-serving generation is activeGen while currentGen is the one being
      \* built, so without that exclusion GC deletes the StatefulSet traffic is
      \* landing on. Violates Inv_ActiveHasSTS.
    GCIgnoresGenerationFloor,
      \* TRUE removes GCOrphans' generation floor, the `g < NewestKept(...)`
      \* conjunct, which is the guard that makes a stale keep set harmless. With
      \* it removed, a sweep whose view of currentGeneration lags deletes the
      \* StatefulSet of the generation being created. Violates
      \* NoDeleteOfCurrentGeneration, an action property rather than an invariant
      \* or a liveness property, because destroying a generation mid-creation
      \* leaves every state invariant intact and the reconciler re-creates what
      \* was removed, so the model still converges. That is exactly why the
      \* implementation shipped the hazard.
    GCDrainingGeneration,
      \* TRUE removes GCOrphans' `g # drainingGen` exclusion, so the sweep takes
      \* the StatefulSet the drain is waiting on out from under it. Only
      \* reachable because the sweep runs in every phase; with the old
      \* terminal-phase gate the conjunct was vacuous, since a terminal phase has
      \* nothing draining. Violates Inv_DrainingHasSTS.
    AdvanceWithoutMatchingSTS,
      \* TRUE removes `StsMatchesSpec(currentGen)` from ReconcileCreating_Advance,
      \* so creating advances to switching before the new generation's
      \* StatefulSet exists; the Service selector is then flipped to a generation
      \* that has none. Violates Inv_ServiceValid.
    DriftDuringDrain
      \* TRUE lets ReconcileTerminal_Drift fire in draining/cleaning, modelling
      \* drift detection that is not gated on phase. Starting a new generation
      \* mid-drain abandons drainingGen, leaking the StatefulSet it was going to
      \* delete. Violates Inv_DrainingPhase.

Gens     == 0..MaxGen
SpecVers == 0..MaxSpec

Phases == {"uninitialized", "stable", "creating", "switching", "draining", "cleaning", "stopped"}
TerminalPhases == {"stable", "stopped"}

\* Phase gate, widened by a counterexample flag. Written as a set rather than an
\* inline disjunction so the guard at the use site still reads as one membership
\* test and the shipped behaviour is what you get with every flag FALSE.
DriftPhases == IF DriftDuringDrain
               THEN TerminalPhases \cup {"draining", "cleaning"}
               ELSE TerminalPhases

VARIABLES
    phase,          \* current reconciler phase
    currentGen,     \* generation being created / most recently created
    activeGen,      \* generation currently serving traffic  (-1 = none)
    drainingGen,    \* generation being drained              (-1 = none)
    specVer,        \* current spec version (env-controlled; drives rollouts)
    specWantsStop,  \* TRUE when spec.replicas == 0 for the current specVer
    stsSpecVer,     \* stsSpecVer[g]: spec version STS-g was built from, -1 if absent
    svcTargetGen,   \* generation the cluster Service selector points to (-1 = no service)
    podsReady,      \* TRUE when all pods in currentGen are Running+Ready
    podsDrained,    \* TRUE when draining gen has zero running/suspended queries
    instanceReady,  \* TRUE when the referenced FireboltInstance is Ready (env-controlled)
    classReady,     \* TRUE when the referenced FireboltEngineClass is Ready (env-controlled)
    presetReady   \* TRUE when FireboltEnginePreset is admissible for render (env-controlled)

vars == <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
          stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady, classReady, presetReady>>

\* ---------------------------------------------------------------------------
\* Helpers
\* ---------------------------------------------------------------------------

StsExists(g)       == stsSpecVer[g] # -1
StsMatchesSpec(g)  == StsExists(g) /\ stsSpecVer[g] = specVer

\* Terminal phase selector. Mirrors terminalPhase(spec) in engine_reconcile.go:
\* replicas==0 -> stopped, otherwise stable. The single source of truth for the
\* stable-vs-stopped distinction; every "reconcile is done" write funnels
\* through this helper, so any drift between the two terminals is a bug.
TerminalPhase == IF specWantsStop THEN "stopped" ELSE "stable"

\* Outer-Reconcile scheduling gate for {stable, stopped, creating}.
\* Mirrors resolveFireboltEngineClassInfo + resolveFireboltEnginePresetInfo
\* plus the instance-Ready check: the compute layer runs only when all three
\* are open. Switching/Draining/Cleaning ignore this helper.
RenderGatesOpen == instanceReady /\ classReady /\ presetReady

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

Init ==
    /\ phase         = "uninitialized"
    /\ currentGen    = 0
    /\ activeGen     = -1
    /\ drainingGen   = -1
    /\ specVer       = 0
    /\ specWantsStop = FALSE
    /\ stsSpecVer    = [g \in Gens |-> -1]
    /\ svcTargetGen  = -1
    /\ podsReady     = FALSE
    /\ podsDrained   = TRUE
    /\ instanceReady = TRUE
    /\ classReady    = TRUE
    /\ presetReady = TRUE

\* ---------------------------------------------------------------------------
\* Environment actions  (non-deterministic; can fire at any time)
\* ---------------------------------------------------------------------------

\* User changes the engine spec (e.g. scales replicas, changes image) and
\* may also change whether the new spec wants stop (replicas == 0). The
\* two dimensions are independent -- an image change keeps the previous
\* specWantsStop; a scale-to-zero flips it; a scale-from-zero flips it
\* back. A single non-deterministic action covers all combinations.
EnvChangeSpec ==
    /\ specVer < MaxSpec
    /\ specVer' = specVer + 1
    /\ specWantsStop' \in BOOLEAN
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady, classReady, presetReady>>

\* Pods in currentGen become all-ready. For spec.replicas=0 this fires
\* trivially (0/0 pods ready) in the real code; here we require the env
\* to fire EnvPodsReady regardless, which is a sound over-approximation.
EnvPodsReady ==
    /\ ~podsReady
    /\ podsReady' = TRUE
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsDrained, instanceReady, classReady, presetReady>>

\* Pods in drainingGen finish draining (zero running/suspended queries)
EnvPodsDrained ==
    /\ ~podsDrained
    /\ podsDrained' = TRUE
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsReady, instanceReady, classReady, presetReady>>

\* Instance becomes ready or not-ready
EnvSetInstanceReady(v) ==
    /\ instanceReady # v
    /\ instanceReady' = v
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, classReady, presetReady>>

\* FireboltEngineClass becomes ready or not-ready. Symmetric to
\* EnvSetInstanceReady: models the class-Ready gate
\* (resolveFireboltEngineClassInfo refuses to consume a class whose
\* Ready=False/OperatorOwnedFieldSet condition is set; Reconcile then
\* surfaces ConditionReady=False/FireboltEngineClassUnready on the engine
\* without rendering a StatefulSet). The gate fires at the same compute*
\* entry as instanceReady, so every action that respects instanceReady
\* also respects classReady. Switching/Draining/Cleaning intentionally
\* bypass the gate (they do not re-resolve the class), matching the
\* implementation.
EnvSetClassReady(v) ==
    /\ classReady # v
    /\ classReady' = v
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady, presetReady>>

\* FireboltEnginePreset becomes admissible or not. Symmetric to
\* EnvSetClassReady: models the Preset fail-closed gate
\* (resolveFireboltEnginePresetInfo refuses required-and-missing
\* or Ready=False/OperatorOwnedFieldSet; Reconcile then
\* surfaces ConditionReady=False/FireboltEnginePreset{Required,
\* Unready} without rendering a StatefulSet). Missing Ready,
\* or Ready=False/DeletionBlocked, is admissible — same as class.
\* Switching/Draining/Cleaning bypass the gate.
EnvSetPresetReady(v) ==
    /\ presetReady # v
    /\ presetReady' = v
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady, classReady>>

\* Atomic env action that drives instanceReady, classReady, and
\* presetReady to TRUE in a single step. Used purely for liveness:
\* independent WF on the per-flag (TRUE) actions only guarantees each
\* flag is TRUE infinitely often, not simultaneously, so TLC can find
\* a behavior where the flags alternate and the gated reconcile never
\* opens. WF on EnvSetGatesOpen forces a moment where all three gates
\* are open, satisfying the SF on the gated reconcile actions.
EnvSetGatesOpen ==
    /\ \/ instanceReady = FALSE
       \/ classReady    = FALSE
       \/ presetReady = FALSE
    /\ instanceReady' = TRUE
    /\ classReady'    = TRUE
    /\ presetReady' = TRUE
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained>>

\* ---------------------------------------------------------------------------
\* Reconciler actions
\* ---------------------------------------------------------------------------

\* ------ Phase: uninitialized ------
\* First sight of a new FireboltEngine: seed phase=creating, activeGen=-1.
\* Mirrors the phase=="" early-return in engine_controller.go:Reconcile.

ReconcileInit ==
    /\ phase = "uninitialized"
    /\ RenderGatesOpen                        \* instance + class + defaults gates apply
    /\ phase'      = "creating"
    /\ currentGen' = 0
    /\ activeGen'  = -1
    /\ podsReady'  = FALSE
    /\ UNCHANGED <<drainingGen, specVer, specWantsStop, stsSpecVer, svcTargetGen, podsDrained, instanceReady, classReady, presetReady>>

\* ------ Phase: stable / stopped (terminal) ------
\* Detect spec drift or missing STS; start a new generation if needed.
\* When everything is consistent, the reconciler does nothing (stutters).
\*
\* Both terminals share drift-detection and GC behavior; only the surfaced
\* name differs. Mirrors the engine_reconcile.go switch: PhaseStopped is
\* routed into computeStable alongside PhaseStable and "".

ReconcileTerminal_Drift ==
    \* Spec changed or STS missing -> bump currentGen, go to creating.
    \* This is the only path out of a terminal phase.
    /\ phase \in DriftPhases
    /\ RenderGatesOpen
    /\ ~StsMatchesSpec(currentGen)
    /\ currentGen < MaxGen
    /\ currentGen' = currentGen + 1
    /\ phase'      = "creating"
    /\ podsReady'  = FALSE
    /\ UNCHANGED <<activeGen, drainingGen, specVer, specWantsStop, stsSpecVer, svcTargetGen, podsDrained, instanceReady, classReady, presetReady>>

\* The newest generation a sweep is keeping, given the currentGeneration it
\* observed: the floor at or above which it may delete nothing. activeGen and
\* drainingGen are -1 when unset, which never wins the maximum because gcView is
\* never negative.
\*
\* Only gcView is modelled as possibly stale. The implementation reads all three
\* from one status object, so a real keep set is stale in all three at once; this
\* is the coarser statement, and the floor it feeds is what the guard rests on.
Max2(a, b) == IF a > b THEN a ELSE b
NewestKept(gcView) == Max2(Max2(gcView, activeGen), drainingGen)

\* GC: delete STSes that belong to none of currentGen, activeGen, drainingGen.
\* Runs opportunistically in every phase; safe to repeat. Keeping activeGen is
\* what makes the phase gate unnecessary: mid-rollout it is the generation
\* serving traffic, and an engine that never reaches a terminal phase is
\* precisely the one whose abandoned generations accumulate.
\* Unguarded on instanceReady, classReady, and presetReady, unlike every
\* reconciler action below: reclaiming an abandoned generation needs neither a
\* ready instance nor a resolvable class or Preset object. Models
\* gcOrphanedResources() in engine_gc.go, which the
\* top-level Reconcile defers so it runs on the way out of every pass, including
\* the passes those gates end early.
GCOrphans ==
    \* gcView is the currentGeneration this pass observes, and it is allowed to
    \* be any generation up to the real one: the keep set is built from a status
    \* read, and a read can be behind the writes of earlier passes. That is not a
    \* hypothetical -- it is what a cached read does under generation churn.
    \*
    \* Bounded above by currentGen because the implementation never sweeps on a
    \* view it cannot vouch for: it reads the status from the API server, and a
    \* read that fails ends the pass instead of falling back to the status the
    \* reconcile was handed. That status can be ahead of the API server, since a
    \* failed status write leaves a generation in memory that was never accepted,
    \* and a view above currentGen would put the real current generation below the
    \* floor.
    /\ \E gcView \in 0..currentGen :
       \E g \in Gens :
           /\ StsExists(g)
           \* Generation floor. Abandoned generations are strictly older than the
           \* keep set, because once currentGeneration moves on nothing recreates
           \* an older one; a generation at or above the newest kept one can only
           \* mean this view is stale. Refusing it costs a delayed reclaim, which
           \* the next pass makes good.
           /\ (g < NewestKept(gcView) \/ GCIgnoresGenerationFloor)
           \* Subsumed by the floor above, kept as defense in depth. See the
           \* constant block for why it carries no counterexample flag.
           /\ g # gcView
           /\ (g # activeGen \/ GCIgnoresActiveGen)
           \* activeGen and drainingGen are -1 when unset, never a gen in Gens.
           /\ (g # drainingGen \/ GCDrainingGeneration)
           /\ stsSpecVer' = [stsSpecVer EXCEPT ![g] = -1]
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   svcTargetGen, podsReady, podsDrained, instanceReady, classReady, presetReady>>

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
    /\ RenderGatesOpen
    /\ StsExists(currentGen) /\ ~StsMatchesSpec(currentGen)
    /\ currentGen < MaxGen
    /\ currentGen'  = currentGen + 1
    /\ stsSpecVer'  = [stsSpecVer EXCEPT ![currentGen] = -1]
    /\ podsReady'   = FALSE
    /\ UNCHANGED <<phase, activeGen, drainingGen, specVer, specWantsStop, svcTargetGen, podsDrained, instanceReady, classReady, presetReady>>

ReconcileCreating_SpecDrift_AtMax ==
    \* Boundary case: spec drifted but currentGen is already at the model ceiling.
    \* Delete the stale STS so EnsureSTS can rebuild it at the new specVer.
    \* podsReady is reset to FALSE: the old pods are gone with the deleted STS.
    /\ phase = "creating"
    /\ RenderGatesOpen
    /\ StsExists(currentGen) /\ ~StsMatchesSpec(currentGen)
    /\ currentGen = MaxGen
    /\ stsSpecVer'  = [stsSpecVer EXCEPT ![currentGen] = -1]
    /\ podsReady'   = FALSE
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   svcTargetGen, podsDrained, instanceReady, classReady, presetReady>>

ReconcileCreating_EnsureSTS ==
    \* Create the StatefulSet for currentGen (also creates ConfigMap + headless Service
    \* in the real code; omitted here as they don't affect the phase state machine).
    /\ phase = "creating"
    /\ RenderGatesOpen
    /\ ~StsExists(currentGen)                                   \* STS absent
    /\ ~(StsExists(currentGen) /\ ~StsMatchesSpec(currentGen)) \* no spec drift
    /\ stsSpecVer' = [stsSpecVer EXCEPT ![currentGen] = specVer]
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   svcTargetGen, podsReady, podsDrained, instanceReady, classReady, presetReady>>

ReconcileCreating_EnsureService ==
    \* Create the cluster Service when it does not yet exist (first deployment only;
    \* on subsequent rollouts the service already exists from the previous generation).
    \* The service initially points to currentGen and is updated in switching.
    /\ phase = "creating"
    /\ RenderGatesOpen
    /\ StsMatchesSpec(currentGen)
    /\ svcTargetGen = -1
    /\ svcTargetGen' = currentGen
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, podsReady, podsDrained, instanceReady, classReady, presetReady>>

ReconcileCreating_Advance ==
    \* STS exists, service exists, pods ready -> transition to switching.
    /\ phase = "creating"
    /\ RenderGatesOpen
    /\ (StsMatchesSpec(currentGen) \/ AdvanceWithoutMatchingSTS)
    /\ svcTargetGen # -1
    /\ podsReady
    /\ phase' = "switching"
    /\ UNCHANGED <<currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady, classReady, presetReady>>

\* ------ Phase: switching ------
\* Two sub-steps (matches computeSwitching):
\*   1. Flip the cluster Service selector to currentGen (if not already there).
\*   2. Once selector is confirmed, update activeGen and decide next phase.

ReconcileSwitching_UpdateService ==
    \* Flip the service selector to point at the new generation.
    /\ phase = "switching"
    /\ svcTargetGen # currentGen
    /\ svcTargetGen' = currentGen
    /\ UNCHANGED <<phase, currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, podsReady, podsDrained, instanceReady, classReady, presetReady>>

ReconcileSwitching_Complete ==
    \* Service already points to currentGen: finalise the switch.
    \* If there is an old generation to drain, go to draining; otherwise
    \* (first deployment, activeGen = -1) go directly to a terminal phase
    \* chosen by TerminalPhase (stable or stopped).
    /\ phase = "switching"
    /\ (svcTargetGen = currentGen \/ SwitchWithoutService)
    /\ activeGen' = currentGen
    /\ \/ \* First deployment: no old generation to drain.
          /\ activeGen = -1
          /\ phase'       = TerminalPhase
          /\ drainingGen' = drainingGen   \* unchanged (-1)
          /\ UNCHANGED podsDrained
       \/ \* Rollout: old generation must drain before cleanup.
          /\ activeGen >= 0 /\ activeGen # currentGen
          /\ phase'       = "draining"
          /\ drainingGen' = activeGen
          /\ podsDrained' = FALSE         \* reset; new draining target
    /\ UNCHANGED <<currentGen, specVer, specWantsStop, stsSpecVer, svcTargetGen, podsReady, instanceReady, classReady, presetReady>>

\* ------ Phase: draining ------
\* Wait for drain completion, then go to cleaning.
\* The "not yet drained" case is handled by TLA+ stuttering (no explicit action needed).

ReconcileDraining_Complete ==
    /\ phase = "draining"
    /\ drainingGen # -1
    /\ podsDrained
    /\ phase' = "cleaning"
    /\ UNCHANGED <<currentGen, activeGen, drainingGen, specVer, specWantsStop,
                   stsSpecVer, svcTargetGen, podsReady, podsDrained, instanceReady, classReady, presetReady>>

\* ------ Phase: cleaning ------
\* Delete old-generation resources and return to a terminal phase (stable or
\* stopped, chosen by TerminalPhase based on current spec.replicas).

ReconcileCleaning ==
    /\ phase = "cleaning"
    /\ drainingGen # -1
    /\ stsSpecVer'  = [stsSpecVer EXCEPT ![drainingGen] = -1]
    /\ drainingGen' = -1
    /\ phase'       = TerminalPhase
    /\ UNCHANGED <<currentGen, activeGen, specVer, specWantsStop,
                   svcTargetGen, podsReady, podsDrained, instanceReady, classReady, presetReady>>

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

Next ==
    \/ EnvChangeSpec
    \/ EnvPodsReady
    \/ EnvPodsDrained
    \/ EnvSetInstanceReady(TRUE)
    \/ EnvSetInstanceReady(FALSE)
    \/ EnvSetClassReady(TRUE)
    \/ EnvSetClassReady(FALSE)
    \/ EnvSetPresetReady(TRUE)
    \/ EnvSetPresetReady(FALSE)
    \/ EnvSetGatesOpen
    \/ ReconcileInit
    \/ ReconcileTerminal_Drift
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
    /\ phase         \in Phases
    /\ currentGen    \in Gens
    /\ activeGen     \in {-1} \cup Gens
    /\ drainingGen   \in {-1} \cup Gens
    /\ specVer       \in SpecVers
    /\ specWantsStop \in BOOLEAN
    /\ stsSpecVer    \in [Gens -> {-1} \cup SpecVers]
    /\ svcTargetGen  \in {-1} \cup Gens
    /\ podsReady     \in BOOLEAN
    /\ podsDrained   \in BOOLEAN
    /\ instanceReady \in BOOLEAN
    /\ classReady    \in BOOLEAN
    /\ presetReady \in BOOLEAN

\* Matches user-confirmed invariant from code review:
\* "Any persistent CurrentGeneration != ActiveGeneration while the engine is in
\*  a terminal phase would indicate a state-machine bug."
\* Applies to both terminal phases: stable and stopped are structurally
\* identical, only the surfaced name differs.
Inv_TerminalConsistency ==
    phase \in TerminalPhases => currentGen = activeGen

\* The cluster Service always points to a generation whose STS exists,
\* once traffic has been switched (activeGen != -1).
\* During the first deployment the service is pre-populated while still in
\* creating phase (activeGen=-1) so that a spec-drift bump does not require
\* re-creating the service; no real traffic flows until activeGen is set.
\* After the first switch this guard is always enforced.
Inv_ServiceValid ==
    activeGen # -1 => StsExists(svcTargetGen)

\* In any terminal phase the current generation's STS must exist.
\* A stopped engine keeps its zero-replica STS around (see operator-based-scaling.md);
\* its absence would mean the terminal-phase invariants are violated.
Inv_TerminalHasSTS ==
    phase \in TerminalPhases => StsExists(currentGen)

\* The active generation's STS must always exist (once set).
\* Violation would mean serving traffic to a deleted StatefulSet.
Inv_ActiveHasSTS ==
    activeGen # -1 => StsExists(activeGen)

\* While a generation is draining, its StatefulSet must exist: the drain is
\* waiting on that generation's pods to finish serving. GCOrphans excluding
\* drainingGen is what holds this, now that the sweep runs in every phase.
\* Deleting it early cuts in-flight queries and skips the drain, because
\* computeCleaning then finds nothing to delete and moves straight to a terminal
\* phase.
\*
\* Scoped to the draining phase, and not to drainingGen # -1, because the wider
\* form is false in the implementation rather than merely unmodelled:
\* applyEngineState issues its deletes before writing status, so a crash in
\* cleaning leaves the draining generation's StatefulSet gone with drainingGen
\* still set. Draining is the phase where the sweep is the only thing that could
\* remove that StatefulSet, which is what this conjunct is for.
Inv_DrainingHasSTS ==
    phase = "draining" => (drainingGen = -1 \/ StsExists(drainingGen))

\* The service selector only points to activeGen or currentGen, once traffic has
\* been switched (activeGen != -1).
\* Before the first switch (activeGen=-1) spec-drift bumps can leave svcTargetGen
\* pointing to a stale gen; no real traffic flows and the selector is corrected in
\* switching phase. After the first switch this always holds.
Inv_ServiceKnownGen ==
    activeGen # -1 => svcTargetGen \in {activeGen, currentGen}

\* DrainingGeneration is only set while in draining or cleaning phase.
\* A non-nil drainingGen in any terminal phase or in stable/creating/switching indicates a leak.
Inv_DrainingPhase ==
    drainingGen # -1 => phase \in {"draining", "cleaning"}

\* In any terminal phase there is no draining generation.
Inv_TerminalNoDraining ==
    phase \in TerminalPhases => drainingGen = -1

\* The draining generation is always strictly older than the current generation.
\* Violation would mean the operator is draining something it is also creating.
Inv_DrainingOlderThanCurrent ==
    drainingGen # -1 => drainingGen < currentGen

\* Active generation is never newer than current generation.
Inv_GenOrder ==
    activeGen # -1 => activeGen =< currentGen

\* Once the reconciler has quiesced -- in a terminal phase with no pending
\* spec drift -- the phase name matches the spec's replicas=0 intent.
\* If the engine reached phase=stable while specWantsStop=TRUE (with the
\* current STS matching the current spec), users would see "stable" on a
\* spec that asked for zero replicas: a silent contract violation.
\*
\* The invariant is gated on StsMatchesSpec because mid-drift (after an
\* EnvChangeSpec that bumped both specVer and specWantsStop but before
\* ReconcileTerminal_Drift fires) the terminal phase legitimately lags
\* behind the new spec. That lag is exactly what drift detection is for;
\* the invariant applies only once reconciliation has caught up.
Inv_QuiescedPhaseMatchesSpec ==
    (phase \in TerminalPhases /\ StsMatchesSpec(currentGen)) =>
        ((phase = "stopped") = specWantsStop)

\* Combined safety predicate checked by TLC.
Safety ==
    /\ TypeOK
    /\ Inv_TerminalConsistency
    /\ Inv_ServiceValid
    /\ Inv_TerminalHasSTS
    /\ Inv_ActiveHasSTS
    /\ Inv_DrainingHasSTS
    /\ Inv_ServiceKnownGen
    /\ Inv_DrainingPhase
    /\ Inv_TerminalNoDraining
    /\ Inv_DrainingOlderThanCurrent
    /\ Inv_GenOrder
    /\ Inv_QuiescedPhaseMatchesSpec

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* The engine eventually reaches a terminal phase (stable or stopped).
\*
\* "Terminal" rather than "stable" because a zero-replica spec legitimately
\* quiesces in "stopped"; asserting EventuallyStable would rule that out.
\* Both terminals are fixed points of the state machine (no outgoing
\* transitions except on fresh spec drift), so either is acceptable convergence.
\*
\* Requires:
\*   - SF on instance/class/defaults-gated reconcile actions (ReconcileInit,
\*     ReconcileTerminal_Drift, all ReconcileCreating_*): SF is required rather
\*     than WF because EnvSetInstanceReady(FALSE) / EnvSetClassReady(FALSE) /
\*     EnvSetPresetReady(FALSE) have no fairness constraint and can toggle
\*     a gate back to FALSE immediately after every TRUE. With WF the
\*     gate-disabled state satisfies "not continuously enabled", letting WF
\*     fire vacuously forever. SF: if a gated action is enabled infinitely
\*     often (because EnvSetGatesOpen becomes enabled infinitely often), it
\*     fires infinitely often -- progress is guaranteed.
\*   - WF on non-gated reconcile actions (Switching/Draining/Cleaning): these
\*     do not depend on instanceReady so WF is sufficient.
\*   - WF on environment actions that unblock progress:
\*       EnvSetInstanceReady(TRUE) -- instance will eventually become ready
\*       EnvPodsReady              -- pods will eventually become ready
\*       EnvPodsDrained            -- drain will eventually complete
\*
\* Without the environment fairness the engine can be stuck forever on a
\* permanently unready instance or pods that never start -- correct behavior.

EventuallyTerminal == <>(phase \in TerminalPhases)

\* ---------------------------------------------------------------------------
\* Action properties
\* ---------------------------------------------------------------------------

\* Nothing may delete the StatefulSet of the generation the engine is on without
\* moving the engine off it in the same step. The abandon path qualifies: it
\* deletes currentGen's StatefulSet and bumps currentGen together. A sweep does
\* not, so this is the floor's contract, stated as an action property because the
\* hazard leaves every state invariant intact -- deleting a generation
\* mid-creation is indistinguishable, state by state, from not having created it
\* yet, which is why the implementation shipped it and no invariant complained.
\*
\* EventuallyTerminal does not cover it either: the reconciler re-creates what the
\* sweep removed, and strong fairness on the creating-phase actions is enough to
\* reach a terminal phase anyway. The model converges where the implementation
\* burned hundreds of generations an hour, so the property has to name the step
\* rather than the outcome.
\*
\* Scoped to a StatefulSet that still matches the spec, which is what separates
\* the two kinds of delete without needing an action marker. An abandon deletes a
\* generation precisely because it has drifted, including the MaxGen alias where
\* ReconcileCreating_SpecDrift_AtMax deletes in place and reuses the bound instead
\* of growing the state space. A sweep deletes whatever its keep set failed to
\* cover, drift or not, so a sweep that takes the generation being built is caught
\* at every generation including that bound.
NoDeleteOfCurrentGeneration ==
    [][ (/\ stsSpecVer[currentGen] # -1
         /\ stsSpecVer'[currentGen] = -1
         /\ StsMatchesSpec(currentGen))
        => currentGen' # currentGen ]_vars

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

Spec ==
    /\ Init
    /\ [][Next]_vars
    \* Instance-, class-, and Preset-gated actions: SF because the three
    \* readiness flags can toggle adversarially. Same SF-vs-WF reasoning
    \* as the instance gate above.
    /\ SF_vars(ReconcileInit)
    /\ SF_vars(ReconcileTerminal_Drift)
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
    /\ WF_vars(EnvSetClassReady(TRUE))
    /\ WF_vars(EnvSetPresetReady(TRUE))
    /\ WF_vars(EnvSetGatesOpen)

\* Theorems (checked by TLC, provable by TLAPS for the infinite-state version)
THEOREM Spec => []Safety
THEOREM Spec => EventuallyTerminal

====
