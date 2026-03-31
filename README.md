# Firebolt Core Operator

A Kubernetes controller that manages Firebolt engines with zero-downtime scaling via blue-green deployments.

## Overview

The operator watches `FireboltEngine` custom resources and automatically manages the lifecycle of Firebolt engines. When you change the configuration (e.g., scale from 3 to 5 nodes), the operator:

1. Creates a new engine generation with the desired configuration
2. Waits for all pods to become ready
3. Switches traffic to the new generation
4. Drains the old generation (waits for running queries to complete)
5. Deletes the old generation

All of this happens automatically with zero downtime for clients.

## Quick Start

### 1. Deploy the Operator

```bash
# Build and push the operator image
make docker-build docker-push IMG=<your-registry>/core-operator:latest

# Deploy to your cluster (includes CRD installation)
make deploy IMG=<your-registry>/core-operator:latest
```

### 2. Create a Firebolt Engine

Create a `FireboltEngine` resource:

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngine
metadata:
  name: core-engine-production
  namespace: firebolt
spec:
  replicas: 3
  image:
    repository: "ghcr.io/firebolt-db/firebolt-core"
    tag: "v1.2.0"
  resources:
    cpu: "2"
    memory: "8Gi"
```

```bash
kubectl apply -f my-engine.yaml
```

The operator will create a 3-node engine. Connect to it via the engine service:
```
core-engine-production-service.firebolt.svc:3473
```

### 3. Scale or Update

Simply update the `FireboltEngine` resource:

```bash
kubectl patch fireboltengine core-engine-production -n firebolt \
  --type merge -p '{"spec":{"replicas":5}}'
```

Or use the short name:

```bash
kubectl patch fire core-engine-production -n firebolt \
  --type merge -p '{"spec":{"replicas":5}}'
```

The operator handles the zero-downtime transition automatically.

### 4. Delete an Engine

Delete the `FireboltEngine` resource:

```bash
kubectl delete fireboltengine core-engine-production -n firebolt
```

All associated resources (StatefulSets, Services, etc.) are automatically deleted via Kubernetes owner references.

## Configuration Reference

### FireboltEngine Spec

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `spec.replicas` | **Yes** | - | Number of nodes (must be ≥ 1) |
| `spec.image.repository` | **Yes** | - | Container image repository |
| `spec.image.tag` | **Yes** | - | Container image tag |
| `spec.image.pullPolicy` | No | `IfNotPresent` | Image pull policy: `Always`, `Never`, or `IfNotPresent` |
| `spec.resources.cpu` | **Yes** | - | CPU request and limit (Kubernetes quantity, e.g., `"2"`, `"500m"`) |
| `spec.resources.memory` | **Yes** | - | Memory request and limit (Kubernetes quantity, e.g., `"8Gi"`, `"4096Mi"`) |
| `spec.drainCheckInterval` | No | `5s` | How often to check if old pods have finished serving queries |
| `spec.rollout` | No | `graceful` | Rollout strategy: `graceful` waits for drain, `recreate` deletes immediately |
| `spec.nodeSelector` | No | - | Node selector map |
| `spec.tolerations` | No | - | Tolerations array |
| `spec.metadataService.image.repository` | No | *(derived from engine image)* | Metadata service image repository |
| `spec.metadataService.image.tag` | No | *(derived from engine image)* | Metadata service image tag |
| `spec.metadataService.image.pullPolicy` | No | `IfNotPresent` | Metadata service image pull policy |

### Full Example

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngine
metadata:
  name: core-engine-production
  namespace: firebolt
spec:
  replicas: 5
  image:
    repository: "ghcr.io/firebolt-db/firebolt-core"
    tag: "v1.2.0"
    pullPolicy: IfNotPresent
  resources:
    cpu: "4"
    memory: "16Gi"
  drainCheckInterval: "10s"
  rollout: graceful
  metadataService:
    image:
      repository: "ghcr.io/firebolt-db/dedicated-pensieve"
      tag: "v1.2.0"
  nodeSelector:
    dedicated: firebolt-nodes
    zone: us-east-1a
  tolerations:
    - key: dedicated
      operator: Equal
      value: firebolt-nodes
      effect: NoSchedule
```

## Metadata Service

Each engine automatically gets a **metadata service** that provides cluster coordination. The operator deploys and manages it without any manual intervention.

The metadata service consists of:
- A lightweight PostgreSQL instance (1 Gi persistent storage) for metadata storage
- A metadata server that provides the coordination endpoint to engine nodes

The engine nodes are automatically configured to connect to the metadata service via `config.multi_cluster_endpoint`.

### Image Configuration

By default, the metadata service image is derived from the engine image (same registry prefix, `dedicated-pensieve` repository, same tag). You can override it:

```yaml
spec:
  metadataService:
    image:
      repository: "my-registry/dedicated-pensieve"
      tag: "v1.2.0"
```

## Operator-Managed Resources

**Do not modify these resources manually.** The operator creates and manages them automatically.

For an engine named `core-engine-production`, the operator creates:

### Per-Engine Resources (shared across generations)

| Resource | Name Pattern | Purpose |
|----------|--------------|---------|
| **Metadata Service** | `core-engine-production-metadata` | Metadata coordination endpoint |
| **Metadata ConfigMap** | `core-engine-production-metadata` | Metadata service configuration |
| **PostgreSQL Deployment** | `core-engine-production-metadata-pg` | Metadata database |
| **PostgreSQL Service** | `core-engine-production-metadata-pg` | Database endpoint |
| **PostgreSQL PVC** | `core-engine-production-metadata-pg` | Persistent database storage |
| **PostgreSQL Credentials** | `core-engine-production-metadata-pg-creds` | Database credentials |

### Per-Generation Resources (created during blue-green transitions)

| Resource | Name Pattern | Purpose |
|----------|--------------|---------|
| **Engine Service** | `core-engine-production-service` | Stable endpoint for clients |
| **StatefulSet** | `core-engine-production-g{N}` | Pods for generation N |
| **Headless Service** | `core-engine-production-g{N}-hl` | Pod DNS for generation N |
| **Config ConfigMap** | `core-engine-production-g{N}-config` | Core config for generation N |

### Important Rules

1. **Never edit operator-created resources** - The operator assumes full control. Manual changes will be overwritten or cause conflicts.

2. **To delete an engine, delete the `FireboltEngine` resource** - All other resources (including metadata service and PostgreSQL) will be garbage collected automatically.

3. **To modify an engine, edit only the `FireboltEngine` resource** - The operator handles everything else.

## Supported Operations

| Operation | How to Do It | What Happens |
|-----------|--------------|--------------|
| **Create engine** | Create `FireboltEngine` resource | Operator creates generation 0 |
| **Scale up/down** | Update `spec.replicas` | Zero-downtime blue-green transition |
| **Change image** | Update `spec.image` | Zero-downtime blue-green transition |
| **Change resources** | Update `spec.resources` | Zero-downtime blue-green transition |
| **Delete engine** | Delete `FireboltEngine` resource | All resources garbage collected |

Any change to the spec triggers a new generation. The operator:
1. Creates new resources with the updated config
2. Waits for them to be ready
3. Switches traffic
4. Drains and deletes the old resources

## Rollout Strategies

### Graceful (default)

```yaml
spec:
  rollout: "graceful"
```

- New generation is created and must become fully ready
- Traffic is switched to the new generation
- Old generation is drained (operator waits for running queries to complete)
- Old generation is deleted only after drain completes

Use this for production workloads where you want zero query interruption.

### Recreate

```yaml
spec:
  rollout: "recreate"
```

- New generation is created and must become fully ready
- Traffic is switched to the new generation
- Old generation is **immediately deleted** (no drain check)

Use this for development/testing or when you need faster transitions and can tolerate interrupted queries.

## Operator Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--namespace` | (optional) | Namespace to watch. Watches all namespaces if empty. |
| `--metrics-bind-address` | `0` | Address for metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Address for health probes |
| `--leader-elect` | `false` | Enable leader election for HA |

## Monitoring

### Engine Status

Check the engine status to see the current state:

```bash
kubectl get fireboltengine core-engine-production -n firebolt -o yaml
```

Or use short names:

```bash
kubectl get fire -n firebolt
```

Output:
```
NAME                      REPLICAS   PHASE    GENERATION   AGE
core-engine-production    5          stable   2            24h
```

### Phases

| Phase | Meaning |
|-------|---------|
| `stable` | Engine is running normally, no transition in progress |
| `creating` | New generation is being created, waiting for pods to be ready |
| `switching` | Traffic is being switched to the new generation |
| `draining` | Waiting for old generation pods to finish serving queries |
| `cleaning` | Deleting old generation resources |

## Troubleshooting

### Engine stuck in "creating" phase

Pods in the new generation are not becoming ready. Check:
```bash
kubectl get pods -l firebolt.io/engine=core-engine-production -n firebolt
kubectl describe pod <pod-name> -n firebolt
kubectl logs <pod-name> -n firebolt
```

### Engine stuck in "draining" phase

Old pods still have running queries. This is normal for long-running queries. If you need to force the transition:

1. Change to `rollout: recreate` in the engine spec, or
2. Manually delete the old StatefulSet (not recommended)

### Engine changes not being picked up

Check operator logs:
```bash
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
