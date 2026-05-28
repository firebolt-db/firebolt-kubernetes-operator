# Gateway Sizing

The Envoy gateway is the only zero-downtime entry point, so its replica count and memory limit have to absorb both steady-state traffic and the **retry amplification** introduced by the `X-Firebolt-Drained` shutdown path.

The operator pins Envoy's `per_connection_buffer_limit_bytes` to **2 MiB** on both the listener and the dynamic-forward-proxy cluster. The value is intentionally not exposed on the CR (see the comment on `gatewayPerConnectionBufferLimitBytes` in `internal/controller/instance_gateway.go`): it sits at the centre of the operator's zero-downtime + memory-budget contract, and a per-instance override would invite settings that silently break either retry coverage or the gateway memory limit.

Two consequences this fixed value imposes on operations:

- **Memory budget.** Peak buffering per gateway pod is roughly `expected_concurrent_requests x (1 + retry_factor) x 2 MiB`, where `retry_factor` is the fraction of in-flight requests you expect to be retried during a cutover (typically small -- bounded by the active health-check interval and the size of the engine fleet behind a single authority). Raise `spec.gateway.replicas` and `spec.gateway.template.spec.containers[name=="envoy"].resources.limits.memory` together when expected concurrency grows; OOMKills here translate directly into client-visible failures because the gateway is the only zero-downtime entry point.
- **Requests larger than 2 MiB are not retried.** Envoy can only replay a request whose body fits in the per-connection buffer. Anything bigger is dispatched without buffering, and any 503 it gets -- including a retry-safe `X-Firebolt-Drained` 503 from the engine's pre-work shutdown fence -- propagates to the client unretried. Workloads that send single requests above this threshold (multi-MiB COPY ingest, large multi-statement batches) are out of scope for the operator-managed zero-downtime path; split them client-side, or accept that those specific requests can fail during a cutover.

## Per-engine circuit breakers

The operator also stamps Envoy [circuit-breaker thresholds](https://www.envoyproxy.io/docs/envoy/latest/api-v3/config/cluster/v3/circuit_breaker.proto) on the dynamic-forward-proxy cluster. Because each unique engine authority materialises its own STRICT_DNS sub-cluster, these thresholds apply **per engine, per gateway pod** -- a runaway engine cannot consume more than its share of connection-pool slots, pending-request queue, in-flight stream budget, or retries, and therefore cannot starve sibling engines on the same gateway pod.

Defaults (matched in `internal/controller/instance_gateway.go` against constants of the same names, and asserted by `TestBuildEnvoyConfigYAMLCircuitBreakers`):

| Field | Value | What it caps |
|---|---|---|
| `max_connections` | `1024` | Concurrent upstream TCP connections to one engine through one gateway pod. With `max_requests_per_connection: 1` this is also the concurrent in-flight query cap per engine. |
| `max_pending_requests` | `1024` | Queue depth before Envoy returns a synthetic 503 with response flag `UO` (upstream overflow). |
| `max_requests` | `1024` | HTTP/2 active streams per engine sub-cluster. Held in lockstep with `max_connections` because `max_requests_per_connection: 1` collapses the two dimensions. |
| `max_retries` | `256` | Cluster-wide simultaneous retry budget; higher than Envoy's default of 3 because the route's `num_retries` is 50 and a cutover can keep many requests retrying at once. |

These values are hard-coded for the same reason as `per_connection_buffer_limit_bytes`: a per-instance override would either be a no-op (limits set high) or actively break the per-engine isolation contract (limits set low enough to throttle steady-state traffic on one engine while leaving the gateway's global memory budget unchanged). Operators expecting per-engine concurrency in excess of `max_connections` should raise `spec.gateway.replicas` so the *total* gateway capacity grows in proportion -- not change these per-pod caps.

Operationally, a request rejected by a tripped circuit breaker shows up as a synthetic Envoy 503 with response flag `UO` / `UAEX` / `URX` in the access log. Sustained 503s with these flags against one engine indicate that engine is saturating the per-pod cap; investigate the engine before raising the gateway's replica count.
