# E2E testing

The E2E suite runs Ginkgo specs against a Kind cluster. Workload pods run in Kubernetes, while the Firebolt Operator controllers run in process inside the test binary. The suite deploys the locally built manager binary as the Gateway agent, while reconciliation stays in process. It does not deploy admission webhooks.

This split makes controller restart and fault injection cheap while retaining real Kubernetes behavior for StatefulSets, Services, pods, DNS, volumes, and scheduling.

## Prepare and run

Prepare the existing test cluster, build the Gateway agent image, and publish the workload images to its local registry:

```bash
make prepare-test-e2e
```

Run the suite:

```bash
make test-e2e
```

Run a focused set through the Make target rather than invoking Ginkgo directly:

```bash
make test-e2e GINKGO_FOCUS='Crash Recovery'
```

`make prepare-test-e2e` packages the static manager binary in a scratch image. `make test-e2e` passes its reference as `E2E_WAKE_AGENT_IMAGE`; override that Make variable only when using an explicitly prepared image. Gateway admission is mandatory, so an unset image is a setup error.

Set `GINKGO_PROCS=1` for serial debugging. The default is half the host's online CPUs, with a floor of one.

The standard build tag is `e2e`. Heavy query configurations add `heavy`; the `latest` image variant adds the matching build tag so embedded image defaults and registry contents agree.

## Suite architecture

[`test/e2e/e2e_suite_test.go`](../../test/e2e/e2e_suite_test.go) owns suite-wide Kubernetes clients, image references, namespace setup, prerequisites, and synchronized setup and teardown.

[`test/e2e/helpers_test.go`](../../test/e2e/helpers_test.go) provides resource operations, readiness waits, in-process controller lifecycles, failure diagnostics, and fault-injection seams.

The main lifecycle helpers are:

- `StartOperator`, which starts an Engine reconciler filtered to one Instance.
- `StartInstanceOperator`, which starts the Instance and EngineClass controllers for an Instance lifecycle.
- `SetupTestInstance`, which composes the common setup.
- `TeardownTestInstance`, which stops the managers and removes test resources in dependency order.

Each in-process manager disables metrics and health listeners, scopes its cache to the E2E namespace, and uses unique controller names so multiple test lifecycles can coexist.

## Webhook posture

The E2E managers do not register admission webhooks. A spec that creates invalid input therefore exercises controller-side fallback behavior rather than API-server rejection.

Use `make test-webhook-integration` for behavior that specifically depends on admission registration, defaulting, or rejection. See [Admission and controller-side validation](../architecture/admission-and-controller-validation.md).

## Operator options and garbage collection

The Engine reconciler starts with abandoned-generation GC disabled by default. Happy-path tests thereby assert that the primary phase path does not orphan resources.

A spec that changes desired state during `creating` can deliberately abandon a half-built generation. Start that lifecycle with `WithGC()`:

```go
lc, err := SetupTestInstance(ctx, instanceName, WithGC())
Expect(err).NotTo(HaveOccurred())
```

Without GC, old `*-g<N>` StatefulSets can remain running and helpers that count ready pods by Engine label observe too many pods. That failure looks like readiness never converges even though the desired generation itself is healthy.

`WithDeleteGate` is a narrower fault-injection option for tests that need selected generation deletes to fail. Release its gate during cleanup so teardown is not blocked.

## Waiting and time budgets

Use polling helpers and Gomega `Eventually`; do not use long fixed sleeps.

The suite distinguishes expensive provisioning from focused condition convergence:

- Engine and Instance startup and full blue-green transitions use the shared multi-minute readiness budgets because pulling and starting release images can exceed 15 seconds.
- Focused controller conditions and injected transitions should converge within 15 seconds.
- Operator cache startup is bounded at 10 seconds.
- Polling normally uses the shared one-second interval or a deliberately shorter interval for a local transition.

When adding a wait, reuse an existing helper or timeout constant if it describes the same operation. A new long timeout often hides a missing event, an image problem, or a controller that stopped reconciling.

## Zero-downtime assertions

A zero-downtime test must gather enough requests to make a zero-failure assertion meaningful. Start the background runner before the transition, require a minimum success count, and fail on every observed data-plane query error. The runner retries only explicit client-pod failures to resolve the stable Gateway Service name, with a bounded attempt count; those requests never reached the Gateway and measure Kind/CoreDNS availability rather than the Operator's routing contract. Exhausted DNS retries still fail the test.

Do not accept transient HTTP failures, connection errors, or generic request timeouts as expected rollout behavior. These assertions express the required rollout behavior and must expose regressions in admission, withdrawal, and shutdown. A passing sampled run can miss a short race; it is not proof that every request ordering is safe.

## Gateway test scope

All product SQL helpers, continuous load, authentication, and discovery tests use the Gateway. Direct Pod access is limited to explicit component checks and observations such as TLS handshakes, readiness, metrics, and logs. Do not keep an old Engine busy by sending it fresh queries directly after cutover.

The drain-under-load test stages the replacement using `CrashBeforeCreatingToSwitching`, admits one finite long query through the Gateway, observes it on the old generation, and releases the transition. The query must remain in flight across withdrawal and complete successfully; fresh queries use the Gateway throughout. This staging avoids spending the held-query budget on replacement startup.

`gateway_routing_test.go` disables metric drain checks so they cannot mask a missing routing fence. It checks two registered sessions, old admissions during engine-reconciler restart, requests admitted to the replacement while that reconciler is stopped, Gateway Pod replacement with old liability retained, fresh Pod UID/session registration, and retirement after successful query completion. It reads actual coordination records and agent reports and observes execution on the replacement Engine Pod.

Configuration tests verify that the generated fields select the intended Envoy policy. They do not execute Envoy, demonstrate DNS refresh timing, or prove traffic withdrawal. A health-check failure counter does not identify an HTTP status or prove that a particular endpoint stopped receiving traffic. Endpoint assertions must require the intended address to be present and healthy; absence of an unhealthy flag can also mean an empty cluster.

Runtime coverage must distinguish routing admission fences from DNS and health observations. Verify late admission responses, retries pending across cutover, client cancellation, processor transport failure, agent restart, missing Gateway reports, and positive process termination. DNS behavior still needs separate checks: withdraw a still-healthy old address from DNS, keep a request in flight there, and verify that new requests move to the replacement while the existing request completes. Separate controlled upstream tests must verify that a received request followed by a reset is not replayed, a generic 503 is not retried, and a pre-work drained response can be retried. Idle-listener shutdown under concurrent fresh connections is a separate regression from shutdown with a long-running query holding the listener open.

Gateway termination tests must observe an executing query and a parked wake request before deleting the Gateway pod. Verify that the inbound listener stops accepting connections, the agent continues processing routing updates and wake demand, accepted requests complete once, and the containers exit within the pod budget. Exercise an existing downstream connection as well as fresh clients. Draining Envoy listeners does not exit the Envoy process; the pre-stop hook must observe completion before it returns and Kubernetes sends SIGTERM.

Use an independent connection to check listener closure. A failed connection through a shared `kubectl port-forward` can terminate that forwarding process and interrupt the very request the test is measuring. Require a connection-refused result from the independent probe; a timeout or unrelated network error does not prove listener closure.

The agent is an init container with `restartPolicy: Always`. Inspect `initContainerStatuses` for its lifecycle; `containerStatuses` contains Envoy and any ordinary user sidecars. Pod-wide readiness alone does not prove that a held wake request can finish.

Run `make envoy` to fetch the checksum-pinned Envoy binary, then `make test-envoy-integration` for the generated-configuration runtime tests. These tests use local DNS and controlled upstreams without Kubernetes. The unit-test workflow runs them once, independently of the engine image variant. `ENVOY_BINARY=/path/to/envoy` selects an existing binary for local runs.

Cold and warm-empty routing tests must hold DNS or route discovery until the query is parked, then release it and require success without client retries. A successful local route probe demonstrates usability of that immutable generation destination; it is not evidence that every Gateway worker withdrew another generation. Keep these concerns separate in assertions.

The Helm workflow tests both `WAKE_CACHE_CASE=cold` and `WAKE_CACHE_CASE=warm` through `make helm-test-wake`. The warm case captures the active route, sends a successful Gateway query, waits for auto-stop and a disabled assignment, then wakes through the same Gateway Pod and Envoy process. The waking query must succeed without client retries and use a different immutable generation authority. The old DNS subcluster need not become empty: deleting its Service may leave a cached answer, but closed admission prevents reuse of that destination.

The Engine controller model abstracts metric drain into a boolean. `formal/GatewayRouting.tla` separately models registration, admission liability, fencing, restart, cancellation, and positive stop evidence. Its safety invariant forbids dispatch to a retired route. Formal checks establish the modeled interleavings; they do not execute Envoy or prove the Core shutdown implementation. Bind each implementation boundary to unit and runtime regressions, and retain a counterexample that demonstrates the model detects premature retirement.

## Crash-recovery coverage

E2E builds activate the named crash points in
[`internal/controller/crash_points_e2e.go`](../../internal/controller/crash_points_e2e.go).
The recovery specs block at selected write boundaries, release the blocked
reconcile, restart the in-process manager, and verify convergence and query
availability. Property tests and the formal model cover resource-write prefixes
without the matching status transition.

Always release registered crash points and clear Engine-scoped registrations in
cleanup. Run the focused group through the repository target:

```bash
make test-e2e GINKGO_FOCUS='Crash Recovery'
```

## Cleanup ordering

Delete Engine resources while their in-process controller is still running so finalizers can complete. Stopping the controller first leaves terminating CRs that collide with later specs reusing a name.

Cleanup should:

1. stop background workers and release fault-injection channels or gates;
2. delete Engines and wait for their owned resources to disappear;
3. delete or tear down the Instance lifecycle;
4. stop any remaining in-process managers;
5. clear test-scoped crash points and diagnostics.

Do not delete the Kind cluster or Docker images from a spec. The cluster and local registry are shared suite infrastructure.

## Failure diagnostics

Register pod-log dumping for specs that create an Instance or Engine. When readiness fails, inspect the Engine container first; a StatefulSet can exist while its pod is in `ImagePullBackOff`, `CrashLoopBackOff`, admission rejection, or scheduling failure.

Useful checks include:

```bash
kubectl get fire,fireng,firengc -n firebolt-e2e
kubectl get pods,sts,svc,pvc -n firebolt-e2e
kubectl describe sts <statefulset> -n firebolt-e2e
kubectl logs <pod> -n firebolt-e2e -c engine
```

If a pod cannot pull an expected image, verify registry publication before diagnosing the controller. See [Local image registry](local-image-registry.md).

## Adding a spec

1. Choose the smallest lifecycle that supplies the controllers the behavior needs.
2. Use unique, DNS-safe resource names.
3. Register failure log collection before setup.
4. Assert both readiness and the relevant phase or condition.
5. Use the gateway for zero-downtime assertions; direct headless-Service clients are outside that contract.
6. Add `WithGC()` only when the spec intentionally abandons generations.
7. Clean resources while their controller is alive.
8. Run the focused spec repeatedly before the full suite.
