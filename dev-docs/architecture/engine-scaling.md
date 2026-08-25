# Engine scaling and reconciliation

FireboltEngine changes use a blue-green generation transition instead of mutating the serving StatefulSet in place. This page explains the contributor-facing implementation: where each responsibility lives, which resources form a generation, and which invariants a change must preserve.

For the user-visible contract, use the published documentation:

- [`docs/engine/engine-scaling.mdx`](../../docs/engine/engine-scaling.mdx) explains scaling, stopping, and resuming.
- [`docs/architecture.mdx`](../../docs/architecture.mdx) defines the published architecture and lifecycle contract.
- [`docs/engine/engine-rollouts.mdx`](../../docs/engine/engine-rollouts.mdx) explains rollout strategies and drain checks.
- [`docs/instance/gateway/gateway-query-routing.mdx`](../../docs/instance/gateway/gateway-query-routing.mdx) defines the gateway and zero-downtime routing contract.

Do not duplicate CRD field reference material here. The API types and [`docs/crd-reference/engine-crd-reference.mdx`](../../docs/crd-reference/engine-crd-reference.mdx) own that surface.

## Why scaling creates a generation

An Engine's rendered configuration contains generation-specific topology, including deterministic StatefulSet pod names and peer addresses. Patching a running StatefulSet after its pods consumed an older topology can leave a mixed cluster that cannot converge.

A generation therefore acts as an immutable rollout unit:

1. Materialize the complete desired generation separately from the serving generation.
2. Wait until its pods are ready.
3. Switch the stable routing Service selector.
4. Drain the generation that previously served traffic.
5. Delete its generation-scoped resources.

This same mechanism handles replica changes, image changes, template changes, storage changes, configuration changes, and scale-to-zero. The reconciler does not maintain a separate scaling workflow.

## Reconciler code map

| Responsibility | Primary source | Notes |
| --- | --- | --- |
| Entry point, gates, conditions, finalizer | [`engine_controller.go`](../../internal/controller/engine_controller.go) | Resolves the EngineClass and Instance, validates templates and bounds, drives reads, compute, writes, status, and cleanup |
| Observed resource state and drain probes | [`engine_state.go`](../../internal/controller/engine_state.go) | Reads generation resources, pod readiness, and drain state |
| Pure phase computation and builders | [`engine_reconcile.go`](../../internal/controller/engine_reconcile.go) | Produces an `EngineReconcileResult` without Kubernetes I/O |
| Ordered Kubernetes writes | [`engine_apply.go`](../../internal/controller/engine_apply.go) | Applies ensures, deletes, selector changes, and status intent in a crash-recoverable order |
| Abandoned-generation cleanup | [`engine_gc.go`](../../internal/controller/engine_gc.go) | Bounded sweep with ownership, keep-set, selector, and generation-floor guards |
| Auto-stop decisions | [`engine_autostop.go`](../../internal/controller/engine_autostop.go) | Converts activity, schedule, and wake demand into replica intent |
| Wake-demand polling | [`wake_demand.go`](../../internal/controller/wake_demand.go) | Polls read-only gateway agents and maintains the demand cache |
| Formal state-machine contract | [`formal/FireboltEngine.tla`](../../formal/FireboltEngine.tla) | Models phase transitions, generation safety, crash prefixes, and the GC keep set |

The normal data path is:

```text
Reconcile
    |
    +--> resolve EngineClass and validate effective inputs
    +--> read current Kubernetes resources
    +--> apply Instance gate where the phase requires live Instance data
    +--> computeEngineReconcile (pure decision)
    +--> applyEngineState (ordered writes)
    +--> update conditions and status
    +--> sweep abandoned generations
```

Early gates still allow the abandoned-generation sweep once enough state has been read. An Engine blocked on an invalid template, an unready class, an Instance dependency, or a resource bound must not retain abandoned resources indefinitely.

## Effective Engine inputs

FireboltEngineClass is an optional default layer. For inherited fields, precedence is:

```text
FireboltEngine value, when set
→ referenced FireboltEngineClass value, when set
→ operator default
```

Pod-template maps such as labels and annotations merge, while several structured pod fields replace the class value when the Engine supplies one. Use the `effective*` helpers in `engine_reconcile.go`; do not reproduce merge logic at call sites.

The Engine container image follows the same three-level precedence. A per-Engine override is expressed through the Engine's pod template:

```yaml
spec:
  template:
    spec:
      containers:
        - name: engine
          image: example.com/firebolt/engine:tag
```

`effectiveEngineImage` resolves the Engine template first, then the class template, then the embedded default. Image and `imagePullPolicy` resolve independently, so either can be overridden without restating the other.

`buildStatefulSet` and `stsMatchesSpec` must consume the same effective helper for every pod-affecting field. If the builder and comparator disagree, an unchanged Engine can roll forever or a real change can fail to roll.

## Resource topology

One FireboltEngine owns a stable routing Service and one or more generation-scoped resource sets:

```text
FireboltEngine
├── <engine>-service                  stable, selector changes
├── generation gN                    desired/current generation
│   ├── <engine>-gN-config           rendered Engine configuration
│   ├── <engine>-gN-hl               StatefulSet peer discovery
│   ├── <engine>-gN                  StatefulSet
│   ├── optional TLS Certificate
│   └── optional per-pod PVCs
└── generation gN-1                  active or draining during rollout
    └── corresponding generation resources
```

The two Service roles are different:

- `<engine>-gN-hl` is generation-specific and uses `publishNotReadyAddresses: true`. It gives StatefulSet pods deterministic peer DNS before readiness, which allows cluster formation.
- `<engine>-service` is the stable routing endpoint. It is also headless, but excludes not-ready pods and selects exactly the generation that should receive new traffic.

The ConfigMap is generated before the StatefulSet because its node list depends on predictable StatefulSet identities. Storage is mounted at the operator-owned data path and can resolve to `emptyDir`, `hostPath`, or per-pod persistent volume claims.

PVC lifetime follows the StatefulSet retention policy: claims are deleted when an old generation's StatefulSet is deleted, but retained for an ordinary within-generation scale-down. This preserves a scaled-down ordinal's cache if it returns without retaining claims for generations that have completed blue-green cleanup.

## Phase machine

The six phases are:

```text
stable or stopped
        |
        | drift or desired replica change
        v
     creating ---- spec changes here ----> abandon and restart creating
        |
        | all desired pods ready
        v
     switching ---- selector updated ----> terminal on initial deployment
        |
        | an older active generation exists
        v
     draining ---- drain complete or skipped
        v
     cleaning ---- old resources removed
        v
stable or stopped
```

`stable` and `stopped` share `computeStable`. The terminal name depends on replicas: a nonzero Engine is `stable`; a zero-replica Engine is `stopped`. The stopped generation retains a zero-replica StatefulSet, ConfigMap, and headless Service so drift repair and resume use the normal generation path.

Spec changes have phase-specific handling:

- In `stable` or `stopped`, drift records a new generation intent and enters `creating`.
- In `creating`, new drift abandons the incomplete generation and advances again. Patching an already-created StatefulSet would risk mixing topology and configuration.
- In `switching`, `draining`, or `cleaning`, new drift waits for the current transition to reach a terminal phase. This bounds the active transition state and avoids overlapping drain chains.

The phase machine references at most two generations at once, but the cluster can temporarily contain older abandoned resources after a missed or rejected delete. Do not encode “at most two resource generations exist” as an invariant.

## Readiness, traffic, and drain

Promotion requires every desired pod in the new generation to be ready. At zero replicas, readiness is vacuously true, allowing scale-to-zero to pass through `creating` without waiting for pods that should not exist.

Switching changes only the stable routing Service selector. The gateway resolves that headless Service and supplies the remaining data-plane protections: per-request connection behavior, Envoy active health checks, transport retries, and the retry-safe Engine shutdown-fence response. The reconciler intentionally does not gate phase progress on EndpointSlice membership.

Graceful rollout drain checks the old generation after traffic switches. The scrape transport comes from the parent FireboltInstance: direct Pod IP is the default, while API-server proxying is optional and requires its opt-in RBAC grant. `rollout: recreate` or disabled drain checking skips the operator-side wait, but the Engine's own SIGTERM handling still applies.

## Partial writes and crash recovery

Kubernetes resource writes and status writes are not one transaction. `applyEngineState` can create or delete several resources and then fail or stop before the phase update is stored. The next reconciliation must infer progress from both status and observed resources and safely repeat any incomplete action.

This is why:

- ensure operations tolerate already-existing resources;
- delete operations tolerate already-absent resources;
- Service selection is checked from the live Service rather than inferred only from status;
- status cannot be treated as a complete event log;
- write ordering is represented by crash points and property-test prefixes.

Production builds compile crash points as no-ops. E2E builds can block the write
layer at named points in `internal/controller/engine_apply.go`; the test releases
the blocked goroutine before restarting the in-process manager and then verifies
convergence. That E2E sequence does not by itself prove that the matching status
write was interrupted. The property harness models resource-write prefixes
without their status write, and the TLA+ model permits the same partial states.

When write ordering changes, review the crash-point call sites, property prefixes,
formal model, and E2E recovery coverage together. See [E2E testing](../testing/e2e-testing.md)
and [Formal verification](../testing/formal-verification.md).

## Abandoned-generation sweep

The primary phase path issues deletes when it abandons or cleans a generation. A cached miss, rejected delete, or interruption can leave an unreferenced resource behind, so `engine_gc.go` performs a bounded sweep.

The keep set protects:

- `currentGeneration`;
- `activeGeneration`;
- `drainingGeneration`;
- the generation selected by the stable Service.

A deletion candidate must also be generation-labeled, owned by the current Engine, older than the newest generation named by status, and not already terminating. Cert-manager-derived Secrets use Certificate provenance because their controller owner is the Certificate rather than the Engine.

The generation floor is the stale-cache safety boundary. A resource newer than the reconciler's status view may belong to a concurrent, later reconcile and must not be deleted merely because it is absent from the stale keep set.

## Change-impact guide

| Change | Required companion review |
| --- | --- |
| Phase transition or terminal-phase rule | `FireboltEngine.tla`, generated state cover, property tests, and public architecture |
| Write ordering or new side effect | Crash points, property-test prefix model, and recovery tests |
| New pod-affecting field | Effective resolver, StatefulSet builder, drift comparator, webhook/controller allowlist, CRD docs, and plugin |
| EngineClass-inherited field | Engine-if-set precedence, class watch/hash behavior, and engine/class tests |
| EnginePreset overlay or fail-closed gate | `defaultsReady` in `FireboltEngine.tla` (not merge content), Preset hash/watch tests, and inheritance docs |
| Generation-owned resource | read state, apply/delete path, finalizer cleanup, GC ownership and keep-set logic |
| Stable Service or readiness behavior | Gateway routing, shutdown chain, Service tests, and zero-downtime E2E coverage |
| Drain semantics | both scrape transports, rollout documentation, timeouts, and drain-under-load E2E tests |
| Auto-stop or wake ordering | `EngineWake.tla`, wake state cover, property tests, and gateway-agent integration |

After a modeled change, follow the workflow in [Formal verification](../testing/formal-verification.md). A passing build is not enough when builder/comparator symmetry, routing, or crash recovery changes.
