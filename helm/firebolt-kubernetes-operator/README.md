# firebolt-kubernetes-operator

Helm chart for the Firebolt Kubernetes Operator

## Installation

```bash
helm install firebolt-operator ./helm/firebolt-kubernetes-operator \
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
| extraAnnotations | object | `{}` | Extra annotations added to all operator manifests. |
| extraLabels | object | `{}` | Extra labels added to all operator manifests. |
| extraVolumeMounts | list | `[]` | Extra volume mounts for the operator container. Rendered as-is into `container.volumeMounts`. Pair each entry with an `extraVolumes` entry of the same `name`. |
| extraVolumes | list | `[]` | Extra volumes attached to the operator Pod. Rendered as-is into `pod.spec.volumes`. Useful for mounting externally-provisioned certs, custom CAs, config files, or sidecar outputs (Vault Agent, CSI secrets-store, projected service-account tokens, etc.). |
| fullnameOverride | string | `""` | Override the full resource name. |
| healthProbeBindAddress | string | `":8081"` | Address the health probe endpoint binds to. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"controller"` | Container image repository. |
| image.tag | string | `""` | Overrides the image tag whose default is the chart appVersion. |
| imagePullSecrets | list | `[]` | Secrets for pulling images from private registries. |
| leaderElection.enabled | bool | `true` | Enable leader election for the controller manager. |
| metrics.bindAddress | string | `":8443"` | Address the metrics endpoint binds to. |
| metrics.enabled | bool | `true` | Enable the metrics Service. |
| metrics.secure | bool | `true` | Serve metrics via HTTPS with authn/authz. |
| nameOverride | string | `""` | Override the chart name used in resource names. |
| nodeSelector | object | `{}` | Node selector for the operator pod. |
| podAnnotations | object | `{}` | Extra annotations added only to the operator pod. |
| podLabels | object | `{}` | Extra labels added only to the operator pod. |
| priorityClassName | string | `""` | Priority class name for the operator pod. |
| rbac.create | bool | `true` | Whether to create ClusterRole, ClusterRoleBinding, and leader-election RBAC resources. |
| replicaCount | int | `1` | Number of operator replicas. |
| resources | object | requests: 10m/64Mi, limits: 500m/128Mi | CPU/memory resource requests and limits for the operator pod. |
| serviceAccount.annotations | object | `{}` | Annotations to add to the ServiceAccount (e.g. for IRSA / Workload Identity). |
| serviceAccount.create | bool | `true` | Whether to create a ServiceAccount. |
| serviceAccount.name | string | `""` | The name of the ServiceAccount to use. If empty, a name is generated from the fullname template. |
| tolerations | list | `[]` | Tolerations for the operator pod. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for the operator pod. |
| watchNamespace | string | `""` | Namespace to watch for FireboltEngine resources. Empty watches all namespaces. |
| webhook.certDir | string | `"/tmp/k8s-webhook-server/serving-certs"` | Path (inside the container) where the operator reads tls.crt and tls.key. Passed to the operator as `--webhook-cert-path=<certDir>`. Override only when mounting the certs at a different path via `extraVolumes` / `extraVolumeMounts`. |
| webhook.certSecretName | string | `""` | Optional shortcut for the common case: name of an existing Secret in the release namespace with keys `tls.crt` and `tls.key`. When set, the chart mounts this Secret read-only at `webhook.certDir`. The Secret itself is NOT created by this chart; provision it via cert-manager Certificate, ExternalSecret, Vault Agent, etc. Leave empty to mount certs via `extraVolumes` / `extraVolumeMounts` instead. |
| webhook.enabled | bool | `false` | Enable the admission webhook server. When false, the operator is started with `--enable-webhooks=false` and no webhook Service, port, or cert mount is created. Left false by default so existing consumers without a cert provisioner keep working. |
| webhook.port | int | `9443` | Port the webhook server listens on inside the container. Also used as the port of the webhook Service. Only relevant when `enabled` is true. |

## CRD Sync

CRDs in this chart are symlinks to `config/crd/bases/`. After running
`make manifests` (which regenerates CRDs from Go struct annotations), the
chart automatically picks up the changes. Since Helm does not upgrade CRDs
in the `crds/` directory, apply CRD updates manually:

```bash
kubectl apply -f helm/firebolt-kubernetes-operator/crds/
```

Or use the `firebolt-operator-crds` chart for managed CRD upgrades via
`helm upgrade`.

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
