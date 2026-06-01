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
helm upgrade --install firebolt-operator oci://ghcr.io/firebolt-db/helm-charts/kubernetes-operator --skip-crds
```

There are two supported ways to install the CRDs:

- **Separate CRD chart (recommended for managed upgrades):** install
  `firebolt-operator-crds` first, then install or upgrade the operator chart.
  This chart keeps CRDs in Helm templates, so future `helm upgrade
  firebolt-crds ...` runs can update the CRD definitions. Use `--skip-crds`
  on the operator chart in this flow so Helm does not also try its bundled
  install-time CRD step.
- **Bundled operator chart CRDs:** the operator chart also carries CRDs in its
  `crds/` directory. Helm treats that directory specially: CRDs are installed
  before chart templates on `helm install`, skipped with a warning if they
  already exist, and can be skipped explicitly with `--skip-crds`. Helm does
  not upgrade or delete CRDs from `crds/`, so use this path when install-time
  bootstrapping is enough and manage CRD updates separately.

**Local development (Kind):**

```bash
make prepare-test-e2e   # one-time: creates Kind cluster + publishes test images
make local-deploy       # builds operator, loads into Kind, deploys via Helm
```

`make prepare-test-e2e` starts a local Docker registry container (`kind-registry` on `127.0.0.1:5001`) and configures every kind node to mirror `ghcr.io` and `docker.io` through it. Workload images are pushed to that registry once and pulled on demand by each node, so the multi-GB engine image is no longer duplicated per kind node. To recreate the registry from scratch (e.g. after a stale push), run `make flush-local-registry`.

### Create a FireboltInstance

An instance provisions the metadata infrastructure. See the [FireboltInstance CRD Reference](docs/crd-reference/instance-crd-reference.mdx) for all fields, phases, gateway sizing, and examples.

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltInstance
metadata:
  name: quickstart
  namespace: firebolt
spec:
  metadata: {}
  gateway: {}
```

Check progress:

```bash
kubectl get fire -n firebolt
```

### Configure object storage

Every FireboltEngine requires object storage for managed tablet data. Local filesystem storage mode is not supported by the Firebolt Operator and the engine will refuse to start without object storage.

On a real cluster, point the engine at an existing bucket (S3, GCS, or Azure Blob) and grant access through your platform's workload identity. For a local cluster (Kind, minikube), deploy a small S3-compatible emulator ([floci](https://github.com/floci-io/floci)) and create a bucket:

```bash
kubectl apply -f examples/object-storage-local.yaml
```

This deploys floci into the `firebolt` namespace at `http://floci.firebolt.svc.cluster.local:4566` and creates a `my-engine-bucket` bucket, which the engine references through `spec.customEngineConfig.storage` below.

### Create a FireboltEngine

Once the instance is `Ready`, create an engine that references it. The engine container image lives on an `EngineClass` rather than on the engine itself (FB-1145), so the minimal viable engine ships in two manifests — a class with the image, and an engine referencing the class. The `customEngineConfig.storage` block points the engine at the object storage configured above. See the [FireboltEngine CRD Reference](docs/crd-reference/engine-crd-reference.mdx) and the [EngineClass CRD Reference](docs/crd-reference/engineclass-crd-reference.mdx) for the full field surface.

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
  instanceRef: quickstart
  engineClassRef: default
  replicas: 2
  customEngineConfig:
    storage:
      type: minio
      api_scheme: "s3://"
      bucket_name: my-engine-bucket
      minio:
        endpoint: http://floci.firebolt.svc.cluster.local:4566
  resources:
    requests:
      cpu: "2"
      memory: "4Gi"
    limits:
      cpu: "2"
      memory: "4Gi"
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

## Firebolt Operator flags

The Firebolt Operator supports these runtime flags. The binary default is what
the manager uses when you run it directly. The Helm chart default is what the
`kubernetes-operator` chart passes with its default `values.yaml`.

| Flag | Binary default | Helm chart default | Description |
|------|----------------|--------------------|-------------|
| `--version` | `false` | Not set | Print the version and exit. |
| `--namespace` | `""` | Not set | Namespace to watch. Watches all namespaces when empty. |
| `--metrics-bind-address` | `0` | `:8443` | Address for the metrics endpoint. Use `0` to disable metrics. |
| `--metrics-secure` | `true` | `true` | Serve metrics over HTTPS with Kubernetes authentication and authorization. |
| `--metrics-cert-path` | `""` | Not set | Directory that contains the metrics server certificate. |
| `--metrics-cert-name` | `tls.crt` | Not set | Metrics server certificate file name. |
| `--metrics-cert-key` | `tls.key` | Not set | Metrics server key file name. |
| `--health-probe-bind-address` | `:8081` | `:8081` | Address for health probes. |
| `--leader-elect` | `false` | `true` | Enable leader election for HA deployments. |
| `--enable-webhooks` | `true` | `false` | Enable the admission webhook server. |
| `--webhook-cert-path` | `""` | Not set | Directory that contains the webhook certificate. The chart sets `/tmp/k8s-webhook-server/serving-certs` when `webhook.enabled=true`. |
| `--webhook-cert-name` | `tls.crt` | Not set | Webhook certificate file name. |
| `--webhook-cert-key` | `tls.key` | Not set | Webhook key file name. |
| `--enable-http2` | `false` | Not set | Enable HTTP/2 for the metrics and webhook servers. |
| `--engine-max-cpu` | `""` | Not set | Maximum allowed `FireboltEngine.spec.resources` CPU request and limit. Empty disables the bound. |
| `--engine-max-memory` | `""` | Not set | Maximum allowed `FireboltEngine.spec.resources` memory request and limit. Empty disables the bound. |
| `--engine-max-ephemeral-storage` | `""` | Not set | Maximum allowed `FireboltEngine.spec.resources` ephemeral-storage request and limit. Empty disables the bound. |
| `--zap-devel` | `false` | Not set | Enable controller-runtime development logging defaults. |
| `--zap-encoder` | `json` | `json` | Log encoding. Valid values are `json` and `console`. |
| `--zap-log-level` | `info` | `info` | Minimum log level. Valid values include `debug`, `info`, `error`, and `panic`. |
| `--zap-stacktrace-level` | `error` | `error` | Level at and above which stack traces are captured. |
| `--zap-time-encoding` | `rfc3339` | Not set | Timestamp encoding for zap logs. |

## Running tests

```bash
make test               # unit tests (envtest, no cluster required)
make test-e2e           # E2E tests (requires Kind cluster)
make lint               # golangci-lint
```

## Documentation

### Reference

- [docs/crd-reference/instance-crd-reference.mdx](docs/crd-reference/instance-crd-reference.mdx) -- FireboltInstance spec, phases, and monitoring
- [docs/crd-reference/engine-crd-reference.mdx](docs/crd-reference/engine-crd-reference.mdx) -- FireboltEngine spec, phases, conditions, and managed resources
- [docs/crd-reference/engineclass-crd-reference.mdx](docs/crd-reference/engineclass-crd-reference.mdx) -- EngineClass spec, the operator-owned rejection set on `spec.template`, and the watch-driven rollout contract
- [docs/gateway-sizing.mdx](docs/gateway-sizing.mdx) -- gateway replica count, memory limits, and the 2 MiB buffer constraint
- [docs/troubleshooting.mdx](docs/troubleshooting.mdx) -- common issues with instances and engines
- [docs/monitoring.mdx](docs/monitoring.mdx) -- observability and metrics
- [docs/security.mdx](docs/security.mdx) -- operator vs. platform responsibilities for pod hardening, network isolation, and secrets

### Design

- [docs/architecture.mdx](docs/architecture.mdx) -- full architecture and reconciliation model
- [docs-internal/operator-based-scaling.md](docs-internal/operator-based-scaling.md) -- zero-downtime blue-green scaling design
- [docs-internal/formal-verification.md](docs-internal/formal-verification.md) -- TLA+ specifications and model checking
- [docs-internal/SDLC.md](docs-internal/SDLC.md) -- release lifecycle and image tagging conventions
- [docs-internal/option-b-per-engine-envoy-clusters.md](docs-internal/option-b-per-engine-envoy-clusters.md) -- per-engine Envoy cluster model (proposal, not implemented)

## Where to go next

For implementation detail, conventions, and rules for making changes, see [`AGENTS.md`](AGENTS.md).
