# AGENTS.md

## What this project does

Firebolt Kubernetes Operator manages two CRDs -- **FireboltInstance** (shared infrastructure per namespace: PostgreSQL, Pensieve metadata service, Envoy gateway) and **FireboltEngine** (stateful compute nodes with zero-downtime blue-green deployments). Built with Go and controller-runtime.

Engines require a ready instance. See [docs/architecture.md](docs/architecture.md) for the full design.

## Repo structure

```
api/v1alpha1/          # CRD type definitions and webhooks
cmd/main.go            # operator entry point
config/
  crd/bases/           # generated CRD manifests
  images/              # embedded default image tags (dev / latest variants)
  rbac/                # generated RBAC manifests (kubebuilder output)
  manager/             # manager deployment manifest
  samples/             # example CRs
docs/                  # design documents (architecture, scaling, SDLC)
examples/              # user-facing example manifests
formal/                # TLA+ specifications and model-checker configs
helm/
  kubernetes-operator/ # operator Helm chart
  firebolt-operator-crds/ # CRD-only Helm chart
internal/
  controller/          # reconcilers, state machines, gateway, drain logic
  metrics/             # Prometheus metric recorders
scripts/               # CI helpers, Kind setup, image loading, code generation
test/
  e2e/                 # Ginkgo E2E tests (build tag: e2e)
  testhelpers/         # shared test utilities
```

Unit tests live alongside their source files in `internal/controller/` and `internal/metrics/`. E2E tests are isolated under `test/e2e/`.

## Build, test, and lint

- `make build` -- compile to `bin/manager`
- `make manifests` -- regenerate CRDs from type markers
- `make generate` -- regenerate DeepCopy methods
- `make test` -- unit tests (uses envtest; no cluster required)
- `make test-e2e` -- E2E tests on a Kind cluster (build tag `e2e`; operator runs in-process)
- `make test-property` -- rapid property-based harness for the outer reconcile loop
- `make formal-check` -- run TLC model checker on all TLA+ specs
- `make formal-verify` -- regenerate TLA+ state-cover fixtures and fail if stale
- `make lint` -- golangci-lint (must pass before PR)
- `make lint-fix` -- auto-fix lint issues
- `make helm-lint` -- lint both Helm charts
- `make helm-template` -- render Helm templates locally
- `make helm-docs` -- regenerate Helm chart READMEs from `values.yaml` comments
- `make prepare-test-e2e` -- create Kind cluster and load test images
- `make local-deploy` -- build operator, load into Kind, deploy via Helm
- `make local-undeploy` -- remove the operator Helm release
- `make cleanup-test-e2e` -- delete the Kind cluster

After every code change: `make build && make lint`. Use `make lint-fix` to auto-fix where possible.

### Running a single test

```bash
go test ./internal/controller/... -run "TestControllerSuite/SomeSpec"
GINKGO_FOCUS="your test description" make test-e2e
```

### Build tags

| Tag | Effect |
|-----|--------|
| *(none)* | Unit tests only. Crash points compile as no-ops. Embeds `defaults.dev.env`. |
| `e2e` | Enables E2E test files. Activates real crash-point injection. |
| `e2e,heavy` | Same as `e2e` but uses the heavy query configuration. |
| `latest` | Swaps embedded defaults to `defaults.latest.env`. Combine with `e2e` for the latest-variant E2E run. |

## Proactive Collaboration

You are expected to operate as a strategic collaborator, not a code generator.

- **Challenge weak assumptions.** If a request contradicts the existing structure, conventions, or sound engineering practice, propose a better path before implementing. Surface the trade-off explicitly.
- **Surface viability concerns early.** Before writing code, sanity-check that the requested approach fits the stack, the data model, and any cross-component constraints. If something will not work, say so.
- **Think end-to-end.** A change is not done when the code compiles. It is done when the surrounding integration works: callers updated, configuration wired, downstream consumers verified.
- **Security and correctness by default.** Prefer least-privilege, encrypted, validated, and observable defaults. Flag insecure or fragile patterns even when they were not part of the request.

You MUST follow these collaboration rules on every task.

## Keep documentation up to date

You MUST keep documentation in sync with code. When making changes:

- **AGENTS.md** -- update the relevant `AGENTS.md` (root or scoped) if you change structure, conventions, public interfaces, commands, config formats, or add or remove modules. If your change makes existing AGENTS.md content wrong, fix it before finishing.
- **README.md** -- update if your change affects what a human reader needs to know to understand the project: setup, headline architecture, or what the project is for. Keep the README an overview; push detail into AGENTS.md.
- **docs/architecture.md** -- architectural changes (state-machine phases, reconciler control flow, gateway/data-plane contracts, drain/shutdown handling, RBAC surface) MUST include a matching update in the same commit. This is the canonical record of *why* the system is shaped the way it is.
- **docs/** generally -- keep design documents up to date as code changes. Documents about historical or unimplemented features do not need updates.

A pull request that changes structure or interfaces without a documentation update is incomplete.

## Document resolved issues

You MUST capture non-obvious problems you solve. When you encounter and fix a quirk, footgun, surprising framework behavior, environment trap, or anything that took meaningful debugging:

- Add a short entry to `## Known issues` in the most-relevant `AGENTS.md` (root or scoped).
- State the symptom, the cause, and the resolution in two or three sentences.
- This prevents the next agent from rediscovering the same problem.

If the issue is project-wide, put it in the root `AGENTS.md`. If it is scoped to one module, put it in that module's `AGENTS.md`.

## Proactive harness

Code is not done until it is covered by tests where tests are reasonable.

- You MUST add or update tests for behavior you introduce, change, or fix. Use the project's existing test runners: `make test` for unit tests (envtest), `make test-e2e` for E2E tests (Kind + Ginkgo).
- If a change is genuinely untestable in the current harness (e.g., cloud-only infrastructure, external service integration), you MUST say so explicitly in your response and explain why.
- Prefer fast, deterministic tests at the lowest reasonable level (unit > integration > end-to-end).
- A change that ships without tests, and without an explicit reason for not having them, is incomplete.

## Known issues

- **envtest's `kube-apiserver` ignores SIGTERM on macOS.** The embedded apiserver (>=1.35) does not act on SIGTERM on Darwin -- lease-renewal logs continue right up until envtest's SIGKILL fallback fires. Etcd handles SIGTERM cleanly, so this is apiserver-specific, not an envtest signalling bug. We can't fix it from this side; the workaround lives in two places: (1) `Makefile` sets `KUBEBUILDER_CONTROLPLANE_STOP_TIMEOUT=5s` only when `uname -s` reports `Darwin`, so SIGKILL fires fast instead of after the 20s upstream default; (2) `internal/controller/suite_test.go`'s `AfterSuite` swallows the resulting "timeout waiting for process kube-apiserver to stop" error, but only on `runtime.GOOS == "darwin"` and only for that exact error string. Linux/CI behaviour is unchanged -- a real teardown failure there still fails the suite. See controller-runtime#1571 / #2560 for upstream context.

## Project-specific rules

### Engine reconciler and blue-green state machine

Before touching the engine reconciler -- especially `engine_reconcile.go`, `engine_state.go`, or `instance_gateway.go` -- read the relevant section of [docs/architecture.md](docs/architecture.md) first.

- The blue-green state machine (`creating -> switching -> draining -> cleaning`) is formalised in [formal/FireboltEngine.tla](formal/FireboltEngine.tla). A change in one belongs in both, and `make formal-verify` is the CI guard.
- Zero-downtime during pod termination is enforced by a layered data-plane contract (headless DNS, Envoy active health check, engine `/health/ready=503` on SIGTERM, engine pre-work shutdown fence, gateway retry on `X-Firebolt-Drained`). The "Graceful pod shutdown" and "Why no EndpointSlice gate" subsections of `docs/architecture.md` document the chain and call out a previously-removed design (FB-661) that should not be reintroduced. If you find yourself adding an EndpointSlice watch / RBAC / state field to fix a 5xx during cutover, check whether one of the existing layers is broken before adding a sixth.

### RBAC changes

When canonical RBAC changes (typically regeneration of [`config/rbac/role.yaml`](config/rbac/role.yaml) from `// +kubebuilder:rbac:` markers via `make manifests`), review whether [`helm/kubernetes-operator/templates/clusterrole.yaml`](helm/kubernetes-operator/templates/clusterrole.yaml) needs matching edits -- it is hand-maintained separately from kubebuilder output.

### Testing rules

**Unit tests** (`internal/controller/*_test.go`): use envtest (embedded K8s API server). Run with `make test`.

**E2E tests** (`test/e2e/`): require a Kind cluster with images loaded. Build tag `e2e`; heavy stress tests use tag `e2e,heavy`. The operator runs **in-process** during E2E -- it is not deployed into the cluster.

E2E rules:

- Zero-downtime tests must not have transient failures -- if they flake, fix the test or the controller.
- Maximum wait for any condition: **15 seconds**. Never `sleep` longer than 15 seconds.
- Prefer a short polling loop that exits as soon as the condition is met over a long fixed sleep.
- Run focused tests via `make test-e2e GINKGO_FOCUS=...`; do not invoke `ginkgo` directly.
- When a test fails because pods never become ready, check engine container logs.

Do not delete Docker images or kind clusters; assume the kind cluster is already set up.

### Commit conventions

Ask before creating or amending commits.

```
<type>(<scope>): <description> (FB-<ticket>)
```

Types: `build`, `chore`, `ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`, `test`.

Scope is optional but encouraged. Common scopes: `api`, `controller`, `helm`, `e2e`.

Rules:

- Subject: imperative mood, lowercase, no trailing period; keep it under 100 characters.
- Body: explain what changed and *why*.
- The `FB-<ticket>` reference is required; take it from the branch name or last commit.

Example: `fix(controller): report ready-vs-total pods in PodsNotReady message (FB-661)`

**Staging**: always add files by explicit path (`git add file1 file2`). Never use `git add -A`, `git add .`, or `git add --all` -- untracked files that are not meant to be versioned will be committed.

### Planning work

When breaking work into a plan, divide it into self-contained commits. Each commit is a single logical change -- do not mix refactors with new features. Order:

1. Refactors and type extractions first (non-functional changes).
2. New types and CRDs.
3. Modifications to existing types.
4. Controller/reconciler logic last.

### Code quality

- Ignoring errors is forbidden except with a documented rationale in a comment.
