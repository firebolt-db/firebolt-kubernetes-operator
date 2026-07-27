---- MODULE SigningKeyRotation ----
\* TLA+ specification of the operator-owned JWT signing-key rotation state
\* machine (stepSigningKeyRotation in internal/controller/instance_auth.go)
\* together with the engine fleet it coordinates.
\*
\* Models the rotation lifecycle of the single non-Active key that can be
\* outstanding at any time:
\*   none -> minting -> pending -> demoted -> retiring -> removing -> none
\*
\* mapped onto the Go representation as:
\*   minting  : Certificate applied, Secret not yet issued; the key is NOT
\*              yet in Status.Auth.SigningKeys (mintNextSigningKey appends
\*              only once the Secret is ready)
\*   pending  : Phase=ValidationOnly, DemotedAt=nil   (awaiting promotion)
\*   demoted  : Phase=ValidationOnly, DemotedAt!=nil, RetireEligibleAt=nil
\*   retiring : Phase=ValidationOnly, RetireEligibleAt!=nil (retain window)
\*   removing : Phase=Removing
\*
\* Each engine observes a *rendered* key list (signingKeysForRender: the
\* Active key first, plus the outstanding key unless it is removing) only
\* after completing a blue-green rollout onto it; engineObs[e] is the render
\* engine e last rolled onto (its ObservedAuthHash, decomposed). An engine
\* signs tokens with the Active key of its own observed render and can
\* validate tokens signed by any key of its observed render.
\*
\* Verified properties:
\*   Safety            - the invariants below, most importantly:
\*     Inv_SignerUniversallyValidatable - every token any engine signs can
\*         be validated by every other engine (the property the per-step
\*         enginesConvergedOn gates exist to protect)
\*     Inv_ObservedKeysExist - no engine's observed render references a key
\*         whose material has been deleted (premature deleteSigningKey)
\*   RotationCompletes - an in-flight rotation eventually finishes, provided
\*         cert-manager issues, the retain window elapses, and every engine
\*         keeps reconciling (fair EnvEngineSync)
\*
\* To check with TLC:
\*   java -jar tla2tools.jar -config SigningKeyRotation.cfg SigningKeyRotation.tla
\*
\* Design decisions:
\*   - One reconciler action per behavior step, matching Go's "at most one
\*     rotation step per reconcile" (stepSigningKeyRotation returns after
\*     the first case that fires). The state-cover harness additionally
\*     accepts multi-step reconciler closures, since a single Go reconcile
\*     may legitimately complete a mint whose Secret is already issued.
\*   - Convergence (enginesConvergedOn) is modeled as engineObs[e] = Render
\*     for every engine: the Go gate compares each engine's ObservedAuthHash
\*     against the hash of the current render, and authHash is injective on
\*     the (ordered) rendered key list, so hash equality and render equality
\*     coincide at this abstraction level.
\*   - Time is abstracted to three monotone-per-cycle environment facts:
\*     rotationDue (RotationInterval elapsed for the Active key), certReady
\*     (cert-manager issued the minting key's Secret), and retainElapsed
\*     (RetainDuration elapsed since RetireEligibleAt). Each is reset by the
\*     reconciler action that consumes its cycle, mirroring where the Go
\*     anchors move (promotion resets dueness via the new Active key's
\*     CreatedAt; ConfirmRetireEligible stamps a fresh RetireEligibleAt).
\*   - EnvRotationDue is guarded on activeKey < MaxGen purely to bound the
\*     state space; the Go rotation has no generation ceiling.
\*   - The engine fleet is a fixed CONSTANT set. Engine creation is subsumed:
\*     a new engine renders the current key list immediately, which is
\*     exactly the effect of EnvEngineSync. Engine deletion only shrinks
\*     the set the gate quantifies over, weakening no invariant here.
\*   - Bootstrap (no key at all -> first Active key) is out of scope: it
\*     gates the whole AuthReady condition before any engine can render
\*     auth at all (bootstrapSigningKey), so no cross-engine coordination
\*     exists to model. Init starts at the post-bootstrap fixed point.

EXTENDS Integers, TLC

CONSTANTS
    Engines,   \* the FireboltEngines bound to this instance, e.g. {e1, e2}
    MaxGen     \* upper bound on key generations (bounds the state space)

Gens == 1..MaxGen

OtherStates == {"none", "minting", "pending", "demoted", "retiring", "removing"}

VARIABLES
    activeKey,     \* generation of the Phase=Active key
    otherKey,      \* generation of the single non-Active key (0 = none)
    otherState,    \* lifecycle position of the non-Active key
    rotationDue,   \* env: RotationInterval elapsed for the Active key
    certReady,     \* env: the outstanding key's Secret is issued
    retainElapsed, \* env: RetainDuration elapsed since RetireEligibleAt
    engineObs      \* engineObs[e]: the render engine e last rolled onto

vars == <<activeKey, otherKey, otherState, rotationDue, certReady,
          retainElapsed, engineObs>>

\* ---------------------------------------------------------------------------
\* Helpers
\* ---------------------------------------------------------------------------

\* The rendered key list engines roll onto (signingKeysForRender): the Active
\* key plus the outstanding key while it is pending/demoted/retiring. A
\* minting key is not yet in Status; a removing key is deliberately excluded.
RenderedOther == IF otherState \in {"pending", "demoted", "retiring"}
                 THEN otherKey
                 ELSE 0

Render == [active |-> activeKey, other |-> RenderedOther]

\* enginesConvergedOn: every engine's ObservedAuthHash matches the hash of
\* the current render.
Converged == \A e \in Engines : engineObs[e] = Render

\* The keys engine e holds for validation (its observed render).
KeysOf(obs) == {obs.active} \cup ({obs.other} \ {0})

\* Keys whose material currently exists in the cluster.
ExistingKeys == {activeKey} \cup ({otherKey} \ {0})

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

\* Post-bootstrap fixed point: one Active key, every engine rolled onto it.
Init ==
    /\ activeKey     = 1
    /\ otherKey      = 0
    /\ otherState    = "none"
    /\ rotationDue   = FALSE
    /\ certReady     = FALSE
    /\ retainElapsed = FALSE
    /\ engineObs     = [e \in Engines |-> [active |-> 1, other |-> 0]]

\* ---------------------------------------------------------------------------
\* Environment actions
\* ---------------------------------------------------------------------------

\* The Active key's RotationInterval elapses. Only meaningful between
\* rotations: mid-rotation dueness is ignored by every stepSigningKeyRotation
\* branch, and after promotion the new Active key's CreatedAt re-anchors the
\* clock. Guarded on MaxGen to bound the state space.
EnvRotationDue ==
    /\ otherState = "none"
    /\ ~rotationDue
    /\ activeKey < MaxGen
    /\ rotationDue' = TRUE
    /\ UNCHANGED <<activeKey, otherKey, otherState, certReady, retainElapsed, engineObs>>

\* cert-manager issues the minting key's Secret.
EnvCertIssued ==
    /\ otherState = "minting"
    /\ ~certReady
    /\ certReady' = TRUE
    /\ UNCHANGED <<activeKey, otherKey, otherState, rotationDue, retainElapsed, engineObs>>

\* RetainDuration elapses after RetireEligibleAt was stamped.
EnvRetainElapsed ==
    /\ otherState = "retiring"
    /\ ~retainElapsed
    /\ retainElapsed' = TRUE
    /\ UNCHANGED <<activeKey, otherKey, otherState, rotationDue, certReady, engineObs>>

\* Engine e completes a blue-green rollout onto the current render: its
\* ObservedAuthHash now matches the hash enginesConvergedOn expects.
EnvEngineSync(e) ==
    /\ engineObs[e] # Render
    /\ engineObs' = [engineObs EXCEPT ![e] = Render]
    /\ UNCHANGED <<activeKey, otherKey, otherState, rotationDue, certReady, retainElapsed>>

\* ---------------------------------------------------------------------------
\* Reconciler actions (stepSigningKeyRotation, one case each)
\* ---------------------------------------------------------------------------

\* A rotation is due and no key is outstanding: apply the next generation's
\* Certificate (mintNextSigningKey, not-yet-ready branch). The key is not in
\* Status yet, so the render is unchanged and no engine is disturbed.
MintStart ==
    /\ otherState = "none"
    /\ rotationDue
    /\ activeKey < MaxGen
    /\ otherKey'   = activeKey + 1
    /\ otherState' = "minting"
    /\ certReady'  = FALSE
    /\ UNCHANGED <<activeKey, rotationDue, retainElapsed, engineObs>>

\* The minting key's Secret is issued: append it to Status as
\* Phase=ValidationOnly (mintNextSigningKey, ready branch). This changes the
\* render, so every engine must roll before the next gate can pass.
MintComplete ==
    /\ otherState = "minting"
    /\ certReady
    /\ otherState' = "pending"
    /\ UNCHANGED <<activeKey, otherKey, rotationDue, certReady, retainElapsed, engineObs>>

\* Promote the pending key to Active, demoting the old Active key to
\* ValidationOnly with DemotedAt stamped (promoteSigningKey). Gated on every
\* engine having rolled onto a render that includes the pending key —
\* promoting earlier would let a rolled engine sign tokens a not-yet-rolled
\* engine cannot validate. Dueness re-anchors on the new Active key.
Promote ==
    /\ otherState = "pending"
    /\ Converged
    /\ activeKey'   = otherKey
    /\ otherKey'    = activeKey
    /\ otherState'  = "demoted"
    /\ rotationDue' = FALSE
    /\ UNCHANGED <<certReady, retainElapsed, engineObs>>

\* Every engine has rolled onto the promoted render, so no engine can still
\* be signing with the demoted key: stamp RetireEligibleAt and let the
\* retain window start counting. Anchoring at this confirmation (rather
\* than at promotion time) is what keeps the retain window meaningful for
\* tokens signed by engines that lagged the promotion.
ConfirmRetireEligible ==
    /\ otherState = "demoted"
    /\ Converged
    /\ otherState'    = "retiring"
    /\ retainElapsed' = FALSE
    /\ UNCHANGED <<activeKey, otherKey, rotationDue, certReady, engineObs>>

\* The retain window elapsed: flip the key to Phase=Removing, dropping it
\* from the render. Engines roll off it one by one.
StartRemoval ==
    /\ otherState = "retiring"
    /\ retainElapsed
    /\ otherState' = "removing"
    /\ UNCHANGED <<activeKey, otherKey, rotationDue, certReady, retainElapsed, engineObs>>

\* Every engine has rolled onto a render without the removing key, so none
\* can need it to validate anything: delete its Certificate and Secret and
\* drop it from Status (deleteSigningKey). The rotation is complete.
DeleteKey ==
    /\ otherState = "removing"
    /\ Converged
    /\ otherKey'      = 0
    /\ otherState'    = "none"
    /\ certReady'     = FALSE
    /\ retainElapsed' = FALSE
    /\ UNCHANGED <<activeKey, rotationDue, engineObs>>

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

\* Bounded-model terminator: once the generation ceiling is reached and the
\* rotation cycle has fully completed, no action is enabled — the real system
\* simply idles there (the ceiling exists only in the model, via
\* EnvRotationDue's MaxGen guard). The explicit stutter keeps TLC's deadlock
\* check meaningful for every other state: a deadlock anywhere else would
\* still be reported as a genuine stuck rotation.
Quiesced ==
    /\ activeKey = MaxGen
    /\ otherState = "none"
    /\ ~rotationDue
    /\ UNCHANGED vars

Next ==
    \/ EnvRotationDue
    \/ EnvCertIssued
    \/ EnvRetainElapsed
    \/ \E e \in Engines : EnvEngineSync(e)
    \/ MintStart
    \/ MintComplete
    \/ Promote
    \/ ConfirmRetireEligible
    \/ StartRemoval
    \/ DeleteKey
    \/ Quiesced

\* ---------------------------------------------------------------------------
\* Safety invariants
\* ---------------------------------------------------------------------------

TypeOK ==
    /\ activeKey     \in Gens
    /\ otherKey      \in {0} \cup Gens
    /\ otherState    \in OtherStates
    /\ rotationDue   \in BOOLEAN
    /\ certReady     \in BOOLEAN
    /\ retainElapsed \in BOOLEAN
    /\ engineObs     \in [Engines -> [active : Gens, other : {0} \cup Gens]]

\* Every token any engine signs (with the Active key of its own observed
\* render) is validatable by every other engine (whose validation set is
\* its own observed render). This is THE property the convergence gates
\* protect: remove the Converged conjunct from Promote and TLC produces a
\* counterexample where a rolled engine signs with the new key while a
\* lagging engine cannot validate it.
Inv_SignerUniversallyValidatable ==
    \A e1, e2 \in Engines : engineObs[e1].active \in KeysOf(engineObs[e2])

\* No engine's observed render references a key whose material has been
\* deleted. Remove the Converged conjunct from DeleteKey and TLC produces a
\* counterexample where a lagging engine still holds the deleted key.
Inv_ObservedKeysExist ==
    \A e \in Engines : KeysOf(engineObs[e]) \subseteq ExistingKeys

\* At most one non-Active key, tracked consistently.
Inv_OtherConsistent ==
    (otherState = "none") <=> (otherKey = 0)

\* Before promotion the outstanding key is exactly the next generation;
\* after promotion the demoted key is strictly older than the Active key.
Inv_KeyDirection ==
    /\ otherState \in {"minting", "pending"} => otherKey = activeKey + 1
    /\ otherState \in {"demoted", "retiring", "removing"} => otherKey < activeKey

\* certReady tracks the outstanding key's Secret: necessarily issued once
\* the key is in Status (it is appended only when ready), gone after
\* deletion. Keeps the projection from Go state deterministic.
Inv_CertReadyShape ==
    /\ otherState \in {"pending", "demoted", "retiring", "removing"} => certReady
    /\ otherState = "none" => ~certReady

\* retainElapsed only accumulates inside the retain window it measures.
Inv_RetainElapsedShape ==
    /\ otherState \in {"none", "minting", "pending", "demoted"} => ~retainElapsed
    /\ otherState = "removing" => retainElapsed

\* Dueness is re-anchored by promotion and can only re-arm between
\* rotations, mirroring where CreatedAt moves in Go.
Inv_DueShape ==
    otherState \in {"demoted", "retiring", "removing"} => ~rotationDue

Safety ==
    /\ TypeOK
    /\ Inv_SignerUniversallyValidatable
    /\ Inv_ObservedKeysExist
    /\ Inv_OtherConsistent
    /\ Inv_KeyDirection
    /\ Inv_CertReadyShape
    /\ Inv_RetainElapsedShape
    /\ Inv_DueShape

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* An in-flight rotation eventually completes. Requires fairness on the
\* reconciler steps and on the environment facts that unblock them:
\* cert-manager eventually issues, the retain window eventually elapses,
\* and every engine eventually rolls onto the current render. Without the
\* engine fairness a permanently-stalled engine correctly blocks the
\* rotation forever (fail-safe), so no property would hold.
\*
\* WF suffices everywhere: no action ever disables another's precondition
\* except by making progress. In particular an engine stale w.r.t. the
\* current render stays continuously stale until it syncs, because every
\* render-changing reconciler action is either gated on full convergence
\* (Promote, DeleteKey via ConfirmRetireEligible) or fires from a state
\* where all engines were already converged (MintComplete, StartRemoval).
RotationCompletes == (otherState # "none") ~> (otherState = "none")

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

Spec ==
    /\ Init
    /\ [][Next]_vars
    /\ WF_vars(MintStart)
    /\ WF_vars(MintComplete)
    /\ WF_vars(Promote)
    /\ WF_vars(ConfirmRetireEligible)
    /\ WF_vars(StartRemoval)
    /\ WF_vars(DeleteKey)
    /\ WF_vars(EnvCertIssued)
    /\ WF_vars(EnvRetainElapsed)
    /\ \A e \in Engines : WF_vars(EnvEngineSync(e))

\* Theorems (checked by TLC)
THEOREM Spec => []Safety
THEOREM Spec => RotationCompletes

====
