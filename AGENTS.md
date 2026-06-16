# AGENTS.md

## What this project does

Firebolt Kubernetes Operator manages three CRDs -- **FireboltInstance** (shared infrastructure per namespace: PostgreSQL, Pensieve metadata service, Envoy gateway), **FireboltEngine** (stateful compute nodes with zero-downtime blue-green deployments), and **FireboltEngineClass** (optional namespaced fragment of shared engine settings -- a pod template plus storage, engine-config, rollout/autoStop, and optional Engine Web UI sidecar defaults -- referenced by multiple engines in the same namespace; namespaced rather than cluster-scoped so the SA / volume / IAM identifiers it carries resolve consistently). Built with Go and controller-runtime.

Engines require a ready instance. See [docs/architecture.mdx](docs/architecture.mdx) for the full design.

## Repo structure

```
api/v1alpha1/          # CRD type definitions and webhooks
cmd/main.go            # operator entry point
cmd/kubectl-firebolt/  # kubectl-firebolt plugin entry point
config/
  crd/bases/           # generated CRD manifests
  images/              # embedded default image tags (dev / latest variants)
  rbac/                # generated RBAC manifests (kubebuilder output)
  manager/             # manager deployment manifest
  samples/             # example CRs
docs/                  # published Mintlify docs (MDX + docs.json)
examples/              # user-facing example manifests
formal/                # TLA+ specifications and model-checker configs
helm/
  firebolt-operator/   # operator Helm chart
  firebolt-operator-crds/ # CRD-only Helm chart
internal/
  controller/          # reconcilers, state machines, gateway, drain logic
  infra/               # kubectl-firebolt plugin library (typed CRs + kubectl)
  metrics/             # Prometheus metric recorders
scripts/               # CI helpers, Kind setup, image loading, code generation
test/
  e2e/                 # Ginkgo E2E tests (build tag: e2e)
  testhelpers/         # shared test utilities
```

Unit tests live alongside their source files in `internal/controller/` and `internal/metrics/`. E2E tests are isolated under `test/e2e/`.

## Build, test, and lint

- `make` / `make help` -- print the Firebolt logo and the lifecycle-grouped target list (this is the default goal; bare `make` does **not** build)
- `make build` (or `make all`) -- compile to `bin/manager`
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
- `make prepare-test-e2e` -- full e2e setup: regenerate manifests/DeepCopy, create or reuse the Kind cluster, and publish test images to the local registry (runs `manifests generate setup-kind load-test-images`)
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

### Release workflow (main)

[`/.github/workflows/release-main.yaml`](.github/workflows/release-main.yaml) runs on every push to `main`:

- **release-please** maintains separate release PRs for the app (`version.txt`) and each Helm chart, accumulating conventional commits until merge.
- **Pre-releases** — every app and/or Helm change on `main` builds artifacts tagged `{next-version}-pre.0.{utc-datetime}.{short-sha}` (for example `0.2.0-pre.0.20260613004124.a71720862062`). The next version comes from the open release-please PR when one exists, otherwise from [`.release-please-manifest.json`](.release-please-manifest.json). Pre-release images and charts are build/package validation only — they are not pushed to GHCR.
- **Official releases** — merging a release-please PR creates the GitHub release/tag. release-please is the single authority over every component's version (`version.txt` and each chart's `version`/manifest entry); the workflow never bumps a chart `version` out of band. An app release builds and pushes the operator image to GHCR (tag `vX.Y.Z` + `:latest`, cosign-signed); a chart release packages and pushes the OCI charts to GHCR.
- **App → chart appVersion coupling** — an **app** release additionally runs `sync-chart-appversion`, which rewrites only the two charts' `appVersion` to the new operator image tag (`vX.Y.Z`) and commits it as `chore(deps): set chart appVersion to vX.Y.Z`. release-please cannot reference another component's version itself ([googleapis/release-please#2655](https://github.com/googleapis/release-please/issues/2655)), so this minimal glue exists — but it touches `appVersion` only, never chart `version` or the manifest. The `deps` scope is deliberate: the `chore`+`deps` entry in `release-please-config.json` `changelog-sections` is unhidden, so the commit renders under a "Dependencies" section **and** counts as a releasable change for each chart component (release-please's `helm` release-type gates a release purely on a non-empty rendered changelog, with no commit-type allowlist). That `chore(deps)` commit flows back through release-please as ordinary chart release PRs; **merging those chart PRs is what bumps each chart `version` and publishes the chart.** So an app release publishes its charts in a follow-up step, not atomically.
- **Single chart publisher** — `release-helm-standalone` publishes charts whenever release-please cuts a chart release (a chart release PR merged), whatever opened that PR (a plain `helm/**` change or the app-driven `chore(deps)` commit). It stands down when an app release happens in the same run, because that run only opens the chart PRs rather than publishing.

The operator image tag is `vX.Y.Z` (matching the chart `appVersion`), distinct from the component-scoped git release tags release-please creates (`firebolt-operator-v<v>`, `firebolt-operator-chart-<v>`, `firebolt-operator-crds-chart-<v>`).

## Proactive Collaboration

You are expected to operate as a strategic collaborator, not a code generator.

- **Verify; never assume.** Do not assume how something works -- confirm it against the source of truth (the code first, documentation second) before acting or advising. State the fact you relied on (file, line, target, or doc) when it matters. An unverified claim is a bug waiting to happen.
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
- **docs/** -- keep the published docs (`docs/`) up to date as code changes.

A pull request that changes structure or interfaces without a documentation update is incomplete.

## Proactive harness

Code is not done until it is covered by tests where tests are reasonable.

- You MUST add or update tests for behavior you introduce, change, or fix. Use the project's existing test runners: `make test` for unit tests (envtest), `make test-e2e` for E2E tests (Kind + Ginkgo).
- If a change is genuinely untestable in the current harness (e.g., cloud-only infrastructure, external service integration), you MUST say so explicitly in your response and explain why.
- Prefer fast, deterministic tests at the lowest reasonable level (unit > integration > end-to-end).
- A change that ships without tests, and without an explicit reason for not having them, is incomplete.

## How we work (agentic workflow)

Every change is tracked and lands through a branch and a pull request -- no exceptions.

- **A documented issue comes first.** Every task MUST have a corresponding issue before any work starts -- a Linear ticket or a GitHub issue. If none exists, create one. No issue, no work.
- **Never touch `main` directly.** You MUST NEVER work in, commit to, or push to `main`. All work happens on a feature branch and merges via a reviewed PR. `main` is only ever advanced by merging a PR or by the release automation (release-please merging a release PR, and the `sync-chart-appversion` job's `chore(deps)` appVersion commit).
- **Branch names tie the branch to its issue**, so work is traceable. Prefix the branch with the tracker identifier:
  - Linear: `FB-<number>-<short-kebab-description>` (e.g. `FB-<number>-pods-not-ready-message`).
  - GitHub issue: `<issue-number>-<short-kebab-description>`.
  - Branch off the latest `main`, not off another in-flight feature branch.
- **PRs follow the template.** Pull requests MUST follow [`.github/PULL_REQUEST_TEMPLATE.md`](.github/PULL_REQUEST_TEMPLATE.md): fill in **Background** (with `ISSUE-REF`), **Summary** (what changes and what it means going forward), and **Test Plan**. Re-check the template before opening a PR in case it has changed.
- **Conventional-commit subjects drive releases.** [release-please](release-please-config.json) opens release PRs on `main` that accumulate conventional commits for the app (`version.txt`) and each Helm chart (`helm/firebolt-operator`, `helm/firebolt-operator-crds`). Merging a release PR creates the GitHub release and tag. Every commit subject MUST follow the format in "Commit conventions" (under Project-specific rules below).

Linear specifics:

- **Team:** `Firebolt` (key `FB`). All issue identifiers use the `FB-` prefix.
- **Project:** `Firebolt Operator`.
- Any new Linear issue created in connection with this repo MUST be filed under team `Firebolt` AND project `Firebolt Operator`. Filing it in the team without the project leaves it unscoped.

## Known issues

Non-obvious problems, framework footguns, and environment traps that can still bite the next agent are recorded inline below.

- **GitHub Actions `pull_request.paths` filters also suppress `closed` events.** If a workflow creates external state on `opened` or `synchronize` and must clean it up on `closed`, do not rely on trigger-level `paths` filters. Put path relevance checks inside the job instead, and let `closed` events reach the workflow so cleanup can run even when the final PR diff no longer includes the original files.
- **release-please root component (`.`) greedily owns sub-package commits.** A release-please component whose path is `.` is treated as a meta-package that owns **every** file in the repo, including files under nested component paths (matching is `file.indexOf("<path>/") === 0`, and `.` matches everything). Two consequences: (1) a commit touching only `helm/**` is attributed to the chart components **and** the root app component, opening a spurious app release PR — and because an app release commits a `chore(deps)` appVersion change, that creates a release cycle (app release → `chore(deps)` → new app PR); (2) a commit touching any **non-shipping** path (`docs/**`, `examples/**`, `formal/**`) with a **releasable** type (`fix:`, `feat:`) cuts a real operator release for a change that never touched the operator binary. Fix for both: list every non-operator directory in the `.` package's `"exclude-paths"` (`["helm", "docs", "examples", "formal"]`) so the root component only releases on genuine operator-shipping changes (`api/`, `cmd/`, `config/`, `internal/`). Caveats: (a) `exclude-paths` only filters by directory prefix; it cannot exclude individual root-level files ([googleapis/release-please#2266](https://github.com/googleapis/release-please/issues/2266)), so keep the `chore(deps)` appVersion commit scoped to files under `helm/` only (never a root file like `.release-please-manifest.json`); (b) release-please does **not** auto-close a release PR that is no longer warranted, so after excluding a path that already produced a spurious PR, close that PR by hand — once the path is excluded it will not be recreated.
- **release-please can't set a Helm `appVersion` from another component's version.** release-please's `helm` release-type updates only the chart `version` (and CHANGELOG), never `appVersion`. There is no native way to make a chart's `appVersion` track *another* component's version while keeping independent chart versions: `linked-versions` forces one shared version line, and cross-component references in `extra-files` are an open, unimplemented request ([googleapis/release-please#2655](https://github.com/googleapis/release-please/issues/2655)). The `sync-chart-appversion` job bridges this by committing a narrow `chore(deps): set chart appVersion to vX.Y.Z` on every app release, which then flows back through release-please as chart release PRs. Keep that glue limited to `appVersion` only — if it ever writes chart `version` or `.release-please-manifest.json`, it competes with release-please's version ownership and produces conflicting chart versions / manifest churn.
- **A `chore` commit can be releasable — it depends on `changelog-sections`, not a fixed type allowlist.** It is a common misconception that release-please only releases on `feat`/`fix`/`perf`/`revert`. That allowlist (`src/util/filter-commits.ts`) is only wired into the `node`/`python`/`java` strategies. The `helm` and `simple` release-types (what this repo uses) gate a release **solely** on `changelogEmpty()` — whether the rendered release notes are more than one line. So any commit whose type/scope maps to an **unhidden** `changelog-sections` entry is releasable. This repo's config has `{ "type": "chore", "scope": "deps", "section": "Dependencies" }` with no `hidden: true`, and the bundled `conventional-changelog-conventionalcommits` honors `scope`, so `chore(deps): …` releases (and renders under "Dependencies") while a plain `chore:` or other-scoped `chore(...)` renders nothing and does not. Footgun: adding `"hidden": true` to that entry (or dropping the `deps`-scoped entry) silently stops the `sync-chart-appversion` commit from ever cutting a chart release — the appVersion bump would land on `main` but no chart release PR would open.
- **Separate release PRs conflict in `.release-please-manifest.json` and don't self-heal by default.** With `separate-pull-requests: true`, every component's release PR edits the **same** `.release-please-manifest.json`, and the per-component entries sit on **adjacent lines**. Merging one component's PR changes a line next to another open PR's line, so git's 3-way merge flags the still-open PR as conflicting even though the edits are logically independent ([googleapis/release-please#1870](https://github.com/googleapis/release-please/issues/1870)). It does **not** auto-resolve under the default config because `maybeUpdateExistingPullRequest` skips pushing an update when the PR body is unchanged — and a sibling component's release notes don't change just because another component released, so release-please never rebases the stale branch. Fix: set `"always-update": true` (top level of `release-please-config.json`), which forces a rebuild of each open release branch off the current `main` tip every run. The related failure mode — merging two release PRs at the exact same instant → duplicate-tag errors — is handled separately by the workflow's `concurrency` group serializing runs on `main`.
- **release-please `prs` output has no `path`/`version`.** The `prs` output of `release-please-action` is an array of PullRequest objects exposing `headBranchName`, `title`, `body`, etc. — but **no** per-package `path` or `version`. A resolver that does `select(.path == $path) | .version` silently matches nothing and falls back to the committed manifest, so pre-release tags get the *current* version instead of the *next* one. Resolve the next version from each component's PR `headBranchName` (`…--components--<component>`) or `title` (`chore(release): <component> <version>`) instead — see [`scripts/ci/resolve-next-release-version.sh`](scripts/ci/resolve-next-release-version.sh). Match the component exactly to avoid the `firebolt-operator` vs `firebolt-operator-chart` prefix collision.

## Project-specific rules

### Engine reconciler and blue-green state machine

Before touching the engine reconciler -- especially `engine_reconcile.go`, `engine_state.go`, or `instance_gateway.go` -- read the relevant section of [docs/architecture.mdx](docs/architecture.mdx) first.

- The blue-green state machine (`creating -> switching -> draining -> cleaning`) is formalised in [formal/FireboltEngine.tla](formal/FireboltEngine.tla). A change in one belongs in both, and `make formal-verify` is the CI guard.
- Zero-downtime during pod termination is enforced by a layered data-plane contract (headless DNS, Envoy active health check, engine `/health/ready=503` on SIGTERM, engine pre-work shutdown fence, gateway retry on `X-Firebolt-Drained`). The "Graceful pod shutdown" and "Why no EndpointSlice gate" subsections of `docs/architecture.mdx` document the chain and call out a previously-removed design that should not be reintroduced. If you find yourself adding an EndpointSlice watch / RBAC / state field to fix a 5xx during cutover, check whether one of the existing layers is broken before adding a sixth.

### FireboltEngineClass merge layer

FireboltEngineClass holds a reusable pod-template fragment plus defaults for a subset of non-template engine settings (`uiSidecar`, `storage`, `customEngineConfig`, `rollout`, `drainCheckEnabled`, `drainCheckInterval`, `autoStop`) that engines in the same namespace reference via `spec.engineClassRef`. `uiSidecar: true` makes `buildStatefulSet` inject a built-in UI sidecar (the operator-owned `engine-web` nginx container, via `effectiveSidecarsWithUI`); the `engine-web` container name is reserved in `FireboltEngineClassPodTemplateRules` (like `engine`), so a user container/init container with that name is rejected and injection needs no collision guard. Like `storage` it reshapes the pod, so `stsMatchesSpec` detects a toggle through the sidecar comparator and rolls a new generation. FireboltEngineClass is namespaced — the resolver, the FireboltEngine admission check, the controller watch, and the deletion-blocking webhook all key by `{Namespace, Name}`, not `Name` alone. Every inherited field resolves **engine-if-set → class-if-set → operator default**. The pod-template / storage merge runs inside `buildStatefulSet` and the config merge inside `buildConfigMap`, with the resolved class template hash stamped on the StatefulSet as `firebolt.io/engine-class-hash` so `stsMatchesSpec` detects template drift (an in-place class spec edit or a flip to a different class in the same namespace).

- Centralised in the `effective*` helpers (`engine_reconcile.go`): template fields (`effectiveServiceAccountName`, `effectiveNodeSelector`, `effectiveTolerations`, `effectiveAffinity`, `effectivePodLabels`, `effectivePodAnnotations`, `effectiveEngineImage`, `engineClassSidecars`, `engineClassInitContainers`) and non-template fields (`effectiveStorage`, `effectiveCustomEngineConfig`, `effectiveRollout`, `effectiveDrainCheckEnabled`, `effectiveAutoStop`, and the class-aware `getDrainCheckInterval`). Every helper has the same shape: takes `(spec, classInfo)` and returns the merged value. `buildStatefulSet` and `stsMatchesSpec` MUST consume the same helper for any given field — divergence produces phantom drift (rebuilds-without-changes, infinite rollouts).
- Class drift for the two pod-affecting non-template fields is detected by their own effective-aware comparators, NOT the class hash: `storageMatchesSpec(sts, spec, classInfo)` and `customEngineConfigHash(spec, classInfo)` both resolve through the effective helper, so a class storage/config edit rolls a new generation. `rollout` / `drainCheckEnabled` / `drainCheckInterval` / `autoStop` do not reshape the pod and are read live, so they are intentionally absent from both the hash and `stsMatchesSpec`. The `firebolt.io/engine-class-hash` annotation covers only `spec.template`.
- `rollout` and `drainCheckEnabled` carry NO `+kubebuilder:default` on `FireboltEngineSpec`: the default (graceful / true) is applied last in the `effective*` resolver, not by the apiserver, so an unset engine value stays empty and can fall through to the class. Re-adding a CRD default to either field would silently disable class inheritance for it.
- The operator-owned set on `FireboltEngineClass.spec.template` lives in `api/v1alpha1/operatorauthority.go` (`ValidateOperatorOwnedPodTemplate`). The validating webhook returns those `field.Forbidden` errors verbatim; the FireboltEngineClass status controller re-runs the same check as a defense-in-depth `Ready=False/OperatorOwnedFieldSet` signal. Adding a new operator-owned field belongs in `ValidateOperatorOwnedPodTemplate` and propagates everywhere automatically.

### kubectl-firebolt plugin

`internal/infra/` + `cmd/kubectl-firebolt/` implement the `kubectl-firebolt` plugin — a command-line client that creates and inspects FireboltEngine / FireboltInstance / FireboltEngineClass resources. It builds those CRs from the typed `api/v1alpha1` structs and applies them with `kubectl`, so it is a first-class consumer of the CRD surface that compiles against the same types.

- **Any large functional change to the operator MUST be verified to stay in line with the plugin.** This covers CRD field shapes, the engine-create contract (`engineClassRef`, the per-engine `spec.template` overrides, storage, `customEngineConfig`), resource kinds/group, and readiness conditions. Update `internal/infra/` in the same change and confirm `go build ./...`, `go test ./internal/infra/...`, and `make kubectl-firebolt` still pass.
- Because the plugin compiles against the CRD types, a field rename surfaces as a build error in `internal/infra/`. Fix it by realigning the plugin — do **not** loosen it to hide the change. See [internal/infra/AGENTS.md](internal/infra/AGENTS.md) for the plugin's own invariants.
- Image and pull-policy on the engine container live only on the FireboltEngineClass template; `FireboltEngineSpec` deliberately has no image field. There is no per-engine image override; rolling a new image is done by mutating the referenced class. The e2e Image Switching test in `test/e2e/e2e_test.go` is the canonical pattern.

### RBAC changes

`config/rbac/role.yaml` is the canonical RBAC manifest, regenerated from `// +kubebuilder:rbac:` markers via `make manifests` (controller-gen step). The Helm chart template [`helm/firebolt-operator/templates/manager-rbac.yaml`](helm/firebolt-operator/templates/manager-rbac.yaml) is GENERATED from that canonical file by [`scripts/sync-helm-rbac.py`](scripts/sync-helm-rbac.py), which `make manifests` runs immediately after controller-gen. So a `// +kubebuilder:rbac:` marker edit + `make manifests` updates both files in lockstep. Do not hand-edit the chart template — the next `make manifests` run will overwrite the edit. Historical drift between the two files used to require a hand-merge step (`pods/proxy` missing after b284983, `endpointslices` dead since e538444); the sync script removes that drift entirely.

The chart picks the RBAC envelope at install time from `.Values.watchNamespaces`:
- Empty list (default) → one `ClusterRole` + one `ClusterRoleBinding` (cluster-wide install).
- Non-empty list → a `Role` + `RoleBinding` pair in each listed namespace, no `ClusterRole`. Same rules block; the operator's `cache.Options.DefaultNamespaces` is scoped to the same list via `--namespaces=<comma-list>`.

[`internal/controller/rbac_chart_toggle_test.go`](internal/controller/rbac_chart_toggle_test.go) pins the four-cell `watchNamespaces × rbac.apiserverProxyGrant` matrix against `helm template`. If a chart edit silently drops the per-NS branch or renders both envelopes at once, that test fails.

`pods/proxy: get` is intentionally NOT in the canonical RBAC because the default `FireboltInstance.spec.metricScrapeMode=PodIP` doesn't use it. The chart template [`helm/firebolt-operator/templates/apiserver-proxy-rbac.yaml`](helm/firebolt-operator/templates/apiserver-proxy-rbac.yaml) renders the grant on the opt-in `rbac.apiserverProxyGrant=true` value and mirrors the same `watchNamespaces` shape. If you add a `// +kubebuilder:rbac:groups="",resources=pods/proxy,verbs=get` marker back, the verb leaks into the always-on manager RBAC — keep it out of the canonical source.

### Admission webhooks and controller-side fallbacks

The operator supports two admission postures and every invariant has to behave the same in both. The operator CLI flag `--enable-webhooks` defaults to `true`, but the Helm chart sets `webhook.enabled: false` by default (cert bootstrap is the caller's responsibility), so the realistic shipped state is **webhooks off**. `make test`, `make test-e2e`, and `make helm-test` all run with webhooks off too — the in-process operators and envtest setup do not register the webhook server.

Every webhook-enforced invariant has a controller-side counterpart so the same CR write produces the same outcome regardless of admission. When touching any of these, change both sides together and add envtest coverage for the controller branch:

- **`FireboltInstance.spec.id` defaulting.** Mutating defaulter mints a ULID at admission. Controller fallback at `instance_controller.go:115` mints and `Update`s on the first reconcile; the CRD's CEL rule `oldSelf == '' || self == oldSelf` (`fireboltinstance_types.go:301`) is what lets the controller's empty-to-value Update through.
- **`FireboltInstance.spec.metadata.{postgres.credentialsSecretRef.Name,replicas}`.** Webhook rejects. CRD CEL enforces `replicas == 1`. Controller's `checkExternalPostgresSecret` surfaces the empty-secret case as `MetadataReady=False/PostgresSecretPreflightFailed`.
- **`FireboltInstance.spec.{gateway,metadata}.template` operator-owned paths.** Webhook walks both templates with `GatewayPodTemplateRules` / `MetadataPodTemplateRules`. Controller's `validateInstanceTemplates` re-runs the same rules every reconcile and surfaces `{Gateway,Metadata}Ready=False/TemplateRejected` with the field path; the offending component is not rendered.
- **`FireboltEngineClass.spec.template` operator-owned paths.** Webhook walks `FireboltEngineClassPodTemplateRules`. `FireboltEngineClassReconciler.classReadiness` stamps `Ready=False/OperatorOwnedFieldSet` as defense-in-depth, AND `FireboltEngineReconciler.resolveFireboltEngineClassInfo` reads that condition: when set, it returns `errFireboltEngineClassUnready` and Reconcile surfaces `engine.ConditionReady=False/FireboltEngineClassUnready` with a pointer to the offending class — the engine StatefulSet is NOT rendered until the class becomes admissible.
- **`FireboltEngineClass` deletion while bound.** Webhook refuses DELETE. `FireboltEngineClassReconciler` carries the finalizer `compute.firebolt.io/fireboltengineclass-deletion-guard`; while a `deletionTimestamp` is set and a bound engine exists, the reconciler holds the finalizer and stamps `Ready=False/DeletionBlocked` with the count. Force-removing the finalizer (`kubectl patch metadata.finalizers`) is the legitimate escape hatch.
- **`FireboltEngine.spec.engineClassRef`.** Webhook rejects when the named class is missing in the engine's namespace. Controller surfaces `errFireboltEngineClassNotFound` via log + backoff (no dedicated condition by design — class state is binary, missing-class is rare in practice).
- **`FireboltEngine.spec.template` operator-owned paths.** Webhook walks the engine's own template with `FireboltEngineClassPodTemplateRules` (engine and class templates merge into one pod, so they share the contract). Controller's `validateEngineTemplate` re-runs the same rules every reconcile and surfaces `engine.ConditionReady=False/TemplateRejected` with the field path; the engine StatefulSet is not rendered. Closes the webhook-off gap where a reserved engine env key (`POD_INDEX`, `FIREBOLT_CORE_MODE`, `FB_AWS_EC2_METADATA_CLIENT_ENABLED`) set on `spec.template` would otherwise be appended after the operator-injected vars in `buildEngineContainerEnv` and win under the kubelet's last-wins env semantics. Reuses the FireboltInstance controller's `TemplateRejected` reason.
- **`FireboltEngine.spec.resources` bounds (`--engine-max-*`).** Webhook rejects. Controller's `r.ResourceBounds.Validate` runs every reconcile via the same code path and surfaces `engine.ConditionReady=False/ResourceBoundsExceeded` with the field path and the configured maximum. Plumb the bounds from `cmd/main.go` into both the webhook and the reconciler so they cannot diverge.

CRD CEL rules (the third enforcement layer) are independent of webhook state — they are baked into the CRD and run inside the apiserver. Use them for invariants where webhook + controller cannot together close a race (the only current example is the `spec.id` empty-to-ULID transition).

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
- The engine operator runs with the orphaned-generation GC **disabled by default** (`StartOperator` / `SetupTestInstance`), so happy-path specs assert the primary reconcile path never orphans a generation. A spec that drives **mid-flight spec changes** — rapid replica edits that abandon a half-built blue-green generation — MUST start its operator with `WithGC()`: the primary path deliberately leaves abandoned generations for the GC to reap, so without GC their `*-g<N>` StatefulSets linger. Symptom of getting this wrong: a spec times out in `WaitForEngineReady` waiting for *N* ready pods while extra `*-g<N>` pods from a superseded generation stay Running (it counts ready pods across all generations by the engine label).

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

Example: `fix(controller): report ready-vs-total pods in PodsNotReady message (FB-<ticket>)`

**Staging**: always add files by explicit path (`git add file1 file2`). Never use `git add -A`, `git add .`, or `git add --all` -- untracked files that are not meant to be versioned will be committed.

### Planning work

When breaking work into a plan, divide it into self-contained commits. Each commit is a single logical change -- do not mix refactors with new features. Order:

1. Refactors and type extractions first (non-functional changes).
2. New types and CRDs.
3. Modifications to existing types.
4. Controller/reconciler logic last.

### Code quality

- Ignoring errors is forbidden except with a documented rationale in a comment.
- No ticket references (`FB-<ticket>`, `FIR-<ticket>`, or any tracker ID) in code or docs: not in comments, identifiers, strings, or doc prose. Tickets go in the commit message only (see Commit conventions).
- Comments and docs state how the code behaves now, not what just changed. Do not narrate edits ("previously", "changed to", "fixed the bug where", "new:").
