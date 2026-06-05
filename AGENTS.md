# AGENTS.md

## What this project does

Firebolt Kubernetes Operator manages three CRDs -- **FireboltInstance** (shared infrastructure per namespace: PostgreSQL, Pensieve metadata service, Envoy gateway), **FireboltEngine** (stateful compute nodes with zero-downtime blue-green deployments), and **FireboltEngineClass** (optional namespaced pod-template fragment shared by multiple engines in the same namespace; namespaced rather than cluster-scoped so the SA / volume / IAM identifiers it carries resolve consistently). Built with Go and controller-runtime.

Engines require a ready instance. See [docs/architecture.mdx](docs/architecture.mdx) for the full design.

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
docs/                  # published Mintlify docs (MDX + docs.json)
docs-internal/         # internal design notes, SDLC, slides (not published)
examples/              # user-facing example manifests
formal/                # TLA+ specifications and model-checker configs
helm/
  firebolt-operator/   # operator Helm chart
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
- `make setup-local-registry` -- start the local Docker registry that kind nodes mirror through (idempotent)
- `make flush-local-registry` -- drop and recreate the local registry (clears cached images)
- `make prepare-test-e2e` -- create Kind cluster and publish test images to the local registry
- `make local-deploy` -- build operator, load into Kind, deploy via Helm
- `make local-undeploy` -- remove the operator Helm release
- `make cleanup-test-e2e` -- delete the Kind cluster (local registry is left running)

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
- **docs/architecture.mdx** -- architectural changes (state-machine phases, reconciler control flow, gateway/data-plane contracts, drain/shutdown handling, RBAC surface) MUST include a matching update in the same commit. This is the canonical record of *why* the system is shaped the way it is.
- **docs/** and **docs-internal/** -- keep published docs (`docs/`) and internal design notes (`docs-internal/`) up to date as code changes. Documents about historical or unimplemented features in `docs-internal/` do not need updates unless they become wrong.

A pull request that changes structure or interfaces without a documentation update is incomplete.

## Document resolved issues

You MUST capture non-obvious problems you solve. When you encounter and fix a quirk, footgun, surprising framework behavior, environment trap, or anything that took meaningful debugging:

- Add a short entry to [KNOWN_ISSUES.md](KNOWN_ISSUES.md) at the repo root, or to the scoped `KNOWN_ISSUES.md` in the module where the issue lives.
- State the symptom, the cause, and the resolution in two or three sentences.
- This prevents the next agent from rediscovering the same problem.

If the issue is project-wide, put it in the root `KNOWN_ISSUES.md`. If it is scoped to one module, put it in that module's `KNOWN_ISSUES.md`.

## Proactive harness

Code is not done until it is covered by tests where tests are reasonable.

- You MUST add or update tests for behavior you introduce, change, or fix. Use the project's existing test runners: `make test` for unit tests (envtest), `make test-e2e` for E2E tests (Kind + Ginkgo).
- If a change is genuinely untestable in the current harness (e.g., cloud-only infrastructure, external service integration), you MUST say so explicitly in your response and explain why.
- Prefer fast, deterministic tests at the lowest reasonable level (unit > integration > end-to-end).
- A change that ships without tests, and without an explicit reason for not having them, is incomplete.

## Known issues

See [KNOWN_ISSUES.md](KNOWN_ISSUES.md) for the running log of non-obvious problems, framework footguns, and environment traps. Add new entries there, not here.

- **GitHub Actions `pull_request.paths` filters also suppress `closed` events.** If a workflow creates external state on `opened` or `synchronize` and must clean it up on `closed`, do not rely on trigger-level `paths` filters. Put path relevance checks inside the job instead, and let `closed` events reach the workflow so cleanup can run even when the final PR diff no longer includes the original files.

## Project-specific rules

### Engine reconciler and blue-green state machine

Before touching the engine reconciler -- especially `engine_reconcile.go`, `engine_state.go`, or `instance_gateway.go` -- read the relevant section of [docs/architecture.mdx](docs/architecture.mdx) first.

- The blue-green state machine (`creating -> switching -> draining -> cleaning`) is formalised in [formal/FireboltEngine.tla](formal/FireboltEngine.tla). A change in one belongs in both, and `make formal-verify` is the CI guard.
- Zero-downtime during pod termination is enforced by a layered data-plane contract (headless DNS, Envoy active health check, engine `/health/ready=503` on SIGTERM, engine pre-work shutdown fence, gateway retry on `X-Firebolt-Drained`). The "Graceful pod shutdown" and "Why no EndpointSlice gate" subsections of `docs/architecture.mdx` document the chain and call out a previously-removed design (FB-661) that should not be reintroduced. If you find yourself adding an EndpointSlice watch / RBAC / state field to fix a 5xx during cutover, check whether one of the existing layers is broken before adding a sixth.

### FireboltEngineClass merge layer

FireboltEngineClass holds a reusable pod-template fragment that engines in the same namespace reference via `spec.engineClassRef`. FireboltEngineClass is namespaced — the resolver, the FireboltEngine admission check, the controller watch, and the deletion-blocking webhook all key by `{Namespace, Name}`, not `Name` alone. The merge runs inside `buildStatefulSet`, with the resolved class hash stamped on the StatefulSet as `firebolt.io/engine-class-hash` so `stsMatchesSpec` detects class drift (either an in-place class spec edit or a flip to a different class in the same namespace).

- Centralised in the `effective*` helpers (`engine_reconcile.go`): `effectiveServiceAccountName`, `effectiveNodeSelector`, `effectiveTolerations`, `effectiveAffinity`, `effectivePodLabels`, `effectivePodAnnotations`, `effectiveEngineImage`, `engineClassSidecars`, `engineClassInitContainers`. Every helper has the same shape: takes `(spec, classInfo)` and returns the merged value. `buildStatefulSet` and `stsMatchesSpec` MUST consume the same helper for any given field — divergence produces phantom drift (rebuilds-without-changes, infinite rollouts).
- The operator-owned set on `FireboltEngineClass.spec.template` lives in `api/v1alpha1/operatorauthority.go` (`ValidateOperatorOwnedPodTemplate`). The validating webhook returns those `field.Forbidden` errors verbatim; the FireboltEngineClass status controller re-runs the same check as a defense-in-depth `Ready=False/OperatorOwnedFieldSet` signal. Adding a new operator-owned field belongs in `ValidateOperatorOwnedPodTemplate` and propagates everywhere automatically.
- Image and pull-policy on the engine container were intentionally moved out of `FireboltEngineSpec.Image` into the FireboltEngineClass template (FB-1145). There is no per-engine image override; rolling a new image is done by mutating the referenced class. The e2e Image Switching test in `test/e2e/e2e_test.go` is the canonical pattern.

### RBAC changes

When canonical RBAC changes (typically regeneration of [`config/rbac/role.yaml`](config/rbac/role.yaml) from `// +kubebuilder:rbac:` markers via `make manifests`), review whether [`helm/firebolt-operator/templates/clusterrole.yaml`](helm/firebolt-operator/templates/clusterrole.yaml) needs matching edits -- it is hand-maintained separately from kubebuilder output.

### Admission webhooks and controller-side fallbacks

The operator supports two admission postures and every invariant has to behave the same in both. The operator CLI flag `--enable-webhooks` defaults to `true`, but the Helm chart sets `webhook.enabled: false` by default (cert bootstrap is the caller's responsibility), so the realistic shipped state is **webhooks off**. `make test`, `make test-e2e`, and `make helm-test` all run with webhooks off too — the in-process operators and envtest setup do not register the webhook server.

Every webhook-enforced invariant has a controller-side counterpart so the same CR write produces the same outcome regardless of admission. When touching any of these, change both sides together and add envtest coverage for the controller branch:

- **`FireboltInstance.spec.id` defaulting.** Mutating defaulter mints a ULID at admission. Controller fallback at `instance_controller.go:115` mints and `Update`s on the first reconcile; the CRD's CEL rule `oldSelf == '' || self == oldSelf` (`fireboltinstance_types.go:301`) is what lets the controller's empty-to-value Update through.
- **`FireboltInstance.spec.metadata.{postgres.credentialsSecretRef.Name,replicas}`.** Webhook rejects. CRD CEL enforces `replicas == 1`. Controller's `checkExternalPostgresSecret` surfaces the empty-secret case as `MetadataReady=False/PostgresSecretPreflightFailed`.
- **`FireboltInstance.spec.{gateway,metadata}.template` operator-owned paths.** Webhook walks both templates with `GatewayPodTemplateRules` / `MetadataPodTemplateRules`. Controller's `validateInstanceTemplates` re-runs the same rules every reconcile and surfaces `{Gateway,Metadata}Ready=False/TemplateRejected` with the field path; the offending component is not rendered.
- **`FireboltEngineClass.spec.template` operator-owned paths.** Webhook walks `FireboltEngineClassPodTemplateRules`. `FireboltEngineClassReconciler.classReadiness` stamps `Ready=False/OperatorOwnedFieldSet` as defense-in-depth, AND `FireboltEngineReconciler.resolveFireboltEngineClassInfo` reads that condition: when set, it returns `errFireboltEngineClassUnready` and Reconcile surfaces `engine.ConditionReady=False/FireboltEngineClassUnready` with a pointer to the offending class — the engine StatefulSet is NOT rendered until the class becomes admissible.
- **`FireboltEngineClass` deletion while bound.** Webhook refuses DELETE. `FireboltEngineClassReconciler` carries the finalizer `compute.firebolt.io/fireboltengineclass-deletion-guard`; while a `deletionTimestamp` is set and a bound engine exists, the reconciler holds the finalizer and stamps `Ready=False/DeletionBlocked` with the count. Force-removing the finalizer (`kubectl patch metadata.finalizers`) is the legitimate escape hatch.
- **`FireboltEngine.spec.engineClassRef`.** Webhook rejects when the named class is missing in the engine's namespace. Controller surfaces `errFireboltEngineClassNotFound` via log + backoff (no dedicated condition by design — class state is binary, missing-class is rare in practice).
- **`FireboltEngine.spec.template` operator-owned paths.** Webhook walks the engine's own template with `FireboltEngineClassPodTemplateRules` (engine and class templates merge into one pod, so they share the contract). Controller's `validateEngineTemplate` re-runs the same rules every reconcile and surfaces `engine.ConditionReady=False/TemplateRejected` with the field path; the engine StatefulSet is not rendered. Closes the webhook-off gap where a reserved engine env key (`POD_INDEX`, `FIREBOLT_CORE_MODE`, `FB_AWS_EC2_METADATA_CLIENT_ENABLED`) set on `spec.template` would otherwise be appended after the operator-injected vars in `buildEngineContainerEnv` and win under the kubelet's last-wins env semantics (FB-1492). Reuses the FireboltInstance controller's `TemplateRejected` reason.
- **`FireboltEngine.spec.resources` bounds (`--engine-max-*`).** Webhook rejects. Controller's `r.ResourceBounds.Validate` runs every reconcile via the same code path and surfaces `engine.ConditionReady=False/ResourceBoundsExceeded` with the field path and the configured maximum. Plumb the bounds from `cmd/main.go` into both the webhook and the reconciler so they cannot diverge.

CRD CEL rules (the third enforcement layer) are independent of webhook state — they are baked into the CRD and run inside the apiserver. Use them for invariants where webhook + controller cannot together close a race (the only current example is the `spec.id` empty-to-ULID transition).

The full per-invariant matrix lives at [`docs-internal/webhook-hardening-plan.md`](docs-internal/webhook-hardening-plan.md).

### Local image-loading mechanism

E2E and helm-test workflows publish workload images (engine, metadata, postgres, envoy, curl) to a **local Docker registry container** rather than copying each image into every kind node's containerd snapshotter via `kind load docker-image`. The legacy `kind load` flow duplicates the multi-GB engine image per node and overflows Docker Desktop's default ~64 GB VM disk on multi-node clusters.

Layout:

- Container: `kind-registry` (image `registry:2`), exposed on `127.0.0.1:5001`, attached to the `kind` Docker network so kind nodes resolve it as `kind-registry:5000`.
- Each kind node has `containerdConfigPatches` pointing at `/etc/containerd/certs.d`, plus per-host `hosts.toml` files written by `scripts/setup-kind-cluster.sh` that alias `ghcr.io` and `docker.io` to `http://kind-registry:5000`. Containerd hot-reloads `certs.d`, no daemon restart.
- `scripts/load-e2e-images.sh` does `docker pull -> docker tag -> docker push -> docker rmi` per image so Docker's local content store stays empty after publishing.
- The image-switch E2E specs need a synthetic `<tag>-uptest` reference; the load script publishes the same engine / metadata content under both `${TAG}` and `${TAG}-uptest`. OCI registries dedupe by digest, so the second tag is just a manifest write. Keep `upgradeTagSuffix` in `test/e2e/e2e_suite_test.go` in sync with `UPGRADE_TAG_SUFFIX` in the load script.

Operations:

- Start / repair: `make setup-local-registry` (idempotent; called transitively by `make setup-kind`).
- Flush stale cache: `make flush-local-registry` (`docker rm -f kind-registry` + recreate). Use after a kind upgrade changes the network, or when a malformed push is masking a real bump.
- Verify: `curl http://localhost:5001/v2/_catalog`.

The registry persists across `make cleanup-test-e2e` (which only deletes the kind cluster) so the next `make prepare-test-e2e` reuses cached layers. `kind-load-operator` (used by `make local-deploy`) still uses `kind load docker-image` because the operator binary image is small and built locally — not worth registry-ifying.

### Testing rules

**Unit tests** (`internal/controller/*_test.go`): use envtest (embedded K8s API server). Run with `make test`.

**E2E tests** (`test/e2e/`): require a Kind cluster with images published to the local registry (see "Local image-loading mechanism"). Build tag `e2e`; heavy stress tests use tag `e2e,heavy`. The operator runs **in-process** during E2E -- it is not deployed into the cluster.

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
