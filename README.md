# Firebolt Kubernetes Operator

A Kubernetes operator that manages Firebolt infrastructure: metadata services, an Envoy query-routing proxy, and compute engines with zero-downtime scaling via blue-green deployments.

## Overview

The operator manages three custom resources:

- **FireboltInstance** provisions the shared infrastructure that engines depend on: PostgreSQL, the metadata service, and an Envoy gateway proxy.
- **FireboltEngine** deploys stateful compute nodes. Each engine references a `FireboltInstance` and cannot operate without one.
- **EngineClass** *(optional, namespaced)* holds a reusable pod-template fragment that multiple engines in the same namespace can share via `spec.engineClassRef` — service account / IAM binding, scheduling, sidecars, and the engine container image. Namespaced (not cluster-scoped) because the template carries namespace-resolved identifiers like ServiceAccount names and Secret/PVC volume references.

When you change an engine's configuration (e.g., scale from 3 to 5 nodes), the operator performs a zero-downtime blue-green transition: it creates a new generation, waits for readiness, switches traffic, drains the old generation, and deletes it. Editing the referenced `EngineClass` triggers the same blue-green flow on every consumer engine.

## Prerequisites

- **Kubernetes 1.28+** -- the CRDs use CEL transition rules (`oldSelf`) for field immutability.

## Quick start

### Deploy the operator

**Production:**

```bash
helm upgrade --install firebolt-crds oci://ghcr.io/firebolt-db/helm-charts/firebolt-operator-crds
helm upgrade --install firebolt-operator oci://ghcr.io/firebolt-db/helm-charts/kubernetes-operator
```

**Local development (Kind):**

```bash
make prepare-test-e2e   # one-time: creates Kind cluster + publishes test images
make local-deploy       # builds operator, loads into Kind, deploys via Helm
```

`make prepare-test-e2e` starts a local Docker registry container (`kind-registry` on `127.0.0.1:5001`) and configures every kind node to mirror `ghcr.io` and `docker.io` through it. Workload images are pushed to that registry once and pulled on demand by each node, so the multi-GB engine image is no longer duplicated per kind node. To recreate the registry from scratch (e.g. after a stale push), run `make flush-local-registry`.

### Create a FireboltInstance

An instance provisions the metadata infrastructure. See the [FireboltInstance CRD Reference](docs/instance-crd-reference.md) for all fields, phases, gateway sizing, and examples.

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

Check progress:

```bash
kubectl get fi -n firebolt
```

### Create a FireboltEngine

Once the instance is `Ready`, create an engine that references it. The engine container image lives on an `EngineClass` rather than on the engine itself (FB-1145), so the minimal viable engine ships in two manifests — a class with the image, and an engine referencing the class. See the [FireboltEngine CRD Reference](docs/engine-crd-reference.md) and the [EngineClass CRD Reference](docs/engineclass-crd-reference.md) for the full field surface.

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: EngineClass
metadata:
  name: default
  namespace: firebolt
spec:
  template:
    spec:
      containers:
        - name: engine
          image: ghcr.io/firebolt-db/engine:dev
---
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngine
metadata:
  name: my-engine
  namespace: firebolt
spec:
  instanceRef: production
  engineClassRef: default
  replicas: 3
  resources:
    requests:
      cpu: "2"
      memory: "8Gi"
    limits:
      cpu: "2"
      memory: "8Gi"
```

### Scale or update

```bash
kubectl patch fireng my-engine -n firebolt \
  --type merge -p '{"spec":{"replicas":5}}'
```

The operator handles the zero-downtime transition automatically.

### Stop and resume

Set `spec.replicas` to `0` to stop the engine without deleting the CR:

```bash
kubectl patch fireng my-engine -n firebolt \
  --type merge -p '{"spec":{"replicas":0}}'
```

Resume by setting a non-zero replica count:

```bash
kubectl patch fireng my-engine -n firebolt \
  --type merge -p '{"spec":{"replicas":3}}'
```

### Connecting to engines

**Through the instance gateway (recommended):** the Envoy proxy routes requests based on the `X-Firebolt-Engine` header and handles retries during blue-green transitions.

```
POST http://<instance-name>-gateway.<namespace>.svc.cluster.local/
Headers:
  X-Firebolt-Engine: my-engine
  Content-Type: text/plain
Body: <SQL>
```

**Directly against the per-engine Service:** each engine exposes a headless Service at `<engine>-service.<namespace>.svc.cluster.local:3473`. Use this when your client implements its own connection-level load balancing and DNS re-resolution.

With this entry point the caller is responsible for
- Periodically re-resolving the Service hostname (Kubernetes TTL on the in-cluster DNS response is typically 5s) so that newly-ready pods are picked up and draining pods are dropped
- Treating a request on a single endpoint that fails with a transport error as "pick another endpoint", not "retry this request";

## Rollout strategies

**Graceful (default):** new generation is created, traffic is switched, old generation is drained (waits for running queries to complete), then deleted. Use for production.

**Recreate:** new generation is created, traffic is switched, old generation is immediately deleted. Use for dev/test or when you can tolerate interrupted queries.

## Operator flags

| Flag | Default | Description |
|------|---------|-------------|
| `--namespace` | (all) | Namespace to watch. Watches all namespaces if empty |
| `--metrics-bind-address` | `0` | Address for the metrics endpoint |
| `--health-probe-bind-address` | `:8081` | Address for health probes |
| `--leader-elect` | `false` | Enable leader election for HA deployments |

## Running tests

```bash
make test               # unit tests (envtest, no cluster required)
make test-e2e           # E2E tests (requires Kind cluster)
make lint               # golangci-lint
```

## Documentation

### Reference

- [docs/instance-crd-reference.md](docs/instance-crd-reference.md) -- FireboltInstance spec, phases, and monitoring
- [docs/engine-crd-reference.md](docs/engine-crd-reference.md) -- FireboltEngine spec, phases, conditions, and managed resources
- [docs/engineclass-crd-reference.md](docs/engineclass-crd-reference.md) -- EngineClass spec, the operator-owned rejection set on `spec.template`, and the watch-driven rollout contract
- [docs/gateway-sizing.md](docs/gateway-sizing.md) -- gateway replica count, memory limits, and the 2 MiB buffer constraint
- [docs/troubleshooting.md](docs/troubleshooting.md) -- common issues with instances and engines
- [docs/monitoring.md](docs/monitoring.md) -- observability and metrics

### Design

- [docs/architecture.md](docs/architecture.md) -- full architecture and reconciliation model
- [docs/operator-based-scaling.md](docs/operator-based-scaling.md) -- zero-downtime blue-green scaling design
- [docs/formal-verification.md](docs/formal-verification.md) -- TLA+ specifications and model checking
- [docs/SDLC.md](docs/SDLC.md) -- release lifecycle and image tagging conventions
- [docs/option-b-per-engine-envoy-clusters.md](docs/option-b-per-engine-envoy-clusters.md) -- per-engine Envoy cluster model (proposal, not implemented)

## Where to go next

For implementation detail, conventions, and rules for making changes, see [`AGENTS.md`](AGENTS.md).
