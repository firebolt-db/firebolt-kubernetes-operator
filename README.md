# Firebolt Engine Operator

A Kubernetes operator that manages Firebolt infrastructure: metadata services, an Envoy query routing proxy, and compute engines with zero-downtime scaling via blue-green deployments.

## Overview

The operator manages two custom resources:

- **FireboltInstance** provisions the shared infrastructure that engines depend on: PostgreSQL, the metadata service, and an Envoy gateway proxy.
- **FireboltEngine** deploys stateful compute nodes. Each engine references a `FireboltInstance` and cannot operate without one.

When you change an engine's configuration (e.g., scale from 3 to 5 nodes), the operator performs a zero-downtime blue-green transition: it creates a new generation, waits for readiness, switches traffic, drains the old generation, and deletes it.

## Prerequisites

- **Kubernetes 1.28+** — The CRDs use CEL transition rules (`oldSelf`) for field immutability, which require Kubernetes 1.28 or later.

## Quick Start

### 1. Deploy the Operator

**Production / CI:**

```bash
make docker-build docker-push IMG=<your-registry>/firebolt-kubernetes-operator:latest
helm upgrade --install firebolt-operator helm/firebolt-kubernetes-operator \
  --set image.repository=<your-registry>/firebolt-kubernetes-operator \
  --set image.tag=latest
```

**Local development (Kind):**

```bash
make prepare-test-e2e   # one-time: creates Kind cluster + loads test images
make local-deploy       # builds operator, loads into Kind, deploys via Helm
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

This creates an internal PostgreSQL database, deploys the metadata service, initializes an account, and deploys the Envoy gateway proxy. Check progress with:

```bash
kubectl get fi -n firebolt
```

```
NAME         PHASE   GATEWAY   METADATA   AGE
production   Ready   true      true       2m
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
    repository: "ghcr.io/firebolt-db/engine"
    tag: "v1.2.0"
  resources:
    cpu: "2"
    memory: "8Gi"
```

The engine will not start until the referenced instance has a populated metadata endpoint and account ID.

### Connecting to Engines

There are two supported entry points. Both are first-class; pick based on
whether you want the operator to absorb transient failures for you or to do
your own load balancing.

**1. Through the instance gateway (recommended default).**
This is the only entry point on which the operator promises to keep queries
succeeding across engine scaling and blue-green rollouts. The gateway is a
per-instance Envoy proxy that routes requests to a specific engine based on
the `X-Firebolt-Engine` request header.

```
POST http://<instance-name>-gateway.<namespace>.svc.cluster.local/
Headers:
  X-Firebolt-Engine: my-engine
  Content-Type: text/plain
Body: <SQL>
```

The gateway resolves the engine hostname at request time, follows pod
readiness automatically, and retries transport-level failures
(connect refused / TCP reset) so callers do not have to.

**2. Directly against the per-engine Service.**
Each engine exposes a headless Service at
`<engine>-service.<namespace>.svc.cluster.local:3473`. Because the Service is
headless (`ClusterIP: None`), DNS returns the set of ready pod IPs directly;
there is no virtual IP and kube-proxy is not in the data path.

This entry point is intended for clients that implement their own
connection-level load balancing - for example, a client library that
resolves the Service hostname, maintains a pool of connections to the
returned pod IPs, observes connection-level failures, and re-resolves DNS
on failure. Single-connection callers that want the operator to handle
transient failures should use the gateway instead.

With this entry point the caller is responsible for:
- periodically re-resolving the Service hostname (Kubernetes TTL on the
  in-cluster DNS response is typically 5s) so that newly-ready pods are
  picked up and draining pods are dropped;
- treating a request on a single endpoint that fails with a transport
  error as "pick another endpoint", not "retry this request";
### 4. Scale or Update

```bash
kubectl patch fireng my-engine -n firebolt \
  --type merge -p '{"spec":{"replicas":5}}'
```

The operator handles the zero-downtime transition automatically.

### 5. Stop and Resume

Set `spec.replicas` to `0` to stop the engine without deleting the CR. The operator tears down the active generation through the same blue-green path (honoring `spec.rollout` for drain behavior) and leaves the engine in the `stopped` phase:

```bash
kubectl patch fireng my-engine -n firebolt \
  --type merge -p '{"spec":{"replicas":0}}'
```

A stopped engine keeps its CR, so `spec.image`, `spec.resources`, and everything else are preserved across stop/resume. The engine Service has zero endpoints while stopped, and requests through the gateway with `X-Firebolt-Engine: my-engine` return HTTP 503 until the engine is resumed.

Resume by setting a non-zero replica count — the operator starts a new blue-green generation with the requested size:

```bash
kubectl patch fireng my-engine -n firebolt \
  --type merge -p '{"spec":{"replicas":3}}'
```

See [Stopping an Engine](docs/operator-based-scaling.md#stopping-an-engine) in the design doc for the detailed transition flow and the client-facing contract.

### 6. Delete

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
| `spec.id` | No | (auto-generated ULID) | Stable unique identifier for the instance, used as the metadata account ID. Immutable once set. |
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
| `spec.gateway` | **Yes** | - | Envoy gateway proxy configuration (can be empty `{}` for defaults) |
| `spec.gateway.image` | No | `envoyproxy/envoy:v1.37.2` | Override the Envoy container image |
| `spec.gateway.replicas` | No | `2` | Number of gateway pods |
| `spec.gateway.resources` | No | (operator default) | CPU/memory for gateway pods |
| `spec.gateway.nodeSelector` | No | - | Node selector for gateway pods |
| `spec.auth` | No | disabled | Authentication configuration (not enforced yet; reserved for future engine-level auth) |
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
NAME         PHASE   GATEWAY   METADATA   AGE
production   Ready   true      true       24h
```

Inspect details:

```bash
kubectl get fi production -n firebolt -o yaml
```

Key status fields: `phase`, `metadataReady`, `gatewayReady`, `metadataEndpoint`, `gatewayEndpoint`, `conditions` (per-component detail including `PostgresReady`).

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
      repository: "ghcr.io/firebolt-db/firebolt-metadata"
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
    resources:
      requests:
        cpu: "100m"
        memory: "256Mi"
    nodeSelector:
      firebolt.dev/pool: system

  # Auth is not enforced yet; reserved for future engine-level auth.
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
| `spec.replicas` | **Yes** | - | Number of engine nodes. Set to `0` to stop the engine (the CR is preserved; see [Stop and Resume](#5-stop-and-resume)). |
| `spec.image.repository` | **Yes** | - | Container image repository |
| `spec.image.tag` | **Yes** | - | Container image tag |
| `spec.image.pullPolicy` | No | `IfNotPresent` | Image pull policy |
| `spec.resources.cpu` | **Yes** | - | CPU request and limit (e.g., `"2"`, `"500m"`) |
| `spec.resources.memory` | **Yes** | - | Memory request and limit (e.g., `"8Gi"`) |
| `spec.rollout` | No | `graceful` | `graceful` waits for drain; `recreate` deletes immediately |
| `spec.drainCheckEnabled` | No | `true` | Set to `false` to skip the operator-side drain check. The engine's `shutdown_wait_unfinished` still runs on SIGTERM. |
| `spec.drainCheckInterval` | No | `5s` | How often to poll old pods for drain status |
| `spec.terminationGracePeriodSeconds` | No | `60` | Grace period between SIGTERM and SIGKILL for engine pods. The engine waits up to `grace − 5s` for in-flight queries after SIGTERM; raise this for workloads with long-running queries. |
| `spec.nodeSelector` | No | - | Node selector for engine pods |
| `spec.tolerations` | No | - | Tolerations for engine pods |
| `spec.metadataEndpointOverride` | No | - | Override the instance-derived metadata endpoint (for cross-cluster setups) |

### Engine Phases

| Phase | Meaning |
|-------|---------|
| `stable` | Terminal. All resources match spec, `replicas > 0`, engine is serving traffic. |
| `creating` | New generation being created; waiting for pods to be ready. |
| `switching` | Traffic being switched to the new generation. |
| `draining` | Waiting for old generation pods to finish serving queries. |
| `cleaning` | Deleting old generation resources. |
| `stopped` | Terminal. `spec.replicas == 0`. Engine is intentionally parked; CR and active-generation resources are preserved but no pods are running. Set `spec.replicas` to a non-zero value to resume. |

### Conditions

| Condition | Meaning |
|-----------|---------|
| `InstanceReady=True` | Referenced `FireboltInstance` is ready and providing metadata. |
| `InstanceReady=False` | Instance is missing, not ready, or lacks metadata endpoint / account ID. |
| `Ready=True, Reason=EngineReady` | Engine is serving traffic with all replicas ready. |
| `Ready=False, Reason=Rolling` | A blue-green transition is in progress (`creating` / `switching` / `draining` / `cleaning`). |
| `Ready=False, Reason=PodsNotReady` | Phase is `stable` but some pods are not yet ready (e.g., image pull in progress). |
| `Ready=False, Reason=Stopped` | `spec.replicas == 0`. The engine is intentionally parked; not a transient failure. |
| `Ready=False, Reason=InstanceNotReady` | The referenced `FireboltInstance` is not ready. |

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
| **Engine Service** | `my-engine-service` | Headless Service exposing the current generation's pod IPs. See "Connecting to Engines". |
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
    repository: "ghcr.io/firebolt-db/engine"
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

Common causes: instance still provisioning, metadata service pods not ready, gateway pods not ready.

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

### Local Deployment (Kind)

To deploy the operator into a local Kind cluster for manual testing:

```bash
make prepare-test-e2e   # one-time: creates Kind cluster + loads test images
make local-deploy       # builds binary, packages Docker image, loads into Kind, deploys via Helm
```

Then create resources:

```bash
kubectl apply -f examples/local-instance.yaml
```

To redeploy after code changes, just re-run `make local-deploy` — it rebuilds everything.

To tear down:

```bash
make local-undeploy
```

**Resetting CRDs and stuck resources:** If CRD deletion hangs (e.g., custom resources exist but no operator is running to handle finalizers), use the cleanup script:

```bash
./scripts/cleanup-resources.sh                  # default namespace
./scripts/cleanup-resources.sh -n firebolt      # specific namespace
./scripts/cleanup-resources.sh --all-namespaces  # every namespace
```

This strips finalizers from all `FireboltEngine` and `FireboltInstance` resources, deletes them, and removes the CRDs.

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

**Controlling parallelism with `GINKGO_PROCS`:**

By default, `make test-e2e` runs Ginkgo with `--procs=$(nproc)/2` (half of the host's online CPUs, with a floor of 1). On a single-node Kind cluster this is usually a good balance between throughput and resource contention, but you can override it:

```bash
make test-e2e GINKGO_PROCS=1                                  # serial run, easiest to debug
make test-e2e GINKGO_PROCS=2 GINKGO_FOCUS="Single Node Engine"  # lower parallelism for one focused test
make test-e2e GINKGO_PROCS=8                                  # higher parallelism on a beefy host
```

Lower `GINKGO_PROCS` if you see scheduling failures such as `0/1 nodes are available: 1 Insufficient memory` — each engine pod requests 2 GiB, so the per-node memory budget caps effective parallelism on small Kind clusters.

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

### Bumping Default Image Versions

The default engine and metadata image references live in [`config/images/defaults.env`](config/images/defaults.env). They are embedded into the operator binary and consumed by the E2E suite, so a bump here updates both runtime defaults and tests in lockstep.

Conventions to follow when bumping:

- **`ENGINE_TAG` and `ENGINE_NEW_TAG` must reference the same underlying engine build**, differing only by the `release-` vs `debug-` prefix. The "switch image without downtime" E2E test (`test/e2e/e2e_test.go`) flips between them, so keeping the underlying build identical means the test exercises only the operator's blue/green logic — not behavioural drift between two different engine versions.
- **`PENSIEVE_TAG` should track the same `<timestamp>.<sha>` build** as the engine, without a `release-`/`debug-` prefix (metadata has no such split).
- **`PENSIEVE_NEW_TAG` should be the short SHA** (last 12 chars) of the new build, since the metadata switch test (`test/e2e/instance_test.go`) only needs a tag distinct from `PENSIEVE_TAG`.
- **`ENGINE_TAG` and `ENGINE_NEW_TAG` must not be equal** — the E2E suite fails fast at startup if they are, since the upgrade test would be a no-op.

Example for build `4.32.0-pre.0.20260428141824.5abdf30556cd`:

```env
ENGINE_TAG=release-4.32.0-pre.0.20260428141824.5abdf30556cd
ENGINE_NEW_TAG=debug-4.32.0-pre.0.20260428141824.5abdf30556cd
PENSIEVE_TAG=4.32.0-pre.0.20260428141824.5abdf30556cd
PENSIEVE_NEW_TAG=5abdf30556cd
```

After bumping, re-run `make prepare-test-e2e` so the new images are pulled and loaded into Kind, then `make test-e2e` to verify.

### Linting


```bash
make lint
```

Linter configuration is in `.golangci.yml`.

## Design Documentation

- [docs/architecture.md](docs/architecture.md) — full architecture and reconciliation model of the operator.
- [docs/operator-based-scaling.md](docs/operator-based-scaling.md) — zero-downtime blue-green scaling design.
- [docs/option-b-per-engine-envoy-clusters.md](docs/option-b-per-engine-envoy-clusters.md) — forward-looking sketch of a per-engine Envoy cluster model; not implemented.

## License

Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
