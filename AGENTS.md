# AGENTS.md

## Project and sources of truth

Firebolt Operator is a Go/controller-runtime operator for FireboltInstance,
FireboltEngine, FireboltEngineClass, ClusterFireboltEngineClass, and
FireboltEnginePreset. Engines require a ready Instance.

Read the relevant guide before changing its implementation:

| Area | Guide |
| --- | --- |
| Reconcilers, rollout phases, gateway, and RBAC | [Architecture](docs/architecture.mdx) |
| Retirement, retries, and query safety | [Retirement contract](dev-docs/architecture/engine-retirement-and-query-safety.md) |
| Class and Preset precedence | [Configuration inheritance](docs/engineclass/configuration-inheritance.mdx) |
| Admission and controller validation | [Validation guide](dev-docs/architecture/admission-and-controller-validation.md) |
| State machines and model coverage | [Formal verification](dev-docs/testing/formal-verification.md) |
| E2E setup and diagnostics | [E2E testing](dev-docs/testing/e2e-testing.md), [local registry](dev-docs/testing/local-image-registry.md) |
| Component versions and publication | [Release process](dev-docs/process/release-process.md) |
| CLI consumer of the CRDs | [Plugin rules](internal/infra/AGENTS.md) |

Source layout: `api/v1alpha1/` holds CRD types and webhooks;
`internal/controller/` holds reconcilers; `internal/infra/` backs
`cmd/kubectl-firebolt/`; `cmd/` builds the operator. Generated manifests live in
`config/` and `helm/`. Tests live beside Go code and under `test/e2e/`;
models live under `formal/`; automation lives under `scripts/` and `.github/`.
Published documentation is in `docs/`; contributor guides are in `dev-docs/`.

## Working rules

- Verify behavior against code before advising or editing. Identify the source
  when it matters; explain uncertainties and challenge approaches that do not fit.
- Check callers, configuration, generated output, documentation, and downstream
  consumers as part of a change. Preserve least privilege and explicit validation.
- Track implementation work in an issue before starting. Use the Firebolt team
  and Firebolt Operator project for Linear work.
- Work on an issue-prefixed branch from latest `main`, unless instructed otherwise.
  Never implement or commit directly on `main`; changes land through reviewed PRs.
- Keep commits self-contained. Separate refactors from behavior changes; introduce
  types and CRD changes before the controller logic that consumes them.
- Preserve unrelated changes. Stage explicit paths or hunks; never use
  `git add .`, `git add -A`, or `git add --all`.
- Follow [the PR template](.github/PULL_REQUEST_TEMPLATE.md): actual issue reference
  and reason in Background, resulting behavior in Summary, coverage in Test Plan.
- Do not ignore errors without a documented rationale. State what was tested and
  what was not verified; explain any behavior that the current harness cannot test.

### Commit conventions

Use `<type>(<scope>): <description> (<issue-ref>)`. Scope is optional; common
scopes are `api`, `controller`, `helm`, and `e2e`. Types are `build`, `chore`,
`ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`, and `test`.
Use an imperative, lowercase subject under 100 characters, without a final period.
Take the issue reference from the tracked work. Keep tracker references out of
source comments, identifiers, strings, and documentation prose.

CI-only changes use `ci:`, never `fix(ci):`; release-please uses the commit type
for release eligibility. Explain the reason for non-obvious changes in the body.

## Validation

Bare `make` prints help. After every code change, run `make build && make lint`.
Use `make lint-fix` where appropriate. Add or update tests for changed behavior,
preferably fast deterministic tests at the lowest useful level.

| Change | Relevant targets |
| --- | --- |
| CRD markers, RBAC, or API types | `make manifests generate` |
| Controller behavior | `make test`; focused E2E where integration matters |
| Admission registration/defaulting | `make test-webhook-integration` |
| Reconcile invariants | `make test-property` and applicable formal targets |
| Helm templates or values | `make helm-lint helm-template helm-docs` |
| Documentation | `make docs-check` |
| Plugin contract | `go build ./...`, `go test ./internal/infra/...`, `make kubectl-firebolt` |

Make targets default to `IMAGE_VARIANT=latest`; set `IMAGE_VARIANT=dev` for the
other image set. Raw Go commands without tags embed the dev defaults. `e2e`
enables E2E tests and crash injection; `heavy` adds heavy query configurations;
`latest` selects release-oriented defaults. Never enable the test-only
`wakehandofftest` tag in release images.

Run focused E2E specs through `make test-e2e GINKGO_FOCUS='...'`, not Ginkgo
directly. The operator runs in process; workload pods run in Kind. Reuse the
existing cluster and local image registry; do not delete Docker images or Kind
clusters as a debugging shortcut. Follow the E2E guide for preparation and waits.

- Do not accept transient query failures in zero-downtime assertions.
- Poll for conditions instead of long fixed sleeps; focused transitions should
  converge within 15 seconds. Use existing readiness budgets for provisioning.
- Inspect engine container logs when pods fail to become ready.
- Use `WithGC()` for specs that abandon a generation through mid-flight edits;
  happy-path specs keep abandoned-generation GC disabled.

## Implementation safeguards

### Reconcilers and formal models

- Keep protocol and state-transition changes aligned with their TLA+ specs,
  generated fixtures, and Go tests. Consult [model scope](formal/model-scope.tsv)
  before claiming formal coverage; a passing model does not prove unmodeled code.
- Use [the shared generator](scripts/gen-tla-state-tests.py) for fixtures. Maintain
  invariant/action mappings and declare unmodeled functions with a reason.
- Run `make formal-check formal-check-counterexample formal-verify` as applicable.
  Run `make formal-check-mutants` from a clean tree when changing its covered
  reconciler or harness. Repair stale mutants; do not delete negative controls or
  weaken guards to make checks pass.
- If a touched function does not change its model, explain the abstraction boundary
  with `Model-scope-ok: <reason>` in the commit or PR.
- Preserve the documented shutdown and retry contract. Investigate its existing
  layers before adding EndpointSlice watches, permissions, or cutover gates.

### Templates, inheritance, and admission

- Builders and drift detection must use the same `effective*` helpers. Keep Preset
  overlays aligned when adding inherited fields; retain empty-Preset equivalence.
- Preserve namespaced-first class resolution and Engine → Preset → class → default
  precedence for fields those objects support. Keep cluster classes SKU-only.
- Do not add CRD defaults to inherited `rollout` or `drainCheckEnabled` fields;
  apply defaults last in the resolver so omission still permits inheritance.
- Keep pod-affecting comparisons separate from policies read live. Reuse the shared
  image and pull-policy resolvers; explicit overrides take precedence.
- Maintain shared admission and controller validation, including identical runtime
  bounds. Add controller/envtest coverage with webhooks disabled; use CRD/CEL
  validation for invariants that must hold on every API-server write.
- Preserve render-validation gates without interrupting an ongoing switch, drain,
  or cleanup. Follow the validation guide for each object's enforcement layer.
- Put operator-owned path rules in `api/v1alpha1/operatorauthority.go`. Add a
  Secret-backed volume to both the reserved-volume and Secret-volume sets; add a
  non-Secret volume only to the reserved set.
- Do not infer or rewrite metadata images from `spec.metadataNG`; compatible image
  selection belongs to the user.
- Keep the typed `kubectl-firebolt` client aligned with API and readiness changes.
  Fix consumer build errors rather than weakening its types.

### RBAC and generated output

Edit kubebuilder RBAC markers and run `make manifests`. The canonical
`config/rbac/role.yaml` feeds `scripts/sync-helm-rbac.py`; never hand-edit generated
`helm/firebolt-operator/templates/manager-rbac.yaml`.

Preserve both cluster-wide and namespace-scoped chart envelopes and their tests.
Keep `pods/proxy` access behind `rbac.apiserverProxyGrant`, outside the canonical
manager role. Namespace-scoped installs still need read-only cluster-class access.

## CI and releases

Read the release guide before changing release automation. Preserve independent
component versions, operator-only `appVersion` synchronization, and publication
based on component release outputs. Keep non-shipping paths excluded from the
root application component, `always-update` enabled, and `chore(deps)` visible in
release notes.

- Use `scripts/ci/fetch-verified.sh <name> <dest>` for downloaded third-party binary
  artifacts; pin their URL and SHA-256 in `scripts/ci/pinned-tools.tsv`. Update
  both together. Download and verify before unpacking; never pipe downloads into
  `tar` or `sh`. Follow the formal guide for the archived model-checker tool.
- Revalidate cached artifacts. Keep manifest dependencies and cache keys aligned
  with pin changes; retain the checks in `internal/ci/pinned_tools_test.go`.
- Required checks must report on every PR. For conditional or matrix workers,
  preserve stable rollup job names with `if: always()`, no permissions, and explicit
  failure on detection errors or unexpected skips. Do not hide required checks
  behind workflow-level path filters.
- Release-PR test shortcuts must verify the branch prefix, same-repository origin,
  and expected bot author. Forks and lookalike branches run the full suites.
- Workflows that create external state must receive `closed` events for cleanup.
  Put relevance checks inside the job rather than trigger-level path filters.

## Documentation and instruction maintenance

- Keep README an overview. Put implementation explanations and operational
  procedures in the relevant `docs/` or `dev-docs/` guide.
- Update `docs/architecture.mdx` in the same commit as changes to state machines,
  reconciler flow, data-plane contracts, or RBAC. Keep other affected guides and
  published references in sync with code.
- Keep AGENTS.md focused on actionable rules and links to authoritative guides.
  Put directory-specific rules in the nearest scoped AGENTS.md. Do not duplicate
  implementation walkthroughs, incident narratives, or lists of completed fixes.
- Record only unresolved issues that need a current workaround, briefly stating
  symptom, cause, and workaround. Remove them when resolved.
- Describe current behavior in comments and docs. Avoid edit narratives,
  historical commit IDs, and tracker references.
