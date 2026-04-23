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
- TLC runtime: < 30 seconds at `MaxGen=2, MaxSpec=3`

**FireboltInstance** (`formal/FireboltInstance.tla`):

- 3-phase lifecycle: `Provisioning → Ready ↔ Degraded`
- Sequential pipeline per reconcile: Postgres → Metadata → Gateway
- `setInstanceReadyRollup` called even on early returns, so phase always reflects all three conditions
- Safety: `TypeOK` invariant; liveness: `EventuallyReady` and `ReadyIsStable`
- `InstancePhaseFailed` excluded: not reachable through internal transitions, only preservable if externally injected
- TLC runtime: < 5 seconds (32 reachable states)

### Phase 2 — `rapid` stateful property tests (complete)

`internal/controller/engine_property_test.go` uses `pgregory.net/rapid` to drive `computeEngineReconcile` with random operation sequences and verify the same invariants as the TLA+ spec after every step. Runs fully in-memory (no envtest) under `make test`.

- **Operations**: `Reconcile`, `CrashReconcile` (applies resources but not status), `ApplySpecChange`, `ScaleReplicas` (range 0–5, making `stopped` reachable), `PodsBecomesReady`, `DrainCompletes`, `DeleteEngine`
- **Invariants**: `Inv_TerminalConsistency`, `Inv_TerminalNoDraining`, `Inv_ActiveHasSTS`, `Inv_AlwaysRequeues` — mirror the TLA+ `Safety` predicate
- `CrashReconcile` simulates a crash between the last resource write and the status update in `applyEngineState`, exercising crash recovery on the next step

### Phase 3 — TLA+ → test-case generation (harness)

Extract every distinct execution path from the TLC state graph. Turn each path into a deterministic envtest scenario that replaces/extends the random rapid sequences. This closes the model-to-implementation gap: TLC drives the real reconciler.

Deliverable: `scripts/gen-tla-tests.py` + generated test stubs.

### Phase 4 — Kamera (conditional on maturity)

If Kamera reaches a stable release, adopt it to do Phase 1 + Phase 3 directly against the Go code without maintaining a parallel TLA+ spec.

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
