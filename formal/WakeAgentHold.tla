---- MODULE WakeAgentHold ----
\* TLA+ specification of the wake agent's hold path: the in-memory bookkeeping
\* that parks a query for an engine with no ready endpoints and releases it when
\* endpoints appear.
\*
\* One question, asked mechanically: is the waiter registration safe against a
\* release that belongs to a RETIRED channel generation?
\*
\* The agent hands every parked request a channel to wait on, refcounted per
\* engine. The readiness watch closes that channel and drops the registration on
\* the not-ready -> ready edge, so a later request for the same engine registers
\* a FRESH channel under the same name. A request from the previous generation
\* releases its reference after its own wait ends -- the release is a deferred
\* call, so it happens strictly later, and possibly after the new generation is
\* already registered. Whether that stale release may touch the new
\* registration is what ReleaseByEngineName decides, and it is the reason this
\* module exists.
\*
\* Verified properties:
\*   Safety              - TypeOK, Inv_NoStrandedWaiter, Inv_WaiterRefsAccurate
\*   HoldLeavesPark      - a parked request is eventually released, without
\*                         relying on its own timeout
\*
\* To check with TLC:
\*   java -jar tla2tools.jar -config WakeAgentHold.cfg WakeAgentHold.tla
\*
\* This module has NO clock and no demand timestamps. Those belong to
\* EngineWake.tla, which models the other half of the same feature: how stale a
\* demand stamp may be by the time the operator acts on it. The two are separate
\* modules because no property spans them -- everything here is about the ORDER
\* of two bookkeeping operations, and everything there is about elapsed time --
\* and one module would multiply two independent state spaces (120k states
\* against 4k and 300) while checking nothing across the seam.
\*
\* ---------------------------------------------------------------------------
\* Design decisions
\* ---------------------------------------------------------------------------
\*
\*   - Channel identity is a number, and identity is the entire subject: the
\*     hazard is a release naming a channel that is no longer the registered
\*     one, so `closed` must be a SET of retired channel ids rather than a
\*     high-water mark. A high-water mark would mark a stranded waiter's channel
\*     as closed (its id is below one that was closed later) and mask exactly the
\*     bug this module is here to find.
\*
\*   - "awake" is a real state, not an artefact. The release is deferred, so a
\*     request's wait ends strictly before it releases its reference, and the
\*     window between the two is where the stale release lives. Both exits from
\*     the wait -- the channel closing and the request giving up -- land in it.
\*
\*   - Endpoint readiness is environment-owned and may flap at any time, which is
\*     what makes a second channel generation reachable. The agent's re-check
\*     after a wakeup (an engine that flapped back to not-ready answers 503
\*     rather than routing into a name that no longer resolves) decides a
\*     RESPONSE CODE and touches no tracker state, so it is not modelled.
\*
\*   - The hold capacity limiter is not modelled. A shed request never parks, so
\*     it cannot participate in this hazard; what matters about the cap -- that
\*     demand is stamped before it is consulted -- is EngineWake.tla's subject.
\*
\*   - Requests are anonymous and reusable: a finished request returns to
\*     "absent" and may arrive again. Modelling a fixed pool of two is enough,
\*     because the hazard needs exactly one request exiting while another is
\*     parked on a later generation.

EXTENDS Integers, FiniteSets

CONSTANTS
    Holds,     \* the in-flight requests modelled
    MaxChan,   \* waiter-channel generations modelled
    \* --- counterexample flag, FALSE in the shipped configuration ---
    ReleaseByEngineName
      \* TRUE keys the waiter release on the engine name alone instead of on the
      \* identity of the channel the request parked on. A release from a retired
      \* generation then unregisters the CURRENT generation's waiter, stranding a
      \* request that is still parked on it: no future readiness edge can close a
      \* channel nothing has a registration for, so the request sits until its
      \* own timeout even though the engine came up. Violates
      \* Inv_NoStrandedWaiter.

Chans == 1..MaxChan

\* A request is either absent, parked on a waiter channel, or awake: its wait has
\* ended and it has not yet released its reference.
HoldStates == {"absent", "parked", "awake"}

VARIABLES
    ready,     \* TRUE when the engine has >= 1 ready endpoint
    waiter,    \* the channel id currently registered for the engine (0: none)
    refs,      \* the registered channel's reference count
    chanGen,   \* channel ids handed out so far
    closed,    \* channel ids the readiness watch has closed
    hold,      \* hold[h]: this request's state
    holdChan   \* holdChan[h]: the channel it parked on (0: none)

vars == <<ready, waiter, refs, chanGen, closed, hold, holdChan>>

\* Requests that still count against the registered channel's refcount: parked
\* ones, and awake ones that have not released yet.
Registered == {h \in Holds : hold[h] # "absent" /\ holdChan[h] = waiter}

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

\* A fresh agent: cache synced, engine not ready, nothing in flight.
Init ==
    /\ ready    = FALSE
    /\ waiter   = 0
    /\ refs     = 0
    /\ chanGen  = 0
    /\ closed   = {}
    /\ hold     = [h \in Holds |-> "absent"]
    /\ holdChan = [h \in Holds |-> 0]

\* ---------------------------------------------------------------------------
\* Environment: the engine's endpoints
\* ---------------------------------------------------------------------------

\* Endpoints appear. setReady closes the registered channel and drops the
\* registration, which is what releases every request parked on it at once.
EndpointsBecomeReady ==
    /\ ~ready
    /\ ready'  = TRUE
    /\ closed' = IF waiter = 0 THEN closed ELSE closed \cup {waiter}
    /\ waiter' = 0
    /\ refs'   = 0
    /\ UNCHANGED <<chanGen, hold, holdChan>>

\* Endpoints go away: a scale-down, a node drain, a rolling restart, or a flap.
\* setReady(FALSE) only forgets the readiness; it touches no waiter bookkeeping,
\* which is precisely why a channel can be retired while its successor is being
\* registered.
EndpointsGoAway ==
    /\ ready
    /\ ready' = FALSE
    /\ UNCHANGED <<waiter, refs, chanGen, closed, hold, holdChan>>

\* ---------------------------------------------------------------------------
\* The hold path
\* ---------------------------------------------------------------------------

\* A query arrives for an engine with no ready endpoints and parks on the
\* engine's waiter channel, creating one if none is registered. The fast path --
\* endpoints already ready, so the query routes straight through -- changes
\* nothing observable and is not modelled.
HoldArrives(h) ==
    /\ hold[h] = "absent"
    /\ ~ready
    /\ \/ waiter # 0
       \/ chanGen < MaxChan
    /\ hold' = [hold EXCEPT ![h] = "parked"]
    /\ IF waiter = 0
         THEN /\ chanGen'  = chanGen + 1
              /\ waiter'   = chanGen + 1
              /\ refs'     = 1
              /\ holdChan' = [holdChan EXCEPT ![h] = chanGen + 1]
         ELSE /\ refs'     = refs + 1
              /\ holdChan' = [holdChan EXCEPT ![h] = waiter]
              /\ UNCHANGED <<chanGen, waiter>>
    /\ UNCHANGED <<ready, closed>>

\* The request's channel was closed, so its wait ends.
HoldWakes(h) ==
    /\ hold[h] = "parked"
    /\ holdChan[h] \in closed
    /\ hold' = [hold EXCEPT ![h] = "awake"]
    /\ UNCHANGED <<ready, waiter, refs, chanGen, closed, holdChan>>

\* The request gives up: its hold timeout fired, or the client hung up. As far as
\* the tracker is concerned this is the same exit as HoldWakes -- the release
\* below is deferred either way -- and it is this exit, taken while a later
\* channel generation is registered, that produces the stale release.
HoldExpires(h) ==
    /\ hold[h] = "parked"
    /\ hold' = [hold EXCEPT ![h] = "awake"]
    /\ UNCHANGED <<ready, waiter, refs, chanGen, closed, holdChan>>

\* Shipped: the release is keyed on the identity of the channel the request was
\* handed, so a release belonging to a retired generation is a no-op.
\* ReleaseByEngineName drops that test and keys on the engine name alone.
ReleaseApplies(h) ==
    IF ReleaseByEngineName THEN waiter # 0 ELSE waiter # 0 /\ waiter = holdChan[h]

\* The request releases its waiter reference and finishes.
HoldFinishes(h) ==
    /\ hold[h] = "awake"
    /\ hold'     = [hold EXCEPT ![h] = "absent"]
    /\ holdChan' = [holdChan EXCEPT ![h] = 0]
    /\ IF ReleaseApplies(h)
         THEN IF refs <= 1
                THEN /\ waiter' = 0
                     /\ refs'   = 0
                ELSE /\ refs'   = refs - 1
                     /\ UNCHANGED waiter
         ELSE UNCHANGED <<waiter, refs>>
    /\ UNCHANGED <<ready, chanGen, closed>>

\* The agent process restarts. Every registration is in memory, so it all goes,
\* and the parked requests die with their connections. chanGen is deliberately
\* not reset: channel identities must stay distinct across a restart for the
\* release test to mean anything.
AgentRestarts ==
    /\ \/ waiter # 0
       \/ \E h \in Holds : hold[h] # "absent"
    /\ waiter'   = 0
    /\ refs'     = 0
    /\ hold'     = [h \in Holds |-> "absent"]
    /\ holdChan' = [h \in Holds |-> 0]
    /\ UNCHANGED <<ready, chanGen, closed>>

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

Next ==
    \/ EndpointsBecomeReady
    \/ EndpointsGoAway
    \/ \E h \in Holds : HoldArrives(h)
    \/ \E h \in Holds : HoldWakes(h)
    \/ \E h \in Holds : HoldExpires(h)
    \/ \E h \in Holds : HoldFinishes(h)
    \/ AgentRestarts

\* ---------------------------------------------------------------------------
\* Safety invariants
\* ---------------------------------------------------------------------------

TypeOK ==
    /\ ready \in BOOLEAN
    /\ waiter \in 0..MaxChan
    /\ refs \in 0..Cardinality(Holds)
    /\ chanGen \in 0..MaxChan
    /\ closed \subseteq Chans
    /\ hold \in [Holds -> HoldStates]
    /\ holdChan \in [Holds -> 0..MaxChan]
    \* A registered channel has been handed out, and a request parked on one it
    \* was handed.
    /\ waiter # 0 => waiter <= chanGen
    /\ \A h \in Holds : holdChan[h] <= chanGen

\* THE property. Every parked request is either already released or still
\* registered, so some future readiness edge will release it. A request that is
\* neither is stranded: it sits until its own timeout even though the engine it
\* was waiting for came up, which is the wake failing in the one place a user
\* sees it.
Inv_NoStrandedWaiter ==
    \A h \in Holds :
        hold[h] = "parked" => (holdChan[h] \in closed \/ holdChan[h] = waiter)

\* The refcount is what it counts. It exists to bound map growth on an untrusted
\* key space, so a drifted count is either a leak or a premature
\* deregistration -- and the latter is what strands a waiter, which makes this
\* the companion to the invariant above rather than bookkeeping for its own sake.
Inv_WaiterRefsAccurate ==
    /\ waiter = 0 => refs = 0
    /\ waiter # 0 => refs = Cardinality(Registered)

Safety ==
    /\ TypeOK
    /\ Inv_NoStrandedWaiter
    /\ Inv_WaiterRefsAccurate

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* A parked request eventually stops being parked.
\*
\* HoldExpires is deliberately NOT fair, so the timeout cannot be what satisfies
\* this: the only fair way out of "parked" is the readiness edge closing the
\* channel the request is registered on. That makes this the liveness reading of
\* Inv_NoStrandedWaiter -- a stranded request is one whose channel no longer has
\* a registration, so no readiness edge can ever close it.
HoldLeavesPark ==
    \A h \in Holds : hold[h] = "parked" ~> hold[h] # "parked"

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

\* Weak fairness on the readiness edge and on the wakeup it causes, and nothing
\* else.
\*
\* WF is enough for EndpointsBecomeReady: a request can only park while the
\* engine is not ready, and nothing but this action sets readiness, so from the
\* moment a request is parked the action is CONTINUOUSLY enabled until it fires.
\*
\* Everything else is adversarial: requests may arrive or give up at any time,
\* endpoints may flap, and the agent may restart. In particular HoldExpires and
\* AgentRestarts are unfair on purpose -- either one would otherwise satisfy
\* HoldLeavesPark by abandoning the request, which is the outcome the property
\* exists to rule out.
Spec ==
    /\ Init
    /\ [][Next]_vars
    /\ WF_vars(EndpointsBecomeReady)
    /\ \A h \in Holds : WF_vars(HoldWakes(h))

\* Theorems (checked by TLC)
THEOREM Spec => []Safety
THEOREM Spec => HoldLeavesPark

====
