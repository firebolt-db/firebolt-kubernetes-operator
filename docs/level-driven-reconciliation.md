# Level-Driven Reconciliation

This document describes the reconciliation architecture used by the Firebolt operator. The operator manages two custom resources with a strict dependency relationship: **FireboltInstance** provisions the metadata infrastructure (PostgreSQL, metadata service, gateway, account initialization), and **FireboltEngine** deploys stateful compute nodes that require a ready instance. An engine cannot be created or updated without a ready instance in its namespace.

## Resource dependency model

The operator enforces a hierarchical dependency between instances and engines:

```
┌──────────────────┐         ┌──────────────────┐
│ FireboltInstance  │◄────────│  FireboltEngine   │
│                  │  reads  │                  │
│ Provisions:      │ status  │ spec.instanceRef  │
│ - PostgreSQL     │         │ points to the     │
│ - Metadata svc   │         │ instance by name  │
│ - Gateway        │         │                  │
│ - Account init   │         │ Blocked until     │
│                  │         │ instance has:     │
│ status:          │         │ - metadataEndpoint│
│   metadataEndpoint│        │ - accountId       │
│   accountId      │         │                  │
└──────────────────┘         └──────────────────┘
```

**Rules:**

- Each `FireboltEngine` declares its parent via `spec.instanceRef` (the name of a `FireboltInstance` in the same namespace).
- The engine reconciler resolves the referenced instance on every reconcile. If the instance does not exist, is still provisioning, or lacks a populated `metadataEndpoint` or `accountId`, reconciliation returns an error and requeues. No engine resources are created with missing metadata configuration. This gate only applies to the **stable** and **creating** phases (which build ConfigMaps referencing instance data). Phases that operate on already-created resources — **switching**, **draining**, **cleaning** — proceed without blocking on instance readiness.
- The engine controller watches `FireboltInstance` resources and re-reconciles all referencing engines when an instance's status changes. This eliminates backoff delay when an instance transitions to ready.
- The engine reports its dependency status via a `status.conditions[]` entry of type `InstanceReady`. This condition is written as part of the single `updateStatus` call at the end of each reconcile, avoiding double status writes. Users can inspect this condition to understand why an engine is not progressing.
- The instance reconciler is independent and has no dependency on engines.

## Design principles

The operator uses a **level-triggered** (not edge-triggered) reconciliation model. Each invocation of `Reconcile` reads the full desired state (`.spec`) and the full observed state (cluster resources), computes the delta, and applies it. The reconciler does not depend on knowing _what changed_ — only _what is_.

This means:

- **Idempotent**: calling `Reconcile` twice with the same inputs produces the same result.
- **Crash-safe**: if the operator crashes at any point, the next reconciliation will observe the actual cluster state and resume from the correct phase.
- **No queued operations**: there is no internal queue of "things to do". The status phase and observed resources determine the next action.

## Engine reconciler architecture

The engine reconciler is split into three layers, with instance resolution as a hard prerequisite:

```
┌─────────────────────────────────────────────────────────────┐
│  Reconcile()                                                │
│  Entry point — reads CR, delegates to layers below          │
│  File: engine_controller.go                                 │
└──────┬──────────────────┬───────────────────────────────────┘
       │                  │
       ▼                  ▼
┌──────────────────┐ ┌────────────────────┐
│ resolveInstance  │ │  getEngineState    │
│ Info (gate)      │ │  (read layer)      │
│                  │ │                    │
│ Reads the        │ │  Reads all K8s     │
│ FireboltInstance │ │  resources for     │
│ referenced by    │ │  this engine.      │
│ spec.instanceRef │ │                    │
│                  │ │  File:             │
│ Blocks if the    │ │  engine_state.go   │
│ instance is not  │ │                    │
│ ready.           │ │                    │
└────────┬─────────┘ └─────────┬──────────┘
         │                     │
         └──────────┬──────────┘
                    ▼
         ┌──────────────────────────┐
         │  computeEngineReconcile  │
         │  (pure logic layer)      │
         │                          │
         │  No I/O. Takes spec,     │
         │  status, observed state, │
         │  and InstanceInfo.       │
         │  Returns a struct        │
         │  describing what to      │
         │  create/update/delete.   │
         │                          │
         │  File:                   │
         │  engine_reconcile.go     │
         └────────────┬─────────────┘
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
| Instance gate | `engine_controller.go` | Yes (reads `FireboltInstance`) | Requires envtest |
| Read | `engine_state.go` | Yes (K8s API reads) | Requires envtest |
| Compute | `engine_reconcile.go` | None | Pure unit tests |
| Write | `engine_apply.go` | Yes (K8s API writes) | Requires envtest |

The instance gate runs after the read layer but before the compute layer. It only blocks for phases that need instance data (**stable** and **creating**, which build ConfigMaps containing `metadata_endpoint` and `account_id`). Phases that operate on existing resources (**switching**, **draining**, **cleaning**) skip the gate and proceed normally, ensuring that a transient instance issue does not stall an in-flight rollout. When the gate blocks, it sets the `InstanceReady=False` condition on the engine status and requeues. The condition update is part of the single `updateStatus` call — there is no separate status write for conditions.

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
| **stable** | All resources match spec. No work to do. Requeues after 30s for drift detection. On spec change, writes only the status intent (`Phase=creating`, bumped `currentGeneration`) and requeues — no resources are created in this pass. | `creating` (on spec change) |
| **creating** | New-generation StatefulSet, headless Service, and ConfigMap are ensured. Waits for all pods to become ready. Absorbs further spec changes into the current generation (no new generation created). | `switching` (all pods ready) |
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
| `spec.drainCheckEnabled` | `true` | Set to `false` to skip the SQL drain check entirely. Requires a running node that can execute the drain-check query when enabled. |
| `spec.drainCheckInterval` | `5s` | How often to poll each pod. Only used when drain check is enabled. |
| `spec.rollout` | `graceful` | Set to `recreate` to skip draining and delete old pods immediately. |

When `drainCheckEnabled: false`, the operator transitions directly from `switching` to `cleaning` without waiting, which is safe when there is no query routing layer that could send traffic to old pods.

## Error handling

The operator follows strict error propagation rules to ensure failures are always visible.

**No swallowed errors.** Every error from an I/O operation is either:
1. Returned to the caller (causing a retry via requeue), or
2. Logged and aggregated when multiple independent cleanup operations must all be attempted (e.g. `reconcileDelete`).

Specific policies:

| Category | Policy |
|---|---|
| Status update failures | Always propagated — a failed status write returns an error so the next reconcile retries with fresh state. |
| Resource list/delete during cleanup | Errors are logged, collected, and aggregated. The finalizer is only removed when all cleanup operations succeed. This prevents premature garbage collection when the API server is unhealthy. |
| Pod readiness and drain checks | Errors are propagated from `checkPodsReady` and `checkDrainComplete` rather than defaulting to "not ready". This surfaces API failures rather than masking them as slow rollouts. |
| JSON marshalling | Config values passed to `json.MarshalIndent` are always well-typed maps. The error path is unreachable and guarded with a panic to catch programming bugs immediately. |
| Terminal errors | Unrecoverable conditions (e.g. multiple accounts in metadata service) set the instance phase to `Failed` and surface the error, rather than entering an infinite retry loop. |

## Status update strategy

Status updates use `r.Status().Update()` with a single retry on conflict. If a resource version conflict occurs (because a concurrent spec update changed the object), the operator re-fetches the latest object, applies the new status, and retries once. This avoids unnecessary reconcile-loop failures from optimistic concurrency.

## Crash recovery

The operator is crash-safe at every phase boundary. If the process terminates:

- **During stable → creating transition**: the `stable` phase writes only the status intent (`Phase=creating`, bumped `currentGeneration`) in one pass, then requeues. Resources are not created until the status update is persisted. If the operator crashes before the status write, no resources were created and the next reconcile retries from `stable`. If it crashes after, the next reconcile enters `creating` and creates the resources normally.
- **During creating**: the next reconcile sees an existing StatefulSet with not-ready pods and waits. All `ensure` calls are idempotent, so partial resource creation is safe.
- **During switching**: the next reconcile checks the service selector and either updates it or proceeds.
- **During draining**: the next reconcile re-runs the drain check.
- **During cleaning**: the next reconcile re-deletes any remaining old resources (delete is idempotent).
- **During stable**: no work needed.

No persistent state outside of the Kubernetes API server is required.

## Resource ownership

All per-engine resources have:

- An `ownerReference` pointing to the `FireboltEngine` CR (for garbage collection on CR deletion).
- A `firebolt.io/engine` label (for listing/filtering).
- A `firebolt.io/generation` label (for generation-based selection).
- A finalizer on the CR itself to ensure cleanup runs before the CR is removed.

---

## FireboltInstance reconciler

The `FireboltInstanceReconciler` manages the infrastructure that engines depend on: PostgreSQL, the metadata service, the core-gateway, and account initialization. It follows the same level-triggered principles as the engine reconciler.

### Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Reconcile()                                                     │
│  Entry point — reads FireboltInstance CR, runs steps in order    │
│  File: instance_controller.go                                    │
└──────┬───────────┬──────────────┬──────────────┬─────────────────┘
       │           │              │              │
       ▼           ▼              ▼              ▼
┌───────────┐ ┌──────────┐ ┌───────────┐ ┌──────────────┐
│ PostgreSQL│ │ Metadata │ │ Account   │ │ Gateway      │
│ (native)  │ │ (native) │ │ Init      │ │ (native)     │
│           │ │          │ │ (gRPC)    │ │              │
│ instance_ │ │ instance_│ │ instance_ │ │ instance_    │
│ postgres  │ │ metadata │ │ account_  │ │ gateway.go   │
│ .go       │ │ .go      │ │ init.go   │ │              │
└───────────┘ └──────────┘ └───────────┘ └──────────────┘
```

### Reconcile steps

Each `Reconcile` call runs through five sequential steps. If any step fails, the reconciler requeues after a short delay and retries from the beginning (earlier steps are idempotent and effectively no-ops when resources already exist).

| Step | Description | Implementation |
|---|---|---|
| 1. Ensure PostgreSQL | Creates Secret (auto-generated credentials), StatefulSet (with volumeClaimTemplate), and headless Service for a `postgres:16-alpine` instance. Skipped when `spec.metadata.postgres` references an external database. | `instance_postgres.go` |
| 2. Ensure metadata service | Creates ConfigMap (XML config), Deployment (with config and credentials volume mounts), and ClusterIP Service for the metadata service. Values are derived from the instance spec (PG connection, image, replicas, resources). All resources use the `{instance}-metadata` naming convention. | `instance_metadata.go` |
| 3. Check metadata readiness | Waits for the metadata service Deployment to have at least one ready replica before proceeding. | `instance_controller.go` |
| 4. Account initialization | Connects to the metadata gRPC API via in-cluster DNS and ensures exactly one active account exists. If the account exists but is not active (e.g. a previous activation was interrupted), the operator retries activation. Multiple accounts trigger a terminal `Failed` phase. Persists the `accountId` in instance status. | `instance_account_init.go` |
| 5. Ensure Gateway | Creates ConfigMap (YAML config), ServiceAccount, Role, RoleBinding, Deployment (with security context, probes, config volume), ClusterIP Service, and PodDisruptionBudget for the gateway. Values are derived from the instance spec and account ID. All resources use the `{instance}-gateway` naming convention. | `instance_gateway.go` |

### Instance lifecycle phases

```
  ┌──────────────┐     all components ready     ┌────────┐
  │ Provisioning ├─────────────────────────────►│ Ready  │
  └──────────────┘                               └───┬────┘
                                                     │
  ┌──────────┐         all components recover        │ component
  │ Degraded │◄─────────────────────────────────────-┘ becomes
  │          ├──────────────────────────────────►Ready  unready
  └──────────┘

  ┌──────────┐
  │ Failed   │  terminal — requires manual intervention
  └──────────┘
```

The instance starts in `Provisioning` and transitions to `Ready` once both the metadata service and gateway have at least one ready replica. If a previously-ready component becomes unhealthy, the phase transitions to `Degraded`. It returns to `Ready` once all components recover.

The `Failed` phase is terminal and indicates a condition that cannot be resolved by re-reconciliation alone (e.g. multiple accounts found in the metadata service). The operator continues to requeue but will not transition out of `Failed` without manual intervention.

When the metadata service or gateway becomes not-ready, the operator clears the corresponding endpoint from the instance status (`metadataEndpoint` or `gatewayEndpoint`). This ensures that dependent engines observe consistent state and block until the instance is fully operational again.

### Integration with engine reconciler

Each `FireboltEngine` declares its parent instance via `spec.instanceRef`. During reconciliation, the engine controller resolves this reference and reads two fields from the instance's status:

- `metadata_endpoint` — the in-cluster address of the metadata gRPC service
- `account_id` — the metadata account identifier

These are written to the engine ConfigMap. The resolution is only required during the **stable** and **creating** phases (which build ConfigMaps). Phases that operate on existing resources (**switching**, **draining**, **cleaning**) skip instance resolution entirely, ensuring that a transient instance issue does not stall an in-flight rollout.

When the instance gate blocks, it sets the `InstanceReady=False` condition on the engine's status and requeues after 10 seconds. When the instance is healthy, the condition is updated to `InstanceReady=True`. In both cases the condition update is part of the single `updateStatus` call at the end of the reconcile — the engine controller performs exactly one status write per reconcile loop, never two.

The engine controller watches `FireboltInstance` resources via `Watches()` with a mapper that enqueues all engines referencing the changed instance by name. This means engines react within seconds when their parent instance becomes ready, rather than waiting for error-driven backoff to expire.

The `spec.metadataEndpointOverride` field on the engine overrides the instance-derived endpoint (but not the account ID), supporting cross-cluster scenarios where the engine connects to a metadata service via private link.

### Instance resource ownership

All resources created by the instance reconciler have:

- An `ownerReference` pointing to the `FireboltInstance` CR.
- A `firebolt.io/instance` label for listing/filtering.
- A `firebolt.io/component` label (`postgres`, `metadata`, or `gateway`).
- A finalizer on the CR to ensure cleanup of all labelled resources on deletion.
