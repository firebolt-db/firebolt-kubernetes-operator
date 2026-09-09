# Engine retirement and query safety

What happens to queries when an Engine generation goes away — rollout, scale-down, auto-stop, or deletion — and why the design stops where it does.

## Guarantees

1. **A query never executes twice.** The Gateway retries a request only when the Engine provably has not processed it: transport failures before delivery (`connect-failure`, `refused-stream`, `reset-before-request`) or an explicit pre-execution rejection (`503` with `X-Firebolt-Drained`, emitted by the Engine's shutdown fence before any executor or storage work runs). Plain 5xx responses and post-delivery resets return to the client unretried, because the Engine may already have applied side effects.
2. **Accepted work finishes.** On SIGTERM the Engine immediately reports not-ready and rejects fresh queries with the drained response, but completes every query it already accepted, within the Pod termination budget.
3. **Planned retirement is invisible to clients.** A query dispatched to a draining Engine before the Gateway notices is rejected before execution and retried against a healthy generation. The client sees one request and one successful response.

## Deliberate limits

- A query running longer than the termination budget is interrupted when its Pod is deleted.
- Involuntary disruption — OOM kill, node loss, forced deletion — fails in-flight queries to the client. They are not replayed; guarantee 1 outranks availability.
- For one to two seconds after SIGTERM, new queries can still reach the draining Engine. They cost one internal reject-and-retry hop, nothing more.

## The retirement sequence

1. The operator retires a generation: blue-green switch, scale-down, auto-stop, or deletion. Kubernetes sends the Engine Pod SIGTERM.
2. The Engine flips `/health/ready` on the query port to 503 at once and keeps executing accepted queries. Fresh queries get `503` plus `X-Firebolt-Drained` before execution.
3. The Gateway's Envoy health-checks `/health/ready` every second with `unhealthy_threshold: 1`, so the Pod stops receiving new dispatches within about a second.
4. Requests caught inside that window are rejected pre-execution and retried under the conditions in guarantee 1; the `previous_hosts` predicate steers the retry toward an endpoint not yet tried.
5. The Engine finishes accepted work and exits. The kubelet force-kills only at the termination budget.

Wake-on-zero composes with this sequence: a query for a stopped Engine is held by the Gateway's wake agent, demand is recorded, the operator scales the Engine up, and the hold releases once Envoy can route to it. The agent fails open; see the first requirement below.

## Requirements this design imposes

- **The data plane must not depend on the control plane.** A Gateway serves queries, and becomes Ready after a restart, while the operator is down or unreachable. Nothing on the query path may fail closed on operator availability.
- **The Engine owns its drain.** Not-ready on SIGTERM, drained rejection before execution, and completion of accepted work are Engine responsibilities. Operator-side bookkeeping is not a substitute: the operator cannot observe individual requests, and external ledgers go stale exactly when Pods crash.
- **Retry conditions stay pre-delivery only.** Reintroducing `reset`, `5xx`, or any post-delivery condition silently breaks guarantee 1.

## Why not a coordination protocol

A stronger design exists: every query takes an admission permit from a per-Pod agent, Gateways register sessions with the operator, and the operator delays SIGTERM until every registered Gateway acknowledges a routing fence with zero outstanding permits. It would remove the one-to-two-second window entirely. It is rejected here because:

- The reject-and-retry path must exist anyway. Evictions, node drains, and crashes never ask the operator for permission, so the protocol removes that path's use only in the most orderly case.
- Enforcing permits makes the query path fail closed on the agent and on operator availability, violating the first requirement above.
- It adds a distributed protocol — sessions, fences, retirement records, Pod finalizers — where every failure mode (agent restart, operator restart, Pod replacement, same-name recreation) needs its own recovery rule, in exchange for removing an internal retry.

If the window ever matters in practice, measure before redesigning: count drained retries during rollouts, and reopen the decision on evidence.

## Where this is enforced

- [`internal/controller/instance_gateway.go`](../../internal/controller/instance_gateway.go) — the Gateway route `retry_policy` and the engine cluster's active health check.
- `TestBuildEnvoyConfigYAMLRetryPolicy` in [`internal/controller/instance_gateway_test.go`](../../internal/controller/instance_gateway_test.go) — pins the exact retry conditions and the drained matcher.
- The Engine exposes `/health/ready` on the query port and emits the drained response; its shutdown behavior is owned and validated in the Engine's own repository.
- [`test/e2e/drain_under_load_test.go`](../../test/e2e/drain_under_load_test.go) and the Helm wake checks exercise the sequence end to end.
