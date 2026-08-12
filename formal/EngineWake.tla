---- MODULE EngineWake ----
\* TLA+ specification of the wake-on-zero demand protocol: the path from a query
\* arriving at a gateway for a parked engine to that engine's replica count
\* being raised, and back down again once it goes quiet.
\*
\* Three actors, all of which this module represents explicitly, because every
\* hazard in this protocol lives in the seam between two of them:
\*
\*   wake agent        - stamps demand when a query arrives for an engine with
\*                       no ready endpoints, and retains the stamp for
\*                       `Retention`
\*   operator poller   - copies the agent's stamp into its own cache, filtered
\*                       to engines at spec.replicas = 0, replacing the cache
\*                       wholesale on every poll
\*   engine reconciler - computeAutoStopDecision, which honours cached demand
\*                       only while it is fresh within `WakeTTL`
\*
\* plus an environment that owns the clock, the metric scrape, poll loss and
\* agent restarts.
\*
\* The agent's stamp and the operator's cache are SEPARATE variables. That is the
\* whole point of the model: they are two views of the same fact, taken at
\* different instants, and the protocol's correctness is a statement about how
\* far apart they may drift. Collapsing them into one variable would make every
\* interesting property vacuous.
\*
\* Verified properties:
\*   Safety                         - TypeOK, Inv_ScrapeOnlyWhenRunning,
\*                                    Inv_DemandOnlyForStoppedEngines
\*   ScaleUpOnlyOnFreshDemand       - an engine's replicas only ever grow while
\*                                    the operator holds demand fresh within TTL
\*   RunningEngineOnlyScalesToIdle  - demand never resizes a running engine
\*   WakeIsNotLost                  - repeatedly-observed fresh demand for a
\*                                    stopped engine always ends in a scale-up
\*   FreshDemandWakesOrExpires      - fresh demand at the gateway either wakes
\*                                    the engine or provably stops being fresh
\*
\* To check with TLC:
\*   java -jar tla2tools.jar -config EngineWake.cfg EngineWake.tla
\*
\* The agent's OTHER half -- parking a request on the EndpointSlice-derived
\* readiness signal and releasing it when endpoints appear -- is
\* WakeAgentHold.tla. The two are separate modules because no property spans
\* them: everything here is about how stale a demand timestamp may be, and
\* everything there is about the ORDER of two in-memory bookkeeping operations,
\* with no clock in it at all. One module would multiply two independent state
\* spaces (120k states against 4k and 300) and check nothing across the seam.
\*
\* ---------------------------------------------------------------------------
\* Design decisions
\* ---------------------------------------------------------------------------
\*
\*   - Time is a bounded counter advanced by EnvTick, and freshness is
\*     `now - stamp < WakeTTL` over that domain. The three durations that matter
\*     are modelled at their real RATIO, not their real magnitude:
\*     WakeTTL < Retention, and IdleTimeout is the same order as WakeTTL. What
\*     the protocol depends on is that the agent keeps a stamp for longer than
\*     the operator will act on it, so a stamp can be re-observed after it has
\*     stopped being actionable. Two ticks of TTL against three of retention is
\*     the smallest domain in which that gap exists at all.
\*
\*   - Only the OPERATOR is assumed fair: weak fairness on a successful poll and
\*     strong fairness on the wake scale-up. The environment is unconstrained --
\*     no fairness on the clock, on demand arriving, on scrapes, on poll failure
\*     or on agent restarts -- so every property below holds against an
\*     adversary that drops polls and restarts the agent at will.
\*
\*   - The replica count is a three-level abstraction, not a number: parked
\*     (IdleReplicas), the level auto-stop scales up to (ActiveReplicas), and a
\*     hand-set level auto-stop never chooses (UserReplicas). UserReplicas is
\*     reachable as an initial condition only, because the reconciler cannot
\*     produce it; without it "demand must not resize a RUNNING engine" is not
\*     expressible, since a wake targets ActiveReplicas and an engine already
\*     there would be left alone by accident rather than by the filter that is
\*     supposed to keep the demand out of the cache.
\*
\*   - Endpoint readiness is NOT a variable here. The agent stamps demand when
\*     an engine has no ready endpoints, so a stamp for a RUNNING engine is
\*     exactly what a node drain or a rolling restart produces; leaving readiness
\*     out lets DemandArrives fire at any replica count, which is a superset of
\*     the real behaviours and is what
\*     Inv_DemandOnlyForStoppedEngines needs to be non-vacuous. Readiness is a
\*     variable in WakeAgentHold.tla, where it drives something.
\*
\*   - What is deliberately NOT here. autoStop.enabled = FALSE returns before
\*     anything in this protocol runs, so modelling it adds a variable and no
\*     hazard. A schedule window is a pure function of the wall clock that lands
\*     on the same target as a wake and interacts with nothing else. The hold
\*     capacity limiter is out of scope beyond the fact that demand is stamped
\*     BEFORE the cap is consulted, which is why a shed request still registers
\*     demand: DemandArrives stamps unconditionally and nothing here can shed.
\*     The blue-green phase machine, which gates auto-stop to the terminal
\*     phases, is FireboltEngine.tla's subject.
\*
\*   - The three counterexample CONSTANTS below each remove one shipped guard, in
\*     the same idiom as FireboltEngine.tla's five flags and
\*     SigningKeyRotation.tla's AnchorAtDemotion. All are FALSE in EngineWake.cfg;
\*     each has an EngineWakeNaive*.cfg that flips exactly one of them and pins
\*     the resulting violation in formal/counterexamples.tsv.

EXTENDS Integers

CONSTANTS
    MaxTime,         \* clock bound
    WakeTTL,         \* operator's wake TTL, in ticks
    Retention,       \* agent's demand retention, in ticks (must exceed WakeTTL)
    IdleTimeout,     \* auto-stop idle timeout, in ticks
    ActiveReplicas,  \* autoStop.activeReplicas
    IdleReplicas,    \* autoStop.idleReplicas
    UserReplicas,    \* a hand-set replica count auto-stop never chooses
    \* --- counterexample flags, all FALSE in the shipped configuration ---
    StoppedBeforeWake,
      \* TRUE moves the `spec.replicas == 0` early return ABOVE the wake check
      \* in computeAutoStopDecision. An auto-stopped engine IS an engine at zero
      \* replicas, so the stopped branch then swallows every wake and the engine
      \* can never come back. Violates WakeIsNotLost.
    WakeIgnoresTTL,
      \* TRUE drops `now - WakeRequestedAt < WakeTTL` from the wake guard, so a
      \* stamp the agent still retains but the operator should no longer act on
      \* resurrects an expired demand. Violates ScaleUpOnlyOnFreshDemand.
    PollIgnoresReplicas
      \* TRUE drops the poller's filter to engines at spec.replicas = 0. The
      \* agent stamps demand whenever an engine has no ready endpoints, which
      \* also covers a running engine mid node-drain, so an unfiltered stamp
      \* pins such an engine at ActiveReplicas. Violates
      \* Inv_DemandOnlyForStoppedEngines.

NoStamp == -1
Times   == 0..MaxTime
Stamps  == {NoStamp} \cup Times

\* Levels the replica count can take. The reconciler only ever writes
\* ActiveReplicas or IdleReplicas; UserReplicas is reachable as an initial
\* condition only.
ReplicaLevels == {IdleReplicas, ActiveReplicas, UserReplicas}

\* What one metric scrape of a running engine reported. "quiet" also covers the
\* no-scrape case of a parked engine: runAutoStop leaves the observation
\* zero-valued when spec.replicas is 0, which Inv_ScrapeOnlyWhenRunning pins.
Activities == {"quiet", "busy", "scrapeFailed"}

\* status.autoStopReason tokens this protocol can write. "Disabled" and
\* "ScheduleActive" belong to the two branches deliberately left out of scope.
Reasons == {"Stopped", "WakeRequested", "ScrapeFailed", "ActivityObserved",
            "Idle", "Initializing"}

VARIABLES
    now,           \* the clock
    stamp,         \* the AGENT's last demand stamp for the engine (NoStamp: none)
    cache,         \* the OPERATOR's cached copy of it (NoStamp: none)
    replicas,      \* spec.replicas
    lastActivity,  \* status.lastActivityTime (NoStamp: unset)
    activity,      \* what the last metric scrape reported
    reason         \* status.autoStopReason, i.e. the last decision taken

vars == <<now, stamp, cache, replicas, lastActivity, activity, reason>>

\* ---------------------------------------------------------------------------
\* Derived predicates
\* ---------------------------------------------------------------------------

\* Fresh at the agent: a stamp the agent still reports AND one the operator
\* would still act on.
StampFresh == stamp # NoStamp /\ now - stamp < WakeTTL

\* Fresh at the operator: what computeAutoStopDecision tests. WakeIgnoresTTL
\* removes the age test, which is the whole content of the guard.
CacheFresh   == cache # NoStamp /\ now - cache < WakeTTL
WakeObserved == cache # NoStamp /\ (WakeIgnoresTTL \/ now - cache < WakeTTL)

\* Whether the wake branch is reached at all. In the shipped precedence it sits
\* above the stopped early return, so it is reached at every replica count;
\* StoppedBeforeWake inverts that.
WakeWins == WakeObserved /\ ~(StoppedBeforeWake /\ replicas = IdleReplicas)

IdleElapsed == lastActivity # NoStamp /\ now - lastActivity >= IdleTimeout

\* ---------------------------------------------------------------------------
\* Initial state
\* ---------------------------------------------------------------------------

\* The engine starts at any of the three replica levels with nothing observed
\* yet: parked (the interesting case), at the level auto-stop scales to, or at a
\* hand-set level auto-stop did not choose.
Init ==
    /\ now          = 0
    /\ stamp        = NoStamp
    /\ cache        = NoStamp
    /\ replicas     \in ReplicaLevels
    /\ lastActivity = NoStamp
    /\ activity     = "quiet"
    /\ reason       = IF replicas = IdleReplicas THEN "Stopped" ELSE "Initializing"

\* ---------------------------------------------------------------------------
\* Environment
\* ---------------------------------------------------------------------------

EnvTick ==
    /\ now < MaxTime
    /\ now' = now + 1
    /\ UNCHANGED <<stamp, cache, replicas, lastActivity, activity, reason>>

\* Only a running engine is scraped: runAutoStop skips the scrape entirely at
\* zero replicas, so the observation stays "quiet" there.
EnvScrapeObserves(a) ==
    /\ replicas > IdleReplicas
    /\ activity # a
    /\ activity' = a
    /\ UNCHANGED <<now, stamp, cache, replicas, lastActivity, reason>>

\* ---------------------------------------------------------------------------
\* The wake agent
\* ---------------------------------------------------------------------------

\* A query arrives for an engine with no ready endpoints, so the agent stamps
\* demand. Unconditional and before anything else, mirroring handleHold: a
\* request that is about to be shed, or whose client hangs up immediately, still
\* proves someone asked, and the timestamp is what survives the client leaving.
DemandArrives ==
    /\ stamp' = now
    /\ stamp' # stamp
    /\ UNCHANGED <<now, cache, replicas, lastActivity, activity, reason>>

\* The retention window elapses and the agent forgets the stamp.
AgentPrunesDemand ==
    /\ stamp # NoStamp
    /\ now - stamp >= Retention
    /\ stamp' = NoStamp
    /\ UNCHANGED <<now, cache, replicas, lastActivity, activity, reason>>

\* The agent process restarts. Its demand map is entirely in memory, so the
\* stamp goes with it. The operator's cache does NOT: it outlives the agent that
\* produced it, which is one of the two ways the two views come apart.
AgentRestarts ==
    /\ stamp # NoStamp
    /\ stamp' = NoStamp
    /\ UNCHANGED <<now, cache, replicas, lastActivity, activity, reason>>

\* ---------------------------------------------------------------------------
\* The operator's demand poller
\* ---------------------------------------------------------------------------

\* A successful poll. The cache is REPLACED, not merged: what the agents report
\* now, filtered to engines the operator sees at zero replicas, is the whole
\* cache afterwards. So this action can equally well erase demand -- when the
\* engine is running, or when the agent no longer reports it.
PollObserves ==
    /\ cache' = IF (replicas = IdleReplicas \/ PollIgnoresReplicas)
                  THEN stamp
                  ELSE NoStamp
    /\ cache' # cache
    /\ UNCHANGED <<now, stamp, replicas, lastActivity, activity, reason>>

\* A poll that reached no agent: listing gateway pods failed, or every scrape
\* did. The engine's entry is missing from the fresh map, so the wholesale
\* replace drops it even though the agent is still holding the stamp. This is
\* the lost-wakeup mechanism, and it is why the agent's retention outlives the
\* operator's TTL: the next successful poll re-observes the same stamp.
PollLosesCache ==
    /\ cache # NoStamp
    /\ cache' = NoStamp
    /\ UNCHANGED <<now, stamp, replicas, lastActivity, activity, reason>>

\* ---------------------------------------------------------------------------
\* The engine reconciler: one action per arm of computeAutoStopDecision
\* ---------------------------------------------------------------------------
\*
\* The arms are mutually exclusive and exhaustive, so exactly one is enabled in
\* every state: the decision is a total function of the state, as a pure
\* function should be. Where the enabled arm changes nothing the model records a
\* self-loop, which is what lets the generated state cover accept a no-op
\* reconcile exactly where the model permits one.

\* Wake: scale to ActiveReplicas. Above the stopped branch, which is
\* load-bearing -- see StoppedBeforeWake.
ReconcileWake_ScaleUp ==
    /\ WakeWins
    /\ replicas # ActiveReplicas
    /\ replicas' = ActiveReplicas
    /\ reason'   = "WakeRequested"
    /\ UNCHANGED <<now, stamp, cache, lastActivity, activity>>

\* Wake on an engine already at ActiveReplicas: report it, change nothing.
ReconcileWake_Pinned ==
    /\ WakeWins
    /\ replicas = ActiveReplicas
    /\ reason' = "WakeRequested"
    /\ UNCHANGED <<now, stamp, cache, replicas, lastActivity, activity>>

\* Parked and nobody is asking: stay out of the way.
ReconcileStopped ==
    /\ ~WakeWins
    /\ replicas = IdleReplicas
    /\ reason' = "Stopped"
    /\ UNCHANGED <<now, stamp, cache, replicas, lastActivity, activity>>

\* A failed scrape refreshes the idle clock exactly as observed activity does,
\* so a scrape-failure window looks as un-idle to the next successful poll as a
\* window full of queries would.
ReconcileScrapeFailed ==
    /\ ~WakeWins
    /\ replicas > IdleReplicas
    /\ activity = "scrapeFailed"
    /\ lastActivity' = now
    /\ reason'       = "ScrapeFailed"
    /\ UNCHANGED <<now, stamp, cache, replicas, activity>>

ReconcileActivity ==
    /\ ~WakeWins
    /\ replicas > IdleReplicas
    /\ activity = "busy"
    /\ lastActivity' = now
    /\ reason'       = "ActivityObserved"
    /\ UNCHANGED <<now, stamp, cache, replicas, activity>>

\* First quiet observation on an engine with no idle anchor yet: there is
\* nothing to measure against, so anchor and look again.
ReconcileInitialize ==
    /\ ~WakeWins
    /\ replicas > IdleReplicas
    /\ activity = "quiet"
    /\ lastActivity = NoStamp
    /\ lastActivity' = now
    /\ reason'       = "Initializing"
    /\ UNCHANGED <<now, stamp, cache, replicas, activity>>

\* Quiet for long enough: park the engine.
ReconcileIdle_ScaleDown ==
    /\ ~WakeWins
    /\ replicas > IdleReplicas
    /\ activity = "quiet"
    /\ IdleElapsed
    /\ replicas' = IdleReplicas
    /\ reason'   = "Idle"
    /\ UNCHANGED <<now, stamp, cache, lastActivity, activity>>

\* Quiet, but not for long enough yet.
ReconcileWarm ==
    /\ ~WakeWins
    /\ replicas > IdleReplicas
    /\ activity = "quiet"
    /\ lastActivity # NoStamp
    /\ ~IdleElapsed
    /\ reason' = "ActivityObserved"
    /\ UNCHANGED <<now, stamp, cache, replicas, lastActivity, activity>>

\* ---------------------------------------------------------------------------
\* Next-state relation
\* ---------------------------------------------------------------------------

Next ==
    \/ EnvTick
    \/ \E a \in Activities : EnvScrapeObserves(a)
    \/ DemandArrives
    \/ AgentPrunesDemand
    \/ AgentRestarts
    \/ PollObserves
    \/ PollLosesCache
    \/ ReconcileWake_ScaleUp
    \/ ReconcileWake_Pinned
    \/ ReconcileStopped
    \/ ReconcileScrapeFailed
    \/ ReconcileActivity
    \/ ReconcileInitialize
    \/ ReconcileIdle_ScaleDown
    \/ ReconcileWarm

\* ---------------------------------------------------------------------------
\* Safety invariants
\* ---------------------------------------------------------------------------

TypeOK ==
    /\ now \in Times
    /\ stamp \in Stamps
    /\ cache \in Stamps
    /\ lastActivity \in Stamps
    \* No view of the past is dated in the future: the agent stamps at `now`,
    \* the operator copies what the agent has, and the reconciler anchors the
    \* idle clock at `now`.
    /\ stamp # NoStamp => stamp <= now
    /\ cache # NoStamp => cache <= now
    /\ lastActivity # NoStamp => lastActivity <= now
    /\ replicas \in ReplicaLevels
    /\ activity \in Activities
    /\ reason \in Reasons

\* A parked engine is never scraped, so its observation carries no activity and
\* no scrape failure. Without this, "quiet" would be indistinguishable from
\* "not observed" and the idle path could be entered on a scrape that never ran.
Inv_ScrapeOnlyWhenRunning ==
    replicas = IdleReplicas => activity = "quiet"

\* THE poller property. While the operator holds actionable demand, the engine
\* it belongs to is either parked or was itself woken by that demand.
\*
\* The agent stamps demand for any engine with no ready endpoints, which
\* includes a RUNNING engine during a node drain or a rolling restart. The
\* poller's filter to engines at spec.replicas = 0 is what keeps such a stamp
\* out of the cache; without it the wake branch -- which sits above the idle
\* check -- would pin a hand-sized engine at ActiveReplicas for the TTL, scaling
\* it DOWN in the middle of its own outage and freezing its idle timer.
\*
\* The freshness qualifier is not a weakening: an expired entry in the cache is
\* inert, and the reconciler is what refuses it. What must never happen is the
\* operator holding demand it WOULD act on against an engine that is running.
Inv_DemandOnlyForStoppedEngines ==
    CacheFresh => (replicas = IdleReplicas \/ reason = "WakeRequested")

Safety ==
    /\ TypeOK
    /\ Inv_ScrapeOnlyWhenRunning
    /\ Inv_DemandOnlyForStoppedEngines

\* ---------------------------------------------------------------------------
\* Action properties
\* ---------------------------------------------------------------------------

\* Retention against TTL, stated where it belongs: on the transition, not on the
\* state. The agent keeps a stamp for longer than the operator will act on it,
\* so the operator's cache legitimately holds entries that are too old to
\* honour. What must hold is that no engine ever GROWS on the strength of one.
\*
\* A state invariant cannot say this: `reason = "WakeRequested"` persists into
\* states where the cache has since gone stale or been wiped, and both are
\* correct. The claim is about the instant the decision is taken, and the
\* reconciler does not advance the clock, so `now` and `cache` in the pre-state
\* ARE the decision's inputs.
ScaleUpOnlyOnFreshDemand ==
    [][ replicas' > replicas => CacheFresh ]_vars

\* Demand never resizes a running engine. The only replica change a running
\* engine may undergo in this protocol is being parked at IdleReplicas; moving
\* it to ActiveReplicas from any other running level would mean auto-stop
\* overriding a count it did not choose.
RunningEngineOnlyScalesToIdle ==
    [][ (replicas > IdleReplicas /\ replicas' # replicas) => replicas' = IdleReplicas ]_vars

\* ---------------------------------------------------------------------------
\* Liveness
\* ---------------------------------------------------------------------------

\* An engine the operator repeatedly sees fresh demand for is eventually
\* started.
\*
\* The antecedent is []<> rather than a single occurrence, and that is the
\* honest form. A single fresh observation CAN be lost: PollLosesCache erases
\* the cache the instant before the reconciler would have read it, and if the
\* stamp then ages past the TTL the wake is gone -- bounded, in production, by
\* the client's own retry and by the agent retaining the stamp for twice the
\* TTL, but real. What the protocol guarantees is that demand which keeps being
\* observable is not lost, and with strong fairness on the scale-up that is
\* exactly what the reconciler delivers.
WakeIsNotLost ==
    []<>(CacheFresh /\ replicas = IdleReplicas) => <>(replicas > IdleReplicas)

\* The leads-to companion, over the AGENT's stamp rather than the operator's
\* cache: fresh demand at the gateway either wakes the engine or provably stops
\* being fresh. The escape clause is the contract, not a loophole -- an
\* abandoned wake must age out rather than pin the engine -- and paired with
\* ScaleUpOnlyOnFreshDemand it says a wake happens exactly while the demand is
\* current.
FreshDemandWakesOrExpires ==
    (StampFresh /\ replicas = IdleReplicas) ~> (replicas > IdleReplicas \/ ~StampFresh)

\* ---------------------------------------------------------------------------
\* Temporal spec
\* ---------------------------------------------------------------------------

\* Two fairness assumptions, and no more.
\*
\* WF on PollObserves: the demand poller is an unconditional ticker, so a poll
\* that reaches an agent eventually happens. Its failing counterpart,
\* PollLosesCache, is deliberately left unfair -- nothing forces a scrape to
\* fail, and nothing stops it failing arbitrarily often.
\*
\* SF on ReconcileWake_ScaleUp: the reconciler is not starved while a wake is
\* possible. Strong rather than weak fairness because the poller can disable the
\* scale-up between any two of its own polls, so the action is enabled
\* infinitely often without ever being continuously enabled. In the
\* implementation this is what the poller's GenericEvent emission buys: a newly
\* observed stamp enqueues a reconcile rather than waiting on the auto-stop
\* poll.
\*
\* Nothing else is fair. The clock, demand arrivals, scrapes, agent restarts and
\* poll failures are all adversarial, which is what makes the properties above
\* statements about the protocol rather than about a well-behaved environment.
Spec ==
    /\ Init
    /\ [][Next]_vars
    /\ WF_vars(PollObserves)
    /\ SF_vars(ReconcileWake_ScaleUp)

\* Theorems (checked by TLC)
THEOREM Spec => []Safety
THEOREM Spec => ScaleUpOnlyOnFreshDemand
THEOREM Spec => RunningEngineOnlyScalesToIdle
THEOREM Spec => WakeIsNotLost
THEOREM Spec => FreshDemandWakesOrExpires

====
