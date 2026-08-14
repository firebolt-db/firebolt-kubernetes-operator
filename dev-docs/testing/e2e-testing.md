# E2E testing

The E2E suite runs Ginkgo specs against a Kind cluster. Workload pods run in Kubernetes, while the Firebolt Operator controllers run in process inside the test binary. The suite does not deploy the operator image or admission webhooks.

This split makes controller restart and fault injection cheap while retaining real Kubernetes behavior for StatefulSets, Services, pods, DNS, volumes, and scheduling.

## Prepare and run

Prepare the existing test cluster and publish its workload images:

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

A zero-downtime test must gather enough requests to make a zero-failure assertion meaningful. Start the background runner before the transition, require a minimum success count, and fail on every observed query error.

Do not accept transient failures as expected rollout behavior. The routing contract is layered specifically so blue-green transitions do not leak a 5xx to requests within its supported request-size and gateway-entry constraints.

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
