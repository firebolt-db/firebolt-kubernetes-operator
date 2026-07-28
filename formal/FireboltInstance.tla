---- MODULE FireboltInstance ----
\* TLA+ specification of the FireboltInstance reconciler state machine.
\*
\* Models the three-phase lifecycle:
\*   Provisioning -> Ready <-> Degraded
\*
\* The reconciler runs a sequential three-step pipeline every reconcile:
\*   Postgres -> Metadata -> Gateway
\*
\* It stops at the first failing step and calls setInstanceReadyRollup before
\* every status write, so the phase always reflects ALL three conditions even
\* when the reconcile returned early.
\*
\* Verified properties:
\*   Safety                  - TypeOK invariant: valid phase and boolean
\*                             component flags
\*   PhaseReflectsComponents - every ReconcileRun writes a phase that reflects
\*                             ALL components, not a subset of them
\*   EventuallyReady         - once all components stabilise, phase reaches Ready
\*   ReadyIsStable           - Ready persists as long as all components remain
\*                             available
\*
\* To check with TLC:
\*   java -jar tla2tools.jar -config FireboltInstance.cfg FireboltInstance.tla
\*
\* Design decisions:
\*   - Phase is derived from the roll-up condition (computePhase +
\*     setInstanceReadyRollup). The roll-up is called even on early returns
\*     (writeStatusAndPoll), so the invariant Phase=Ready <=> AllReady holds
\*     AFTER every ReconcileRun firing, not necessarily in between.
\*   - EventuallyReady uses <>[]AllReady (permanent stability) rather than the
\*     simpler AllReady ~> ... (transient). The weaker precondition is unsound:
\*     an adversarial environment can degrade a component in the very next step
\*     after AllReady becomes true, before ReconcileRun gets to fire. Once any
\*     component is down, PhaseFrom returns the same phase (a stuttering step),
\*     so <<ReconcileRun>>_vars is never enabled inside the resulting cycle —
\*     neither WF nor SF can force it to fire. Under permanent stability AllReady
\*     stays true, so ReconcileRun is continuously vars-enabled and WF suffices.
\*   - WF_vars(ReconcileRun) is sufficient: under the <>[]AllReady precondition
\*     the vars-enabled condition is held continuously once reached, so WF fires
\*     it. SF is not required.
\*   - InstancePhaseFailed is intentionally excluded from this model. The real
\*     code preserves Failed if already set (e.g. via kubectl patch), but no
\*     internal reconciler transition produces it; it is therefore unreachable
\*     from the Init state and does not affect any modeled property.

EXTENDS Integers, TLC

CONSTANTS
    \* TRUE narrows the readiness roll-up to postgres alone — the early-return
    \* bug the design notes below say the implementation avoids (stopping at
    \* the first pipeline step and rolling up only what was checked). FALSE in
    \* FireboltInstance.cfg, which checks the design as it ships;
    \* FireboltInstanceNaiveRollup.cfg flips it and must then violate
    \* PhaseReflectsComponents (`make formal-check-counterexample` enforces
    \* this). Same shape as the guard-removal flags in FireboltEngine.tla.
    \*
    \* The property that makes this flag falsifiable is phrased over
    \* AllComponentsUp, never over AllReady: temporal properties stated in
    \* terms of AllReady move their hypothesis and conclusion together when
    \* the roll-up definition breaks, so they stay true about the broken
    \* predicate. AllComponentsUp is the ground truth the roll-up must match.
    RollupOneComponent

ASSUME RollupOneComponent \in BOOLEAN

Components == {"postgres", "metadata", "gateway"}

VARIABLES
    phase,    \* current FireboltInstance phase
    compAvail \* compAvail[c]: env-controlled readiness of each component

vars == <<phase, compAvail>>

Phases == {"uninitialized", "provisioning", "ready", "degraded"}

\* The ground truth: every component condition is True. Used ONLY by the
\* verified properties, never by the reconciler actions, so a broken roll-up
\* definition cannot drag the properties along with it.
AllComponentsUp == \A c \in Components : compAvail[c]

\* The readiness roll-up the reconciler computes. Models the output of
\* setInstanceReadyRollup that computePhase consumes. This is the definition
\* under test: PhaseReflectsComponents asserts that phases derived from it
\* agree with AllComponentsUp.
AllReady ==
    IF RollupOneComponent
    THEN compAvail["postgres"]
    ELSE \A c \in Components : compAvail[c]

\* Derive next phase from current component availability.
\* Models computePhase(instance) called after setInstanceReadyRollup.
PhaseFrom(oldPhase) ==
    IF AllReady THEN "ready"
    ELSE IF oldPhase \in {"ready", "degraded"} THEN "degraded"
    ELSE "provisioning"

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

Init ==
    /\ phase     = "uninitialized"
    /\ compAvail = [c \in Components |-> FALSE]

\* ---------------------------------------------------------------------------
\* Environment actions
\* ---------------------------------------------------------------------------

\* A component becomes available.
EnvComponentReady(c) ==
    /\ ~compAvail[c]
    /\ compAvail' = [compAvail EXCEPT ![c] = TRUE]
    /\ UNCHANGED phase

\* A component becomes unavailable (pod crash, network, etc.)
EnvComponentDegrades(c) ==
    /\ compAvail[c]
    /\ compAvail' = [compAvail EXCEPT ![c] = FALSE]
    /\ UNCHANGED phase

\* ---------------------------------------------------------------------------
\* Reconciler actions
\* ---------------------------------------------------------------------------

\* First reconcile: status.Phase is empty; seed it to Provisioning.
ReconcileInit ==
    /\ phase = "uninitialized"
    /\ phase' = "provisioning"
    /\ UNCHANGED compAvail

\* Main reconcile: evaluate sequential pipeline, call setInstanceReadyRollup,
\* call computePhase, write status.
\*
\* Modeled as a single atomic action because the real reconciler runs the full
\* pipeline in one goroutine and performs a single status write per reconcile.
ReconcileRun ==
    /\ phase \in {"provisioning", "ready", "degraded"}
    /\ phase' = PhaseFrom(phase)
    /\ UNCHANGED compAvail

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

Next ==
    \/ ReconcileInit
    \/ ReconcileRun
    \/ \E c \in Components :
           \/ EnvComponentReady(c)
           \/ EnvComponentDegrades(c)

\* ---------------------------------------------------------------------------
\* Safety invariants
\* ---------------------------------------------------------------------------

TypeOK ==
    /\ phase     \in Phases
    /\ compAvail \in [Components -> BOOLEAN]

Safety ==
    /\ TypeOK

\* ---------------------------------------------------------------------------
\* Action properties
\* ---------------------------------------------------------------------------

\* Every phase the reconciler writes reflects ALL components. This cannot be a
\* state invariant: between an environment flap and the next ReconcileRun the
\* phase legitimately lags the components (see the design decisions above), so
\* the honest statement is about ReconcileRun's postcondition, not about every
\* reachable state. Phrased over AllComponentsUp rather than AllReady on
\* purpose — see the RollupOneComponent comment.
\*
\*   * ready is written iff every component is available
\*   * degraded is only written on a genuine degradation from ready/degraded
\*   * provisioning is only re-written while still provisioning
PhaseReflectsComponents ==
    [][ReconcileRun =>
           /\ ((phase' = "ready") <=> AllComponentsUp')
           /\ ((phase' = "degraded") =>
                   (~AllComponentsUp' /\ phase \in {"ready", "degraded"}))
           /\ ((phase' = "provisioning") =>
                   (~AllComponentsUp' /\ phase = "provisioning"))]_vars

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* If all three components become and permanently stay available, phase eventually
\* reaches Ready.  The precondition <>[]AllReady (permanent stability) is needed
\* because a transiently-ready state is insufficient: the environment can degrade
\* a component before ReconcileRun fires, at which point ReconcileRun becomes a
\* stuttering step and no fairness condition can force it to make progress.
EventuallyReady == (<>[]AllReady) => <>(phase = "ready")

\* Once Ready with all components available, phase stays Ready for as long as all
\* components remain permanently available.  The consequent is ([]AllReady =>
\* [](phase = "ready")) rather than [](AllReady => (phase = "ready")) because
\* the reconciler only enforces Phase=Ready iff AllReady AFTER a ReconcileRun
\* fires.  A transient state where AllReady is true but phase is still "degraded"
\* (component recovered before the next reconcile) is valid and does not violate
\* this property.
ReadyIsStable == [](((phase = "ready") /\ AllReady) => ([]AllReady => [](phase = "ready")))

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

Spec ==
    /\ Init
    /\ [][Next]_vars
    /\ WF_vars(ReconcileInit)
    /\ WF_vars(ReconcileRun)
    /\ \A c \in Components : WF_vars(EnvComponentReady(c))

\* Theorems (checked by TLC)
THEOREM Spec => []Safety
THEOREM Spec => PhaseReflectsComponents
THEOREM Spec => EventuallyReady
THEOREM Spec => ReadyIsStable

====
