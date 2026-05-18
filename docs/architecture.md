# Architecture

This document describes the reconciliation architecture used by the Firebolt operator. The operator manages three custom resources: **FireboltInstance** provisions the metadata infrastructure (PostgreSQL, metadata service, gateway); **FireboltEngine** deploys stateful compute nodes that require a ready instance; **EngineClass** is an optional cluster-scoped pod-template fragment that one or more engines can reference to inherit shared pod-level settings (service account, scheduling, pod annotations, sidecars, engine container image). An engine cannot be created or updated without a ready instance in its namespace; an engine may optionally reference an `EngineClass`.

## Resource dependency model

The operator enforces a hierarchical dependency from engines to their instance, plus an optional shared dependency on a cluster-scoped EngineClass:

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
└──────────────────┘         └─────────┬────────┘
                                       │ reads spec.template
                                       │ (when spec.engineClassRef set)
                                       ▼
                             ┌─────────────────────┐
                             │     EngineClass     │
                             │  (cluster-scoped,   │
                             │   optional)         │
                             │                     │
                             │  spec.template:     │
                             │    PodTemplateSpec  │
                             │    merged into the  │
                             │    engine pod spec  │
                             │                     │
                             │  status.boundEngines│
                             │    drives deletion- │
                             │    blocking webhook │
                             └─────────────────────┘
```

**Rules:**

- Each `FireboltEngine` declares its parent via `spec.instanceRef` (the name of a `FireboltInstance` in the same namespace).
- The engine reconciler resolves the referenced instance on every reconcile. If the instance does not exist, is still provisioning, or lacks a populated `metadataEndpoint` or `spec.id`, reconciliation returns an error and requeues. No engine resources are created with missing metadata configuration. This gate only applies to the **stable**, **stopped**, and **creating** phases (which may build ConfigMaps referencing instance data: `stopped` is included because a missing ConfigMap can be re-materialized in place against the current instance info even at zero replicas). Phases that operate on already-created resources — **switching**, **draining**, **cleaning** — proceed without blocking on instance readiness.
- The engine controller watches `FireboltInstance` resources and re-reconciles all referencing engines when an instance's status changes. This eliminates backoff delay when an instance transitions to ready.
- The engine reports its dependency status via a `status.conditions[]` entry of type `InstanceReady`. This condition is written as part of the single `updateStatus` call at the end of each reconcile, avoiding double status writes. Users can inspect this condition to understand why an engine is not progressing.
- The instance reconciler is independent and has no dependency on engines.
- The optional `spec.engineClassRef` references a cluster-scoped `EngineClass`. The reference is checked at admission time by the FireboltEngine validating webhook (hard-reject if the named class does not exist), so a runtime "class missing" state is not part of the steady-state status surface. The engine controller watches `EngineClass` and re-reconciles every referencing engine when a class is created, edited, or deleted — a class spec edit therefore rolls a fresh blue-green generation on every consumer engine immediately, rather than waiting for the 30s drift requeue.
- `EngineClass` has its own status reconciler that maintains `status.boundEngines` (the count of FireboltEngines referencing the class) for user-facing visibility. The EngineClass validating webhook refuses deletion by listing referencing engines live from the API server at admission time rather than trusting the cached count — `status.boundEngines` starts at zero on a freshly admitted class, so a status-based gate would race the reconciler. `failurePolicy: Fail` on the webhook configuration ensures a webhook outage cannot open a deletion window.

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

The instance gate runs after the read layer but before the compute layer. It only blocks for phases that may build ConfigMaps containing `instance.multi_engine.metadata_endpoint` and `instance.id`: **stable**, **stopped**, and **creating**. `stopped` is included because if a ConfigMap is missing at zero replicas, the reconciler re-materializes it in place using live instance info — the same recovery path as `stable`. Phases that operate on existing resources (**switching**, **draining**, **cleaning**) skip the gate and proceed normally, ensuring that a transient instance issue does not stall an in-flight rollout. When the gate blocks, it sets the `InstanceReady=False` condition on the engine status and requeues. The condition update is part of the single `updateStatus` call — there is no separate status write for conditions.

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

### StatefulSet event propagation

A FireboltEngine can get stuck in `creating` (or in `stable` with `PodsNotReady`) when its generation StatefulSet exists but the StatefulSet controller cannot create the desired pods — missing ServiceAccount, ResourceQuota exceeded, PodSecurity / admission rejection, RBAC denial, PVC unbindable, and similar. The operator owns the StatefulSet but the actionable error is recorded by Kubernetes as a Warning event (typically `FailedCreate`) on the StatefulSet object, so without help users would have to run `kubectl describe sts <name>` to triage.

To surface this on the FireboltEngine itself, after computing the Ready condition the reconciler queries the apiserver for Warning events on the current-generation StatefulSet whenever:

- `CurrentSTS != nil` — there is an STS to look up events for, and
- `CurrentPodTotal < spec.replicas` — pods are missing (rather than just unready), and
- `Ready.Reason ∈ {Rolling, PodsNotReady}` — the existing reason is a generic "stuck" reason that we are allowed to refine. `InstanceNotReady`, `DrainCheckFailing`, `Stopped`, and `EngineReady` are higher-precedence diagnostics or healthy states and are not overridden.

When a Warning event matches, the Ready condition is rewritten with that event's `Reason` (e.g. `FailedCreate`) and a message of the form `StatefulSet <name>: <event message> (x<count>)`. The lookup uses the Clientset (not the controller-runtime cache) with field selector `involvedObject.uid=<UID>,type=Warning`: events are high-volume cluster-wide, and a watch would inflate the controller's cache for a signal we consult only on already-stuck engines. Fetch failures are logged and swallowed — the diagnostic is best-effort and must never poison the main reconcile path. Once pods come up the trigger gate stops firing and the next reconcile restores `EngineReady`.

## Generation model

Each spec change (while in `stable` or `stopped`) increments `status.currentGeneration`. Resources for each generation are named with a `-g<N>` suffix:

```
engine-g0          # StatefulSet for generation 0
engine-g0-hl       # Headless Service for generation 0
engine-g0-config   # ConfigMap for generation 0
engine-service     # Cluster Service (shared, selector changes)
```

At most two generations exist simultaneously: the active one serving traffic and the new one being created (or the old one being drained/cleaned).

`stsMatchesSpec` is the central drift detector. It compares the live StatefulSet against the resolved engine spec field-by-field; any mismatch returns false and the reconciler bumps `currentGeneration`. Two annotations on the StatefulSet act as content hashes for inputs that don't have a clean direct comparison:

| Annotation | Source | What a change means |
|---|---|---|
| `firebolt.io/custom-engine-config-hash` | `spec.customEngineConfig` after the protected-paths strip | The engine ConfigMap content changed; roll a new generation. |
| `firebolt.io/engine-class-hash` | Resolved `EngineClass.spec.template` (or absent when `spec.engineClassRef` is nil) | Either the referenced class was edited in place, the engine flipped to a different class, or `engineClassRef` was cleared. Any of those rolls a new generation. |

## EngineClass merge layer

When an engine sets `spec.engineClassRef`, the reconciler resolves the referenced `EngineClass` and merges its `spec.template` underneath the operator-built pod template before stamping it onto the StatefulSet. Precedence on every field:

1. **Operator defaults** (image fallback, hardcoded ports / probes / command / reserved env, headless-DNS contract fields) — always win.
2. **EngineClass template** — fills in user-owned fields the engine spec doesn't set.
3. **FireboltEngine spec** — wins over the class on conflict.

Merge rules (centralised in the `effective*` helpers in `engine_reconcile.go` so `buildStatefulSet` and `stsMatchesSpec` agree on the resolved value):

| Field | Rule |
|---|---|
| `serviceAccountName` | engine spec > class > `""` |
| `nodeSelector` | map-merge, engine keys win |
| `tolerations` | class slice + engine slice, concatenated |
| `affinity` | engine wins if non-nil, else class (no field-merge) |
| pod-template labels | reserved keys (`firebolt.io/engine`, `firebolt.io/generation`) non-overridable; engine `spec.podLabels` > class labels |
| pod-template annotations | engine `spec.podAnnotations` > class annotations |
| pod-level `securityContext` | engine spec > class > operator default (FSGroup, FSGroupChangePolicy always stamped) |
| pod-level `volumes` | operator-owned volumes (`nodes-config`, data) come first; class volumes appended; class names colliding with operator-owned volumes are dropped |
| pod-level `imagePullSecrets` | passed through from class |
| init containers | class slice + engine slice, concatenated (mirrors `tolerations`) |
| sidecar containers (anything not named `engine`) | passed through from class, appended after the operator-built engine container |
| engine container `image` / `imagePullPolicy` | class > operator default |
| engine container `resources` | engine spec wins wholesale if it carries any requests/limits/claims, else class fills in (no field-level merge) |
| engine container `securityContext` | engine spec wins if non-nil, else class |
| engine container `env` | operator-injected vars (`POD_INDEX`, `FIREBOLT_ALLOW_AWS_IRSA`, `FIREBOLT_CORE_MODE`) first; class env appended (reserved keys rejected at admission) |
| engine container `envFrom` | passed through from class |
| engine container `volumeMounts` | operator-owned mounts first; class mounts appended; mounts colliding with operator volume names are dropped |
| engine container `lifecycle` | passed through from class |

The engine controller watches `EngineClass` via `EnqueueRequestsFromMapFunc` so a class edit immediately enqueues every consumer engine. The validating webhook on `EngineClass` rejects user input on paths the operator owns end-to-end (see [docs/engineclass-crd-reference.md](engineclass-crd-reference.md)) — the merge layer therefore assumes the resolved template only carries fields it knows how to handle.

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

## Autoscaler

Autoscaling is opt-in via `spec.autoscaling.enabled=true`. When enabled, the operator owns `spec.replicas` (HPA-style) and toggles it between `minReplicas` (default 0, enabling scale-to-zero) and `maxReplicas` based on observed query activity and an optional UTC `schedule`.

The autoscaler reuses the **same Prometheus signal** the drain check consumes — `firebolt_running_queries + firebolt_suspended_queries` — summed across all running pods of the active generation. Sharing the signal keeps "the engine is busy" in exactly one place: a pod that the drain check would refuse to evict is the same pod the autoscaler counts as activity.

### Decision precedence

`computeAutoscalerDecision` is a pure function over `(spec, status, observation, now)`. Precedence, top-down:

1. **Disabled** — if `spec.autoscaling` is unset or `enabled=false`, the autoscaler emits no decision and `spec.replicas` is fully user-owned.
2. **Wake requested** — `metadata.annotations["firebolt.io/wake-requested"]` carries an RFC 3339 timestamp younger than `DefaultAutoscalerWakeTTL` (5 minutes). Replicas are scaled to `maxReplicas`. Reason `WakeRequested`. See [Gateway wake-up protocol](#gateway-wake-up-protocol).
3. **Schedule active** — `now` falls inside any window in `spec.autoscaling.schedule`. Replicas are pinned at `maxReplicas`. Schedule wins over both idle and stopped paths so an "always-on during business hours" policy can wake a parked engine.
4. **Stopped** — `spec.replicas == 0`, no fresh wake annotation, no schedule window active. No-op.
5. **Scrape failed or activity observed** — refresh `status.lastActivityTime`, do not scale. Scrape failures are grouped with activity intentionally: a broken probe must never look quiet enough to scale down.
6. **Quiet ≥ idleTimeout, replicas > minReplicas** — patch `spec.replicas = minReplicas` and stamp `status.autoscaledAt`.
7. **First quiet observation** — `status.lastActivityTime` is anchored to `now` so a fresh engine gets one full `idleTimeout` of grace.

### Level-driven encoding

Scale events are encoded by patching `spec.replicas` via the standard `r.Update`. The `FireboltEngine` watch fires; the next reconcile takes the normal blue-green path through `creating → switching → draining → cleaning`. The autoscaler runs only in **terminal phases** (`stable`/`stopped`) so it cannot fight a rollout in progress.

Because scale-down only fires when `firebolt_running_queries + firebolt_suspended_queries == 0` was just observed, the subsequent drain check on the old generation completes immediately — no wasted grace period. A separate "skip drain because the autoscaler vouched for it" path is unnecessary.

### Configuration

| Field | Default | Description |
|---|---|---|
| `spec.autoscaling.enabled` | `false` | Master toggle. |
| `spec.autoscaling.maxReplicas` | (required) | Replica count when active. |
| `spec.autoscaling.minReplicas` | `0` | Floor; `0` enables scale-to-zero. |
| `spec.autoscaling.idleTimeout` | `30m` | Quiet window before scaling to `minReplicas`. |
| `spec.autoscaling.pollInterval` | `1m` | Scrape cadence. |
| `spec.autoscaling.schedule[]` | `[]` | UTC `HH:MM`-`HH:MM` always-on windows; optional `days` filter (`Mon`..`Sun`). End < start crosses midnight. |

### Status fields

| Field | Meaning |
|---|---|
| `status.lastActivityTime` | Most recent observation that recorded activity (or, for a fresh engine, the first quiet observation). Drives the idle clock. |
| `status.autoscaledAt` | Timestamp of the most recent autoscaler-driven `spec.replicas` mutation. Distinguishes autoscaler scale events from user edits. |
| `status.autoscalerReason` | Token: `Disabled` / `WakeRequested` / `ScheduleActive` / `Stopped` / `ActivityObserved` / `ScrapeFailed` / `Idle`. |

## Gateway wake-up protocol

A FireboltEngine that has been autoscaled to zero replicas needs a way to come back to life when a query arrives. The operator and the Envoy-based gateway exchange a single, level-driven signal for this:

```
                       patch annotation
                ┌─────────────────────────────┐
   ┌──────────┐ │   metadata.annotations      │  ┌────────────┐
   │ Gateway  ├─►   firebolt.io/wake-requested├─►│ Engine CR  │
   │ (Envoy)  │ │   = "<RFC 3339 timestamp>"  │  └─────┬──────┘
   └──────────┘ └─────────────────────────────┘        │ Watch fires
                                                       ▼
                                               ┌────────────┐
                                               │ Autoscaler │
                                               │ runs:      │
                                               │ wake fresh │
                                               │ → MaxRepl. │
                                               └─────┬──────┘
                                                     │ patch spec.replicas
                                                     ▼
                                               ┌────────────┐
                                               │ Blue-green │
                                               │ creating   │
                                               └────────────┘
```

### Why an annotation

- **Level-driven**: the annotation timestamp is part of the resource state. Any reconcile (operator restart, periodic poll, watch event) reads the same value and converges identically.
- **Fire-and-forget**: a wake-up does not require a synchronous response. The gateway buffers the triggering query locally and retries against engine DNS once Envoy's active health checks observe a ready upstream.
- **Coalescing**: 1000 simultaneous queries for the same stopped engine produce a handful of identical patches via K8s optimistic concurrency, not a thundering-herd RPC.
- **Operator-down tolerant**: K8s API still accepts the patch when the operator manager is restarting; the next reconciler instance picks it up.

### Wake annotation TTL

`DefaultAutoscalerWakeTTL` (5 minutes) bounds how long an unrefreshed annotation continues to trigger scale-up. Long enough to cover engine cold-start (image pull, blue-green creating phase) and short enough that an abandoned wake does not pin an engine after the gateway has given up. The gateway is expected to keep stamping the annotation while it has buffered queries waiting.

The annotation is honored only when `spec.autoscaling.enabled=true`. Without an autoscaling policy the operator has no `MaxReplicas` to scale to, and respecting a wake from a non-policy actor would silently override the user's `spec.replicas==0` intent.

### Gateway RBAC

Each FireboltInstance now provisions per-gateway RBAC alongside the gateway Deployment:

| Resource | Name | Purpose |
|---|---|---|
| `ServiceAccount` | `<instance>-gateway` | Identity attached to gateway pods. |
| `Role` | `<instance>-gateway-wake` | Grants `get`, `list`, `patch` on `fireboltengines` in the same namespace. |
| `RoleBinding` | `<instance>-gateway-wake` | Binds the SA to the Role. |

RBAC cannot restrict patch to a specific subresource or field, so the gateway holds patch on the whole CR. The wake protocol constrains the gateway to a strategic-merge patch that only touches `metadata.annotations[firebolt.io/wake-requested]`; misuse beyond that is reviewed via Kubernetes audit logs, not enforced by RBAC.

> **Note:** the gateway-side implementation (intercept routing for stopped engines, buffer the request, patch the annotation, retry against engine DNS) is not yet wired in. It would extend the Envoy Lua filter rendered by the operator in `internal/controller/instance_gateway.go`. This document defines the contract and the operator-side enforcement (RBAC, fresh-annotation handling); the Envoy-side hook is tracked as a follow-up.


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
| 1. Ensure PostgreSQL | Creates Secret (auto-generated credentials), StatefulSet (with volumeClaimTemplate), and headless Service for a `postgres:16-alpine` instance. The pod runs as the image's built-in non-root postgres user (UID 70) with read-only root filesystem, all Linux capabilities dropped, `RuntimeDefault` seccomp, and emptyDir volumes for `/var/run/postgresql` and `/tmp` (the only paths the postgres entrypoint needs to write outside its data PVC). Skipped when `spec.metadata.postgres` references an external database. | `instance_postgres.go` |
| 2. Ensure metadata service | Creates ConfigMap (XML config), Deployment (with config and credentials volume mounts), and ClusterIP Service for the metadata service. Values are derived from the instance spec (PG connection, image, replicas, resources). The XML config includes `<default_account_id>` set to `spec.id`; the metadata service uses this to provision the account on startup. The pod runs as the metadata image's built-in non-root `dedicated-pensieve` user (UID 1111) with read-only root filesystem, all Linux capabilities dropped, `RuntimeDefault` seccomp, an emptyDir backing `/tmp`, and `automountServiceAccountToken: false` (pensieve does not call the Kubernetes API). All resources use the `{instance}-metadata` naming convention. | `instance_metadata.go` |
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

The `spec.metadataEndpointOverride` field on the engine overrides the instance-derived endpoint (but not the instance ID), supporting cross-cluster scenarios where the engine connects to a metadata service via private link.

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
- A dynamic forward proxy in **sub-cluster mode** (`sub_clusters_config`, *not* `dns_cache_config`). Each authority synthesises a full STRICT_DNS sub-cluster on first use, so all A-records of the headless engine Service become individual upstream hosts with normal load-balancing, outlier-detection and `previous_hosts` retry semantics. DNS-cache mode would have collapsed the headless Service back into a single sticky IP per cluster and made `previous_hosts` a no-op.
- `max_requests_per_connection: 1` on the DFP cluster: every query gets a fresh TCP connect and therefore a fresh DNS lookup. This collapses the stale-IP window after a selector flip to a single TCP handshake instead of the STRICT_DNS refresh interval.
- Active HTTP health checks on `/health/ready` every `1s` with `healthy_threshold: 1` / `unhealthy_threshold: 1`. The engine flips this endpoint to 503 immediately on SIGTERM, so Envoy ejects a draining pod from the load-balanced set within one probe interval — independently of DNS.
- A route-level retry policy that retries on transport-level failures (`connect-failure`, `refused-stream`, `reset`) **and** on responses carrying the `X-Firebolt-Drained` header (`retriable-headers`). The header is set only by the engine's pre-work shutdown fence (see [Graceful pod shutdown](#graceful-pod-shutdown)), so that one specific shape of 503 is provably side-effect free and safe to retry. Bare 5xx is **not** retried, because once an engine has executed a request it may have applied side effects and a retry could duplicate them. `num_retries: 50` combined with the `previous_hosts` retry-host predicate means each successive attempt lands on a pod we have not tried yet, until either the sub-cluster's host set is exhausted or the client deadline expires.
- `per_connection_buffer_limit_bytes` hard-coded to **2 MiB** on both the listener and the DFP cluster (kept in lockstep). This is the **request-replay budget**: a retry — including the X-Firebolt-Drained one — can only be issued when the full request body fits in this buffer. Requests larger than the limit are dispatched without buffering and any 503 they receive propagates to the client unretried. The value is intentionally not surfaced on the CR (see the rationale comment on `gatewayPerConnectionBufferLimitBytes` in `instance_gateway.go`): it sits at the centre of two operator-owned invariants — retry coverage and gateway memory budget — and a per-instance override would invite settings that silently break either. Memory budget per gateway pod scales as `concurrent_requests × (1 + retry_factor) × 2 MiB`; size `gateway.replicas` and `gateway.resources.limits.memory` to that envelope. The standalone helm chart (`firebolt-instance-helm`) renders the same 2 MiB value through its own gateway-configmap so the two deployment paths behave identically. See the README's *Gateway sizing* section for the operational guidance.
- An admin listener on `127.0.0.1:9901` used by the gateway pod's own `preStop` hook (`POST /healthcheck/fail`) to fail the gateway's readiness *before* the kubelet sends SIGTERM, so service load-balancers stop sending it new requests before its filters tear down.

Because the per-engine routing Service is headless (`ClusterIP: None`), the `:authority` hostname resolves directly to the set of *ready* pod IPs. kube-proxy is not in the data path, so there is no terminating-endpoint race where a SYN would be DNAT'd to a pod whose listener has already closed.

### Traffic path

```
Client (X-Firebolt-Engine: my-engine) → Gateway Service (:80) → Envoy (:8080) → headless {engine}-service (pod IPs) → Engine Pod
```

The gateway Deployment uses a rolling update strategy with `maxSurge: 25%` and `maxUnavailable: 0`, ensuring zero downtime during gateway upgrades.

### Graceful pod shutdown

When a blue-green cutover or scale-down deletes the old-generation StatefulSet, the kubelet sends SIGTERM to its pods while client queries may still be in flight at the gateway. Zero-downtime is preserved end-to-end by a chain of independent mechanisms; **no operator-side gate on EndpointSlice routability is required** (see [Why no EndpointSlice gate](#why-no-endpointslice-gate)). In order of when they fire after SIGTERM:

1. **Engine `/health/ready` flips to 503.** The kubelet readiness probe sees this on its next scrape and marks the pod NotReady. The K8s endpoint controller removes the pod from the cluster Service's EndpointSlices; CoreDNS stops returning that pod IP for the headless Service.
2. **Envoy active health check ejects the host.** Envoy probes `/health/ready` directly on each pod IP every `1s`. With `unhealthy_threshold: 1`, one failed probe is enough to remove the host from the load-balanced set. This is independent of DNS, so it does not wait on EndpointSlice / CoreDNS propagation.
3. **`max_requests_per_connection: 1` + per-request DNS.** Any query that arrives at the gateway after the host is excluded from DNS opens a fresh TCP connection (no keep-alive reuse), does a fresh DNS lookup, and never sees the dying pod.
4. **Engine pre-work shutdown fence.** A query that did slip onto an open connection or a stale-DNS host *before* either of the above hides it hits the engine's HTTP handler, which fast-fails with `503 Service Unavailable`, `Connection: close`, and the `X-Firebolt-Drained` header **before** any executor or Storage Manager work runs. The connection drops out of any pool, and the request is provably side-effect free.
5. **Gateway retries the 503 on a different host.** The `retriable-headers` rule matches `X-Firebolt-Drained: present`; combined with the `previous_hosts` retry-host predicate, the retry never picks the same draining pod. The client sees a single 200 response — **provided the request body fits within `per_connection_buffer_limit_bytes` (hard-coded at 2 MiB)**. Bodies that exceed the buffer are dispatched without buffering and the 503 is propagated to the client unretried; workloads that send single requests above 2 MiB are out of scope for the zero-downtime contract.

In-flight queries that the engine accepted *before* SIGTERM continue to run, bounded by `shutdown_wait_unfinished` (= `terminationGracePeriodSeconds − 5s`). The fence only fences *new* requests; it does not interrupt work already in progress.

### Why no EndpointSlice gate

A previous design (removed in **FB-661**, commits `be577f2` and `d6dce81`) had the operator wait in `Switching` for the cluster Service's EndpointSlice to contain at least one Ready endpoint. It was deleted when the cluster Service became headless: with kube-proxy out of the data path, K8s automatically excludes not-ready pods from the headless DNS A-record set, and the gate was redundant.

The same conclusion holds for the *symmetric* version of the gate — "wait until the EndpointSlice no longer references the **draining** generation before transitioning `draining → cleaning`":

- The chain in [Graceful pod shutdown](#graceful-pod-shutdown) closes the race in the data plane: any late request on a draining pod gets a clean retriable 503 and the gateway recovers before the client.
- An EndpointSlice gate only shifts *when* SIGTERM fires (after the slice update vs. concurrent with it); it does not shrink the window where Envoy might still pick a draining host, which is bounded by the active-health-check interval, not by slice propagation.
- Reintroducing it costs an extra Watch + RBAC + reconcile-state field with no liveness improvement under the existing data-plane contract.

If you find yourself wanting to add such a gate to fix a 5xx during cutover, the right question is which of mechanisms 1–5 above is broken — not whether to bolt on a sixth.

---

## Rolling update parameters

### Metadata Deployment

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| `maxSurge` | `0` | Never run two metadata pods concurrently — the metadata service assumes single-writer against Postgres |
| `maxUnavailable` | `1` | Old pod is terminated before the new one starts; brief metadata-unavailable window during rollouts |
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
