---- MODULE GatewayRouting ----
\* Durable registration and withdrawal for one engine generation. Envoy request
\* lifecycle correctness is an assumption discharged by runtime tests: a permit
\* is retained until dispatch and retries are impossible. An unknown permit is
\* never inferred complete from time, readiness or loss of network contact.
\*
\* Registration and Close are atomic mutations of one durable ConfigMap. Fence
\* and Grant serialize within one agent. A report combines the fence revision
\* and permit count in one snapshot. A restarted agent cannot inherit its old
\* session's registration while the same Envoy process is still running.
\*
\* The finite model explores two concurrent gateways with one reusable request
\* each. Current-generation traffic is omitted: it cannot acquire old permits.
\* Liveness is deliberately not promised during partitions or lost accounting.
EXTENDS Naturals, FiniteSets

CONSTANTS Gateways, AllowUnregisteredGrant, AllowUnfencedAck, AllowClosedPublication

VARIABLES registered, holders, fenced, permits, unknown, alive, stopped,
          lostAgent, acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled

vars == <<registered, holders, fenced, permits, unknown, alive, stopped,
          lostAgent, acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

Init ==
    /\ registered = {}
    /\ holders = {}
    /\ fenced = {}
    /\ permits = [g \in Gateways |-> 0]
    /\ unknown = {}
    /\ alive = Gateways
    /\ stopped = {}
    /\ lostAgent = {}
    /\ acknowledged = {}
    /\ closed = FALSE
    /\ retired = FALSE
    /\ unsafeGrant = FALSE
    /\ operatorUp = TRUE
    /\ instanceClosed = FALSE
    /\ replacementEnabled = FALSE

Register(g) ==
    /\ operatorUp
    /\ g \in alive \ registered
    /\ g \notin lostAgent
    /\ registered' = registered \cup {g}
    \* A new session receives only the current route, never a closed epoch.
    /\ fenced' = IF closed THEN fenced \cup {g} ELSE fenced
    /\ UNCHANGED <<holders, permits, unknown, alive, stopped, lostAgent,
                    acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

Grant(g) ==
    /\ g \in alive \ (fenced \cup lostAgent)
    /\ g \in registered \/ AllowUnregisteredGrant
    /\ permits[g] = 0
    /\ permits' = [permits EXCEPT ![g] = 1]
    /\ unsafeGrant' = unsafeGrant \/ retired
    /\ UNCHANGED <<registered, holders, fenced, unknown, alive, stopped,
                    lostAgent, acknowledged, closed, retired, operatorUp, instanceClosed, replacementEnabled>>

\* Completion and client cancellation both release only after all dispatch and
\* retry paths are stopped. Losing that evidence uses LoseAgent instead.
CompleteOrCancel(g) ==
    /\ permits[g] = 1
    /\ permits' = [permits EXCEPT ![g] = 0]
    /\ UNCHANGED <<registered, holders, fenced, unknown, alive, stopped,
                    lostAgent, acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

Close ==
    /\ operatorUp
    /\ ~closed
    /\ closed' = TRUE
    /\ holders' = registered
    /\ UNCHANGED <<registered, fenced, permits, unknown, alive, stopped,
                    lostAgent, acknowledged, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

Fence(g) ==
    /\ closed
    /\ g \in (registered \cap alive) \ (fenced \cup lostAgent)
    /\ fenced' = fenced \cup {g}
    /\ UNCHANGED <<registered, holders, permits, unknown, alive, stopped,
                    lostAgent, acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

ReportZero(g) ==
    /\ g \in (registered \cap alive) \ lostAgent
    /\ g \in fenced \/ AllowUnfencedAck
    /\ permits[g] = 0
    /\ g \notin unknown
    /\ acknowledged' = acknowledged \cup {g}
    /\ UNCHANGED <<registered, holders, fenced, permits, unknown, alive,
                    stopped, lostAgent, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

\* Agent failure/restart does not stop Envoy. The predecessor's accounting
\* cannot be replaced by a fresh empty report from the new process.
LoseAgent(g) ==
    /\ g \in alive \ lostAgent
    /\ lostAgent' = lostAgent \cup {g}
    /\ unknown' = IF permits[g] = 1 THEN unknown \cup {g} ELSE unknown
    /\ permits' = [permits EXCEPT ![g] = 0]
    /\ UNCHANGED <<registered, holders, fenced, alive, stopped, acknowledged,
                    closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

ProcessDies(g) ==
    /\ g \in alive
    /\ alive' = alive \ {g}
    /\ permits' = [permits EXCEPT ![g] = 0]
    /\ unknown' = unknown \ {g}
    /\ UNCHANGED <<registered, holders, fenced, stopped, lostAgent,
                    acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

ConfirmStopped(g) ==
    /\ operatorUp
    /\ g \notin alive
    /\ stopped' = stopped \cup {g}
    /\ UNCHANGED <<registered, holders, fenced, permits, unknown, alive,
                    lostAgent, acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

Retire ==
    /\ operatorUp
    /\ closed /\ ~retired
    /\ holders \subseteq acknowledged \cup stopped
    /\ retired' = TRUE
    /\ UNCHANGED <<registered, holders, fenced, permits, unknown, alive,
                    stopped, lostAgent, acknowledged, closed, unsafeGrant, operatorUp, instanceClosed, replacementEnabled>>

\* All coordination survives an operator restart; requests can continue while
\* the operator is down, but no registration, close or retirement can advance.
RestartOperator ==
    /\ operatorUp' = ~operatorUp
    /\ UNCHANGED <<registered, holders, fenced, permits, unknown, alive,
                    stopped, lostAgent, acknowledged, closed, retired, unsafeGrant, instanceClosed, replacementEnabled>>

\* Instance teardown closes publication in the same durable record as its
\* route fences. Checking a separate Instance object before CAS is insufficient:
\* a concurrent engine reconciler may still publish a higher generation.
CloseInstance ==
    /\ operatorUp /\ ~instanceClosed
    /\ instanceClosed' = TRUE
    /\ replacementEnabled' = FALSE
    /\ closed' = TRUE
    /\ holders' = IF closed THEN holders ELSE registered
    /\ UNCHANGED <<registered, fenced, permits, unknown, alive, stopped,
                    lostAgent, acknowledged, retired, unsafeGrant, operatorUp>>

PublishReplacement ==
    /\ operatorUp
    /\ ~instanceClosed \/ AllowClosedPublication
    /\ replacementEnabled' = TRUE
    /\ UNCHANGED <<registered, holders, fenced, permits, unknown, alive, stopped,
                    lostAgent, acknowledged, closed, retired, unsafeGrant, operatorUp, instanceClosed>>

Next == CloseInstance \/ PublishReplacement \/ Close \/ Retire \/ RestartOperator \/
    \E g \in Gateways: Register(g) \/ Grant(g) \/ CompleteOrCancel(g) \/
        Fence(g) \/ ReportZero(g) \/ LoseAgent(g) \/ ProcessDies(g) \/ ConfirmStopped(g)

TypeOK ==
    /\ registered \subseteq Gateways /\ holders \subseteq Gateways
    /\ fenced \subseteq Gateways /\ unknown \subseteq Gateways
    /\ alive \subseteq Gateways /\ stopped \subseteq Gateways
    /\ lostAgent \subseteq Gateways /\ acknowledged \subseteq Gateways
    /\ permits \in [Gateways -> 0..1]
    /\ closed \in BOOLEAN /\ retired \in BOOLEAN /\ unsafeGrant \in BOOLEAN
    /\ operatorUp \in BOOLEAN /\ instanceClosed \in BOOLEAN /\ replacementEnabled \in BOOLEAN

NoDispatchAfterRetirement == ~unsafeGrant
NoOutstandingAtRetirement == retired =>
    (/\ unknown = {} /\ \A g \in Gateways: permits[g] = 0)
InstanceClosureIsSticky == instanceClosed => ~replacementEnabled
Safety == TypeOK /\ NoDispatchAfterRetirement /\ NoOutstandingAtRetirement /\ InstanceClosureIsSticky
Spec == Init /\ [][Next]_vars
====
