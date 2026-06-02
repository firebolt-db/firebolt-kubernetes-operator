---
title: Operator-based scaling
description: Zero-downtime engine scaling with the Firebolt Kubernetes operator.
sidebarTitle: Operator-based scaling
---

This document describes the architecture for zero-downtime scaling of Firebolt engines using a lightweight Kubernetes operator. The operator manages three CRDs: `FireboltInstance` (shared infrastructure), `FireboltEngine` (compute nodes), and the optional namespaced `FireboltEngineClass` (pod-template fragment shared by multiple engines in the same namespace — see [fireboltengineclass-crd-reference](crd-reference/fireboltengineclass-crd-reference)).

## Goals

- Zero-downtime scaling via blue-green engine transitions
- Single configuration change triggers full orchestration
- No Helm or manual multi-step processes
- Custom Resource Definitions (CRDs) with status subresources for clean state management
- Ephemeral engines (no persistent storage)
- Instance-level infrastructure managed declaratively (PostgreSQL, metadata, Envoy gateway)

## Architecture Overview

```text
┌────────────────────────────────────────────────────────────┐
│                        Operator                            │
│                                                            │
│  - Watches FireboltInstance and FireboltEngine CRs         │
│  - Manages instance infra (PG, metadata, Envoy gateway)    │
│  - Manages StatefulSets + Services per engine generation   │
│  - Pre-generates engine config (predictable pod names)     │
│  - Switches traffic via engine Service selector            │
│  - Runs drain check before deleting old generation         │
└────────────────────────────────────────────────────────────┘
        │
        │ watches / updates
        ▼
┌─────────────────────────────────────────────────────────────┐
│                   FireboltInstance CR                        │
│  firebolt-production                                        │
│                                                             │
│  Provisions: PostgreSQL, metadata service, Envoy gateway    │
│  Engines reference this via spec.instanceRef                │
│                                                             │
│  status:                                                    │
│    phase: Ready                                             │
│    metadataEndpoint: ...-metadata.ns.svc:7000               │
│    id: 01KP98J0...                                          │
│    gatewayEndpoint: ...-gateway.ns.svc.cluster.local        │
└─────────────────────────────────────────────────────────────┘
        │
        │ engines reference instance
        ▼
┌─────────────────────────────────────────────────────────────┐
│                    FireboltEngine CR                         │
│  engine-production                                     │
│                                                             │
│  spec:                          status:                     │
│    instanceRef: firebolt-prod     currentGeneration: 1      │
│    replicas: 5                    activeGeneration: 1       │
│    image:                         phase: stable             │
│      repository: .../engine       observedGeneration: 3     │
│      tag: v1.2                    conditions:               │
│    resources:                       - type: InstanceReady   │
│      requests:                        status: "True"        │
│        cpu: "2"                                             │
│        memory: "8Gi"                                        │
│      limits:                                                │
│        cpu: "2"                                             │
│        memory: "8Gi"                                        │
└─────────────────────────────────────────────────────────────┘

        │ creates / manages
        ▼
┌────────────────────────────────────────────────────────────┐
│                 Per-Generation Resources                   │
│                                                            │
│  ┌─────────────────────────────────────────────────────┐   │
│  │ Generation 1 (active)                               │   │
│  │                                                     │   │
│  │  StatefulSet: engine-production-g1             │   │
│  │  Headless Service: engine-production-g1-hl     │   │
│  │  ConfigMap: engine-production-g1-config        │   │
│  └─────────────────────────────────────────────────────┘   │
│                                                            │
│  ┌─────────────────────────────────────────────────────┐   │
│  │ Generation 0 (draining)                             │   │
│  │                                                     │   │
│  │  StatefulSet: engine-production-g0             │   │
│  │  Headless Service: engine-production-g0-hl     │   │
│  │  ConfigMap: engine-production-g0-config        │   │
│  └─────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────┐
│                     Engine Service                         │
│             engine-production-service                 │
│        selector: firebolt.io/generation=1                  │
│                                                            │
│         (Stable endpoint, operator updates selector)       │
└────────────────────────────────────────────────────────────┘
```

## Components

### 1. FireboltEngine Custom Resource

User-facing configuration. The operator watches `FireboltEngine` CRs in the configured namespace (or all namespaces).

For the complete list of supported fields, see the [README](../README.md#configuration-reference).

### 2. Status Subresource

The engine's status is stored in the `.status` subresource of the CR, managed exclusively by the operator.

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngine
metadata:
  name: engine-production
  namespace: firebolt
spec:
  instanceRef: firebolt-production
  # Engine container image lives on an FireboltEngineClass (FB-1145); the engine
  # references the class by name. Mutating the class' container image
  # is the canonical path for runtime version upgrades.
  engineClassRef: engine-default
  replicas: 5
  resources:
    requests:
      cpu: "2"
      memory: "8Gi"
    limits:
      cpu: "2"
      memory: "8Gi"
status:
  currentGeneration: 2
  activeGeneration: 2
  phase: stable
  observedGeneration: 5
  conditions:
    - type: InstanceReady
      status: "True"
```

The `spec.instanceRef` field is required and references a `FireboltInstance` in the same namespace. The engine reconciler gates on this instance being ready before creating or updating engine resources.

Generation numbering: `activeGeneration` starts at `-1` (no active generation). The first deployment creates generation `0`.

**Phases:**
- `stable` - Single active generation with `replicas > 0`, no transition in progress
- `creating` - New generation being created
- `switching` - Traffic being switched to new generation
- `draining` - Running drain checks on old generation
- `cleaning` - Deleting old generation resources
- `stopped` - Terminal state when `spec.replicas == 0`. Structurally identical to `stable` (the active generation exists as an empty StatefulSet + headless Service + ConfigMap), but surfaced as a distinct phase so `kubectl get` and GitOps tooling can tell a running engine apart from an intentionally parked one. See [Stopping an Engine](#stopping-an-engine) below.

### State Machine

```text
              ┌────────────────┐                 ┌────────────────┐
              │     stable     │                 │    stopped     │
              │ (replicas > 0) │                 │ (replicas == 0)│
              └────────────────┘                 └────────────────┘
                       │                                  │
                       │ spec change detected             │ spec change detected
                       ▼                                  ▼
                 ┌─────────────────────────────────────────────────┐
                 │                  creating                       │
                 │  (new generation resources being created)       │
                 │  (waiting for all pods to be Ready)             │
                 └─────────────────────────────────────────────────┘
                              │
                              │ all pods Ready (trivially true at replicas=0)
                              ▼
                 ┌─────────────────────────────────────────────────┐
                 │                 switching                       │
                 │  (engine Service selector updated)              │
                 │  (new traffic → new generation)                 │
                 └─────────────────────────────────────────────────┘
                              │
                              │ immediate (selector updated)
                              ▼
                 ┌─────────────────────────────────────────────────┐
                 │                  draining                       │
                 │  (drain check on each old generation pod)       │
                 │  (waiting for running queries to complete)      │
                 │                                                 │
                 │  If rollout=recreate: skip drain, go to cleaning│
                 └─────────────────────────────────────────────────┘
                              │
                              │ all pods report 0 running queries
                              │ (or rollout=recreate skips this)
                              ▼
                 ┌─────────────────────────────────────────────────┐
                 │                  cleaning                       │
                 │  (deleting old generation resources)            │
                 └─────────────────────────────────────────────────┘
                              │
                              │ old resources deleted
                              ▼
                       ┌──────────────┐      ┌──────────────┐
                       │    stable    │  or  │   stopped    │
                       │ replicas > 0 │      │ replicas == 0│
                       └──────────────┘      └──────────────┘
```

The terminal phase is chosen by `spec.replicas`: non-zero lands in `stable`, zero lands in `stopped`. Every other aspect of the transition is identical. Scaling an engine down to zero replicas follows the same blue-green path as any other spec change; the new generation's StatefulSet just has zero replicas.

**Key Invariants:**
- Traffic is switched only after new generation is fully Ready (an empty StatefulSet is trivially Ready, so switching does not block for scale-to-zero)
- Old generation is deleted only after all pods are drained
- At most two generations exist simultaneously (active + draining)
- `activeGeneration >= 0` in both `stable` and `stopped`; the zero-replica active generation keeps its StatefulSet, headless Service, and ConfigMap so drift detection and resume work identically to `stable`

### 3. Per-Generation Resources

For each engine generation, the operator creates resources with naming pattern `{engine-name}-g{N}`. All resources have `ownerReferences` pointing to the `FireboltEngine` CR for cascading deletion.

**StatefulSet:**

Key design choices:
- `podManagementPolicy: Parallel` - All pods start simultaneously for faster cluster formation
- Labels `firebolt.io/engine` and `firebolt.io/generation` for selector-based routing
- `ownerReferences` pointing to the `FireboltEngine` CR for cascading deletion
- Config mounted from pre-generated ConfigMap with pod FQDNs

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: engine-production-g2
  labels:
    firebolt.io/engine: engine-production
    firebolt.io/generation: "2"
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: engine-production
spec:
  serviceName: engine-production-g2-hl
  replicas: 5
  podManagementPolicy: Parallel
  selector:
    matchLabels:
      firebolt.io/engine: engine-production
      firebolt.io/generation: "2"
  # ... pod template with engine container and config volume
```

**Headless Service (for StatefulSet pod DNS):**

The headless service (`-hl` suffix) is a critical Kubernetes component required for StatefulSet pod networking:

1. **Stable DNS names**: Each pod gets a predictable DNS name: `{pod-name}.{headless-service}.{namespace}.svc`. For example, `engine-production-g2-0.engine-production-g2-hl.firebolt.svc`.

2. **Pod-to-pod communication**: engine nodes need to discover and communicate with each other during cluster formation. The headless service provides the DNS resolution that allows pods to find their peers by name.

3. **Pre-generated config**: The operator generates the config.yaml with all pod FQDNs before the StatefulSet is created. This is possible because the pod names and headless service name are deterministic.

4. **`publishNotReadyAddresses: true`**: This setting ensures DNS entries exist for pods even before they pass readiness checks. Without this, pods could not find each other during startup, causing cluster formation to fail.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: engine-production-g2-hl
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: engine-production
spec:
  clusterIP: None
  publishNotReadyAddresses: true
  selector:
    firebolt.io/engine: engine-production
    firebolt.io/generation: "2"
  ports:
    - port: 3473
      name: query
```

Note: This per-generation headless service (`-hl` suffix) exists solely for StatefulSet pod-to-pod DNS. External and gateway traffic go through the separate routing Service (`-service` suffix) described in the next section, which is also headless but serves a different purpose.

**Engine Config ConfigMap (pre-generated by operator):**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: engine-production-g2-config
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: engine-production
data:
  config.yaml: |
    schema_version: "1.0"
    instance:
      id: 01KP98J0000000000000000000
      type: multi_engine
      multi_engine:
        metadata_endpoint: firebolt-production-metadata.firebolt.svc.cluster.local:7000
    engine:
      id: engine-production
      nodes:
      - host: engine-production-g2-0.engine-production-g2-hl.firebolt.svc
      - host: engine-production-g2-1.engine-production-g2-hl.firebolt.svc
      - host: engine-production-g2-2.engine-production-g2-hl.firebolt.svc
      - host: engine-production-g2-3.engine-production-g2-hl.firebolt.svc
      - host: engine-production-g2-4.engine-production-g2-hl.firebolt.svc
      termination_grace_period: 55s
    logging:
      format: json
```

The `instance` section is populated from the parent `FireboltInstance` (`spec.id` becomes the `instance.id` ULID, `status.metadataEndpoint` becomes `instance.multi_engine.metadata_endpoint`). The metadata endpoint can be overridden per-engine via `spec.metadataEndpointOverride`.

### 4. Engine Service

Stable routing endpoint for both the instance gateway and external clients doing their own load balancing. Named with `-service` suffix matching the engine name. The operator updates the `firebolt.io/generation` selector to switch traffic between generations during blue-green rollouts.

The service is **headless** (`clusterIP: None`). DNS resolution for the service hostname returns the set of ready pod IPs directly, with no VIP and no kube-proxy in the data path. This has two properties the operator depends on for zero-downtime routing:

- `publishNotReadyAddresses: false` (default) means only endpoints whose pod-level readiness probe passes appear in the DNS A-record set. A pod that has flipped to not-ready is removed automatically; a newly-ready pod appears as soon as the probe passes, without waiting for an endpoints-controller propagation the reconciler would have to gate on.
- Atomically flipping the selector's `firebolt.io/generation` label from the draining generation to the new one switches the DNS A-record set over without disturbing clients that re-resolve at request time. The instance gateway's dynamic forward proxy is one such client; another is any external caller that wants to maintain its own pod-IP pool.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: engine-production-service
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: engine-production
spec:
  type: ClusterIP
  clusterIP: None
  publishNotReadyAddresses: false
  selector:
    firebolt.io/engine: engine-production
    firebolt.io/generation: "2"
  ports:
    - port: 3473
      name: query
```

For the end-user contract on both entry points (gateway vs direct headless Service), see the "Connecting to Engines" section of the top-level [README](../README.md).

## Reconciliation Loop

```text
┌─────────────────────────────────────────────────────────────┐
│                    Operator Watch Loop                      │
│            (per FireboltEngine CR found)                    │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
              ┌───────────────────────┐
              │ Read FireboltEngine   │
              │ spec + status         │
              └───────────────────────┘
                          │
                          ▼
              ┌───────────────────────┐
              │ Compare desired vs    │
              │ current state         │
              └───────────────────────┘
                          │
          ┌───────────────┴───────────────┐
          ▼                               ▼
┌─────────────────┐             ┌─────────────────┐
│ No change       │             │ Change detected │
│ (requeue)       │             │                 │
└─────────────────┘             └────────┬────────┘
                                         │
                                         ▼
                          ┌───────────────────────────┐
                          │ Check pending mutations   │
                          │ Keep only most recent     │
                          └───────────────────────────┘
                                         │
                                         ▼
                          ┌───────────────────────────┐
                          │ Execute transition plan   │
                          └───────────────────────────┘
```

## Transition Flow

### Scale Up: 3 → 5 nodes

```text
Phase: stable (g0 active, 3 nodes)
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 1. Create g1 resources:                                     │
│    - ConfigMap {engine}-g1-config (5 nodes config)          │
│    - Headless Service {engine}-g1-hl                        │
│    - StatefulSet {engine}-g1 (5 replicas)                   │
│                                                             │
│    Phase: creating                                          │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 2. Wait for all 5 pods Ready                                │
│                                                             │
│    Poll pods with label firebolt.io/generation=1            │
│    Wait until: 5/5 Running + Ready                          │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 3. Switch traffic                                           │
│                                                             │
│    Update engine Service selector: generation=1             │
│    New queries → g1                                         │
│    Existing connections → continue on g0                    │
│                                                             │
│    Phase: switching                                         │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 4. Drain old generation                                     │
│                                                             │
│    For each pod in g0:                                      │
│      Loop:                                                  │
│        GET /metrics via Pods/proxy on the engine container  │
│        If firebolt_running_queries +                        │
│           firebolt_suspended_queries == 0: pod is drained   │
│        Else: wait drainCheckInterval, retry                 │
│                                                             │
│    Wait until all pods report drained                       │
│                                                             │
│    Phase: draining                                          │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 5. Delete old generation                                    │
│                                                             │
│    Delete StatefulSet {engine}-g0                            │
│    Delete Service {engine}-g0-hl                            │
│    Delete ConfigMap {engine}-g0-config                      │
│                                                             │
│    Phase: cleaning                                          │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 6. Update status                                            │
│                                                             │
│    currentGeneration: 1                                     │
│    activeGeneration: 1                                      │
│    drainingGeneration: null                                 │
│    phase: stable                                            │
└─────────────────────────────────────────────────────────────┘
```

### Stopping an Engine

Setting `spec.replicas: 0` stops the engine without deleting the CR. This is the scale-to-zero path:

```bash
kubectl patch fireng/my-engine -p '{"spec":{"replicas":0}}' --type=merge
```

The operator runs the same blue-green transition as any other replica change:

1. `creating`: a new generation is created with a zero-replica StatefulSet (plus its headless Service and ConfigMap).
2. `switching`: the engine Service selector flips to the new generation. Because the new StatefulSet has zero Ready pods, the headless Service's endpoint set is empty.
3. `draining`: old-generation pods are drained per `spec.rollout` (`graceful` waits for zero in-flight queries; `recreate` skips the wait).
4. `cleaning`: old-generation resources are deleted.
5. Terminal phase is `stopped` instead of `stable`, because `spec.replicas == 0`.

The active generation's StatefulSet, headless Service, and ConfigMap persist in the stopped state (the StatefulSet just has zero replicas). This is deliberate — they make drift detection and the resume transition work identically to any other spec change.

Resuming is symmetric. Setting `spec.replicas` back to a non-zero value triggers a new blue-green generation: the zero-replica "active" STS is treated as the old generation, its drain is a trivial no-op, and the new generation brings up the requested pods before the selector flips.

**Client-facing contract while stopped:**

- The engine Service's headless DNS returns zero A-records (no ready endpoints).
- Requests through the instance gateway with `X-Firebolt-Engine: <stopped-engine>` return HTTP 503, because Envoy's `dynamic_forward_proxy` resolves the engine Service and finds no upstream. The response is indistinguishable from a request to a non-existent engine. See docs-internal/option-b-per-engine-envoy-clusters.md for the gateway resolution model.
- No gateway reconfiguration happens on stop/resume — the gateway is engine-set agnostic and discovers engines at request time. Existing gateway retry semantics apply unchanged.
- In-flight requests during the selector flip behave the same as during any other blue-green transition: pods entering termination continue serving in-flight queries until `shutdown_wait_unfinished` expires or they complete, then exit before `terminationGracePeriodSeconds`.

**Status signal contract:**

- `phase: stopped` identifies the terminal stopped state (as opposed to `stable`, which means running).
- `Ready` condition becomes `Status=False, Reason=Stopped` with message `"Engine is stopped (spec.replicas is 0)"`. GitOps tools that gate on `Ready=True` will correctly treat a stopped engine as not-converged-to-serving without mistaking it for an in-progress rollout.

**Stopped engines tolerate instance churn:** the stopped engine's ConfigMap is not re-checked against live `FireboltInstance` data until the engine resumes. If the backing instance is re-provisioned (metadata endpoint changes) while the engine is stopped, the engine remains in `stopped` with its now-stale ConfigMap. This is harmless because the engine has zero pods consuming that ConfigMap, and the next resume triggers a new generation whose ConfigMap is rebuilt against the current instance endpoint.

## Drain Check

The operator scrapes the engine's Prometheus `/metrics` endpoint on port `9090` and checks `firebolt_running_queries + firebolt_suspended_queries`. If both gauges read zero, the pod is considered drained and safe to delete. Otherwise, the operator retries after `drainCheckInterval`.

The scrape goes through the Kubernetes API server's `Pods/proxy` subresource (not pod IPs directly), so the operator works the same way in-cluster and out-of-cluster without needing reachability to pod networks. Required RBAC is `pods/proxy: get`.

On SIGTERM, the engine process waits up to `terminationGracePeriodSeconds − 5s` (`shutdown_wait_unfinished`) for in-flight queries to finish before exiting. Envoy's active health checks eject the pod from routing within ~1s of SIGTERM, so no new queries are routed to a draining pod.

See [architecture](architecture) for the full drain-check specification.

## Spec Change Handling

The operator reads the live `.spec` on every reconcile and compares it against the current cluster state. There is no snapshotting or pending mutation queue.

### During `stable`

A spec change is detected by comparing the live spec against the resources of the active generation. If the spec differs, the reconciler writes a status intent (`Phase=creating`, bumped `currentGeneration`) and requeues. No resources are created in this pass.

### During `creating`

If the spec changes while a generation is being created, the in-progress generation is **abandoned**: its StatefulSet, headless Service, and ConfigMap are deleted, `currentGeneration` is bumped, and the next reconcile creates fresh resources for the new generation. This avoids patching a live STS whose pods have already read a stale config (e.g. an outdated node list), which would cause a permanent readiness deadlock.

### During `switching`, `draining`, `cleaning`

Spec changes are **not acted on** until the current transition completes and the engine returns to `stable`. This prevents unbounded resource accumulation.

### Rapid Successive Changes

```
g0 active (3 nodes), phase: stable
  - User changes: 5 → 3 → 7 → 4 → 6 → 2 (rapid succession)
  
Operator behavior:
  - Each reconcile sees the latest spec value
  - Only one generation transition occurs (g0 → g1)
  - The final g1 uses the last observed spec (2 replicas)
```

This ensures:
- At most one transition is in progress at any time
- The final desired state is always what gets applied
- All state is persisted in the status subresource for crash recovery

## Traffic Flow

Queries flow through an Envoy proxy, which routes to engine services based on the `X-Firebolt-Engine` header. A Lua filter extracts the engine name and sets the upstream hostname to `{engine}-service:3473`, then the dynamic forward proxy resolves it via DNS:

```text
Client (X-Firebolt-Engine: engine-production)
   │
   ▼
┌──────────────────────────────────────────────┐
│        Envoy Gateway Service                 │
│    firebolt-production-gateway:80            │
│    (Lua filter → dynamic forward proxy)      │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│            Engine Service                    │
│    engine-production-service:3473       │
│    selector: firebolt.io/generation=1        │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│    StatefulSet engine-production-g1     │
│                                              │
│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐   │
│  │pod-0│ │pod-1│ │pod-2│ │pod-3│ │pod-4│   │
│  └─────┘ └─────┘ └─────┘ └─────┘ └─────┘   │
└──────────────────────────────────────────────┘

During transition, g0 continues serving existing connections
until drain check confirms 0 active queries on all pods.
```

The Envoy proxy discovers engine endpoints dynamically via DNS. It resolves `{engine}-service` (the per-engine headless Service) and forwards queries to port 3473 on one of the returned pod IPs.

## Operator RBAC

The operator requires the following permissions (generated from kubebuilder markers) for both the engine and instance controllers:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:
  # Engine controller
  - apiGroups: ["compute.firebolt.io"]
    resources: ["fireboltengines"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["compute.firebolt.io"]
    resources: ["fireboltengines/status", "fireboltengines/finalizers"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/proxy"]
    verbs: ["get"]

  # Instance controller
  - apiGroups: ["compute.firebolt.io"]
    resources: ["fireboltinstances"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["compute.firebolt.io"]
    resources: ["fireboltinstances/status", "fireboltinstances/finalizers"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["apps"]
    resources: ["deployments"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["secrets", "persistentvolumeclaims", "serviceaccounts"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["roles", "rolebindings"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]

  # Shared
  - apiGroups: [""]
    resources: ["configmaps", "services"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
```

The operator supports both namespace-scoped and cluster-scoped operation. Use `--namespace` to restrict to a single namespace.

## Operator Deployment

### Command-Line Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--namespace` | (optional) | Namespace to watch. Watches all namespaces if empty. |
| `--metrics-bind-address` | `0` | Address for the metrics endpoint (`:8443` for HTTPS, `:8080` for HTTP, `0` to disable). |
| `--health-probe-bind-address` | `:8081` | Address for the health probe endpoint. |
| `--leader-elect` | `false` | Enable leader election for HA deployments. |

### Startup Behavior

On startup, the operator:
1. Lists all `FireboltEngine` CRs in the watched namespace(s)
2. For each engine, checks the status subresource for current phase
3. If status exists with an in-progress transition: resumes from current phase
4. If no status exists: initializes from scratch (generation 0)

### Multi-Engine Support

The operator can manage multiple independent engines in the same namespace. Each engine is defined by a separate `FireboltEngine` resource:

```
engine-production   → manages engine-production-* resources
engine-staging      → manages engine-staging-* resources
engine-dev          → manages engine-dev-* resources
```

Reconciliation for each engine is independent.

## Configuration Reference

For the complete list of configurable fields in the engine spec, see the [README](../README.md#configuration-reference).

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `currentGeneration` | int | Latest generation number (`activeGeneration` starts at `-1`; first deploy creates generation `0`) |
| `activeGeneration` | int | Generation currently receiving traffic |
| `drainingGeneration` | int/null | Generation being drained (if any) |
| `phase` | string | Current phase (stable/creating/switching/draining/cleaning/stopped) |
| `observedGeneration` | int | Kubernetes metadata generation last reconciled |
| `conditions` | list | Status conditions (e.g. `InstanceReady`) |
| `lastActivityTime` | timestamp/null | Most recent autoscaler observation that recorded activity |
| `autoscaledAt` | timestamp/null | Most recent autoscaler-driven `spec.replicas` mutation |
| `autoscalerReason` | string | One of `Disabled`/`ScheduleActive`/`Stopped`/`Initializing`/`ActivityObserved`/`ScrapeFailed`/`Idle`/`WakeRequested` |

### Autoscaling

When `spec.autoscaling.enabled=true`, the operator owns `spec.replicas` (HPA-style) and toggles it between `minReplicas` (default `0`, scale-to-zero) and `maxReplicas` based on:

- the same `firebolt_running_queries + firebolt_suspended_queries` gauges that drive the drain check, summed across the active generation;
- `idleTimeout` (default 30m) measured from `status.lastActivityTime`;
- optional UTC `schedule[]` windows that pin replicas at `maxReplicas` regardless of activity.

Scale events are level-driven: the autoscaler patches `spec.replicas`; the existing `FireboltEngine` watch fires; the next reconcile converges via the normal blue-green path. The autoscaler runs only in `stable`/`stopped` phases so it cannot fight a rollout. See [architecture](architecture#autoscaler) for the full decision precedence and configuration reference.

### Gateway wake-up

When an engine is at zero replicas, the gateway can wake it by stamping the `firebolt.io/wake-requested` annotation (RFC 3339 timestamp) on the FireboltEngine CR. The engine autoscaler treats a fresh value (within 5 minutes of now) as a request to scale up to `maxReplicas`, bypassing the idle-timeout check. The operator provisions per-instance RBAC for this:

- `ServiceAccount` `<instance>-gateway`
- `Role` `<instance>-gateway-wake` granting `get/list/patch` on `fireboltengines` in the namespace
- `RoleBinding` linking them

The gateway-side buffering, annotation patching, and retry logic are not yet wired in; they would extend the Envoy Lua filter rendered by the operator in `internal/controller/instance_gateway.go`.


## Concurrency and Race Conditions

The operator uses Kubernetes optimistic concurrency control (ResourceVersion) to handle concurrent reconciles safely:

| Scenario | Behavior |
|----------|----------|
| Two reconciles read same status | Second update fails with conflict error, controller-runtime requeues |
| Resource created between Get and Create | Create returns AlreadyExists, requeue handles it |
| Spec changes during `creating` | In-progress generation abandoned, resources deleted, new generation created |
| Spec changes during `draining`/`cleaning` | Deferred until transition completes and engine returns to `stable` or `stopped` |
| Operator crash mid-transition | Restarts and resumes from persisted phase in status subresource |

**Key principle:** All state is persisted in the status subresource before taking action. If an update fails due to conflict, the reconcile is retried with fresh state.

## Failure Handling

| Scenario | Behavior |
|----------|----------|
| New generation fails to become Ready | Transition stalls in `creating` phase. Old generation continues serving. Manual intervention required. |
| Drain check never succeeds | Transition stalls in `draining` phase. New generation is active. Old generation remains. Manual intervention required. |
| Operator restarts mid-transition | Operator reads status subresource, resumes from current phase. |
| Engine deleted | Kubernetes garbage collection removes all owned resources. |

## Limitations

1. **No automatic rollback** - If new generation fails, operator does not revert. Traffic stays on old generation.
2. **No PVC support** - Designed for ephemeral engines only.

## Design Decisions

### Why a CRD instead of ConfigMaps

A Custom Resource Definition provides:
- **Status subresource**: Clean separation of user intent (spec) and operator state (status)
- **Short names**: `kubectl get fire` instead of verbose commands
- **Print columns**: Rich `kubectl get` output showing replicas, phase, generation
- **Validation**: Kubebuilder markers for field validation (min values, enums)
- **Owner references**: Proper GVK for owner references instead of referencing ConfigMaps
- **Structured spec**: Nested objects (image, resources) instead of flat string key-value pairs

### Why StatefulSets instead of Deployments or raw Pods

StatefulSets are used despite the operator pre-generating pod FQDNs because StatefulSets automatically recreate failed pods. Deployments would work for FQDN generation but use ReplicaSets which don't provide stable pod identities needed for config generation. Raw pods managed directly by the operator would require reimplementing pod health monitoring and recreation logic that StatefulSets provide out of the box.
