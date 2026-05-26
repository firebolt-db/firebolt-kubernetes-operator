# FireboltEngine CRD Reference

## Spec Reference

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `spec.instanceRef` | **Yes** | - | Name of the `FireboltInstance` in the same namespace |
| `spec.engineClassRef` | No | - | Name of an [`EngineClass`](engineclass-crd-reference.md) **in this engine's namespace**. When set, the class's `spec.template` is merged underneath this engine's pod template (engine spec wins on conflict). The engine container image is sourced from the class — there is no per-engine image override on this CR; see the EngineClass reference for the merge precedence. |
| `spec.replicas` | **Yes** | - | Number of engine nodes. Set to `0` to stop the engine (the CR is preserved; see [Stop and Resume](../README.md#stop-and-resume)). |
| `spec.resources.requests` | No | - | Kubernetes resource requests for engine pods, e.g. `cpu`, `memory` |
| `spec.resources.limits` | No | - | Kubernetes resource limits for engine pods, e.g. `cpu`, `memory` |
| `spec.rollout` | No | `graceful` | `graceful` waits for drain; `recreate` deletes immediately |
| `spec.drainCheckEnabled` | No | `true` | Set to `false` to skip the operator-side drain check. The engine's `shutdown_wait_unfinished` still runs on SIGTERM. |
| `spec.drainCheckInterval` | No | `5s` | How often to poll old pods for drain status |
| `spec.terminationGracePeriodSeconds` | No | `60` | Grace period between SIGTERM and SIGKILL for engine pods. The engine waits up to `grace - 5s` for in-flight queries after SIGTERM; raise this for workloads with long-running queries. |
| `spec.nodeSelector` | No | - | Node selector for engine pods |
| `spec.tolerations` | No | - | Tolerations for engine pods |
| `spec.podAnnotations` | No | - | Annotations applied to engine pod templates. Operator-managed annotations always win against user-provided values with the same key. Changes trigger a new blue-green generation. |
| `spec.metadataEndpointOverride` | No | - | Override the instance-derived metadata endpoint (for cross-cluster setups) |

## Engine Phases

| Phase | Meaning |
|-------|---------|
| `stable` | Terminal. All resources match spec, `replicas > 0`, engine is serving traffic. |
| `creating` | New generation being created; waiting for pods to be ready. |
| `switching` | Traffic being switched to the new generation. |
| `draining` | Waiting for old generation pods to finish serving queries. |
| `cleaning` | Deleting old generation resources. |
| `stopped` | Terminal. `spec.replicas == 0`. Engine is intentionally parked; CR and active-generation resources are preserved but no pods are running. Set `spec.replicas` to a non-zero value to resume. |

## Conditions

| Condition | Meaning |
|-----------|---------|
| `InstanceReady=True` | Referenced `FireboltInstance` is ready and providing metadata. |
| `InstanceReady=False` | Instance is missing, not ready, or lacks metadata endpoint / instance ID. |
| `Ready=True, Reason=EngineReady` | Engine is serving traffic with all replicas ready. |
| `Ready=False, Reason=Initializing` | First reconcile of a freshly created CR; transient. |
| `Ready=False, Reason=Rolling` | A blue-green transition is in progress (`creating` / `switching` / `draining` / `cleaning`). |
| `Ready=False, Reason=PodsNotReady` | Phase is `stable` but some pods are not yet ready (e.g., image pull in progress). |
| `Ready=False, Reason=Stopped` | `spec.replicas == 0`. The engine is intentionally parked; not a transient failure. |
| `Ready=False, Reason=InstanceNotReady` | The referenced `FireboltInstance` is not ready. |
| `Ready=False, Reason=DrainCheckFailing` | The drain-readiness probe (Prometheus scrape on a draining-generation pod) cannot reach the pod or parse its metrics. The blue-green is paused until the probe recovers. |

## Operator-Managed Resources

**Do not modify these resources manually.** For an engine named `my-engine`:

| Resource | Name Pattern | Purpose |
|----------|--------------|---------|
| **Engine Service** | `my-engine-service` | Headless Service exposing the current generation's pod IPs. See [Connecting to Engines](../README.md#connecting-to-engines). |
| **StatefulSet** | `my-engine-g{N}` | Pods for generation N |
| **Headless Service** | `my-engine-g{N}-hl` | Pod DNS for generation N |
| **Config ConfigMap** | `my-engine-g{N}-config` | Engine config for generation N |

## Monitoring

```bash
kubectl get fire -n firebolt
```

```
NAME        REPLICAS   PHASE    GENERATION   AGE
my-engine   5          stable   2            24h
```

For full examples, see the [`examples/`](../examples/) directory.

For troubleshooting, see [troubleshooting.md](troubleshooting.md).
