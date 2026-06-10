# infra — AGENTS.md

> Scoped instructions for `internal/infra/`. See the repo-root `AGENTS.md` for project-wide rules.

## Overview

`internal/infra/` is the library behind the `kubectl-firebolt` plugin
(`cmd/kubectl-firebolt/`). `Client` is namespace-scoped and manages
FireboltEngine / FireboltInstance / FireboltEngineClass resources by building
them from the typed `api/v1alpha1` structs and applying them via `kubectl`.

| File | Role |
|------|------|
| `client.go` | `Client` (namespace + optional `--context`/`--kubeconfig`). |
| `kubectl.go` | `KubectlCmd` — a kubectl invocation as data (run / capture / render) + the typed-object-to-YAML marshal. |
| `engine.go` | Engine create/list/delete and the FireboltEngine builder (per-engine `spec.template` overrides). |
| `instance.go` | Instance list. |
| `portforward.go` | `kubectl port-forward` spawn + readiness parsing. |

## Invariants

- **Build CRs from the `api/v1alpha1` types, never hand-rolled maps.** This is the reason the plugin lives in the operator repo: a CRD field rename/removal becomes a build error here. Keep it aligned — do not loosen the plugin to dodge a type change.
- **Every `kubectl` call goes through a `KubectlCmd` constructor.** Execution and `--print-commands` rendering share one argv, so the printed script cannot drift from what runs. Add new operations the same way.
- **`engine create` injects no opinionated config or scheduling.** It sets only what the user passes; everything omitted falls through to the operator's defaults or the referenced `FireboltEngineClass`. Optional → set only when given: image (`spec.template`), `engineClassRef` (`--type`), hostPath storage (`--host-path`). `--replicas` defaults to 1.
- **`create` waits for `Ready` only when `replicas > 0`.** `replicas=0` is scale-to-zero — the operator parks the engine in the terminal `Stopped` phase (`Ready=False` by design), so the readiness wait is skipped (otherwise it blocks until timeout on a create that already succeeded).
- **`--instance` is required; `--bucket` is optional.** When `--bucket` is set, the plugin writes `customEngineConfig.storage` with a backend `type` (`--storage-type`), `api_scheme` (`--api-scheme`), and `bucket_name` (`--bucket`) — all three keys, type/scheme caller-controlled so the plugin isn't tied to S3. Object storage may instead come from the referenced `FireboltEngineClass`: its `customEngineConfig` deep-merges beneath the engine's. The operator doesn't enforce storage, so `create` only *warns* when it resolves no bucket — and it resolves the effective config (fetching the `--type` class to check its `customEngineConfig.storage.bucket_name`) rather than just checking the flags.
- **Never set `spec.template.spec.affinity` (or other placement) from the plugin.** The operator's `effectiveAffinity` *replaces* the class's affinity with the engine template's wholesale (no merge), so any affinity here silently drops the class's instance-type/anti-affinity rules — and the previous self-referential required podAffinity left the first pod unschedulable. Placement belongs in the `FireboltEngineClass`.
- **Don't hardcode cloud/storage specifics** (e.g. `s3://`, a fixed hostPath). Expose them as flags or leave them to the class/operator.
- Resource group is `compute.firebolt.io/v1alpha1` (kinds `FireboltEngine`, `FireboltEngineClass`, `FireboltInstance`).

## Build and test

```bash
go build ./internal/infra/... ./cmd/kubectl-firebolt/
go test ./internal/infra/...
make kubectl-firebolt        # build the plugin binary
```

## Conventions

- Mirror the repo: errors wrap with `%w`; never `panic` outside tests; thread `context.Context` into anything that shells out.
- Keep this a thin client — reconciliation logic belongs in `internal/controller/`, not here.

## Plugin conventions (krew best practices)

Follow the [krew plugin development best practices](https://krew.sigs.k8s.io/docs/developer-guide/develop/best-practices/): behave like a native `kubectl` subcommand. Honor `KUBECONFIG` and the standard connection flags — `-n/--namespace` (empty ⇒ omit `-n` so kubectl uses the context's namespace), `--context`, `--kubeconfig`; new flags should mirror kubectl's names. Errors to stderr with a non-zero exit; refer to the tool as `kubectl firebolt`, never the raw binary.

`k8s.io/cli-runtime` (genericclioptions) is intentionally **not** used: it targets plugins that build an in-process client-go client, whereas this plugin shells out to `kubectl`. If the plugin ever drops the `kubectl` dependency for a direct API client, adopt cli-runtime then for the full standard flag surface and config resolution.

Distribution via a krew `plugin.yaml` manifest is still TODO.
