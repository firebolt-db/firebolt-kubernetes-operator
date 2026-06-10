# kubectl-firebolt

A `kubectl` plugin for day-to-day management of FireboltEngine and
FireboltInstance resources via the Firebolt operator. It builds the custom
resources from this repo's `api/v1alpha1` types and applies them with `kubectl`
against your current kubeconfig context.

## Install

```bash
make kubectl-firebolt        # builds bin/kubectl-firebolt
cp bin/kubectl-firebolt /usr/local/bin/   # any dir on PATH
kubectl firebolt --help
```

Any executable named `kubectl-firebolt` on `PATH` is invoked as
`kubectl firebolt`.

## Usage

`--namespace/-n` defaults to the current context's namespace when omitted; the
cluster is your current context (override with `--context` / `--kubeconfig`).
Add `--print-commands` (alias `--debug`) to any command to print the `kubectl`
it would run instead of running it.

```bash
# Instances
kubectl firebolt instance list -n my-ns
kubectl firebolt instance list -n my-ns -o json          # -o table (default)|wide|json|yaml|name
kubectl firebolt instance port-forward my-instance -n my-ns --local-port 8123

# Engines — only --instance is required (see "engine create" below)
kubectl firebolt engine list -n my-ns
kubectl firebolt engine list -n my-ns --instance my-instance -o wide
kubectl firebolt engine create my-engine -n my-ns \
  --instance my-instance \
  [--bucket my-bucket] [--type <engine-class>] [--replicas 2] [--image <repo>:<tag>] \
  [--storage-type gcs] [--api-scheme gs://] [--host-path /mnt/nvme] [--timeout 5m]
kubectl firebolt engine port-forward my-engine -n my-ns --local-port 8123
kubectl firebolt engine delete my-engine -n my-ns

# Plugin version
kubectl firebolt version
```

`list` defaults to an aligned, headered table; `-o/--output` also accepts `wide`,
`json`, `yaml`, and `name` (`<resource>/<name>`). The `--instance` filter on
`engine list` applies to every format.

`engine create` applies a FireboltEngine and waits for `Ready`. It injects **no**
opinionated config or scheduling — it sets only what you pass, and anything
omitted falls through to the operator's defaults or the referenced
`FireboltEngineClass`. Flag rules:

- `--instance` — always required.
- `--bucket` — optional. When given, the plugin writes `customEngineConfig.storage` from `--bucket` plus `--storage-type` (backend selector — `s3` default; `gcs`, `minio`, …) and `--api-scheme` (`s3://` default; `gs://` for GCS) — not tied to S3. Object storage may instead be supplied by the referenced `FireboltEngineClass` (its `customEngineConfig` is inherited). When `--bucket` is omitted, `create` resolves the effective config — it fetches the `--type` class and checks whether *it* sets `customEngineConfig.storage.bucket_name` — and warns only if neither side provides a bucket (naming a class that doesn't configure storage still warns). The operator doesn't require storage, so this is a non-blocking warning. (Under `--print-commands` it falls back to the flag-only heuristic, since it doesn't touch the cluster.)
- `--replicas` — defaults to `1`. `--replicas 0` is scale-to-zero: the operator parks the engine in the terminal `Stopped` phase (`Ready=False` by design), so `create` skips the readiness wait instead of blocking until it times out.
- `--type` — a FireboltEngineClass to reference by name (`engineClassRef`). Optional; omit for no class reference. There is no default class — when omitted, the operator's built-in defaults apply (no class is merged).
- `--image` — optional; omit to use the operator's embedded default image, pass to override.
- `--host-path` — optional; back the engine data volume with a node hostPath. Omit for the operator default (`emptyDir`).
- `--timeout` — how long to wait for `Ready`, as a Go duration (`3m`, `180s`, `1h`; default `5m`), passed through to `kubectl wait` verbatim. On timeout the error points you at `kubectl describe` to see why the engine isn't Ready.

Placement (node/pod affinity, topology spread, same-AZ co-location) is **not** set by the plugin — configure it in the `FireboltEngineClass`, where it composes with the class's instance-type and anti-affinity rules.

### Same-AZ co-location in a FireboltEngineClass

Earlier the plugin injected a same-AZ pod affinity automatically. To keep that
behavior, put it on the class the engine references (`--type`). Use a **soft**
(`preferred`) rule — a *required* self-referential pod affinity leaves the first
pod unschedulable (no peer carries the label yet), which is exactly why it was
removed:

```yaml
apiVersion: compute.firebolt.io/v1alpha1
kind: FireboltEngineClass
metadata:
  name: my-engine-class       # referenced by `kubectl firebolt engine create --type my-engine-class`
spec:
  template:
    spec:
      affinity:
        # The operator uses the class's affinity as a whole (it does not merge
        # per sub-field), so include the class's existing scheduling here too:
        nodeAffinity: {}        # ...instance-type selection...
        podAntiAffinity: {}     # ...one pod per node...
        # Same-AZ co-location (best-effort):
        podAffinity:
          preferredDuringSchedulingIgnoredDuringExecution:
            - weight: 100
              podAffinityTerm:
                topologyKey: topology.kubernetes.io/zone
                labelSelector:
                  matchExpressions:
                    - key: firebolt.io/engine   # operator-stamped on engine pods
                      operator: Exists
```

- **Soft, not required:** `preferred` lets the first pod schedule, then biases peers toward the same zone. `required` self-referential affinity deadlocks from a cold start.
- **Selector:** `firebolt.io/engine: Exists` co-locates pods carrying the engine label. A shared class biases *all* its engines into one zone; for strict per-engine co-location, use a class per engine and match the exact `firebolt.io/engine: <engine-name>`.
- **Hard guarantee:** the only way to *force* a single zone is pinning a concrete zone via `nodeAffinity` (`topology.kubernetes.io/zone In [<zone>]`) — Kubernetes has no self-bootstrapping hard same-AZ.

## Engine class (`--type`)

`--type` references a `FireboltEngineClass` **by name** — whatever classes your
deployment defines (their naming is cluster/cloud specific, so the plugin
imposes no fixed catalog). The class encodes the engine's hardware and
scheduling, and can also supply storage and engine-config defaults (its
`customEngineConfig` deep-merges beneath the engine's), so an engine that
references a class may not need `--bucket`. The operator's webhook rejects a
name that isn't deployed in the namespace. Omit `--type` to create the engine
without a class reference — there is no default class; the operator's built-in
defaults apply.

## Conventions (krew best practices)

This plugin follows the [krew plugin development best practices](https://krew.sigs.k8s.io/docs/developer-guide/develop/best-practices/):

- Invoked as `kubectl firebolt …` (binary `kubectl-firebolt` on `PATH`); help and messages refer to it that way, never as the raw binary.
- Behaves like a native subcommand: honors `KUBECONFIG` and the common connection flags — `-n/--namespace` (defaulting to the context namespace), `--context`, `--kubeconfig`.
- Errors go to stderr with a non-zero exit code; normal output to stdout.
- A single self-contained binary that shells out to `kubectl` (so `kubectl` must be on `PATH`) rather than embedding an API client.

A draft krew manifest lives at [`plugin.yaml`](plugin.yaml). It is **not installable yet**: a release pipeline must build the per-platform archives and fill in the download URLs + sha256 checksums before `kubectl krew install` works.

## Notes

- It is a thin client over the operator: it creates CRs, the operator
  reconciles them. See [`internal/infra/AGENTS.md`](../../internal/infra/AGENTS.md)
  for internals.
