# Firebolt Core Operator

A Kubernetes controller that manages Firebolt Core clusters with zero-downtime scaling via blue-green deployments.

## Overview

The operator watches ConfigMaps with a configurable prefix and automatically manages the lifecycle of Core clusters. When you change the configuration (e.g., scale from 3 to 5 nodes), the operator:

1. Creates a new cluster with the desired configuration
2. Waits for all pods to become ready
3. Switches traffic to the new cluster
4. Drains the old cluster (waits for running queries to complete)
5. Deletes the old cluster

All of this happens automatically with zero downtime for clients.

## Quick Start

### 1. Deploy the Operator

```bash
# Build and push the operator image
make docker-build docker-push IMG=<your-registry>/core-operator:latest

# Deploy to your cluster
make deploy IMG=<your-registry>/core-operator:latest
```

### 2. Create a Cluster

Create a ConfigMap that defines your cluster. The name must start with the configured prefix (default: `core-cluster`):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: core-cluster-production
  namespace: firebolt
data:
  replicas: "3"
  image: "ghcr.io/firebolt-db/firebolt-core"
  tag: "v1.2.0"
  cpu: "2"
  memory: "8Gi"
```

```bash
kubectl apply -f my-cluster.yaml
```

The operator will create a 3-node cluster. Connect to it via the cluster service:
```
core-cluster-production-service.firebolt.svc:3473
```

### 3. Scale or Update

Simply update the ConfigMap:

```bash
kubectl patch configmap core-cluster-production -n firebolt \
  --type merge -p '{"data":{"replicas":"5"}}'
```

The operator handles the zero-downtime transition automatically.

### 4. Delete a Cluster

Delete the ConfigMap:

```bash
kubectl delete configmap core-cluster-production -n firebolt
```

All associated resources (StatefulSets, Services, etc.) are automatically deleted via Kubernetes owner references.

## ConfigMap Prefix

The operator only watches ConfigMaps whose names start with a specific prefix. This is configured via the `--config-prefix` flag (default: `core-cluster`).

**How it works:**
- ConfigMap `core-cluster-production` → **managed** (starts with `core-cluster`)
- ConfigMap `core-cluster-staging` → **managed** (starts with `core-cluster`)
- ConfigMap `my-app-config` → **ignored** (does not start with `core-cluster`)

**Multiple clusters:** You can run multiple independent clusters by creating multiple ConfigMaps with different names:

```
core-cluster-production   → creates core-cluster-production-service
core-cluster-staging      → creates core-cluster-staging-service  
core-cluster-dev          → creates core-cluster-dev-service
```

Each cluster is managed independently with its own transition lifecycle.

## Configuration Reference

### Cluster Definition ConfigMap

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `replicas` | **Yes** | - | Number of nodes (must be ≥ 1) |
| `image` | **Yes** | - | Container image repository (must not be empty) |
| `tag` | **Yes** | - | Container image tag (must not be empty) |
| `cpu` | **Yes** | - | CPU request and limit (Kubernetes format, e.g., `"2"`, `"500m"`) |
| `memory` | **Yes** | - | Memory request and limit (Kubernetes format, e.g., `"8Gi"`, `"4096Mi"`) |
| `imagePullPolicy` | No | `IfNotPresent` | Image pull policy: `Always`, `Never`, or `IfNotPresent` |
| `drainCheckInterval` | No | `5s` | How often to check if old pods have finished serving queries |
| `rollout` | No | `graceful` | Rollout strategy: `graceful` waits for drain, `recreate` deletes immediately |
| `nodeSelector` | No | - | YAML-encoded node selector (see example below) |
| `tolerations` | No | - | YAML-encoded tolerations array (see example below) |

### Full Example

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: core-cluster-production
  namespace: firebolt
data:
  replicas: "5"
  image: "ghcr.io/firebolt-db/firebolt-core"
  tag: "v1.2.0"
  cpu: "4"
  memory: "16Gi"
  imagePullPolicy: "IfNotPresent"
  drainCheckInterval: "10s"
  rollout: "graceful"
  nodeSelector: |
    dedicated: firebolt-nodes
    zone: us-east-1a
  tolerations: |
    - key: dedicated
      operator: Equal
      value: firebolt-nodes
      effect: NoSchedule
```

## Operator-Managed Resources

**Do not modify these resources manually.** The operator creates and manages them automatically.

For a cluster defined by ConfigMap `core-cluster-production`, the operator creates:

| Resource | Name Pattern | Purpose |
|----------|--------------|---------|
| **Status ConfigMap** | `core-cluster-production-status` | Operator's internal state |
| **Cluster Service** | `core-cluster-production-service` | Stable endpoint for clients |
| **StatefulSet** | `core-cluster-production-g{N}` | Pods for generation N |
| **Headless Service** | `core-cluster-production-g{N}-hl` | Pod DNS for generation N |
| **Config ConfigMap** | `core-cluster-production-g{N}-config` | Core config for generation N |

### Important Rules

1. **Never edit the status ConfigMap** - It contains the operator's state. Modifying it can cause undefined behavior.

2. **Never edit operator-created StatefulSets or Services** - The operator assumes full control. Manual changes will be overwritten or cause conflicts.

3. **To delete a cluster, delete the definition ConfigMap** - All other resources will be garbage collected automatically.

4. **To modify a cluster, edit only the definition ConfigMap** - The operator handles everything else.

## Supported Operations

| Operation | How to Do It | What Happens |
|-----------|--------------|--------------|
| **Create cluster** | Create ConfigMap with prefix | Operator creates generation 0 |
| **Scale up/down** | Update `replicas` in ConfigMap | Zero-downtime blue-green transition |
| **Change image** | Update `image` or `tag` | Zero-downtime blue-green transition |
| **Change resources** | Update `cpu` or `memory` | Zero-downtime blue-green transition |
| **Delete cluster** | Delete ConfigMap | All resources garbage collected |

Any change to the ConfigMap triggers a new generation. The operator:
1. Creates new resources with the updated config
2. Waits for them to be ready
3. Switches traffic
4. Drains and deletes the old resources

## Rollout Strategies

### Graceful (default)

```yaml
data:
  rollout: "graceful"
```

- New cluster is created and must become fully ready
- Traffic is switched to the new cluster
- Old cluster is drained (operator waits for running queries to complete)
- Old cluster is deleted only after drain completes

Use this for production workloads where you want zero query interruption.

### Recreate

```yaml
data:
  rollout: "recreate"
```

- New cluster is created and must become fully ready
- Traffic is switched to the new cluster
- Old cluster is **immediately deleted** (no drain check)

Use this for development/testing or when you need faster transitions and can tolerate interrupted queries.

## Operator Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--config-prefix` | `core-cluster` | Prefix for ConfigMaps to watch |
| `--namespace` | (required) | Namespace to operate in |
| `--metrics-bind-address` | `:8080` | Address for metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Address for health probes |
| `--leader-elect` | `false` | Enable leader election for HA |

## Monitoring

### Cluster Status

Check the status ConfigMap to see the current state:

```bash
kubectl get configmap core-cluster-production-status -n firebolt -o jsonpath='{.data.state}' | jq
```

Output:
```json
{
  "currentGeneration": 2,
  "activeGeneration": 2,
  "drainingGeneration": null,
  "phase": "stable",
  "lastReconciled": "2024-01-15T10:00:00Z"
}
```

### Phases

| Phase | Meaning |
|-------|---------|
| `stable` | Cluster is running normally, no transition in progress |
| `creating` | New generation is being created, waiting for pods to be ready |
| `switching` | Traffic is being switched to the new generation |
| `draining` | Waiting for old generation pods to finish serving queries |
| `cleaning` | Deleting old generation resources |

## Troubleshooting

### Cluster stuck in "creating" phase

Pods in the new generation are not becoming ready. Check:
```bash
kubectl get pods -l core-operator/cluster=core-cluster-production -n firebolt
kubectl describe pod <pod-name> -n firebolt
kubectl logs <pod-name> -n firebolt
```

### Cluster stuck in "draining" phase

Old pods still have running queries. This is normal for long-running queries. If you need to force the transition:

1. Change to `rollout: recreate` in the ConfigMap, or
2. Manually delete the old StatefulSet (not recommended)

### ConfigMap changes not being picked up

Ensure the ConfigMap name starts with the configured prefix:
```bash
# Check operator logs
kubectl logs -l app=core-operator -n firebolt
```

## Design Documentation

For detailed architecture and design decisions, see [docs/operator-based-scaling.md](docs/operator-based-scaling.md).

## License

Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
