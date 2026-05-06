# AGENTS.md

## What this project does

Firebolt Kubernetes Operator manages two CRDs:

- **FireboltInstance** — shared infrastructure per namespace (PostgreSQL, Pensieve metadata service, Envoy gateway).
- **FireboltEngine** — stateful compute nodes with zero-downtime blue-green deployments.

Engines require a ready instance. See [docs/architecture.md](docs/architecture.md) for the full design.

Built with Go and controller-runtime.

## Commands

```bash
# Build
make build              # compile to bin/manager
make manifests          # regenerate CRDs from types
make generate           # regenerate DeepCopy methods

# Test
make test               # unit tests (uses envtest)
make test-e2e           # E2E tests on Kind cluster

# Run a single unit test
go test ./internal/controller/... -run "TestControllerSuite/SomeSpec"
# Run a focused E2E test
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

After every code change: `make build && make lint`. Use `make lint-fix` to auto-fix where possible.

Do not delete Docker images or kind clusters; assume the kind cluster is already set up.

## Testing

**Unit tests** (`internal/controller/*_test.go`): use envtest (embedded K8s API server). Run with `make test`.

**E2E tests** (`test/e2e/`): require a real Kind cluster with images loaded. Build tag `e2e`; heavy stress tests use tag `e2e,heavy`. The operator runs **in-process** during E2E — it is not deployed into the cluster.

E2E rules:

- Zero-downtime tests **must not have transient failures** — if they flake, fix the test or the controller.
- Maximum wait for any condition: **15 seconds**. Never `sleep` longer than 15 seconds.
- Prefer a short polling loop that exits as soon as the condition is met over a long fixed sleep.
- Run focused tests via `make test-e2e GINKGO_FOCUS=...`; do not invoke `ginkgo` directly.
- When a test fails because pods never become ready, check engine container logs.

## Commit Conventions

Ask before creating or amending commits.

```
<type>(<scope>): <description> (FB-<ticket>)
```

Types: `build`, `chore`, `ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`, `test`.

Scope is optional but encouraged. Common scopes: `api`, `controller`, `helm`, `e2e`, `build`.

Rules:

- Subject: imperative mood, lowercase, no trailing period; keep it under 100 characters.
- Body: explain what changed and *why*.
- The `FB-<ticket>` reference is required; take it from the branch name or last commit.

Example: `fix(controller): report ready-vs-total pods in PodsNotReady message (FB-661)`

**Staging**: always add files by explicit path (`git add file1 file2`). Never use `git add -A`, `git add .`, or `git add --all` — untracked files that are not meant to be versioned will be committed.

## Plans

When breaking work into a plan, divide it into self-contained commits. Each commit is a single logical change — do not mix refactors with new features. Order:

1. Refactors and type extractions first (non-functional changes).
2. New types and CRDs.
3. Modifications to existing types.
4. Controller/reconciler logic last.

## Code Quality

- Ignoring errors is forbidden except with a documented rationale in a comment.
- Keep documents under `docs/` up to date as code changes. Documents about historical or unimplemented features do not need updates.
