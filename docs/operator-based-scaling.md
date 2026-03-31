# Operator-Based Zero-Downtime Scaling Design

This document describes an architecture for zero-downtime scaling of Firebolt engines using a lightweight Kubernetes operator.

## Goals

- Zero-downtime scaling via blue-green engine transitions
- Single configuration change triggers full orchestration
- No Helm or manual multi-step processes
- Custom Resource Definition (CRD) with status subresource for clean state management
- Ephemeral engines (no persistent storage)

## Architecture Overview

```
┌────────────────────────────────────────────────────────────┐
│                        Operator                            │
│                                                            │
│  - Watches FireboltEngine CRs                              │
│  - Manages StatefulSets + Services per generation          │
│  - Pre-generates engine config (predictable pod names)     │
│  - Switches traffic via engine Service selector            │
│  - Runs drain check before deleting old generation         │
└────────────────────────────────────────────────────────────┘
        │
        │ watches / updates
        ▼
┌─────────────────────────────────────────────────────────────┐
│                    FireboltEngine CR                         │
│  core-engine-production                                     │
│                                                             │
│  spec:                          status:                     │
│    replicas: 5                    currentGeneration: 1      │
│    image:                         activeGeneration: 1       │
│      repository: .../core         phase: stable             │
│      tag: v1.2                    lastReconciled: ...       │
│    resources:                                               │
│      cpu: "2"                                               │
│      memory: "8Gi"                                          │
└─────────────────────────────────────────────────────────────┘

        │ creates / manages
        ▼
┌────────────────────────────────────────────────────────────┐
│                 Per-Generation Resources                   │
│                                                            │
│  ┌─────────────────────────────────────────────────────┐   │
│  │ Generation 1 (active)                               │   │
│  │                                                     │   │
│  │  StatefulSet: core-engine-production-g1             │   │
│  │  Headless Service: core-engine-production-g1-hl     │   │
│  │  ConfigMap: core-engine-production-g1-config        │   │
│  └─────────────────────────────────────────────────────┘   │
│                                                            │
│  ┌─────────────────────────────────────────────────────┐   │
│  │ Generation 0 (draining)                             │   │
│  │                                                     │   │
│  │  StatefulSet: core-engine-production-g0             │   │
│  │  Headless Service: core-engine-production-g0-hl     │   │
│  │  ConfigMap: core-engine-production-g0-config        │   │
│  └─────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────┐
│                     Engine Service                         │
│             core-engine-production-service                 │
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
  name: core-engine-production
  namespace: firebolt
spec:
  replicas: 5
  image:
    repository: ghcr.io/firebolt-db/firebolt-core
    tag: v1.2.0
  resources:
    cpu: "2"
    memory: "8Gi"
status:
  currentGeneration: 2
  activeGeneration: 2
  phase: stable
  lastReconciled: "2024-01-15T10:00:00Z"
```

Initial generation starts at 0.

**Phases:**
- `stable` - Single active generation, no transition in progress
- `creating` - New generation being created
- `switching` - Traffic being switched to new generation
- `draining` - Running drain checks on old generation
- `cleaning` - Deleting old generation resources

### State Machine

```
                 ┌─────────────────────────────────────────────────┐
                 │                   stable                        │
                 │  (only active generation exists)                │
                 └─────────────────────────────────────────────────┘
                              │
                              │ spec change detected
                              ▼
                 ┌─────────────────────────────────────────────────┐
                 │                  creating                       │
                 │  (new generation resources being created)       │
                 │  (waiting for all pods to be Ready)             │
                 └─────────────────────────────────────────────────┘
                              │
                              │ all pods Ready
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
                 ┌─────────────────────────────────────────────────┐
                 │                   stable                        │
                 │  (only new generation exists)                   │
                 └─────────────────────────────────────────────────┘
```

**Key Invariants:**
- Traffic is switched only after new generation is fully Ready
- Old generation is deleted only after all pods are drained
- At most two generations exist simultaneously (active + draining)

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
  name: core-engine-production-g2
  labels:
    firebolt.io/engine: core-engine-production
    firebolt.io/generation: "2"
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: core-engine-production
spec:
  serviceName: core-engine-production-g2-hl
  replicas: 5
  podManagementPolicy: Parallel
  selector:
    matchLabels:
      firebolt.io/engine: core-engine-production
      firebolt.io/generation: "2"
  # ... pod template with Core container and config volume
```

**Headless Service (for StatefulSet pod DNS):**

The headless service (`-hl` suffix) is a critical Kubernetes component required for StatefulSet pod networking:

1. **Stable DNS names**: Each pod gets a predictable DNS name: `{pod-name}.{headless-service}.{namespace}.svc`. For example, `core-engine-production-g2-0.core-engine-production-g2-hl.firebolt.svc`.

2. **Pod-to-pod communication**: Core nodes need to discover and communicate with each other during cluster formation. The headless service provides the DNS resolution that allows pods to find their peers by name.

3. **Pre-generated config**: The operator generates the config.json with all pod FQDNs before the StatefulSet is created. This is possible because the pod names and headless service name are deterministic.

4. **`publishNotReadyAddresses: true`**: This setting ensures DNS entries exist for pods even before they pass readiness checks. Without this, pods could not find each other during startup, causing cluster formation to fail.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: core-engine-production-g2-hl
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: core-engine-production
spec:
  clusterIP: None
  publishNotReadyAddresses: true
  selector:
    firebolt.io/engine: core-engine-production
    firebolt.io/generation: "2"
  ports:
    - port: 3473
      name: query
```

Note: The headless service is NOT used for external traffic. External traffic goes through the engine service (`-service` suffix) which load balances across all ready pods in the active generation.

**Core Config ConfigMap (pre-generated by operator):**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: core-engine-production-g2-config
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: core-engine-production
data:
  config.json: |
    {
      "nodes": [
        {"host": "core-engine-production-g2-0.core-engine-production-g2-hl.firebolt.svc"},
        {"host": "core-engine-production-g2-1.core-engine-production-g2-hl.firebolt.svc"},
        {"host": "core-engine-production-g2-2.core-engine-production-g2-hl.firebolt.svc"},
        {"host": "core-engine-production-g2-3.core-engine-production-g2-hl.firebolt.svc"},
        {"host": "core-engine-production-g2-4.core-engine-production-g2-hl.firebolt.svc"}
      ]
    }
```

### 4. Engine Service

Stable endpoint for clients. Named with `-service` suffix matching the engine name. Operator updates selector to switch traffic between generations.

The service uses `ClusterIP` type, which provides connection queuing at the kernel level. If all backing pods are temporarily unavailable (e.g., during the brief moment of selector switch), new connections are queued rather than immediately rejected. This improves resilience during transitions.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: core-engine-production-service
  ownerReferences:
    - apiVersion: compute.firebolt.io/v1alpha1
      kind: FireboltEngine
      name: core-engine-production
spec:
  type: ClusterIP
  selector:
    firebolt.io/engine: core-engine-production
    firebolt.io/generation: "2"
  ports:
    - port: 3473
      name: query
```

## Reconciliation Loop

```
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

```
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
│        Exec fb drain check on 'core' container              │
│        Parse JSON: if data[0][0] == "0": pod is drained     │
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

## Drain Check

The operator uses the `fb` CLI (available in the Core container) to check if a pod has finished serving queries. The drain check query counts running queries that should block shutdown. If the count is `"0"`, the pod is considered drained and safe to delete. Otherwise, the operator retries after `drainCheckInterval`.

The drain check is executed via `kubectl exec` on the `core` container of each pod in the draining generation.

## Config Snapshotting and Pending Mutations

When the operator starts a transition, it **snapshots** the target configuration into `status.lastAppliedConfig`. This snapshot is used for the entire transition, regardless of subsequent changes to the engine spec.

### Behavior During Transitions

```
Current state:
  - g0 active (3 nodes)
  - phase: stable
  - lastAppliedConfig: {replicas: 3, ...}

User changes: replicas 3 → 5

Operator logic:
  1. Detect change (live spec differs from lastAppliedConfig)
  2. Snapshot new config: lastAppliedConfig = {replicas: 5, ...}
  3. Start transition: phase = creating, create g1 resources with 5 replicas
  4. Continue transition using snapshotted config
```

### Handling Changes During Transition

If the spec changes while a transition is in progress, the new spec is saved as `status.pendingMutation`. Only **one** pending mutation is kept (the most recent).

```
Current state:
  - g0 active (3 nodes)
  - g1 creating (5 nodes)
  - phase: creating
  - lastAppliedConfig: {replicas: 5, ...}  (snapshotted)

User changes: replicas 5 → 7

Operator logic:
  1. Detect change (live spec differs from lastAppliedConfig)
  2. Save as pending: pendingMutation = {replicas: 7, ...}
  3. Continue current transition with snapshotted config (5 replicas)
  4. After g1 transition completes → phase = stable
  5. Immediately apply pendingMutation → start new transition to g2 (7 replicas)
```

### Rapid Successive Changes

```
During g0 → g1 transition:
  - User changes: 5 → 3 → 7 → 4 → 6 → 2 (rapid succession)
  
Operator behavior:
  - Each change overwrites pendingMutation
  - Only the last value (2) is kept
  - After g1 completes, g2 is created with 2 replicas
```

This ensures:
- Current transition completes cleanly with its snapshotted config
- Rapid successive changes result in at most one additional transition
- Only the final desired state matters
- Operator can resume correctly after restart (state is persisted in status subresource)

## Traffic Flow

```
Client
   │
   ▼
┌──────────────────────────────────────────────┐
│            Engine Service                    │
│    core-engine-production-service            │
│    selector: firebolt.io/generation=1        │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│    StatefulSet core-engine-production-g1     │
│                                              │
│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐   │
│  │pod-0│ │pod-1│ │pod-2│ │pod-3│ │pod-4│   │
│  └─────┘ └─────┘ └─────┘ └─────┘ └─────┘   │
└──────────────────────────────────────────────┘

During transition, g0 continues serving existing connections
until drain check confirms 0 active queries on all pods.
```

## Operator RBAC

The operator requires the following permissions (generated from kubebuilder markers):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules:
  - apiGroups: ["compute.firebolt.io"]
    resources: ["fireboltengines"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["compute.firebolt.io"]
    resources: ["fireboltengines/status"]
    verbs: ["get", "update", "patch"]
  - apiGroups: ["compute.firebolt.io"]
    resources: ["fireboltengines/finalizers"]
    verbs: ["update"]
  - apiGroups: [""]
    resources: ["configmaps", "services"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["get", "list", "watch", "create", "update", "patch", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
```

The operator supports both namespace-scoped and cluster-scoped operation. Use `--namespace` to restrict to a single namespace.

## Operator Deployment

### Command-Line Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--namespace` | (optional) | Namespace to watch. Watches all namespaces if empty. |

### Startup Behavior

On startup, the operator:
1. Lists all `FireboltEngine` CRs in the watched namespace(s)
2. For each engine, checks the status subresource for current phase
3. If status exists with an in-progress transition: resumes from current phase
4. If no status exists: initializes from scratch (generation 0)

### Multi-Engine Support

The operator can manage multiple independent engines in the same namespace. Each engine is defined by a separate `FireboltEngine` resource:

```
core-engine-production   → manages core-engine-production-* resources
core-engine-staging      → manages core-engine-staging-* resources
core-engine-dev          → manages core-engine-dev-* resources
```

Reconciliation for each engine is independent.

## Configuration Reference

For the complete list of configurable fields in the engine spec, see the [README](../README.md#configuration-reference).

### Status Fields

| Field | Type | Description |
|-------|------|-------------|
| `currentGeneration` | int | Latest generation number (starts at 0) |
| `activeGeneration` | int | Generation currently receiving traffic |
| `drainingGeneration` | int/null | Generation being drained (if any) |
| `phase` | string | Current phase (stable/creating/switching/draining/cleaning) |
| `lastReconciled` | string | Timestamp of last reconciliation |
| `pendingMutation` | object/null | Queued mutation waiting to be processed |
| `lastAppliedConfig` | object/null | Spec used to create the current/active generation |

## Concurrency and Race Conditions

The operator uses Kubernetes optimistic concurrency control (ResourceVersion) to handle concurrent reconciles safely:

| Scenario | Behavior |
|----------|----------|
| Two reconciles read same status | Second update fails with conflict error, controller-runtime requeues |
| Resource created between Get and Create | Create returns AlreadyExists, requeue handles it |
| Rapid spec changes during transition | Only the latest change is kept as PendingMutation |
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
