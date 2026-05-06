# Formal Verification Strategy

This document captures the rationale, tooling choices, and phased plan for introducing formal verification into the Firebolt operator.

## What we want to verify

The operator has two reconciliation loops with well-defined state machines. The properties hardest to cover with conventional unit and E2E tests — but easiest to get wrong — are:

1. **Phase-transition safety** — no bad state is reachable (e.g. two generations simultaneously serving traffic; service selector pointing to a non-existent generation).
2. **Phase-transition liveness** — every reachable state eventually reaches `stable` if the operator keeps running.
3. **Spec-change-mid-flight rules** — "abandon if creating, defer if draining/cleaning" hold under all interleavings of user edits and reconcile steps.
4. **Crash safety** — crashing at any phase boundary and recovering always leads to a consistent state (partially covered today by `crash_points_e2e.go`, but not exhaustively).
5. **Resource invariants** — owner references, generation labels, and finalizer presence are always correct.
6. **Instance gate** — an engine never reaches `stable` while the referenced `FireboltInstance` is unready, in the phases where the gate is enforced (`stable`, `creating`).

The existing crash-point tests cover (4) at specific predetermined checkpoints. The rest are covered by E2E tests but not exhaustively across all orderings.

## Tool landscape

### TLA+ / TLC (chosen primary tool)

TLA+ is a mathematical specification language paired with TLC, an explicit-state model checker that exhaustively enumerates all reachable states up to a bound. AWS has used it since 2011 on S3, DynamoDB, and internal services (a TLC run found a 35-step S3 bug no other technique would have caught). MongoDB, Datadog, Kafka, and Azure Cosmos DB all publish experience reports.

A 6-phase state machine with two CRDs maps almost directly to PlusCal (TLA+'s pseudocode front-end). The `FireboltEngine` reconciler translates to roughly 150–200 lines of PlusCal: one `phase` variable, `currentGeneration`, `activeGeneration`, `drainingGeneration`, `specWantsStop`, transition actions for each phase, and invariants. TLC exhaustively explores every reachable sequence of reconcile steps within the model.

**Harness potential** — two paths with production evidence:

- *Trace checking*: instrument the reconciler to emit JSON state snapshots at each phase transition; a script feeds the log to TLC as a `TraceSpec`. TLC checks whether every observed execution is a valid behavior under the model. Works well for a sequential level-triggered reconciler because capturing consistent snapshots is trivial (just the CR status after each `applyEngineState` call). MongoDB attempted this against multithreaded C++; it is far simpler for a single-goroutine controller loop.
- *Test-case generation*: have TLC enumerate every distinct execution path (e.g., `stable → spec-change → creating → crash → recover → creating → switching → draining → stable`). Each path becomes a deterministic envtest scenario. MongoDB did this and generated 87 000 test cases with 100 % branch coverage, finding a real bug in the process.

**Learning curve**: ~2 weeks to a first useful PlusCal spec using [learntla.com](https://learntla.com). Approachable for Go engineers who already think in state machines.

**Tooling**: TLA+ Toolbox (IDE), VS Code extension, TLC command-line. All free and open-source. Apalache (SMT-based, handles larger state spaces) is available if TLC's state space explodes — unlikely for this model.

**Weakness**: the model and implementation drift unless the harness (trace checking) is in place to enforce alignment.

### `rapid` stateful property tests (chosen complement)

`pgregory.net/rapid` is a Go property-based testing library with a `StateMachine` interface. You define: initial state, operations (apply spec change, inject crash, delete CR), invariants. Rapid generates random sequences of operations against the real reconciler in envtest and shrinks failures to minimal reproducers.

This is the lowest-effort path to immediate value: it runs actual Go code, requires no new specification language, and the invariants are the same ones written in TLA+. It is not formal — it is probabilistic — but it catches the same class of bugs and produces debuggable failures.

**Learning curve**: 1–2 days if you already write envtest unit tests.

### Acto (automated E2E correctness testing)

A University of Illinois tool (SOSP'23, [xlab-uiuc/acto](https://github.com/xlab-uiuc/acto)) that systematically mutates CRD fields on a real Kind cluster and checks convergence, crash recovery, and misoperation handling. Found 80+ bugs across 36 open-source operators. No code changes required — just a config describing the CRD schema.

Weaker than TLA+ + rapid for state-machine correctness because execution speed bounds the state space, but more systematic than hand-written E2E tests. A reasonable CI addition.

### Kamera (watch, not yet)

A UC Santa Cruz research tool that simulates the Kubernetes API server in-process and runs `controller-runtime` Go controllers against it, exhaustively exploring reachable states on a single thread — the best-of-both-worlds result. The author calls it "research-ready"; no production use yet. Worth re-evaluating in 6–12 months. If it matures, it would subsume Phases 1 and 3 below by doing TLC-style exploration of the real Go code directly.

### Ruled out

| Tool | Reason |
|------|--------|
| **Gobra** | 3.6× annotation-to-code ratio in the only production case (VerifiedSCION); requires modeling all of `controller-runtime`; no K8s use cases. |
| **Anvil** | Formally proves ESR (Eventually Stable Reconciliation) for Kubernetes controllers — exact right abstraction — but is Rust-only (Verus verifier). |
| **Spin/Promela** | Technically capable but weaker ecosystem than TLA+ for distributed systems; no practical advantage here. |
| **TLAPS** | Interactive theorem prover for infinite-state systems; not needed for a finite phase machine. |
| **Alloy 6** | Better for structural/relational properties; TLA+ is the stronger fit for temporal liveness properties. |

## Phased plan

### Phase 1 — TLA+ specs of the reconciler state machines (complete)

**FireboltEngine** (`formal/FireboltEngine.tla`):

- 6 phases: `stable`, `creating`, `switching`, `draining`, `cleaning`, `stopped`
- Generation counters: `currentGeneration`, `activeGeneration`, `drainingGeneration`
- Spec changes at any point (abandon/defer rules); scale-to-zero via `specWantsStop`
- Safety invariants: active generation always has resources; service selector is consistent; quiesced terminal phase matches `spec.replicas` intent
- Liveness: engine always eventually reaches a terminal phase (`stable` or `stopped`)
- TLC runtime: ~2 seconds at `MaxGen=3, MaxSpec=4` (~3,200 reachable states)

**FireboltInstance** (`formal/FireboltInstance.tla`):

- 3-phase lifecycle: `Provisioning → Ready ↔ Degraded`
- Sequential pipeline per reconcile: Postgres → Metadata → Gateway
- `setInstanceReadyRollup` called even on early returns, so phase always reflects all three conditions
- Safety: `TypeOK` invariant; liveness: `EventuallyReady` (`(<>[]AllReady) => <>(phase = "ready")`) and `ReadyIsStable`
- `EventuallyReady` uses permanent-stability precondition `<>[]AllReady`, not transient `AllReady`: the environment can degrade a component before ReconcileRun fires, at which point ReconcileRun is a stuttering step and no fairness condition can force progress
- `InstancePhaseFailed` excluded: not reachable through internal transitions, only preservable if externally injected
- TLC runtime: < 5 seconds (32 reachable states)

### Phase 2 — `rapid` stateful property tests (complete)

`internal/controller/engine_property_test.go` uses `pgregory.net/rapid` to drive `computeEngineReconcile` with random operation sequences and verify the same invariants as the TLA+ spec after every step. Runs fully in-memory (no envtest) under `make test`.

- **Operations**: `Reconcile`, `CrashReconcile` (applies resources but not status), `ApplySpecChange`, `ScaleReplicas` (range 0–5, making `stopped` reachable), `PodsBecomesReady`, `DrainCompletes`, `DeleteEngine`
- **Invariants**: `Inv_TerminalConsistency`, `Inv_TerminalNoDraining`, `Inv_ActiveHasSTS`, `Inv_AlwaysRequeues` — mirror the TLA+ `Safety` predicate
- `CrashReconcile` simulates a crash between the last resource write and the status update in `applyEngineState`, exercising crash recovery on the next step

### Phase 3 — TLA+ state cover (complete)

Phase 2's `rapid` sequences explore the compute layer with random walks. Phase 3 turns the TLC state graph into a deterministic exhaustive state cover for the same compute layer: every reachable TLA+ state becomes a test case that calls `computeEngineReconcile` and asserts the resulting state lies in the model's "reconciler closure" of the start (states reachable via 0+ consecutive reconciler-only transitions). Random walks miss states they did not visit; state cover hits every reachable input by construction.

The level-triggered design motivates state cover over edge cover (path replay). Reconcile correctness is a state-local property — given any reachable state, `Reconcile(X) ∈ successors(X)` — so once every reachable state is checked, every transition is checked by composition. Path replay is the right tool for event-sourced systems; this is not one.

The fixture lives in the controller package as a generated Go file:

- `formal/FireboltEngine.dot` — TLC state graph dump (gitignored, regenerated via `make formal-dump`)
- `scripts/gen-tla-state-tests.py` — DOT parser + closure builder
- `internal/controller/engine_tla_states_data_test.go` — generated fixture (committed)
- `internal/controller/engine_tla_state_test.go` — `TestTLAEngineStateCover` runs against the fixture

At `MaxGen=3, MaxSpec=4` TLC produces 3,202 reachable states (uninitialised excluded at generation time, since the controller's first reconcile handles those via a single early-return that existing unit tests cover). 36 fall on the model's MaxGen ceiling where the spec's bounded handling diverges from the unbounded implementation (documented in `tlaModelBoundary`); 952 are skipped because the outer Reconcile method's instance gate prevents the compute layer from running (`tlaShouldGateOut`). The remaining 2,214 states are exercised against `computeEngineReconcile` in a few seconds, with all Phase 2 invariants checked after each call.

CI guard: `make formal-verify` regenerates the fixture and fails if the result differs from what is committed.

### Phase 4 — Kamera (conditional on maturity)

If Kamera reaches a stable release, adopt it to do Phase 1 + Phase 3 directly against the Go code without maintaining a parallel TLA+ spec.

## The state-exploration / false-positive trade-off

Phases 1–3 protect us from the "harness too simple to find bugs" failure mode but leave us in a complementary trap: the harness is single-actor, fault-injection is limited to one crash point (`CrashReconcile`, which models only "all writes succeed, status write fails"), the Go side exercises only the compute layer, and time / informer staleness / write-conflicts are not modelled. Each axis we *don't* fuzz is a class of bug the harness cannot find.

The opposite trap follows from naive expansion: every new dimension of state exploration produces a class of *invalid* configurations that fail tests without representing real bugs (a fault-probability fuzz that fails too often to make progress; a clock that advances faster than scans can complete; a compactor fenced faster than it can finish). The discipline that makes expansion productive rather than noisy is to pair every new exploration axis with a model update *and* an invariant that distinguishes "invalid configuration" from "real bug". TLA+ already gives us that vocabulary; Phases 5–11 cash it in.

Phases 5–11 each add one axis. Each phase entry calls out the false-positive class it introduces and the invariant or fairness condition that keeps it from masking real bugs.

### Phase 5 — FireboltInstance harness parity

`formal/FireboltInstance.tla` exists; the rapid (Phase 2) and state-cover (Phase 3) harnesses do not. The second CRD has only formal-spec coverage today, no real-Go harness.

- `internal/controller/instance_property_test.go` — rapid stateful test that drives the instance reconciler's compute layer (Postgres → Metadata → Gateway pipeline) with random environment events (each component becoming `Ready` / `Degraded`) and asserts the same invariants as `FireboltInstance.tla` (`TypeOK`, `ReadyIsStable`, "phase reflects condition rollup").
- `formal/FireboltInstance.dot` — TLC state graph dump (gitignored, regenerated via `make formal-dump`).
- `scripts/gen-tla-state-tests.py` extended to handle either spec (selected via flag), or a sibling generator for the instance state graph.
- `internal/controller/instance_tla_states_data_test.go` (generated) and `instance_tla_state_test.go` exercise every reachable `FireboltInstance` state against the real `instance_*.go` compute functions.
- Wired into `make formal-verify` so the CI guard covers both CRDs.

**False-positive risk**: none. Same methodology, different state machine. Either it works mechanically or it surfaces a real bug in the instance reconciler.

### Phase 6 — Generalised crash-point coverage

`internal/controller/crash_points.go` enumerates 9 deterministic crash points used by E2E (`CrashAfterEngineConfigMapCreated`, `CrashAfterStatefulSetCreated`, …). The Phase 2 property test exercises only one (`CrashReconcile` ≈ status write skipped). The TLA spec models reconciles atomically, so every partial-write state is already a valid model state — no spec extension is required for safety, only coverage.

- New `CrashAt(point CrashPoint)` action in `engineSim` for each of the 9 crash points: applies side effects up to that point only. The existing `applyResult` already separates `EnsureStatefulSet`, `EnsureConfigMap`, `EnsureHeadlessSvc`, `EnsureClusterSvc` and the per-resource deletes, so per-write granularity is mostly there; a small refactor surfaces "stop after the *k*-th side effect".
- Note in `FireboltEngine.tla` acknowledging the property test goes finer-grained than the spec, listing the 9 sub-points, and explaining why no new spec actions are required (every partial state already lies in the spec's reachable set).

**False-positive risk**: low. The 9 crash points have hand-written E2E coverage already; the bar here is "rapid finds a sequence with no E2E counterpart that violates an invariant", which is by construction a real bug.

### Phase 8 — Informer cache staleness

Real reconciles read from the informer cache, which can lag by up to the watch round-trip relative to a write the controller itself just made. We previously modelled this as instantaneous.

- `clusterView` struct in `engineSim`: `m.api` is the source of truth (what the K8s API server stores); `m.cache` is the controller's informer view. `buildState` reads from `cache`; `applyResultUpTo` writes to `api`. All safety invariants in `Check` (and `tlaInvariants` in state cover) check against `api`.
- `CacheCatchesUp` action atomically copies the api snapshot to the cache (full snapshot, not per-resource — the simpler model keeps the false-positive surface contained at the cost of not exercising per-resource lag interleavings).
- Bounded staleness comes from rapid's uniform action distribution: with N actions in the StateMachine set, `CacheCatchesUp` fires on average every ~N steps. No fairness condition is needed in the harness; the article's "cache lags forever → test hangs" trap is avoided structurally rather than by an explicit invariant.

The TLA spec is **not** extended in this iteration. Adding `EnvCacheLag` + `WF_vars(EnvCacheCatchesUp)` + `Inv_NoLostWrite` would roughly double the state space and require re-running TLC against deeper bounds; the rapid harness is the right place to exercise cache lag for now. The state-cover test (Phase 3) initializes both views identically because it runs only one Reconcile per state — cache lag is meaningless across a single transition.

**Bug-finding scope**: the engine reconciler today is the only writer to its own child resources, so cache lag is one-way (cache catches up to api, never the reverse). The existing invariants don't catch new bug classes from this dimension because the controller's writes are based on the spec (in-memory, fresh), not on cache reads of resources it created. The infrastructure is in place; future controllers or future code that reads-then-writes a child resource (Get-Update on the resource itself, not on the parent CR) would expose new bug classes that this scaffolding can catch.

**False-positive risk**: low in practice. The two anticipated traps — permanent lag and per-resource interleaving — are sidestepped by atomic full-snapshot catch-up plus rapid's natural distribution. A future per-resource lag model would re-introduce the article's classic trap and would need explicit fairness; documented as a follow-up rather than a present-day concern.

### Phase 9 — Outer Reconcile envtest harness *(optional)*

Phases 2/3/6/7/8 all exercise the compute layer (`computeEngineReconcile`). The instance gate, finalizer handling, owner-ref drift, and optimistic-concurrency on status updates live in the outer `Reconcile` and are not exercised.

- `internal/controller/engine_outer_property_test.go` — rapid harness over the full `Reconcile` against envtest. Slower; gated to a separate `make test-property` target rather than `make test`.

**False-positive risk**: moderate. envtest occasionally stutters for reasons unrelated to controller correctness (apiserver timing, watch propagation). Mitigation: deterministic seeds and short fixed timeouts; a test failure that does not reproduce on replay is treated as infrastructure, not as a controller bug, until shown otherwise.

### Phase 11 — Time / drain timeouts *(deferred)*

The article's highest false-positive class. Deferred until Phases 6–8 are stable so we have the invariant-discipline patterns in place before introducing fuzzed time. Likely shape: a discrete logical clock, bounded drain deadlines, and `Inv_DrainCompletesByDeadline`-style invariants. Until then, drain-timeout behaviour is covered by hand-written E2E tests, not by the property/state-cover harness.

### Implementation order

5 → 6 → 8 → 9 → (defer) 11.

## References

- [How Amazon Web Services Uses Formal Methods (CACM)](https://cacm.acm.org/research/how-amazon-web-services-uses-formal-methods/)
- [eXtreme Modelling in Practice — MongoDB (VLDB)](https://arxiv.org/pdf/2006.00915)
- [Conformance Checking at MongoDB](https://www.mongodb.com/company/blog/engineering/conformance-checking-at-mongodb-testing-our-code-matches-our-tla-specs)
- [How Datadog Uses Formal Modeling, Lightweight Simulations, and Chaos Testing](https://www.datadoghq.com/blog/engineering/formal-modeling-and-simulation/)
- [Validating Traces of Distributed Programs Against TLA+ Specifications (FM 2024)](https://arxiv.org/pdf/2404.16075v2)
- [Anvil: Verifying Liveness of Cluster Management Controllers (OSDI'24)](https://www.usenix.org/conference/osdi24/presentation/sun-xudong)
- [Kivi: Verification for Cluster Management (USENIX ATC'24)](https://www.usenix.org/conference/atc24/presentation/liu-bingzhe)
- [Acto: Push-Button End-to-End Testing for Kubernetes Operators (SOSP'23)](https://www.usenix.org/publications/loginonline/acto-push-button-end-end-testing-operation-correctness-kubernetes-operators)
- [Kamera: Simulation to Verify Kubernetes Controller Logic](https://thenewstack.io/kamera-uses-simulation-to-verify-kubernetes-controller-logic/)
- [learntla.com — PlusCal/TLA+ tutorial](https://learntla.com)
- [rapid — Go property-based testing](https://github.com/flyingmutant/rapid)
