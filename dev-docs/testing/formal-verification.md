# Formal verification

The Firebolt Operator combines TLA+ model checking with generated Go state-cover tests, property-based tests, concrete E2E tests, and deliberately broken negative controls. Each layer answers a different question; a green TLC run alone does not prove that all controller behavior is correct.

## Model inventory

[`formal/model-scope.tsv`](../../formal/model-scope.tsv) is the machine-readable map from production functions to models and state-cover fixtures. It also records explicitly unmodeled functions and the reason for each exclusion.

| Specification | Modeled behavior | Go state cover |
| --- | --- | --- |
| [`FireboltEngine.tla`](../../formal/FireboltEngine.tla) | Six-phase blue-green lifecycle, orphaned-generation keep set, and the instance / class / Preset scheduling gates | `engine_tla_states_data_test.go` |
| [`FireboltInstance.tla`](../../formal/FireboltInstance.tla) | Component readiness rollup and Instance phase | `instance_tla_states_data_test.go` |
| [`SigningKeyRotation.tla`](../../formal/SigningKeyRotation.tla) | Fleet-safe JWT signing-key rotation | `rotation_tla_states_data_test.go` |
| [`EngineWake.tla`](../../formal/EngineWake.tla) | Wake demand, auto-stop ordering, and demand freshness | `wake_tla_states_data_test.go` |
| [`WakeAgentHold.tla`](../../formal/WakeAgentHold.tla) | Wake-agent waiter identity and release ordering | No Go binding; covered by TLC and wake-agent unit tests |

The stateful pod-template merge comparator (including the FireboltEngineClass and FireboltEnginePreset overlays), drain probes and timeouts, and the Go implementation of wake-agent waiter bookkeeping are explicitly outside the current model-to-Go bindings. A Preset or class spec edit is modeled as a `specVer` increment; the fail-closed Preset gate is the `presetReady` boolean, symmetric to `classReady`. Both flags (with `instanceReady`) form `RenderGatesOpen`, which guards terminal and creating actions only. Init (`uninitialized` to `creating`) is ungated. Consult `formal/model-scope.tsv` before describing a change as model-covered.

## Verification layers

### TLC model checking

TLC enumerates reachable model states under the bounds in each `.cfg` file and checks the safety and liveness properties declared there.

```bash
make formal-check
```

The target checks all five specifications.

### Pinned model counterexamples

Naive configurations deliberately remove one safety guard. [`formal/counterexamples.tsv`](../../formal/counterexamples.tsv) records the violation each configuration must continue to produce.

```bash
make formal-check-counterexample
```

A model check is useful only if a known-bad variant still makes it fail. The counterexample runner rejects an unregistered `*Naive*.cfg` file so negative controls cannot silently fall out of CI.

### Generated state-cover tests

For the four Go-bound models, `formal-dump` writes TLC state graphs and `scripts/gen-tla-state-tests.py` projects those graphs into committed Go fixtures.

```bash
make formal-gen
make formal-verify
```

`formal-gen` regenerates the fixtures. `formal-verify` regenerates them and fails when the committed output is stale. Do not hand-edit a generated `*_tla_states_data_test.go` file.

The state-cover tests call the real compute functions from every projected reachable input state and require their outputs to remain within the model's permitted successor relation or reconciler closure, depending on the model.

### Property-based tests

In-memory `rapid` harnesses generate random operation sequences against controller compute functions and check invariants after every action. They complement exhaustive bounded state coverage by exercising richer Go values and action sequences.

The outer-Reconcile harness uses envtest to cover responsibilities outside the pure compute layer, including the Instance gate, finalizers, owner references, Kubernetes writes, and status conflict behavior:

```bash
make test-property
```

Set `RAPID_CHECKS` for a deeper local run:

```bash
make test-property RAPID_CHECKS=100
```

### Pinned Go mutants

[`formal/mutants/manifest.tsv`](../../formal/mutants/manifest.tsv) maps deliberately broken Go patches to the exact test and failure evidence that must catch each mutation.

```bash
make formal-check-mutants
```

This target refuses to run with staged or unstaged tracked changes because it applies and reverses patches in the worktree. Run it from a clean branch or disposable worktree. A mutant passes only when the expected test fails for an expected invariant or closure message; compilation failure is not accepted as evidence.

### Concrete crash-recovery tests

The E2E suite pauses the write layer at named side-effect boundaries and verifies convergence and availability after restarting the in-process manager. See [E2E testing](e2e-testing.md#crash-recovery-coverage).

## Changing modeled code

Before modifying a controller state machine:

1. Find the function in [`formal/model-scope.tsv`](../../formal/model-scope.tsv).
2. Read the corresponding TLA+ specification and its `.cfg` bounds.
3. Decide whether the change affects the model projection, an invariant, or only an unmodeled implementation detail.
4. Update the specification and generated state-cover fixture when modeled behavior changes.
5. Add or update property and example-based tests at the lowest useful layer.
6. Run the relevant Go tests, `make formal-check`, `make formal-check-counterexample`, and `make formal-verify`.
7. From a clean worktree, run `make formal-check-mutants` when the reconciler or state-cover layer changes.

The model-scope CI check is a review guard, not a proof. It asks for model and fixture movement when a bound production function changes. A change that does not affect the abstraction can explain why in the pull request, but the explanation should identify the abstraction boundary precisely.

## Adding a model

To add a model that binds to Go:

1. Add the `.tla` and shipped `.cfg` files under `formal/`.
2. Add at least one naive configuration and register its expected violation in `formal/counterexamples.tsv`.
3. Register production functions and the fixture in `formal/model-scope.tsv`.
4. Add the model projection to `scripts/gen-tla-state-tests.py`.
5. Add the model to `formal-check`, `formal-dump`, `formal-gen`, and the formal-verification workflow.
6. Generate and commit the state-cover fixture and add its Go test.
7. Add a pinned Go mutant when a small implementation mutation can demonstrate that the new state cover fails for the intended reason.

If a model is intentionally TLC-only, record why no Go binding exists. Avoid implying conformance between the specification and implementation when no executable binding enforces it.
