# firebolt-operator

Helm chart for the Firebolt Kubernetes Operator

## Installation

```bash
helm install firebolt-operator ./helm/firebolt-operator \
  --namespace firebolt-system --create-namespace
```

CRDs are bundled in the `crds/` directory and installed automatically on first
install. Helm does not upgrade or delete CRDs from the `crds/` directory. To
manage CRD upgrades independently, use the `firebolt-operator-crds` chart
instead.

## Uninstallation

```bash
helm uninstall firebolt-operator --namespace firebolt-system
```

CRDs are **not** deleted on uninstall (Helm default for the `crds/` directory).
To remove CRDs manually:

```bash
kubectl delete crd fireboltengines.compute.firebolt.io fireboltinstances.compute.firebolt.io
```

> **Warning:** Deleting CRDs cascades and removes all FireboltEngine and
> FireboltInstance resources in the cluster.

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalArgs | list | `[]` | Additional CLI arguments passed to the operator binary. |
| additionalEnv | list | `[]` | Additional environment variables for the operator container. |
| affinity | object | `{}` | Affinity rules for the operator pod. |
| engineResourceBounds.maxCPU | string | `""` | Maximum allowed FireboltEngine.spec.resources CPU (requests and limits). Example: "32". |
| engineResourceBounds.maxEphemeralStorage | string | `""` | Maximum allowed FireboltEngine.spec.resources ephemeral-storage (requests and limits). Example: "10Ti". |
| engineResourceBounds.maxMemory | string | `""` | Maximum allowed FireboltEngine.spec.resources memory (requests and limits). Example: "256Gi". |
| extraAnnotations | object | `{}` | Extra annotations added to all operator manifests. |
| extraLabels | object | `{}` | Extra labels added to all operator manifests. |
| extraVolumeMounts | list | `[]` | Extra volume mounts for the operator container. Rendered as-is into `container.volumeMounts`. Pair each entry with an `extraVolumes` entry of the same `name`. |
| extraVolumes | list | `[]` | Extra volumes attached to the operator Pod. Rendered as-is into `pod.spec.volumes`. Useful for mounting externally-provisioned certs, custom CAs, config files, or sidecar outputs (Vault Agent, CSI secrets-store, projected service-account tokens, etc.). |
| fullnameOverride | string | `""` | Override the full resource name. |
| healthProbeBindAddress | string | `":8081"` | Address the health probe endpoint binds to. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/firebolt-db/firebolt-operator"` | Container image repository. |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Secrets for pulling images from private registries. |
| leaderElection.enabled | bool | `true` | Enable leader election for the controller manager. |
| logging.development | bool | `false` | Development mode (--zap-devel): console encoding, debug level, no sampling. Use for local clusters; production installs should leave this false. When true, encoder/level/stacktraceLevel below are not passed (zap dev defaults apply). |
| logging.encoder | string | `"json"` | Log encoding (json or console). Used when development is false. |
| logging.level | string | `"info"` | Minimum log level (debug, info, error). Used when development is false. |
| logging.stacktraceLevel | string | `"error"` | Level at and above which stack traces are captured (info, error, panic). Used when development is false. Dev mode defaults to warn internally but that threshold is not configurable via --zap-stacktrace-level. |
| metrics.bindAddress | string | `":8443"` | Address the metrics endpoint binds to. Use ":8443" with secure: true (HTTPS) or ":8080" with secure: false (HTTP). |
| metrics.enabled | bool | `true` | Enable the metrics Service. |
| metrics.secure | bool | `true` | Serve metrics via HTTPS with authn/authz. When true, controller-runtime auto-generates self-signed TLS certs and enables Kubernetes authn/authz. The operator ServiceMonitor automatically adapts scheme and TLS config. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| nodeSelector | object | `{}` | Node selector for the operator pod. |
| podAnnotations | object | `{}` | Extra annotations added only to the operator pod. |
| podLabels | object | `{}` | Extra labels added only to the operator pod. |
| podMonitor.allNamespaces | bool | `false` | When true, PodMonitors discover pods across all namespaces. Use when the operator watches multiple namespaces (watchNamespace is empty). When false, PodMonitors only discover pods in the release namespace. |
| podMonitor.engines | object | `{"enabled":false,"interval":"15s"}` | Create a PodMonitor for engine pods (port 9090, /metrics). |
| podMonitor.gateway | object | `{"enabled":false,"interval":"15s"}` | Create a PodMonitor for gateway pods (port 9090, /stats/prometheus). |
| podSecurityContext | object | `{"fsGroup":65532,"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}` | Pod-level security context. fsGroup matches the distroless-nonroot UID used by the default `controller` image so mounted Secret files (e.g. the webhook cert) are readable by the operator process. |
| priorityClassName | string | `""` | Priority class name for the operator pod. |
| rbac.create | bool | `true` | Whether to create ClusterRole, ClusterRoleBinding, and leader-election RBAC resources. |
| replicaCount | int | `1` | Number of operator replicas. |
| resources | object | requests: 10m/64Mi, limits: 500m/128Mi | CPU/memory resource requests and limits for the operator pod. |
| serviceAccount.annotations | object | `{}` | Annotations to add to the ServiceAccount (e.g. for IRSA / Workload Identity). |
| serviceAccount.create | bool | `true` | Whether to create a ServiceAccount. |
| serviceAccount.name | string | `""` | The name of the ServiceAccount to use. If empty, a name is generated from the fullname template. |
| serviceMonitor.operator | object | `{"bearerTokenSecret":{"key":"token","name":""},"enabled":false,"interval":"15s"}` | Create a ServiceMonitor that scrapes the operator's controller-manager metrics Service (`<release>-metrics`). A ServiceMonitor is used (instead of a PodMonitor) because the secure path (`metrics.secure: true`) needs to authenticate to controller-runtime's built-in authn/authz, and only `ServiceMonitor.endpoint.authorization` exposes a credential reference. |
| serviceMonitor.operator.bearerTokenSecret | object | {} | Secret holding the bearer token Prometheus presents to the operator's metrics endpoint. Required when `metrics.secure: true`. Bring your own Secret (e.g. an ESO/Vault-projected one, a `kubernetes.io/service-account-token` Secret bound to your scrape SA, or whatever your Prometheus install already uses); leave `name` empty to render no authorization (the secure endpoint will reject scrapes). |
| tolerations | list | `[]` | Tolerations for the operator pod. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for the operator pod. |
| watchNamespace | string | `""` | Namespace to watch for FireboltEngine resources. Empty watches all namespaces. |
| webhook.caBundle | string | `""` | Static CA bundle (base64-encoded) for admission webhook clients. Ignored when `webhookConfigurationAnnotations` drives CA injection via a controller (cert-manager, etc.); use this as a last-resort manual override when no injector is available. |
| webhook.certDir | string | `"/tmp/k8s-webhook-server/serving-certs"` | Path (inside the container) where the operator reads tls.crt and tls.key. Passed to the operator as `--webhook-cert-path=<certDir>`. Override only when mounting the certs at a different path via `extraVolumes` / `extraVolumeMounts`. |
| webhook.certSecretName | string | `""` | Optional shortcut for the common case: name of an existing Secret in the release namespace with keys `tls.crt` and `tls.key`. When set, the chart mounts this Secret read-only at `webhook.certDir`. The Secret itself is NOT created by this chart; provision it via cert-manager Certificate, ExternalSecret, Vault Agent, etc. Leave empty to mount certs via `extraVolumes` / `extraVolumeMounts` instead. |
| webhook.enabled | bool | `false` | Enable the admission webhook server. When false, the operator is started with `--enable-webhooks=false` and no webhook Service, port, or cert mount is created. Left false by default so existing consumers without a cert provisioner keep working. |
| webhook.mutatingWebhookConfiguration.enabled | bool | `true` | Render the MutatingWebhookConfiguration for the FireboltInstance defaulter. Only effective when `webhook.enabled` is also true. |
| webhook.mutatingWebhookConfiguration.failurePolicy | string | `"Ignore"` | failurePolicy for the mutating webhook. Defaults to Ignore because the defaulter only fills in an empty spec.id (the controller has a fallback path, and the CRD enforces id immutability via CEL), so admission should not be coupled to operator availability. |
| webhook.mutatingWebhookConfiguration.timeoutSeconds | int | `10` | Max admission request timeout in seconds (k8s caps at 30). |
| webhook.port | int | `9443` | Port the webhook server listens on inside the container. Also used as the port of the webhook Service. Only relevant when `enabled` is true. |
| webhook.validatingWebhookConfiguration.enabled | bool | `true` | Render the ValidatingWebhookConfiguration for FireboltInstance validation (reserved keys, metadata replicas, external Postgres secret presence) and FireboltEngineClass validation (operator-owned pod template paths, deletion blocking while bound by FireboltEngines). Only effective when `webhook.enabled` is also true. |
| webhook.validatingWebhookConfiguration.failurePolicy | string | `"Fail"` | failurePolicy for the validating webhook. Defaults to Fail so invalid CR writes are rejected rather than silently admitted during operator outages. Critical for the FireboltEngineClass deletion guard: with Ignore, an outage would open a window in which a bound FireboltEngineClass could be deleted, orphaning every engine that referenced it. |
| webhook.validatingWebhookConfiguration.timeoutSeconds | int | `10` | Max admission request timeout in seconds (k8s caps at 30). |
| webhook.webhookConfigurationAnnotations | object | `{}` | Extra annotations merged into every WebhookConfiguration rendered by this chart. The standard way to wire automated CA injection, e.g.:   cert-manager.io/inject-ca-from: <namespace>/<certificate-name> |

## CRD Sync

CRDs in this chart are symlinks to `config/crd/bases/`. After running
`make manifests` (which regenerates CRDs from Go struct annotations), the
chart automatically picks up the changes. Since Helm does not upgrade CRDs
in the `crds/` directory, apply CRD updates manually:

```bash
kubectl apply -f helm/firebolt-operator/crds/
```

Or use the `firebolt-operator-crds` chart for managed CRD upgrades via
`helm upgrade`.
