# Architecture Summary

> For the full architecture see [docs/architecture.md](architecture.md).

## Two CRDs

- **FireboltInstance** — shared infrastructure per namespace: PostgreSQL, Pensieve metadata service, Envoy gateway.
- **FireboltEngine** — stateful compute nodes with zero-downtime blue-green deployments.

Engines have a hard dependency on a ready instance. The engine reconciler gates on instance readiness during `stable`, `stopped`, and `creating` phases; `switching|draining|cleaning` bypass the gate.

## Engine phase state machine

```
stable → creating → switching → draining → cleaning → stable
                                                  └──► stopped (replicas=0)
```

- **creating**: provisions new-generation StatefulSet + headless Service + ConfigMap; waits for pods ready
- **switching**: updates cluster Service selector to new generation
- **draining**: waits for in-flight queries to finish (scrapes pod `/metrics` via API server proxy)
- **cleaning**: deletes old-generation resources
- **stable / stopped**: terminal phases (stopped = zero replicas)

Spec change mid-flight: abandon if `creating`, defer if `draining|cleaning`.

## Blue-green resource naming

Each spec change bumps `currentGeneration`. Resources per generation:

- `{engine}-g{N}` — StatefulSet
- `{engine}-g{N}-hl` — Headless Service
- `{engine}-g{N}-config` — ConfigMap
- `{engine}-service` — shared Service (selector switches between generations)

## Engine reconciler layers

| File | Role |
|------|------|
| `engine_controller.go` | Entry point; reads CR; instance gate; delegates |
| `engine_state.go` | Read layer — fetches all K8s resources |
| `engine_reconcile.go` | Pure compute layer — zero I/O |
| `engine_apply.go` | Write layer — applies reconcile result |
| `engine_gc.go` | Garbage-collects stale generations |

## Instance reconciler

Sequential pipeline: `instance_postgres.go` → `instance_metadata.go` → `instance_gateway.go`.

Phases: `Provisioning → Ready ↔ Degraded` (terminal: `Failed`).

Status: `metadataReady` / `gatewayReady` boolean flags; per-component conditions `MetadataReady` and `GatewayReady`.

## Key design principles

- **Level-triggered**: every reconcile re-reads all state from scratch.
- **Single status write per reconcile**: all conditions updated atomically.
- **Idempotent writes**: all `ensure*` functions are safe to re-run.
- **Crash-safe**: phase persisted to status at every phase boundary.
- **No swallowed errors**: always return or document intentional suppression.
