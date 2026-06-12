# Firebolt Kubernetes Operator

A Kubernetes operator that manages Firebolt infrastructure: metadata services, an Envoy query-routing proxy, and compute engines with zero-downtime scaling via blue-green deployments.

## Overview

The operator manages three custom resources:

- **FireboltInstance** provisions the shared infrastructure that engines depend on: PostgreSQL, the metadata service, and an Envoy gateway proxy.
- **FireboltEngine** deploys stateful compute nodes. Each engine references a `FireboltInstance` and cannot operate without one.
- **FireboltEngineClass** *(optional, namespaced)* holds a reusable pod-template fragment that multiple engines in the same namespace can share via `spec.engineClassRef` — service account / IAM binding, scheduling, sidecars, and the engine container image. Namespaced (not cluster-scoped) because the template carries namespace-resolved identifiers like ServiceAccount names and Secret/PVC volume references.

When you change an engine's configuration (e.g., scale from 3 to 5 nodes), the operator performs a zero-downtime blue-green transition: it creates a new generation, waits for readiness, switches traffic, drains the old generation, and deletes it. Editing the referenced `FireboltEngineClass` triggers the same blue-green flow on every consumer engine.

## Documentation
For more detailed information checkout our [official documentation](https://docs.firebolt.io/self-managed/firebolt-operator/quickstart)

## `kubectl firebolt` plugin

For convenient management there's **`kubectl-firebolt`**, a kubectl plugin that creates, lists, deletes, and port-forwards FireboltEngines and FireboltInstances without hand-authoring manifests:

```bash
make kubectl-firebolt    # builds bin/kubectl-firebolt; put it on PATH
kubectl firebolt engine create my-engine -n my-ns --instance my-instance --type my-engine-class
kubectl firebolt engine list -o wide
```

It builds the CRs from this repo's `api/v1alpha1` types, so it versions in lockstep with the CRDs. See [`cmd/kubectl-firebolt/README.md`](cmd/kubectl-firebolt/README.md) for install, the full command set, and output formats.

## Use this with a coding agent

Paste the following prompt into your favorite coding agent and let it drive the whole local install for you. Ours is Claude Code.

```text
Install the Firebolt Kubernetes Operator on a local Kind cluster, then bring up a working FireboltInstance and FireboltEngine end to end.

If I only gave you the GitHub repo URL, clone the repo first. If I already opened the repo locally, work from the existing checkout.

Follow the "Quick start" section of README.md — it covers the prerequisites, the local Kind setup, object storage, and creating the FireboltInstance and FireboltEngine. Treat this as a request to actually deploy and verify the operator, not just inspect the codebase. Don't assume I have the prerequisites done; if a required tool is missing or a step is ambiguous, tell me and stop rather than guessing.

Workflow:
- Run a non-mutating discovery step first: print tool versions, Docker daemon status, any existing Kind clusters, and the current kube-context. Fail fast with a clear message if a required tool is missing or Docker is down.
- Before making any cluster changes, show me the resolved plan: which Kind cluster and registry you will create or reuse, which make targets you will run, and which namespaces and manifests you will apply.
- Prefer the repo's existing make targets and example manifests over hand-rolled kubectl/helm commands. Run make prepare-test-e2e (cluster + registry + images), then make local-deploy (build, load, Helm install). Stream the output and stop on the first error.
- Create the FireboltInstance, wait until it reports Ready, then deploy object storage and create the FireboltEngineClass + FireboltEngine. Poll for readiness with short loops; never sleep blindly.
- After everything is up, run a smoke check: confirm the operator deployment is Available and the instance and engine are Ready (kubectl get fire -n firebolt). Optionally send a trivial SQL query through the instance gateway as described in "Connecting to engines".
- When done, report the kube-context, what was deployed and where, the in-cluster gateway endpoint, and any remaining manual steps or warnings. If anything failed, show the failing command output and your best diagnosis before continuing.
```

This is the fast path if you want the agent to drive the install for you. If you would rather run the steps yourself, skip to the [Quick start](#quick-start) below.

## Quick start

For a step-by-step walkthrough, follow the quickstart guide in our [official documentation](https://docs.firebolt.io/self-managed/firebolt-operator/quickstart) or using the documentation source file at [`docs/quickstart.mdx`](docs/quickstart.mdx).

## Firebolt Operator flags

The Firebolt Operator supports these runtime flags. The binary default is what
the manager uses when you run it directly. The Helm chart default is what the
`firebolt-operator` chart passes with its default `values.yaml`.

| Flag | Binary default | Helm chart default | Description |
|------|----------------|--------------------|-------------|
| `--version` | `false` | Not set | Print the version and exit. |
| `--namespace` | `""` | Not set | Namespace to watch. Watches all namespaces when empty. |
| `--metrics-bind-address` | `0` | `:8443` | Address for the metrics endpoint. Use `0` to disable metrics. |
| `--metrics-secure` | `true` | `true` | Serve metrics over HTTPS with Kubernetes authentication and authorization. |
| `--metrics-cert-path` | `""` | Not set | Directory that contains the metrics server certificate. |
| `--metrics-cert-name` | `tls.crt` | Not set | Metrics server certificate file name. |
| `--metrics-cert-key` | `tls.key` | Not set | Metrics server key file name. |
| `--health-probe-bind-address` | `:8081` | `:8081` | Address for health probes. |
| `--leader-elect` | `false` | `true` | Enable leader election for HA deployments. |
| `--enable-webhooks` | `true` | `false` | Enable the admission webhook server. |
| `--webhook-cert-path` | `""` | Not set | Directory that contains the webhook certificate. The chart sets `/tmp/k8s-webhook-server/serving-certs` when `webhook.enabled=true`. |
| `--webhook-cert-name` | `tls.crt` | Not set | Webhook certificate file name. |
| `--webhook-cert-key` | `tls.key` | Not set | Webhook key file name. |
| `--enable-http2` | `false` | Not set | Enable HTTP/2 for the metrics and webhook servers. |
| `--engine-max-cpu` | `""` | Not set | Maximum allowed CPU request and limit on the engine container (`spec.template.spec.containers[name=engine].resources`). Empty disables the bound. |
| `--engine-max-memory` | `""` | Not set | Maximum allowed memory request and limit on the engine container. Empty disables the bound. |
| `--engine-max-ephemeral-storage` | `""` | Not set | Maximum allowed ephemeral-storage request and limit on the engine container. Empty disables the bound. |
| `--zap-devel` | `false` | Not set | Enable controller-runtime development logging defaults. |
| `--zap-encoder` | `json` | `json` | Log encoding. Valid values are `json` and `console`. |
| `--zap-log-level` | `info` | `info` | Minimum log level. Valid values include `debug`, `info`, `error`, and `panic`. |
| `--zap-stacktrace-level` | `error` | `error` | Level at and above which stack traces are captured. |
| `--zap-time-encoding` | `rfc3339` | Not set | Timestamp encoding for zap logs. |

## Running tests

```bash
make lint               # golangci-lint
make test               # unit tests (envtest, no cluster required)
make test-e2e           # E2E tests (requires Kind cluster)
```

## Where to go next
- For **contributor** detail, conventions, and rules for making changes to this repo, see [`AGENTS.md`](AGENTS.md).
- The Helm chart for the operator lives in [helm/firebolt-operator](helm/firebolt-operator/README.md).
- The pure CRD chart for the operator lives in [helm/firebolt-operator-crds](helm/firebolt-operator-crds/README.md).