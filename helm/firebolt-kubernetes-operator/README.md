# firebolt-kubernetes-operator

![Version: 0.1.7](https://img.shields.io/badge/Version-0.1.7-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v1.5.0](https://img.shields.io/badge/AppVersion-v1.5.0-informational?style=flat-square)

Helm chart for the Firebolt Kubernetes Operator

## Installation

```bash
helm install firebolt-operator \
  oci://000000000000.dkr.ecr.us-east-1.amazonaws.com/helm-charts/firebolt-kubernetes-operator \
  --version 0.1.7
```

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| firebolt-analytics |  |  |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| additionalArgs | list | `[]` | Additional CLI arguments passed to the operator binary. |
| additionalEnv | list | `[]` | Additional environment variables for the operator container. |
| affinity | object | `{}` | Affinity rules for the operator pod. |
| extraAnnotations | object | `{}` | Extra annotations added to all operator manifests. |
| extraLabels | object | `{}` | Extra labels added to all operator manifests. |
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
| resources.limits.cpu | string | `"500m"` |  |
| resources.limits.memory | string | `"128Mi"` |  |
| resources.requests.cpu | string | `"10m"` |  |
| resources.requests.memory | string | `"64Mi"` |  |
| serviceAccount.annotations | object | `{}` | Annotations to add to the ServiceAccount (e.g. for IRSA / Workload Identity). |
| serviceAccount.create | bool | `true` | Whether to create a ServiceAccount. |
| serviceAccount.name | string | `""` | The name of the ServiceAccount to use. If empty, a name is generated from the fullname template. |
| tolerations | list | `[]` | Tolerations for the operator pod. |
| topologySpreadConstraints | list | `[]` | Topology spread constraints for the operator pod. |
| watchNamespace | string | `""` | Namespace to watch for FireboltEngine resources. Empty watches all namespaces. |

---

_This README is generated with [helm-docs](https://github.com/norwoodj/helm-docs). Run `make helm-docs` to regenerate._
