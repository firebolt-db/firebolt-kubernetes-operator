---- MODULE FireboltInstance ----
\* TLA+ specification of the FireboltInstance reconciler state machine.
\*
\* Models the three-phase lifecycle:
\*   Provisioning -> Ready <-> Degraded  (terminal: Failed)
\*
\* The reconciler runs a sequential four-step pipeline every reconcile:
\*   Postgres -> Metadata -> Account -> Gateway
\*
\* It stops at the first failing step and calls setInstanceReadyRollup before
\* every status write, so the phase always reflects ALL four conditions even
\* when the reconcile returned early.
\*
\* Verified properties:
\*   Safety  - Failed can only be reached via a terminal account error
\*   EventuallyFailed - a terminal error is always reflected in Failed phase
\*   FailedIsTerminal - Failed is a trap; no transition away from it
\*
\* To check with TLC:
\*   java -jar tla2tools.jar -config FireboltInstance.cfg FireboltInstance.tla
\*
\* Design decisions:
\*   - Phase is derived from the roll-up condition (computePhase +
\*     setInstanceReadyRollup). The roll-up is called even on early returns
\*     (writeStatusAndPoll), so the invariant Phase=Ready <=> AllReady holds
\*     AFTER every ReconcileRun firing, not necessarily in between.
\*   - The account-init terminal error (wrong account ID / duplicate account)
\*     is modeled as a one-way flag (accountFailed). Once set it is never
\*     cleared: the code documents that manual intervention is required.
\*   - Liveness for the happy path (EventuallyReady) is NOT checked as a
\*     temporal property here because it requires the environment to stabilise
\*     all four components simultaneously, which cannot be expressed as a WF
\*     condition without bounding the number of degradations. The safety
\*     invariants fully cover the state-machine correctness.

EXTENDS Integers, TLC

Components == {"postgres", "metadata", "account", "gateway"}

VARIABLES
    phase,         \* current FireboltInstance phase
    compAvail,     \* compAvail[c]: env-controlled readiness of each component
    accountFailed  \* TRUE when account init hits an unrecoverable error

vars == <<phase, compAvail, accountFailed>>

Phases == {"uninitialized", "provisioning", "ready", "degraded", "failed"}

\* All four component conditions are True.
AllReady == \A c \in Components : compAvail[c]

\* Derive next phase from current component availability.
\* Models computePhase(instance) called after setInstanceReadyRollup.
PhaseFrom(oldPhase) ==
    IF accountFailed THEN "failed"
    ELSE IF AllReady THEN "ready"
    ELSE IF oldPhase \in {"ready", "degraded"} THEN "degraded"
    ELSE "provisioning"

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

Init ==
    /\ phase         = "uninitialized"
    /\ compAvail     = [c \in Components |-> FALSE]
    /\ accountFailed = FALSE

\* ---------------------------------------------------------------------------
\* Environment actions
\* ---------------------------------------------------------------------------

\* Postgres, metadata, or gateway become available.
EnvComponentReady(c) ==
    /\ c \in {"postgres", "metadata", "gateway"}
    /\ ~compAvail[c]
    /\ compAvail' = [compAvail EXCEPT ![c] = TRUE]
    /\ UNCHANGED <<phase, accountFailed>>

\* Postgres, metadata, or gateway become unavailable (pod crash, network, etc.)
EnvComponentDegrades(c) ==
    /\ c \in {"postgres", "metadata", "gateway"}
    /\ compAvail[c]
    /\ compAvail' = [compAvail EXCEPT ![c] = FALSE]
    /\ UNCHANGED <<phase, accountFailed>>

\* Account init succeeds (non-terminal path).
EnvAccountReady ==
    /\ ~accountFailed
    /\ ~compAvail["account"]
    /\ compAvail' = [compAvail EXCEPT !["account"] = TRUE]
    /\ UNCHANGED <<phase, accountFailed>>

\* Account init fails transiently (recoverable; condition set False, requeue).
EnvAccountDegrades ==
    /\ ~accountFailed
    /\ compAvail["account"]
    /\ compAvail' = [compAvail EXCEPT !["account"] = FALSE]
    /\ UNCHANGED <<phase, accountFailed>>

\* Account init hits a terminal error (wrong account ID or multiple accounts).
\* Sets InstancePhaseFailed; operator stops running the pipeline.
\* This is a one-way transition: accountFailed is never reset.
EnvAccountTerminalFail ==
    /\ ~accountFailed
    /\ accountFailed' = TRUE
    /\ UNCHANGED <<phase, compAvail>>

\* ---------------------------------------------------------------------------
\* Reconciler actions
\* ---------------------------------------------------------------------------

\* First reconcile: status.Phase is empty; seed it to Provisioning.
ReconcileInit ==
    /\ phase = "uninitialized"
    /\ phase' = "provisioning"
    /\ UNCHANGED <<compAvail, accountFailed>>

\* Main reconcile: evaluate sequential pipeline, call setInstanceReadyRollup,
\* call computePhase, write status.
\*
\* Modeled as a single atomic action because the real reconciler runs the full
\* pipeline in one goroutine and performs a single status write per reconcile.
\* Early-return paths (writeStatusAndPoll) still call setInstanceReadyRollup
\* before writing, so the phase is always consistent with the roll-up.
\*
\* Phase=Failed is terminal: ReconcileRun is disabled in that phase.
ReconcileRun ==
    /\ phase \in {"provisioning", "ready", "degraded"}
    /\ phase' = PhaseFrom(phase)
    /\ UNCHANGED <<compAvail, accountFailed>>

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

Next ==
    \/ ReconcileInit
    \/ ReconcileRun
    \/ \E c \in {"postgres", "metadata", "gateway"} :
           \/ EnvComponentReady(c)
           \/ EnvComponentDegrades(c)
    \/ EnvAccountReady
    \/ EnvAccountDegrades
    \/ EnvAccountTerminalFail

\* ---------------------------------------------------------------------------
\* Safety invariants
\* ---------------------------------------------------------------------------

TypeOK ==
    /\ phase         \in Phases
    /\ compAvail     \in [Components -> BOOLEAN]
    /\ accountFailed \in BOOLEAN

\* Phase=Failed is only reachable via a terminal account error.
\* ReconcileRun returns "failed" only when accountFailed is TRUE, and
\* ReconcileRun is the sole action that changes phase.
Inv_FailedRequiresAccountFailed ==
    phase = "failed" => accountFailed

\* Modeled as Inv_FailedRequiresAccountFailed in formal/FireboltInstance.tla.
\* The analogous panic in Go is in instance_controller.go computePhase
\* (InstancePhaseFailed short-circuit).

Safety ==
    /\ TypeOK
    /\ Inv_FailedRequiresAccountFailed

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* A terminal account error is always eventually reflected in Failed phase.
\*
\* Proof: accountFailed=TRUE makes ReconcileRun enabled (phase in active set)
\* and PhaseFrom returns "failed". WF on ReconcileRun ensures it fires.
EventuallyFailed == accountFailed ~> (phase = "failed")

\* Failed is a trap state: once Failed, always Failed.
\*
\* Structural argument: ReconcileRun requires phase \in {provisioning, ready,
\* degraded}; it is disabled when phase="failed". No other action changes phase.
\* Therefore the only transitions out of "failed" are... none.
FailedIsTerminal == [](phase = "failed" => [](phase = "failed"))

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

Spec ==
    /\ Init
    /\ [][Next]_vars
    /\ WF_vars(ReconcileInit)
    /\ WF_vars(ReconcileRun)
    /\ WF_vars(EnvComponentReady("postgres"))
    /\ WF_vars(EnvComponentReady("metadata"))
    /\ WF_vars(EnvComponentReady("gateway"))
    /\ WF_vars(EnvAccountReady)

\* Theorems (checked by TLC)
THEOREM Spec => []Safety
THEOREM Spec => EventuallyFailed
THEOREM Spec => FailedIsTerminal

====
