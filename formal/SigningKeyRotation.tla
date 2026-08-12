---- MODULE SigningKeyRotation ----
\* TLA+ specification of the FireboltInstance JWT signing-key rotation state
\* machine (stepSigningKeyRotation).
\*
\* Models one rotation pipeline:
\*   mint -> (converge) -> promote -> (converge) -> anchor -> retain
\*        -> removing -> (converge) -> delete
\*
\* Every engine renders the Instance's signing keys into its own config and
\* signs with the FIRST one, the Active key. Rendering is not instantaneous, so
\* at any moment different engines may be signing and validating against
\* different generations of that config. Each irreversible step is therefore
\* gated on fleet convergence: every engine's observedAuthHash must equal the
\* Instance-computed authHash before the step may run.
\*
\* Verified properties:
\*   Safety            - TypeOK, AtMostOneActive, NoOrphanKey, NoValidationGap
\*   EventuallyRotates - a rotation under a fleet that keeps rolling completes
\*
\* To check with TLC:
\*   java -jar tla2tools.jar -config SigningKeyRotation.cfg SigningKeyRotation.tla
\*
\* Design decisions:
\*
\*   - NoValidationGap is the property the convergence gates exist for: for
\*     every ordered pair of engines, the key the first would sign with must be
\*     in the second's validate set. Both sides read each engine's OWN observed
\*     config, because that is what the engine actually holds on disk — writing
\*     the invariant against the operator's current status instead would assume
\*     the very convergence the gates are there to establish.
\*
\*   - AnchorAtDemotion selects which event starts the retain window, and is the
\*     reason this spec exists. FALSE is the shipped design: the window starts at
\*     RetireEligibleAt, stamped only once every engine has rolled onto the
\*     promoted config, i.e. once no engine can still be signing with the
\*     demoted key. TRUE is the naive design, anchoring at DemotedAt — the
\*     moment the operator DECIDED to stop signing with the key rather than the
\*     moment the fleet provably did. Under TRUE, TLC finds a NoValidationGap
\*     counterexample; under FALSE it finds none. SigningKeyRotationNaive.cfg
\*     pins that counterexample so the anchor choice stays machine-checked
\*     rather than argued in prose.
\*
\*   - Time is abstracted away entirely. RetainElapses is an environment action
\*     enabled once the window's anchor event has happened, so it may fire at any
\*     later point. This is strictly more permissive than a real clock (which
\*     also imposes a minimum wait), so any behavior a real deployment can
\*     exhibit is covered, and the state space stays tiny.
\*
\*   - Minting is two steps. mintNextSigningKey applies the Certificate but
\*     appends the key to status only once the Secret is ready, so "requested but
\*     not yet rendered" is a real reachable state that a crash can sit in. The
\*     MintStart/MintReady split makes it reachable here too, and NoOrphanKey
\*     checks that such a key is neither rendered nor observed anywhere.
\*
\*   - AtMostTwoKeys is deliberately NOT an invariant. The implementation's only
\*     mint arm requires that no other key exists (`case other == nil`), so a
\*     two-key maximum is a structural consequence of the mint guard, which this
\*     spec's MintStart copies. Asserting it here would check the model against
\*     itself and pass no matter what the implementation did.
\*
\*   - Bootstrap is out of scope: the Instance always has an Active key before
\*     any rotation step runs (ensureSigningKeys bootstraps one first), so Init
\*     starts from a single Active key with the fleet converged on it.

EXTENDS Integers, FiniteSets

CONSTANTS
    Engines,          \* set of engines belonging to the Instance
    MaxGen,           \* how many rotations to explore
    AnchorAtDemotion  \* TRUE models the naive retain-window anchor

\* Key identifiers. Generation 0 starts on kid 1; rotation n mints kid n+1.
Kids == 1..(MaxGen + 1)

Phases == {"absent", "minting", "validationOnly", "active", "removing"}

VARIABLES
    keyPhase,   \* keyPhase[k]: this key's role in the Instance status
    demoted,    \* keys that have been demoted from Active (DemotedAt set)
    anchored,   \* keys whose RetireEligibleAt has been stamped
    retainDone, \* keys whose retain window has elapsed
    observed,   \* observed[e]: the rendered config engine e currently holds
    gen         \* rotations completed so far (mints that reached status)

vars == <<keyPhase, demoted, anchored, retainDone, observed, gen>>

\* ---------------------------------------------------------------------------
\* The rendered config
\* ---------------------------------------------------------------------------

\* signingKeysForRender: the Active key plus at most one other non-Removing
\* key. A minting key is not in status yet, and a Removing key is excluded.
RenderedKeys == {k \in Kids : keyPhase[k] \in {"active", "validationOnly"}}

ActiveKid == CHOOSE k \in Kids : keyPhase[k] = "active"

\* What an engine holds: the key it signs with (Active, rendered first) and the
\* set it will validate against.
RenderedConfig == [active |-> ActiveKid, keys |-> RenderedKeys]

\* Every engine's observedAuthHash matches the Instance-computed hash.
Converged == \A e \in Engines : observed[e] = RenderedConfig

\* The single non-Active tracked key, mirroring otherSigningKey.
OtherKeys == {k \in Kids : keyPhase[k] \in {"minting", "validationOnly", "removing"}}

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

Init ==
    /\ keyPhase   = [k \in Kids |-> IF k = 1 THEN "active" ELSE "absent"]
    /\ demoted    = {}
    /\ anchored   = {}
    /\ retainDone = {}
    /\ gen        = 0
    /\ observed   = [e \in Engines |-> [active |-> 1, keys |-> {1}]]

\* ---------------------------------------------------------------------------
\* Environment actions
\* ---------------------------------------------------------------------------

\* An engine finishes rolling onto the current rendered config and reports the
\* matching observedAuthHash.
EngineRolls(e) ==
    /\ observed[e] /= RenderedConfig
    /\ observed' = [observed EXCEPT ![e] = RenderedConfig]
    /\ UNCHANGED <<keyPhase, demoted, anchored, retainDone, gen>>

\* The retain window elapses. Enabled once the window's anchor event has
\* happened — which anchor is exactly what AnchorAtDemotion selects.
RetainElapses(k) ==
    /\ k \notin retainDone
    /\ IF AnchorAtDemotion THEN k \in demoted ELSE k \in anchored
    /\ retainDone' = retainDone \cup {k}
    /\ UNCHANGED <<keyPhase, demoted, anchored, observed, gen>>

\* ---------------------------------------------------------------------------
\* Reconciler actions, one per arm of stepSigningKeyRotation
\* ---------------------------------------------------------------------------

\* A rotation is due and no other key is outstanding: apply the Certificate.
\* Not in status yet, so not rendered and not mounted anywhere.
MintStart ==
    /\ OtherKeys = {}
    /\ gen < MaxGen
    /\ keyPhase' = [keyPhase EXCEPT ![gen + 2] = "minting"]
    /\ UNCHANGED <<demoted, anchored, retainDone, observed, gen>>

\* cert-manager has issued the Secret: append the key to status, where it
\* becomes rendered as validate-only.
MintReady ==
    /\ \E k \in Kids :
          /\ keyPhase[k] = "minting"
          /\ keyPhase' = [keyPhase EXCEPT ![k] = "validationOnly"]
    /\ gen' = gen + 1
    /\ UNCHANGED <<demoted, anchored, retainDone, observed>>

\* Promote the minted key to Active and demote the previous one. Gated: until
\* every engine renders the new key, a promoted engine would sign tokens a
\* not-yet-rolled engine cannot validate.
Promote ==
    /\ Converged
    /\ \E k \in Kids :
          /\ keyPhase[k] = "validationOnly"
          /\ k \notin demoted
          /\ keyPhase' = [keyPhase EXCEPT ![k] = "active", ![ActiveKid] = "validationOnly"]
          /\ demoted' = demoted \cup {ActiveKid}
    /\ UNCHANGED <<anchored, retainDone, observed, gen>>

\* Stamp RetireEligibleAt on the demoted key. Gated: every engine signs with
\* the demoted key until it rolls onto the promotion, so the retain window
\* cannot start counting before this convergence is confirmed.
StampAnchor ==
    /\ Converged
    /\ \E k \in Kids :
          /\ keyPhase[k] = "validationOnly"
          /\ k \in demoted
          /\ k \notin anchored
          /\ anchored' = anchored \cup {k}
    /\ UNCHANGED <<keyPhase, demoted, retainDone, observed, gen>>

\* The retain window has elapsed: drop the key from render. Its Certificate and
\* Secret still exist, so an engine that has not yet rolled keeps working.
MarkRemoving ==
    /\ \E k \in Kids :
          /\ keyPhase[k] = "validationOnly"
          /\ k \in demoted
          /\ k \in retainDone
          /\ keyPhase' = [keyPhase EXCEPT ![k] = "removing"]
    /\ UNCHANGED <<demoted, anchored, retainDone, observed, gen>>

\* Delete the Certificate and Secret and forget the key. Gated: an engine that
\* has not yet rolled past the removal may still need it to validate a token
\* signed before the key was demoted.
Delete ==
    /\ Converged
    /\ \E k \in Kids :
          /\ keyPhase[k] = "removing"
          /\ keyPhase' = [keyPhase EXCEPT ![k] = "absent"]
          /\ demoted' = demoted \ {k}
          /\ anchored' = anchored \ {k}
          /\ retainDone' = retainDone \ {k}
    /\ UNCHANGED <<observed, gen>>

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

Next ==
    \/ MintStart
    \/ MintReady
    \/ Promote
    \/ StampAnchor
    \/ MarkRemoving
    \/ Delete
    \/ \E e \in Engines : EngineRolls(e)
    \/ \E k \in Kids : RetainElapses(k)

\* ---------------------------------------------------------------------------
\* Safety invariants
\* ---------------------------------------------------------------------------

TypeOK ==
    /\ keyPhase \in [Kids -> Phases]
    /\ demoted \subseteq Kids
    /\ anchored \subseteq Kids
    /\ retainDone \subseteq Kids
    /\ gen \in 0..MaxGen
    /\ \A e \in Engines :
          /\ observed[e].active \in Kids
          /\ observed[e].keys \subseteq Kids

\* Exactly one key signs at a time. packdb renders one signing_algorithm and
\* signs with signing_keys[0], so two Active keys are not representable.
AtMostOneActive == Cardinality({k \in Kids : keyPhase[k] = "active"}) = 1

\* A key whose Certificate has been applied but whose Secret is not ready yet is
\* not in status, so it can be neither rendered nor observed by any engine.
NoOrphanKey ==
    \A k \in Kids :
        keyPhase[k] = "minting" =>
            /\ k \notin RenderedKeys
            /\ \A e \in Engines : k \notin observed[e].keys

\* THE property. Whatever any engine may currently be signing with must be
\* validatable by every engine, including engines at a different point in their
\* own rollout. This is what every convergence gate protects.
NoValidationGap ==
    \A s \in Engines : \A v \in Engines : observed[s].active \in observed[v].keys

Safety ==
    /\ TypeOK
    /\ AtMostOneActive
    /\ NoOrphanKey
    /\ NoValidationGap

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* Every configured rotation eventually completes: the last generation is minted
\* and promoted, and the retired keys are gone, leaving a single rendered key.
\*
\* Weak fairness on every reconciler action plus the two environment actions is
\* what makes this provable. Unlike FireboltInstance.tla, no <>[] precondition is
\* needed: no adversary can un-roll an engine, so once a step's convergence gate
\* is satisfied it stays satisfied until that step fires.
RotationComplete == gen = MaxGen /\ Cardinality(RenderedKeys) = 1

EventuallyRotates == <>RotationComplete

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

Spec ==
    /\ Init
    /\ [][Next]_vars
    /\ WF_vars(MintStart)
    /\ WF_vars(MintReady)
    /\ WF_vars(Promote)
    /\ WF_vars(StampAnchor)
    /\ WF_vars(MarkRemoving)
    /\ WF_vars(Delete)
    /\ \A e \in Engines : WF_vars(EngineRolls(e))
    /\ \A k \in Kids : WF_vars(RetainElapses(k))

\* Theorems (checked by TLC)
THEOREM Spec => []Safety
THEOREM Spec => EventuallyRotates

====
