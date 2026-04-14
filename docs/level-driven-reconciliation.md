# Level-Driven Reconciliation

This document describes the reconciliation architecture used by the Firebolt Engine operator.

## Design principles

The operator uses a **level-triggered** (not edge-triggered) reconciliation model. Each invocation of `Reconcile` reads the full desired state (`.spec`) and the full observed state (cluster resources), computes the delta, and applies it. The reconciler does not depend on knowing _what changed_ — only _what is_.

This means:

- **Idempotent**: calling `Reconcile` twice with the same inputs produces the same result.
- **Crash-safe**: if the operator crashes at any point, the next reconciliation will observe the actual cluster state and resume from the correct phase.
- **No queued operations**: there is no internal queue of "things to do". The status phase and observed resources determine the next action.

## Architecture

The reconciler is split into three layers:

```
┌─────────────────────────────────────────────────────────────┐
│  Reconcile()                                                │
│  Entry point — reads CR, delegates to layers below          │
│  File: engine_controller.go                                 │
└──────────────────────┬──────────────────────────────────────┘
                       │
          ┌────────────┴────────────┐
          ▼                         ▼
┌──────────────────┐    ┌──────────────────────────┐
│  getEngineState  │    │  computeEngineReconcile  │
│  (read layer)    │    │  (pure logic layer)      │
│                  │    │                          │
│  Reads all K8s   │    │  No I/O. Takes spec,     │
│  resources for   │    │  status, and observed     │
│  this engine.    │    │  state. Returns a struct  │
│                  │    │  describing what to       │
│  File:           │    │  create/update/delete.    │
│  engine_state.go │    │                          │
│                  │    │  File:                    │
│                  │    │  engine_reconcile.go      │
└──────────────────┘    └──────────────────────────┘
                                    │
                                    ▼
                        ┌──────────────────────────┐
                        │  applyEngineState        │
                        │  (write layer)           │
                        │                          │
                        │  Takes the reconcile     │
                        │  result and applies it   │
                        │  to the cluster.          │
                        │                          │
                        │  File: engine_apply.go   │
                        └──────────────────────────┘
```

### Layer responsibilities

| Layer | File | I/O | Testability |
|---|---|---|---|
| Read | `engine_state.go` | Yes (K8s API reads) | Requires envtest |
| Compute | `engine_reconcile.go` | None | Pure unit tests |
| Write | `engine_apply.go` | Yes (K8s API writes) | Requires envtest |

The compute layer is the core of the operator. It is a pure function with no side effects, making it easy to test exhaustively without a running cluster.

## State machine

The engine lifecycle is a five-phase state machine stored in `.status.phase`:

```
                    spec change
         ┌──────────────────────────┐
         │                          │
         ▼                          │
     ┌────────┐    pods ready   ┌──────────┐   selector   ┌──────────┐
     │creating├───────────────►│switching ├────updated───►│draining  │
     └────────┘                └──────────┘               └────┬─────┘
         ▲                        │                            │
         │                        │ (initial deploy,           │ pods drained
         │                        │  no old generation)        │ or drain
         │                        │                            │ check disabled
         │                   ┌────▼───┐                   ┌────▼─────┐
         │                   │ stable │◄──────────────────┤cleaning  │
         │                   └────┬───┘                   └──────────┘
         │                        │                            ▲
         │                        │ spec change                │
         └────────────────────────┘                            │
                                                    old resources deleted
```

### Phase descriptions

| Phase | What happens | Next phase |
|---|---|---|
| **stable** | All resources match spec. No work to do. Requeues after 30s for drift detection. | `creating` (on spec change) |
| **creating** | New-generation StatefulSet, headless Service, and ConfigMap are created. Waits for all pods to become ready. Absorbs further spec changes into the current generation (no new generation created). | `switching` (all pods ready) |
| **switching** | Updates the cluster Service selector to point to the new generation. | `draining` (if old generation exists) or `stable` (initial deploy) |
| **draining** | Waits for old-generation pods to finish serving queries. Skipped entirely when `drainCheckEnabled: false` or `rollout: recreate`. | `cleaning` (drain complete) |
| **cleaning** | Deletes old-generation StatefulSet, headless Service, and ConfigMap. Clears `drainingGeneration`. | `stable` |

### Key invariant

A spec change during `draining` or `cleaning` does **not** create a new generation. The current transition must complete before a new one begins. This prevents unbounded resource accumulation.

## Generation model

Each spec change (while in `stable`) increments `status.currentGeneration`. Resources for each generation are named with a `-g<N>` suffix:

```
core-engine-g0          # StatefulSet for generation 0
core-engine-g0-hl       # Headless Service for generation 0
core-engine-g0-config   # ConfigMap for generation 0
core-engine-service     # Cluster Service (shared, selector changes)
```

At most two generations exist simultaneously: the active one serving traffic and the new one being created (or the old one being drained/cleaned).

## Drain check

During graceful rollouts, the operator checks whether old-generation pods have finished serving in-flight queries before deleting them.

### Mechanism

The operator runs a SQL query via `kubectl exec` inside each draining pod to count running queries. When the count reaches zero, the pod is considered drained.

### Configuration

| Field | Default | Description |
|---|---|---|
| `spec.drainCheckEnabled` | `true` | Set to `false` to skip the SQL drain check entirely. Required when no metadata endpoint (Pensieve) is available. |
| `spec.drainCheckInterval` | `5s` | How often to poll each pod. Only used when drain check is enabled. |
| `spec.rollout` | `graceful` | Set to `recreate` to skip draining and delete old pods immediately. |

When `drainCheckEnabled: false`, the operator transitions directly from `switching` to `cleaning` without waiting, which is safe when there is no query routing layer that could send traffic to old pods.

## Status update strategy

Status updates use `r.Status().Update()` with a single retry on conflict. If a resource version conflict occurs (because a concurrent spec update changed the object), the operator re-fetches the latest object, applies the new status, and retries once. This avoids unnecessary reconcile-loop failures from optimistic concurrency.

## Crash recovery

The operator is crash-safe at every phase boundary. If the process terminates:

- **During creating**: the next reconcile sees an existing StatefulSet with not-ready pods and waits.
- **During switching**: the next reconcile checks the service selector and either updates it or proceeds.
- **During draining**: the next reconcile re-runs the drain check.
- **During cleaning**: the next reconcile re-deletes any remaining old resources (delete is idempotent).
- **During stable**: no work needed.

No persistent state outside of the Kubernetes API server is required.

## Orphan detection

On every reconcile, the operator scans for StatefulSets, Services, and ConfigMaps labeled with `firebolt.io/engine=<name>` that belong to generations other than the current, active, or draining generation. These orphans (from past crashes or bugs) are deleted immediately.

## Resource ownership

All per-engine resources have:

- An `ownerReference` pointing to the `FireboltEngine` CR (for garbage collection on CR deletion).
- A `firebolt.io/engine` label (for listing/filtering).
- A `firebolt.io/generation` label (for generation-based selection).
- A finalizer on the CR itself to ensure cleanup runs before the CR is removed.
