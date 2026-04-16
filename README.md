# Firebolt Engine Operator

A Kubernetes operator that manages Firebolt infrastructure: metadata services, gateways, and compute engines with zero-downtime scaling via blue-green deployments.

## Overview

The operator manages two custom resources:

- **FireboltInstance** provisions the shared infrastructure that engines depend on: PostgreSQL, the metadata service, the gateway, and account initialization.
- **FireboltEngine** deploys stateful compute nodes. Each engine references a `FireboltInstance` and cannot operate without one.

When you change an engine's configuration (e.g., scale from 3 to 5 nodes), the operator performs a zero-downtime blue-green transition: it creates a new generation, waits for readiness, switches traffic, drains the old generation, and deletes it.

## Quick Start

### 1. Deploy the Operator

```bash
make docker-build docker-push IMG=<your-registry>/firebolt-kubernetes-operator:latest
make deploy IMG=<your-registry>/firebolt-kubernetes-operator:latest
```

### 2. Create a FireboltInstance

An instance provisions the metadata infrastructure. Create one per namespace:

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltInstance
metadata:
  name: production
  namespace: firebolt
spec:
  metadata: {}
  gateway: {}
```

This creates an internal PostgreSQL database, deploys the metadata service, initializes an account, and deploys the gateway. Check progress with:

```bash
kubectl get fi -n firebolt
```

```
NAME         PHASE   GATEWAY   METADATA   ENGINES   READY   AGE
production   Ready   true      true       0         0       2m
```

### 3. Create a FireboltEngine

Once the instance is `Ready`, create an engine that references it:

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngine
metadata:
  name: my-engine
  namespace: firebolt
spec:
  instanceRef: production
  replicas: 3
  image:
    repository: "ghcr.io/firebolt-db/firebolt-core"
    tag: "v1.2.0"
  resources:
    cpu: "2"
    memory: "8Gi"
```

The engine will not start until the referenced instance has a populated metadata endpoint and account ID. Connect via:

```
my-engine-service.firebolt.svc:3473
```

### 4. Scale or Update

```bash
kubectl patch fire my-engine -n firebolt \
  --type merge -p '{"spec":{"replicas":5}}'
```

The operator handles the zero-downtime transition automatically.

### 5. Delete

```bash
kubectl delete fire my-engine -n firebolt
kubectl delete fi production -n firebolt
```

All associated resources are cleaned up automatically.

---

## FireboltInstance

### Spec Reference

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `spec.metadata` | **Yes** | - | Metadata service configuration (can be empty `{}` for defaults) |
| `spec.metadata.postgres` | No | (internal) | External PostgreSQL connection. If omitted, the operator deploys an internal PostgreSQL StatefulSet |
| `spec.metadata.postgres.host` | Yes* | - | PostgreSQL hostname |
| `spec.metadata.postgres.port` | No | `5432` | PostgreSQL port |
| `spec.metadata.postgres.database` | Yes* | - | Database name |
| `spec.metadata.postgres.credentialsSecretRef.name` | Yes* | - | Secret with `username` and `password` keys |
| `spec.metadata.image` | No | (operator default) | Override the metadata service container image |
| `spec.metadata.replicas` | No | `1` | Number of metadata service pods (only `1` is currently supported) |
| `spec.metadata.resources` | No | (operator default) | CPU/memory for metadata service pods |
| `spec.metadata.nodeSelector` | No | - | Node selector for metadata service pods |
| `spec.gateway` | **Yes** | - | Gateway configuration (can be empty `{}` for defaults) |
| `spec.gateway.image` | No | (operator default) | Override the gateway container image |
| `spec.gateway.replicas` | No | `2` | Number of gateway pods |
| `spec.gateway.resources` | No | (operator default) | CPU/memory for gateway pods |
| `spec.gateway.nodeSelector` | No | - | Node selector for gateway pods |
| `spec.auth` | No | disabled | Authentication configuration |
| `spec.auth.mode` | Yes* | - | `disabled`, `native`, or `openid` |
| `spec.auth.oidc` | Yes* | - | OIDC config (required when mode is `openid`) |

\* Required when the parent field is set.

### Instance Phases

| Phase | Meaning |
|-------|---------|
| `Provisioning` | Components are being deployed; not yet ready |
| `Ready` | Metadata service and gateway are healthy |
| `Degraded` | Was previously Ready, but one or more components became unhealthy |
| `Failed` | Terminal error requiring manual intervention (e.g., multiple accounts found in metadata) |

### Monitoring

```bash
kubectl get fi -n firebolt
```

```
NAME         PHASE   GATEWAY   METADATA   ENGINES   READY   AGE
production   Ready   true      true       2         2       24h
```

Inspect details:

```bash
kubectl get fi production -n firebolt -o yaml
```

Key status fields: `phase`, `metadataReady`, `gatewayReady`, `metadataEndpoint`, `gatewayEndpoint`, `accountId`.

### Full Example

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltInstance
metadata:
  name: production
  namespace: firebolt
spec:
  metadata:
    postgres:
      host: "postgres.firebolt.svc"
      port: 5432
      database: "firebolt_metadata"
      credentialsSecretRef:
        name: metadata-postgres-credentials
    image:
      repository: "ghcr.io/firebolt-analytics/dedicated-pensieve"
      tag: "1.0.0"
    replicas: 1
    resources:
      requests:
        cpu: "200m"
        memory: "1Gi"
      limits:
        memory: "2Gi"
    nodeSelector:
      firebolt.dev/pool: system

  gateway:
    replicas: 3
    image:
      repository: "ghcr.io/firebolt-analytics/core-gateway"
      tag: "1.0.0"
    resources:
      requests:
        cpu: "100m"
        memory: "256Mi"
    nodeSelector:
      firebolt.dev/pool: system

  auth:
    mode: openid
    oidc:
      issuerURL: "https://company.okta.com/oauth2/default"
      clientID: "firebolt"
      clientSecretRef:
        name: oidc-client-secret
      claimMappings:
        username: "email"
```

---

## FireboltEngine

### Spec Reference

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `spec.instanceRef` | **Yes** | - | Name of the `FireboltInstance` in the same namespace |
| `spec.replicas` | **Yes** | - | Number of engine nodes (must be >= 1) |
| `spec.image.repository` | **Yes** | - | Container image repository |
| `spec.image.tag` | **Yes** | - | Container image tag |
| `spec.image.pullPolicy` | No | `IfNotPresent` | Image pull policy |
| `spec.resources.cpu` | **Yes** | - | CPU request and limit (e.g., `"2"`, `"500m"`) |
| `spec.resources.memory` | **Yes** | - | Memory request and limit (e.g., `"8Gi"`) |
| `spec.rollout` | No | `graceful` | `graceful` waits for drain; `recreate` deletes immediately |
| `spec.drainCheckEnabled` | No | `true` | Set to `false` to skip the SQL drain check |
| `spec.drainCheckInterval` | No | `5s` | How often to poll old pods for drain status |
| `spec.nodeSelector` | No | - | Node selector for engine pods |
| `spec.tolerations` | No | - | Tolerations for engine pods |
| `spec.metadataEndpointOverride` | No | - | Override the instance-derived metadata endpoint (for cross-cluster setups) |

### Engine Phases

| Phase | Meaning |
|-------|---------|
| `stable` | All resources match spec; no transition in progress |
| `creating` | New generation being created; waiting for pods to be ready |
| `switching` | Traffic being switched to the new generation |
| `draining` | Waiting for old generation pods to finish serving queries |
| `cleaning` | Deleting old generation resources |

### Conditions

| Condition | Meaning |
|-----------|---------|
| `InstanceReady=True` | Referenced `FireboltInstance` is ready and providing metadata |
| `InstanceReady=False` | Instance is missing, not ready, or lacks metadata endpoint / account ID |

### Monitoring

```bash
kubectl get fire -n firebolt
```

```
NAME        REPLICAS   PHASE    GENERATION   AGE
my-engine   5          stable   2            24h
```

### Operator-Managed Resources

**Do not modify these resources manually.** For an engine named `my-engine`:

| Resource | Name Pattern | Purpose |
|----------|--------------|---------|
| **Engine Service** | `my-engine-service` | Stable endpoint for clients |
| **StatefulSet** | `my-engine-g{N}` | Pods for generation N |
| **Headless Service** | `my-engine-g{N}-hl` | Pod DNS for generation N |
| **Config ConfigMap** | `my-engine-g{N}-config` | Engine config for generation N |

### Rollout Strategies

**Graceful (default):** new generation is created, traffic is switched, old generation is drained (waits for running queries to complete), then deleted. Use for production.

**Recreate:** new generation is created, traffic is switched, old generation is immediately deleted. Use for dev/test or when you can tolerate interrupted queries.

### Full Example

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngine
metadata:
  name: my-engine
  namespace: firebolt
spec:
  instanceRef: production
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
  nodeSelector:
    dedicated: firebolt-nodes
  tolerations:
    - key: dedicated
      operator: Equal
      value: firebolt-nodes
      effect: NoSchedule
```

---

## Operator Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--namespace` | (all) | Namespace to watch. Watches all namespaces if empty |
| `--metrics-bind-address` | `0` | Address for the metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Address for health probes |
| `--leader-elect` | `false` | Enable leader election for HA deployments |

## Troubleshooting

### Engine stuck with `InstanceReady=False`

The referenced instance is not ready. Check instance status:

```bash
kubectl get fi -n firebolt
kubectl describe fi <instance-name> -n firebolt
```

Common causes: instance still provisioning, metadata service pods not ready, account initialization failed.

### Instance stuck in "Provisioning"

Components are not becoming healthy. Check the underlying resources:

```bash
kubectl get pods -l firebolt.io/instance=<instance-name> -n firebolt
kubectl logs -l firebolt.io/component=metadata -n firebolt
```

### Instance in "Failed" phase

A terminal error occurred (e.g., multiple accounts found in the metadata service). Inspect the operator logs for details and resolve the underlying issue manually. The operator will not automatically recover from this state.

### Engine stuck in "creating" phase

Pods in the new generation are not becoming ready:

```bash
kubectl get pods -l firebolt.io/engine=<engine-name> -n firebolt
kubectl describe pod <pod-name> -n firebolt
kubectl logs <pod-name> -n firebolt
```

### Engine stuck in "draining" phase

Old pods still have running queries. This is normal for long-running queries. To force the transition, set `rollout: recreate` in the engine spec.

## Development

### Running Tests

**Unit tests** (no cluster required):

```bash
make test
```

This runs the controller unit tests and envtest suite, excluding the `test/e2e/` directory.

**E2E tests** (requires a Kind cluster):

First-time setup — create a Kind cluster and load the required container images:

```bash
make prepare-test-e2e
```

Then run the tests (the operator starts in-process, no deployment needed):

```bash
make test-e2e
```

On subsequent runs, only `make test-e2e` is needed — it reuses the existing cluster. Re-run `make prepare-test-e2e` if you change the test images in `test/e2e/defaults.env`.

To run a subset of tests:

```bash
make test-e2e GINKGO_FOCUS="Single Node Engine"
```

The e2e suite uses the `e2e` build tag, which also activates crash-point injection in the controller for crash-recovery testing.

**Heavy E2E tests** (stress / large query workloads):

```bash
go test -tags=e2e,heavy ./test/e2e/ -v -timeout 30m
```

The `heavy` tag swaps in a stress-oriented query configuration. Use this for validating behavior under sustained load. The Kind cluster and images must already be prepared (`make prepare-test-e2e`).

### Build Tags

| Tag | Effect |
|-----|--------|
| *(none)* | Unit tests only. `crash_points.go` compiles crash points as no-ops |
| `e2e` | Enables all e2e test files. Activates `crash_points_e2e.go` with real crash injection |
| `e2e,heavy` | Same as `e2e`, but uses the heavy query configuration instead of the light one |

### Linting

```bash
make lint
```

Linter configuration is in `.golangci.yml`.

## Design Documentation

For detailed architecture and design decisions, see [docs/level-driven-reconciliation.md](docs/level-driven-reconciliation.md).

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
