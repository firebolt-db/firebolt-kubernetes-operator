# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Operator Does

Firebolt Kubernetes Operator manages two CRDs:
- **FireboltInstance**: Shared infrastructure per-namespace (PostgreSQL, Pensieve metadata service, Envoy gateway, account init via gRPC)
- **FireboltEngine**: Stateful compute nodes with zero-downtime blue-green deployments

Strict dependency: engines require a ready instance. The instance reconciler runs sequentially through its components (postgres → metadata → account → gateway). The engine reconciler gates on instance readiness during `stable` and `creating` phases only; `switching|draining|cleaning` bypass the gate.

## Commands

```bash
# Build
make build              # compile to bin/manager
make manifests          # regenerate CRDs from types
make generate           # regenerate DeepCopy methods

# Test
make test               # unit tests (uses envtest)
make test-e2e           # E2E tests on Kind cluster

# Run a single test
go test ./internal/controller/... -run "TestControllerSuite/SomeSpec"
# Or with Ginkgo focus:
GINKGO_FOCUS="your test description" make test-e2e

# Lint
make lint               # golangci-lint (must pass before PR)
make lint-fix           # auto-fix lint issues

# Local cluster
make prepare-test-e2e   # create Kind cluster + load images
make local-deploy       # build + load image + helm upgrade
make local-undeploy     # remove helm release
make cleanup-test-e2e   # delete Kind cluster

# Helm
make helm-lint
make helm-template
```

After every code change: `make build && make lint`.

Do not delete Docker images or kind clusters; assume the kind cluster is already setup.

## Architecture

### Engine Reconciler — Layered Design

The engine controller follows a strict read → compute → write separation:

| File | Role |
|------|------|
| `engine_controller.go` | Entry point; reads CR; gates on instance; delegates to layers |
| `engine_state.go` | Read layer; fetches all K8s resources (StatefulSets, Services, ConfigMaps, Pods) |
| `engine_reconcile.go` | Pure compute layer — **zero I/O**; derives desired state from current state |
| `engine_apply.go` | Write layer; applies the reconcile result to the cluster |
| `engine_gc.go` | Garbage collection for stale generations |

### Engine Phase State Machine

```
stable → creating → switching → draining → cleaning → stable
```

- **creating**: New generation StatefulSet + headless Service + ConfigMap provisioned in parallel
- **switching**: Service selector updated to route traffic to new generation
- **draining**: Old generation drained — operator scrapes `/metrics` via Kubernetes API pod proxy; checks `firebolt_running_queries + firebolt_suspended_queries == 0`
- **cleaning**: Old generation resources deleted

Spec changes mid-flight: if in `creating` → abandon and start over; if in `draining|cleaning` → defer until stable.

### Blue-Green Resource Naming

Each spec change bumps `currentGeneration` (int). Resources per generation:
- `{engine}-g{N}` — StatefulSet
- `{engine}-g{N}-hl` — Headless Service (pod DNS / peer discovery)
- `{engine}-g{N}-config` — ConfigMap (pre-generated pod FQDNs)
- `{engine}-service` — Shared Service (selector switches between generations)

### Instance Reconciler

Sequential pipeline in `instance_controller.go`:
1. `instance_postgres.go` — PostgreSQL StatefulSet or external PG
2. `instance_metadata.go` — Pensieve metadata service Deployment
3. `instance_account_init.go` — gRPC bootstrap to Pensieve
4. `instance_gateway.go` — Envoy gateway Deployment with Lua filter

Instance phases: `Provisioning → Ready ↔ Degraded` (terminal: `Failed`). Per-component boolean flags (`postgresReady`, `metadataReady`, `accountReady`, `gatewayReady`) drive condition reporting.

### Drain Check

The operator scrapes pod metrics via `kubectl proxy`-style API (`GET /api/v1/namespaces/{ns}/pods/{pod}/proxy/metrics`) — no direct pod IP access needed. Engine pods also run a bash preStop hook using `/dev/tcp` (no curl) that reads their own `/metrics` and waits for active queries to reach zero, up to `terminationGracePeriodSeconds - 10s`.

### Key Design Invariants

- **Level-triggered**: every reconcile re-reads all state from scratch; no edge-triggered assumptions
- **Single status write per reconcile**: all conditions updated atomically; conflict retry on resource-version mismatch
- **Idempotent writes**: all `ensure*` functions are safe to re-run
- **No swallowed errors**: returning `nil` on a real error is forbidden; document rationale if done intentionally
- **Crash-safe**: phase is persisted to status before and after each write; `crash_points.go` (no-op in prod) / `crash_points_e2e.go` (real injection in tests) validate recovery at every phase boundary

### Resource Ownership

All child resources carry `ownerReferences` to their CR for cascading delete. Labels used for selection and tracking:
- `firebolt.io/instance`, `firebolt.io/engine`, `firebolt.io/generation`, `firebolt.io/component`

Finalizers on CRs prevent premature deletion before cleanup completes.

## Testing

**Unit tests** (`internal/controller/*_test.go`): use envtest (embedded K8s API server). Run with `make test`.

**E2E tests** (`test/e2e/`): require a real Kind cluster with images loaded. Build tag `e2e`; heavy stress tests use tag `e2e,heavy`. CI runs them in a matrix of 4 parallel groups via `scripts/extract-e2e-groups.sh`.

E2E rules:
- Zero-downtime tests **must not have transient failures** — if they flake, fix the test or the controller
- Maximum wait for any condition: **15 seconds**
- No long `sleep` calls while debugging tests

## Commit Conventions

```
<type>(<scope>): <description> (FB-<ticket>)
```

Types: `feat`, `fix`, `chore`, `ci`, `docs`, `refactor`, `test`

Example: `fix(controller): report ready-vs-total pods in PodsNotReady message (FB-661)`

The FB-<ticket> reference can be taken from branch name or last commit.

## Code Quality

- Ignoring errors is forbidden except with a documented rationale in a comment
