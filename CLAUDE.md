# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Operator Does

Firebolt Kubernetes Operator manages two CRDs:
- **FireboltInstance**: Shared infrastructure per-namespace (PostgreSQL, Pensieve metadata service, Envoy gateway)
- **FireboltEngine**: Stateful compute nodes with zero-downtime blue-green deployments

Strict dependency: engines require a ready instance. The instance reconciler runs sequentially through its components (postgres → metadata → gateway). The engine reconciler gates on instance readiness during `stable`, `stopped`, and `creating` phases only; `switching|draining|cleaning` bypass the gate.

## Commands

```bash
# Build
make build              # compile to bin/manager
make manifests          # regenerate CRDs from types
make generate           # regenerate DeepCopy methods

# Test
make test               # unit tests (uses envtest)
make test-e2e           # E2E tests on Kind cluster
```

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

See [docs/architecture_summary.md](docs/architecture_summary.md) for a concise overview and [docs/architecture.md](docs/architecture.md) for the full design.

## Testing

**Unit tests** (`internal/controller/*_test.go`): use envtest (embedded K8s API server). Run with `make test`.

**E2E tests** (`test/e2e/`): require a real Kind cluster with images loaded. Build tag `e2e`; heavy stress tests use tag `e2e,heavy`. CI runs all 23 Describes in parallel on a single cluster with `GINKGO_PROCS=23`.

E2E rules:
- Zero-downtime tests **must not have transient failures** — if they flake, fix the test or the controller
- Maximum wait for any condition: **15 seconds**
- No long `sleep` calls while debugging tests


When running tests it is important to check the logs of engine containers in case they do not become ready.

## Commit Conventions

```
<type>(<scope>): <description> (FB-<ticket>)
```

Types: `feat`, `fix`, `chore`, `ci`, `docs`, `refactor`, `test`

Example: `fix(controller): report ready-vs-total pods in PodsNotReady message (FB-661)`

The FB-<ticket> reference can be taken from branch name or last commit.

**Staging**: always add files by explicit path (`git add file1 file2`). Never use `git add -A`, `git add .`, or `git add --all` — untracked files that are not meant to be versioned will be committed.

## Code Quality

- Ignoring errors is forbidden except with a documented rationale in a comment
