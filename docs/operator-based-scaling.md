# Operator-Based Zero-Downtime Scaling Design

This document describes an architecture for zero-downtime scaling of Firebolt Core clusters using a lightweight Kubernetes operator.

## Goals

- Zero-downtime scaling via blue-green cluster transitions
- Single configuration change triggers full orchestration
- No Helm or manual multi-step processes
- No custom CRDs required
- Ephemeral clusters (no persistent storage)

## Architecture Overview

```
┌────────────────────────────────────────────────────────────┐
│                        Operator                            │
│                                                            │
│  - Watches ConfigMaps with prefix (default: core-cluster)  │
│  - Manages StatefulSets + Services per generation          │
│  - Pre-generates cluster config (predictable pod names)    │
│  - Switches traffic via cluster Service selector           │
│  - Runs drain check before deleting old cluster            │
└────────────────────────────────────────────────────────────┘
        │
        │ watches / updates
        ▼
┌─────────────────────────────┐     ┌─────────────────────────────────┐
│         ConfigMap           │     │           ConfigMap             │
│  core-cluster-production    │     │  core-cluster-production-status │
│                             │     │                                 │
│  replicas: 5                │     │  generation: 1                  │
│  image: .../firebolt-core   │     │  phase: stable                  │
│  tag: v1.2                  │     │                                 │
└─────────────────────────────┘     └─────────────────────────────────┘

        │ creates / manages
        ▼
┌────────────────────────────────────────────────────────────┐
│                 Per-Generation Resources                   │
│                                                            │
│  ┌─────────────────────────────────────────────────────┐   │
│  │ Generation 1 (active)                               │   │
│  │                                                     │   │
│  │  StatefulSet: core-cluster-production-g1         │   │
│  │  Headless Service: core-cluster-production-g1-hl │   │
│  │  ConfigMap: core-cluster-production-g1-config     │   │
│  └─────────────────────────────────────────────────────┘   │
│                                                            │
│  ┌─────────────────────────────────────────────────────┐   │
│  │ Generation 0 (draining)                             │   │
│  │                                                     │   │
│  │  StatefulSet: core-cluster-production-g0         │   │
│  │  Headless Service: core-cluster-production-g0-hl │   │
│  │  ConfigMap: core-cluster-production-g0-config     │   │
│  └─────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────┐
│                     Cluster Service                        │
│             core-cluster-production-service                │
│        selector: core-operator/generation=1                │
│                                                            │
│         (Stable endpoint, operator updates selector)       │
└────────────────────────────────────────────────────────────┘
```

## Components

### 1. Config ConfigMap

User-facing configuration. Operator watches ConfigMaps with a configurable prefix (default `core-cluster`). The ConfigMap name must start with this prefix.

For the complete list of supported fields, see the [README](../README.md#configuration-reference).

### 2. Status ConfigMap

Operator-managed state. Named with `-status` suffix matching the config ConfigMap.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: core-cluster-production-status  # Config name + "-status"
  namespace: firebolt
  ownerReferences:
    - apiVersion: v1
      kind: ConfigMap
      name: core-cluster-production
      uid: <config-configmap-uid>
data:
  state: |
    {
      "currentGeneration": 2,
      "activeGeneration": 2,
      "drainingGeneration": null,
      "phase": "stable",
      "lastReconciled": "2024-01-15T10:00:00Z",
      "pendingMutation": null
    }
```

Initial generation starts at 0.

**Phases:**
- `stable` - Single active cluster, no transition in progress
- `creating` - New cluster being created
- `switching` - Traffic being switched to new cluster
- `draining` - Running drain checks on old cluster
- `cleaning` - Deleting old cluster resources

### State Machine

```
                 ┌─────────────────────────────────────────────────┐
                 │                   stable                        │
                 │  (only active generation exists)                │
                 └─────────────────────────────────────────────────┘
                              │
                              │ config change detected
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
                 │  (cluster Service selector updated)             │
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
- Traffic is switched only after new cluster is fully Ready
- Old cluster is deleted only after all pods are drained
- At most two generations exist simultaneously (active + draining)

### 3. Per-Generation Resources

For each cluster generation, the operator creates resources with naming pattern `{configmap-name}-g{N}`. All resources have `ownerReferences` pointing to the config ConfigMap for cascading deletion.

**StatefulSet:**

Key design choices:
- `podManagementPolicy: Parallel` - All pods start simultaneously for faster cluster formation
- Labels `core-operator/cluster` and `core-operator/generation` for selector-based routing
- `ownerReferences` pointing to config ConfigMap for cascading deletion
- Config mounted from pre-generated ConfigMap with pod FQDNs

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: core-cluster-production-g2
  labels:
    core-operator/cluster: core-cluster-production
    core-operator/generation: "2"
  ownerReferences:
    - apiVersion: v1
      kind: ConfigMap
      name: core-cluster-production
spec:
  serviceName: core-cluster-production-g2-hl
  replicas: 5
  podManagementPolicy: Parallel
  selector:
    matchLabels:
      core-operator/cluster: core-cluster-production
      core-operator/generation: "2"
  # ... pod template with Core container and config volume
```

**Headless Service (for StatefulSet pod DNS):**

The headless service (`-hl` suffix) is a critical Kubernetes component required for StatefulSet pod networking:

1. **Stable DNS names**: Each pod in a StatefulSet gets a predictable DNS name based on the headless service: `{pod-name}.{headless-service}.{namespace}.svc`. For example, `core-cluster-production-g2-0.core-cluster-production-g2-hl.firebolt.svc`.

2. **Pod-to-pod communication**: Core nodes need to discover and communicate with each other during cluster formation. The headless service provides the DNS resolution that allows pods to find their peers by name.

3. **Pre-generated config**: The operator generates the config.json with all pod FQDNs before the StatefulSet is created. This is possible because the pod names and headless service name are deterministic.

4. **`publishNotReadyAddresses: true`**: This setting ensures DNS entries exist for pods even before they pass readiness checks. Without this, pods could not find each other during startup, causing cluster formation to fail.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: core-cluster-production-g2-hl
  ownerReferences:
    - apiVersion: v1
      kind: ConfigMap
      name: core-cluster-production
spec:
  clusterIP: None
  publishNotReadyAddresses: true  # Required for cluster formation
  selector:
    core-operator/cluster: core-cluster-production
    core-operator/generation: "2"
  ports:
    - port: 3473
      name: query
```

Note: The headless service is NOT used for external traffic. External traffic goes through the cluster service (`-service` suffix) which load balances across all ready pods in the active generation.

**Core Config ConfigMap (pre-generated by operator):**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: core-cluster-production-g2-config
  ownerReferences:
    - apiVersion: v1
      kind: ConfigMap
      name: core-cluster-production
data:
  config.json: |
    {
      "nodes": [
        {"host": "core-cluster-production-g2-0.core-cluster-production-g2-hl.firebolt.svc"},
        {"host": "core-cluster-production-g2-1.core-cluster-production-g2-hl.firebolt.svc"},
        {"host": "core-cluster-production-g2-2.core-cluster-production-g2-hl.firebolt.svc"},
        {"host": "core-cluster-production-g2-3.core-cluster-production-g2-hl.firebolt.svc"},
        {"host": "core-cluster-production-g2-4.core-cluster-production-g2-hl.firebolt.svc"}
      ]
    }
```

### 4. Cluster Service

Stable endpoint for clients. Named with `-service` suffix matching the config ConfigMap. Operator updates selector to switch traffic between generations.

The service uses `ClusterIP` type, which provides connection queuing at the kernel level. If all backing pods are temporarily unavailable (e.g., during the brief moment of selector switch), new connections are queued rather than immediately rejected. This improves resilience during transitions.

```yaml
apiVersion: v1
kind: Service
metadata:
  name: core-cluster-production-service  # Config name + "-service"
  ownerReferences:
    - apiVersion: v1
      kind: ConfigMap
      name: core-cluster-production
spec:
  type: ClusterIP
  selector:
    core-operator/cluster: core-cluster-production
    core-operator/generation: "2"  # Operator updates this
  ports:
    - port: 3473
      name: query
```

## Reconciliation Loop

```
┌─────────────────────────────────────────────────────────────┐
│                    Operator Watch Loop                      │
│              (per config ConfigMap found)                   │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
              ┌───────────────────────┐
              │ Read {cluster} config │
              │ Read {cluster}-status │
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
│ 1. Create g1 resources:                                  │
│    - ConfigMap {cluster}-g1-config (5 nodes config)      │
│    - Headless Service {cluster}-g1-hl                    │
│    - StatefulSet {cluster}-g1 (5 replicas)               │
│                                                             │
│    Phase: creating                                          │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 2. Wait for all 5 pods Ready                                │
│                                                             │
│    Poll pods with label core-operator/generation=1          │
│    Wait until: 5/5 Running + Ready                          │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 3. Switch traffic                                           │
│                                                             │
│    Update cluster Service selector: generation=1            │
│    New queries → g1                                      │
│    Existing connections → continue on g0                 │
│                                                             │
│    Phase: switching                                         │
└─────────────────────────────────────────────────────────────┘
        │
        ▼
┌─────────────────────────────────────────────────────────────┐
│ 4. Drain old cluster                                        │
│                                                             │
│    For each pod in g0:                                   │
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
│ 5. Delete old cluster                                       │
│                                                             │
│    Delete StatefulSet {cluster}-g0                       │
│    Delete Service {cluster}-g0-hl                        │
│    Delete ConfigMap {cluster}-g0-config                  │
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

When the operator starts a transition, it **snapshots** the target configuration into the status ConfigMap (`lastAppliedConfig`). This snapshot is used for the entire transition, regardless of subsequent changes to the config ConfigMap.

### Behavior During Transitions

```
Current state:
  - g0 active (3 nodes)
  - phase: stable
  - lastAppliedConfig: {replicas: 3, ...}

User changes: replicas 3 → 5

Operator logic:
  1. Detect change (live config differs from lastAppliedConfig)
  2. Snapshot new config: lastAppliedConfig = {replicas: 5, ...}
  3. Start transition: phase = creating, create g1 resources with 5 replicas
  4. Continue transition using snapshotted config
```

### Handling Changes During Transition

If the config changes while a transition is in progress, the new config is saved as `pendingMutation`. Only **one** pending mutation is kept (the most recent).

```
Current state:
  - g0 active (3 nodes)
  - g1 creating (5 nodes)
  - phase: creating
  - lastAppliedConfig: {replicas: 5, ...}  (snapshotted)

User changes: replicas 5 → 7

Operator logic:
  1. Detect change (live config differs from lastAppliedConfig)
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
- Operator can resume correctly after restart (state is persisted in status ConfigMap)

## Traffic Flow

```
Client
   │
   ▼
┌──────────────────────────────────────────────┐
│            Cluster Service                   │
│    core-cluster-production-service           │
│    selector: core-operator/generation=1      │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
┌──────────────────────────────────────────────┐
│    StatefulSet core-cluster-production-g1 │
│                                              │
│  ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐ ┌─────┐     │
│  │pod-0│ │pod-1│ │pod-2│ │pod-3│ │pod-4│     │
│  └─────┘ └─────┘ └─────┘ └─────┘ └─────┘     │
└──────────────────────────────────────────────┘

During transition, g0 continues serving existing connections
until drain check confirms 0 active queries on all pods.
```

## Operator RBAC

The operator runs in a single namespace and requires only namespace-scoped permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: core-operator
  namespace: firebolt
rules:
  - apiGroups: [""]
    resources: ["configmaps"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: [""]
    resources: ["services"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: ["apps"]
    resources: ["statefulsets"]
    verbs: ["get", "list", "watch", "create", "update", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]  # For drain check
```

## Operator Deployment

### Command-Line Arguments

| Argument | Default | Description |
|----------|---------|-------------|
| `--config-prefix` | `core-cluster` | Prefix for ConfigMaps the operator watches |
| `--namespace` | (required) | Namespace to operate in |

### Startup Behavior

On startup, the operator:
1. Lists all ConfigMaps matching the prefix in the namespace
2. For each config ConfigMap, checks for existing resources with matching `ownerReferences`
3. If resources exist with correct ownership: adopt and resume management
4. If resources exist without ownership or with different ownership: refuse to manage, log error
5. If no resources exist: initialize from scratch (generation 0)

### Multi-Cluster Support

The operator can manage multiple independent Core clusters in the same namespace. Each cluster is defined by a separate ConfigMap matching the prefix:

```
core-cluster-production   → manages core-cluster-production-* resources
core-cluster-staging      → manages core-cluster-staging-* resources
core-cluster-dev          → manages core-cluster-dev-* resources
```

Reconciliation for each cluster is independent.

## Configuration Reference

For the complete list of configurable fields in the cluster definition ConfigMap, see the [README](../README.md#configuration-reference).

### Status ConfigMap (operator-managed)

| Field | Type | Description |
|-------|------|-------------|
| `currentGeneration` | int | Latest generation number (starts at 0) |
| `activeGeneration` | int | Generation currently receiving traffic |
| `drainingGeneration` | int/null | Generation being drained (if any) |
| `phase` | string | Current phase (stable/creating/switching/draining/cleaning) |
| `lastReconciled` | string | Timestamp of last reconciliation |
| `pendingMutation` | object/null | Queued mutation waiting to be processed |

## Concurrency and Race Conditions

The operator uses Kubernetes optimistic concurrency control (ResourceVersion) to handle concurrent reconciles safely:

| Scenario | Behavior |
|----------|----------|
| Two reconciles read same status | Second update fails with conflict error, controller-runtime requeues |
| Resource created between Get and Create | Create returns AlreadyExists, requeue handles it |
| Rapid config changes during transition | Only the latest change is kept as PendingMutation |
| Operator crash mid-transition | Restarts and resumes from persisted phase in status ConfigMap |

**Key principle:** All state is persisted in the status ConfigMap before taking action. If an update fails due to conflict, the reconcile is retried with fresh state.

## Failure Handling

| Scenario | Behavior |
|----------|----------|
| New cluster fails to become Ready | Transition stalls in `creating` phase. Old cluster continues serving. Manual intervention required. |
| Drain check never succeeds | Transition stalls in `draining` phase. New cluster is active. Old cluster remains. Manual intervention required. |
| Operator restarts mid-transition | Operator reads status ConfigMap, resumes from current phase. |
| ConfigMap deleted | Operator logs error, takes no action. Existing clusters remain. |

## Limitations

1. **No automatic rollback** - If new cluster fails, operator does not revert. Traffic stays on old cluster.
2. **Single namespace** - Operator manages resources in one namespace.
3. **No PVC support** - Designed for ephemeral clusters only.

## Design Decisions

### Why StatefulSets instead of Deployments or raw Pods

StatefulSets are used despite the operator pre-generating pod FQDNs because StatefulSets automatically recreate failed pods. Deployments would work for FQDN generation but use ReplicaSets which don't provide stable pod identities needed for config generation. Raw pods managed directly by the operator would require reimplementing pod health monitoring and recreation logic that StatefulSets provide out of the box.

