# Architecture

This document describes the reconciliation architecture used by the Firebolt operator. The operator manages two custom resources with a strict dependency relationship: **FireboltInstance** provisions the metadata infrastructure (PostgreSQL, metadata service, gateway), and **FireboltEngine** deploys stateful compute nodes that require a ready instance. An engine cannot be created or updated without a ready instance in its namespace.

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
│                  │         │ Blocked until     │
│ status:          │         │ instance has:     │
│   metadataEndpoint│        │ - metadataEndpoint│
│                  │         │ - spec.id         │
└──────────────────┘         └──────────────────┘
```

**Rules:**

- Each `FireboltEngine` declares its parent via `spec.instanceRef` (the name of a `FireboltInstance` in the same namespace).
- The engine reconciler resolves the referenced instance on every reconcile. If the instance does not exist, is still provisioning, or lacks a populated `metadataEndpoint` or `spec.id`, reconciliation returns an error and requeues. No engine resources are created with missing metadata configuration. This gate only applies to the **stable**, **stopped**, and **creating** phases (which may build ConfigMaps referencing instance data: `stopped` is included because a missing ConfigMap can be re-materialized in place against the current instance info even at zero replicas). Phases that operate on already-created resources — **switching**, **draining**, **cleaning** — proceed without blocking on instance readiness.
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
└──────┬──────────────────────────────────────────────────────┘
       │
       ▼
┌──────────────────┐
│  getEngineState  │
│  (read layer)    │
│                  │
│  Reads all K8s   │
│  resources for   │
│  this engine.    │
│                  │
│  File:           │
│  engine_state.go │
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│ resolveInstance  │
│ Info (gate)      │
│                  │
│ Reads the        │
│ FireboltInstance │
│ referenced by    │
│ spec.instanceRef │
│                  │
│ Blocks if the    │
│ instance is not  │
│ ready (only in   │
│ stable/creating).│
└────────┬─────────┘
         │
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
│  to the cluster.         │
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

The instance gate runs after the read layer but before the compute layer. It only blocks for phases that may build ConfigMaps containing `metadata_endpoint` and `account_id`: **stable**, **stopped**, and **creating**. `stopped` is included because if a ConfigMap is missing at zero replicas, the reconciler re-materializes it in place using live instance info — the same recovery path as `stable`. Phases that operate on existing resources (**switching**, **draining**, **cleaning**) skip the gate and proceed normally, ensuring that a transient instance issue does not stall an in-flight rollout. When the gate blocks, it sets the `InstanceReady=False` condition on the engine status and requeues. The condition update is part of the single `updateStatus` call — there is no separate status write for conditions.

The compute layer is the core of the operator. It is a pure function with no side effects, making it easy to test exhaustively without a running cluster.

## State machine

The engine lifecycle is a six-phase state machine stored in `.status.phase`. Two of the six (`stable` and `stopped`) are terminal; the others are transition phases. The terminal phase is chosen by `spec.replicas`: non-zero resolves to `stable`, zero resolves to `stopped`. Every transition phase funnels through a single `terminalPhase(spec)` helper so the distinction is made in exactly one place.

```
         spec change during creating:
         abandon gen, bump, recreate
         ┌──────┐
         │      │
         ▼      │
     ┌────────┐ │  pods ready   ┌──────────┐   selector   ┌──────────┐
     │creating├─┘──────────────►│switching ├────updated───►│draining  │
     └────────┘                 └──────────┘               └────┬─────┘
         ▲                        │                             │
         │                        │ (initial deploy,            │ pods drained
         │                        │  no old generation)         │ or drain
         │                        │                             │ check disabled
         │                   ┌────▼────────────────┐       ┌────▼─────┐
         │                   │ stable  /  stopped  │◄──────┤cleaning  │
         │                   └────┬────────────────┘       └──────────┘
         │                        │                             ▲
         │                        │ spec change                 │
         └────────────────────────┘                             │
                                                     old resources deleted
```

Both terminal phases route spec-change detection through the same `computeStable` code path; from the state machine's perspective `stopped` is just `stable` with `spec.replicas == 0` and a different surfaced name. The `Ready` condition, in contrast, distinguishes them: `stable` with ready pods is `Ready=True, Reason=EngineReady`; `stopped` is always `Ready=False, Reason=Stopped` (see [Top-level Ready condition](#top-level-ready-condition) below).

### Phase descriptions

| Phase | What happens | Next phase |
|---|---|---|
| **stable** | Terminal phase when `spec.replicas > 0`. All resources match spec. No work to do. Requeues after 30s for drift detection. On spec change, writes only the status intent (`Phase=creating`, bumped `currentGeneration`) and requeues — no resources are created in this pass. | `creating` (on spec change) |
| **stopped** | Terminal phase when `spec.replicas == 0`. Structurally identical to `stable` — the active generation still exists as a zero-replica StatefulSet + headless Service + ConfigMap — but surfaced as a distinct phase. Spec-change detection and missing-resource re-materialization work identically to `stable`. | `creating` (on spec change) |
| **creating** | New-generation StatefulSet, headless Service, and ConfigMap are ensured. Waits for all pods to become ready. A zero-replica StatefulSet is trivially "ready" (0/0), so scale-to-zero transitions through this phase without blocking. If the spec changes while creating, the in-progress generation is abandoned (its resources are deleted), `currentGeneration` is bumped, and a fresh generation is created on the next reconcile. This avoids patching a live STS whose pods have already read a stale config. | `switching` (all pods ready) |
| **switching** | Updates the cluster Service selector to point to the new generation. | `draining` (if old generation exists), `stable` (initial deploy, replicas > 0), or `stopped` (initial deploy, replicas == 0) |
| **draining** | Waits for old-generation pods to finish serving queries. Skipped entirely when `drainCheckEnabled: false` or `rollout: recreate`. | `cleaning` (drain complete) |
| **cleaning** | Deletes old-generation StatefulSet, headless Service, and ConfigMap. Clears `drainingGeneration`. | `stable` (replicas > 0) or `stopped` (replicas == 0) |

### Key invariant

A spec change during `draining` or `cleaning` does **not** create a new generation. The current transition must complete before a new one begins. This prevents unbounded resource accumulation.

### Top-level Ready condition

`setReadyCondition` derives `status.conditions[type=Ready]` from the post-reconcile phase and pod state. Its precedence is:

1. `InstanceNotReady` — the referenced `FireboltInstance` is not healthy. Wins over everything else because nothing downstream works without it.
2. `Stopped` — `Phase == stopped`. `Ready=False, Reason=Stopped, Message="Engine is stopped (spec.replicas is 0)"`. Explicitly distinguished from `Rolling` so GitOps tooling can tell an intentionally parked engine apart from one mid-transition.
3. `Rolling` — phase is any non-terminal phase (`creating` / `switching` / `draining` / `cleaning`). `Ready=False, Reason=Rolling`.
4. `PodsNotReady` — phase is `stable` but the active-generation pods have not all reported Ready yet. `Ready=False, Reason=PodsNotReady`.
5. `EngineReady` — default. `Ready=True`. The engine is serving traffic on its active generation.

Reason `Stopped` is the only `Ready=False` reason that is not a transient rollout or instance-dependency failure. GitOps tools that key off `Ready=True` should treat a stopped engine as deliberately not-converged-to-serving rather than retrying it indefinitely.

## Generation model

Each spec change (while in `stable` or `stopped`) increments `status.currentGeneration`. Resources for each generation are named with a `-g<N>` suffix:

```
core-engine-g0          # StatefulSet for generation 0
core-engine-g0-hl       # Headless Service for generation 0
core-engine-g0-config   # ConfigMap for generation 0
core-engine-service     # Cluster Service (shared, selector changes)
```

At most two generations exist simultaneously: the active one serving traffic and the new one being created (or the old one being drained/cleaned).

## Drain check

During graceful rollouts, the operator checks whether old-generation pods have finished serving in-flight queries before deleting them. The operator scrapes pod metrics from outside the pod to decide when it is safe to transition `draining` → `cleaning` (and delete the old-generation StatefulSet). Concurrently, the engine process itself handles in-flight queries via `shutdown_wait_unfinished`: on SIGTERM it waits up to `terminationGracePeriodSeconds − 5s` for queries to finish before exiting.

### Signal

Both callers read the same two Prometheus gauges from the engine pod's metrics endpoint on port `9090`:

- `firebolt_running_queries` — queries currently executing.
- `firebolt_suspended_queries` — queries idle-waiting on a client but still holding a session.

A pod is considered drained when `firebolt_running_queries + firebolt_suspended_queries == 0`.

### Operator-side scrape

The operator scrapes `/metrics` via the Kubernetes API server's `Pods/proxy` subresource (not the pod IP directly). Going through the API server means:

- The operator works identically whether it runs in-cluster or out-of-cluster (e.g. `make run` or E2E in-process), without needing to reach pod IPs directly.
- Required RBAC is `pods/proxy: get`. The previous `pods/exec: create` permission is no longer used.
- Transient scrape failures (pod starting, kubelet flaky, metric temporarily missing) are treated as "not drained yet" and the drain loop simply re-polls. They never fail the reconcile.

### Configuration

| Field | Default | Description |
|---|---|---|
| `spec.terminationGracePeriodSeconds` | `60` | Pod grace period. The engine waits up to `grace − 5s` (`shutdown_wait_unfinished`) for in-flight queries after SIGTERM; raise this for workloads with analytical queries that routinely exceed a minute. |
| `spec.drainCheckEnabled` | `true` | Set to `false` to skip the operator-side drain check entirely. The engine's `shutdown_wait_unfinished` still runs on SIGTERM. |
| `spec.drainCheckInterval` | `5s` | How often the operator polls each pod. Only used when drain check is enabled. |
| `spec.rollout` | `graceful` | Set to `recreate` to skip draining and delete old pods immediately. The engine's `shutdown_wait_unfinished` still runs on pod termination regardless of rollout strategy. |

When `drainCheckEnabled: false`, the operator transitions directly from `switching` to `cleaning` without waiting. The engine's `shutdown_wait_unfinished` still gives in-flight queries a chance to finish during Kubernetes termination; `drainCheckEnabled` only controls whether the operator gates the rollout on top of that.

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
| Pod readiness and drain checks | Errors from `checkPodsReady` are propagated rather than defaulting to "not ready". Errors from `checkDrainComplete` (e.g. a transient metrics-scrape failure) are logged and treated as "not drained yet" — drain is already a bounded-retry loop at the caller, so re-polling is cheaper and less noisy than blowing up the whole reconcile on a flaky scrape. |
| JSON marshalling | Config values passed to `json.MarshalIndent` are always well-typed maps. The error path is unreachable and guarded with a panic to catch programming bugs immediately. |
| Terminal errors | Unrecoverable conditions set the instance phase to `Failed` and surface the error, rather than entering an infinite retry loop. |

## Status update strategy

Status updates use `r.Status().Update()` with a single retry on conflict. If a resource version conflict occurs (because a concurrent spec update changed the object), the operator re-fetches the latest object, applies the new status, and retries once. This avoids unnecessary reconcile-loop failures from optimistic concurrency.

## Crash recovery

The operator is crash-safe at every phase boundary. If the process terminates:

- **During stable → creating transition**: the `stable` phase writes only the status intent (`Phase=creating`, bumped `currentGeneration`) in one pass, then requeues. Resources are not created until the status update is persisted. If the operator crashes before the status write, no resources were created and the next reconcile retries from `stable`. If it crashes after, the next reconcile enters `creating` and creates the resources normally.
- **During creating**: the next reconcile sees an existing StatefulSet with not-ready pods and waits. All `ensure` calls are idempotent, so partial resource creation is safe. If the spec changed and the operator crashed after deleting the old generation's resources but before bumping `currentGeneration`, the next reconcile finds no resources for the current generation and recreates them fresh — converging to the correct state.
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

The `FireboltInstanceReconciler` manages the infrastructure that engines depend on: PostgreSQL, the metadata service, and the Envoy gateway proxy. It follows the same level-triggered principles as the engine reconciler.

### Architecture

```
┌──────────────────────────────────────────────────────────┐
│  Reconcile()                                             │
│  Entry point — reads FireboltInstance CR, runs in order  │
│  File: instance_controller.go                            │
└──────┬───────────┬──────────────┬────────────────────────┘
       │           │              │
       ▼           ▼              ▼
┌───────────┐ ┌──────────┐ ┌──────────────┐
│ PostgreSQL│ │ Metadata │ │ Gateway      │
│ (native)  │ │ (native) │ │ (native)     │
│           │ │          │ │              │
│ instance_ │ │ instance_│ │ instance_    │
│ postgres  │ │ metadata │ │ gateway.go   │
│ .go       │ │ .go      │ │              │
└───────────┘ └──────────┘ └──────────────┘
```

### Reconcile steps

Each `Reconcile` call runs through four sequential steps. If any step fails, the reconciler requeues after a short delay and retries from the beginning (earlier steps are idempotent and effectively no-ops when resources already exist).

| Step | Description | Implementation |
|---|---|---|
| 1. Ensure PostgreSQL | Creates Secret (auto-generated credentials), StatefulSet (with volumeClaimTemplate), and headless Service for a `postgres:16-alpine` instance. Skipped when `spec.metadata.postgres` references an external database. | `instance_postgres.go` |
| 2. Ensure metadata service | Creates ConfigMap (XML config), Deployment (with config and credentials volume mounts), and ClusterIP Service for the metadata service. Values are derived from the instance spec (PG connection, image, replicas, resources). The XML config includes `<default_account_id>` set to `spec.id`; Pensieve Dedicated uses this to provision the account on startup. All resources use the `{instance}-metadata` naming convention. | `instance_metadata.go` |
| 3. Check metadata readiness | Waits for the metadata service Deployment to have at least one ready replica before proceeding. | `instance_controller.go` |
| 4. Ensure Gateway | Creates ConfigMap (Envoy YAML config), Deployment (with security context, probes, config volume), ClusterIP Service, and PodDisruptionBudget for the Envoy gateway proxy. Values are derived from the instance spec and namespace. All resources use the `{instance}-gateway` naming convention. | `instance_gateway.go` |

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

The `Failed` phase is terminal and indicates a condition that cannot be resolved by re-reconciliation alone. The operator continues to requeue but will not transition out of `Failed` without manual intervention.

When the metadata service or gateway becomes not-ready, the operator clears the corresponding endpoint from the instance status (`metadataEndpoint` or `gatewayEndpoint`). This ensures that dependent engines observe consistent state and block until the instance is fully operational again.

### Integration with engine reconciler

Each `FireboltEngine` declares its parent instance via `spec.instanceRef`. During reconciliation, the engine controller resolves this reference and reads two fields from the instance's status:

- `metadataEndpoint` — the in-cluster address of the metadata gRPC service
- `spec.id` — the instance identifier, used as the metadata account ID

These are written to the engine ConfigMap. The resolution is only required during the **stable**, **stopped**, and **creating** phases (all of which may build or re-materialize ConfigMaps). Phases that operate on existing resources (**switching**, **draining**, **cleaning**) skip instance resolution entirely, ensuring that a transient instance issue does not stall an in-flight rollout.

When the instance gate blocks, it sets the `InstanceReady=False` condition on the engine's status and requeues after 10 seconds. When the instance is healthy, the condition is updated to `InstanceReady=True`. In both cases the condition update is part of the single `updateStatus` call at the end of the reconcile — the engine controller performs exactly one status write per reconcile loop, never two.

The engine controller watches `FireboltInstance` resources via `Watches()` with a mapper that enqueues all engines referencing the changed instance by name. This means engines react within seconds when their parent instance becomes ready, rather than waiting for error-driven backoff to expire.

The `spec.metadataEndpointOverride` field on the engine overrides the instance-derived endpoint (but not the account ID), supporting cross-cluster scenarios where the engine connects to a metadata service via private link.

### Instance resource ownership

All resources created by the instance reconciler have:

- An `ownerReference` pointing to the `FireboltInstance` CR.
- A `firebolt.io/instance` label for listing/filtering.
- A `firebolt.io/component` label (`postgres`, `metadata`, or `gateway`).
- A finalizer on the CR to ensure cleanup of all labelled resources on deletion.

---

## Gateway query routing

The Envoy gateway proxy acts as the entry point for client queries and is the only entry point on which the operator promises zero downtime across engine lifecycle events. It uses a Lua filter to pick the target engine from the `X-Firebolt-Engine` request header and a dynamic forward proxy (DFP) to resolve the per-engine headless Service at request time.

### Configuration

The gateway ConfigMap (`{instance}-gateway-config`) is a **pure function of the FireboltInstance** — it does not depend on the set of engines. The operator does not regenerate it on engine create/delete/scale/blue-green events, so those events never trigger a gateway rollout. The configuration contains:

- A Lua HTTP filter that validates `X-Firebolt-Engine` as a single RFC 1123 DNS label (lowercase alphanumerics and hyphens, ≤63 chars, no leading or trailing hyphen, no dots) and rewrites `:authority` to `{engine}-service.{namespace}.svc.cluster.local:3473`.
- A dynamic forward proxy cluster with `dns_refresh_rate: 1s` and `host_ttl: 5s`, so newly-ready pods enter the gateway's pool quickly and draining pods fall out without a long stale-entry window.
- A route-level retry policy that retries only transport-level failures (`connect-failure`, `refused-stream`, `reset`) with `num_retries: 2`. 5xx responses are **not** retried, because once an engine has returned a 5xx it may have already executed side effects and a retry could duplicate work. The retries exist purely to hide the brief endpoint-propagation window after a pod becomes not-ready.
- An admin listener on port 9901 used by the gateway container's `preStop` hook (`POST /healthcheck/fail`) to gracefully fail readiness before SIGTERM.

Because the per-engine routing Service is headless (`ClusterIP: None`), the `:authority` hostname resolves directly to the set of ready pod IPs. kube-proxy is not in the data path, so there is no terminating-endpoint race where a SYN would be DNAT'd to a pod whose listener has already closed.

### Traffic path

```
Client (X-Firebolt-Engine: my-engine) → Gateway Service (:80) → Envoy (:8080) → headless {engine}-service (pod IPs) → Engine Pod
```

The gateway Deployment uses a rolling update strategy with `maxSurge: 25%` and `maxUnavailable: 0`, ensuring zero downtime during gateway upgrades.

---

## Rolling update parameters

### Metadata Deployment

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| `maxSurge` | `1` | Allow one extra pod during rollout |
| `maxUnavailable` | `0` | Never reduce available replicas below desired count |
| Replicas | `1` (enforced by webhook) | Multi-replica metadata is not currently supported |

### Gateway Deployment

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| `maxSurge` | `25%` | Standard Kubernetes default for gradual rollout |
| `maxUnavailable` | `0` | Maintain full capacity during image switches |
| PodDisruptionBudget | `maxUnavailable: 1` | Allow voluntary disruptions while maintaining availability |

---

## Crash-point testing

The E2E test suite verifies crash recovery at every phase boundary using a build-tag-based injection mechanism.

### Mechanism

Two files implement the `MaybeCrash` function:

- `crash_points.go` (build tag `!e2e`): production no-op, compiled away by the Go compiler.
- `crash_points_e2e.go` (build tag `e2e`): real implementation that blocks the reconciliation goroutine until a "restart" signal is received.

### Crash points

| Crash Point | Phase | Location |
|-------------|-------|----------|
| `after_engine_configmap_created` | creating | After ConfigMap is written |
| `after_headless_service_created` | creating | After headless Service is written |
| `after_statefulset_created` | creating | After StatefulSet is written |
| `after_cluster_service_ensured` | creating | After cluster Service is ensured |
| `before_creating_to_switching` | creating → switching | Before status transition |
| `after_service_selector_update` | switching | After Service selector is updated |
| `before_switching_status_update` | switching | Before status write |
| `after_statefulset_deleted` | cleaning | After old StatefulSet is deleted |
| `before_cleaning_to_terminal` | cleaning → stable or stopped | Before final status write |

### Test pattern

```go
// Set a crash point before triggering a state change
restartCh := controller.SetCrashPoint(engineName, controller.CrashAfterServiceSelectorUpdate, func() {
    // Called when the crash point is hit
    crashPointHit.Store(true)
})

// Trigger the transition (e.g. scale change)
UpdateEngineReplicas(ctx, engineName, 3)

// Wait for crash point to be hit, then "restart" by closing the channel
Eventually(func() bool { return crashPointHit.Load() }).Should(BeTrue())
close(restartCh)

// Verify the operator recovers and reaches stable state
WaitForEngineStable(ctx, engineName, timeout)
```

The crash-point channel blocks the reconciliation goroutine inside `MaybeCrash`, simulating an operator process crash. Closing the channel resumes reconciliation, verifying that the operator recovers correctly from the interrupted state.
