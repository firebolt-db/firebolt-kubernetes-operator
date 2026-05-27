# FireboltInstance CRD Reference

## Spec Reference

Pod configuration for the gateway and metadata components lives on a
raw [`PodTemplateSpec`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-template-v1/)
under `spec.gateway.template` and `spec.metadata.template` respectively
— same shape as `EngineClass.spec.template`. The validating webhook
restricts what users may set on those templates; see
[Operator-owned fields](#operator-owned-fields-on-component-templates)
below.

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `spec.id` | No | (auto-generated ULID) | Stable unique identifier for the instance, used as the metadata account ID. Immutable once set. |
| `spec.metadata` | **Yes** | - | Metadata service configuration (can be empty `{}` for defaults) |
| `spec.metadata.postgres` | No | (internal) | External PostgreSQL connection. If omitted, the operator deploys an internal PostgreSQL StatefulSet. |
| `spec.metadata.postgres.host` | Yes* | - | PostgreSQL hostname |
| `spec.metadata.postgres.port` | No | `5432` | PostgreSQL port |
| `spec.metadata.postgres.database` | Yes* | - | Database name |
| `spec.metadata.postgres.credentialsSecretRef.name` | Yes* | - | Secret with `username` and `password` keys |
| `spec.metadata.replicas` | No | `1` | Number of metadata service pods (only `1` is currently supported). |
| `spec.metadata.metricsPort` | No | `9090` | Container port exposing Prometheus metrics from dedicated-pensieve. The operator stamps a corresponding `metrics` port on the container. |
| `spec.metadata.template` | No | (operator default) | Pod template merged with the operator-rendered metadata container. See [Operator-owned fields](#operator-owned-fields-on-component-templates) for what users may and may not set. Image: `spec.metadata.template.spec.containers[name=="metadata"].image`. |
| `spec.metadata.engineRegistration` | No | `false` | Register Engine objects in the metadata service for SQL-level RBAC. |
| `spec.gateway` | **Yes** | - | Envoy gateway proxy configuration (can be empty `{}` for defaults) |
| `spec.gateway.replicas` | No | `2` | Number of gateway pods. See [sizing guidance](#gateway-sizing) — replicas + memory must absorb both steady-state traffic and the retry amplification produced by the X-Firebolt-Drained shutdown path. |
| `spec.gateway.metricsPort` | No | `9090` | Container port exposing Envoy's Prometheus metrics endpoint. The operator stamps a corresponding `metrics` port on the container. |
| `spec.gateway.template` | No | (operator default) | Pod template merged with the operator-rendered Envoy container. See [Operator-owned fields](#operator-owned-fields-on-component-templates). Image: `spec.gateway.template.spec.containers[name=="envoy"].image`. |
| `spec.auth` | No | disabled | Authentication configuration (not enforced yet; reserved for future engine-level auth) |
| `spec.auth.mode` | Yes* | - | `disabled`, `native`, or `openid` |
| `spec.auth.oidc` | Yes* | - | OIDC config (required when mode is `openid`) |

\* Required when the parent field is set.

## Operator-owned fields on component templates

`spec.gateway.template` and `spec.metadata.template` are full
[`PodTemplateSpec`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-template-v1/)
embeds. The validating webhook (`vfireboltinstance.compute.firebolt.io`)
walks every template at admission time and rejects user input on
fields the operator manages end-to-end. This keeps the
StatefulSet/Deployment + Service + drain-hook contracts intact while
giving users the full pod surface for everything they legitimately
need.

The same set of pod-level fields is rejected on **both** components:

| Pod-level field | Reason |
|---|---|
| `spec.template.spec.subdomain` | Operator-owned for the headless-DNS contract. |
| `spec.template.spec.hostname` | Operator-owned. |
| `spec.template.spec.restartPolicy` | Fixed by the Deployment / StatefulSet controller. |
| `spec.template.spec.activeDeadlineSeconds` | Incompatible with long-lived component pods. |
| `spec.template.spec.terminationGracePeriodSeconds` | Operator-stamped per component (15s gateway, 30s metadata). |
| `spec.template.metadata.labels[firebolt.io/*]` | Reserved label prefix. |
| `spec.template.metadata.annotations[firebolt.io/*]` | Reserved annotation prefix (most importantly `firebolt.io/config-hash`, which drives pod rollouts). |

Per-component primary container rejections:

| Container field | Gateway (`envoy`) | Metadata (`metadata`) |
|---|---|---|
| `command`, `args`, `ports`, `readinessProbe`, `livenessProbe`, `startupProbe` | Rejected | Rejected |
| `lifecycle` | Rejected (operator owns the bash `/dev/tcp` preStop drain hook) | Rejected |
| `securityContext` | Rejected (hardened defaults: non-root UID 101, drop ALL caps) | Rejected (RunAsUser pinned to the image's `dedicated-pensieve` UID) |
| `env` | Rejected | Rejected (`POSTGRES_USERNAME_FILE` / `POSTGRES_PASSWORD_FILE` are operator-injected) |
| `envFrom` | Rejected | Rejected |
| `volumeMounts` | Rejected (`config-volume` / `tmp` are operator-rendered) | Rejected (`config` / `postgres-creds` / `tmp` are operator-rendered) |
| `image`, `imagePullPolicy` | **Allowed** | **Allowed** |
| `resources` | **Allowed** | **Allowed** |

Per-component pass-through (allowed without restriction):

- All pod-level scheduling fields: `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName`.
- Pod-level: `securityContext` (PodSecurityContext), `imagePullSecrets`, `serviceAccountName`, additional `volumes` (names that do not collide with operator-owned volume names).
- Additional `containers` (sidecars) — appended after the operator-rendered primary container.
- Additional `initContainers` — passed through verbatim.
- Pod-template `metadata.labels` and `metadata.annotations` outside the `firebolt.io/` reserved prefix.

A second container or initContainer using the operator-rendered
primary name (`envoy`, `metadata`) is rejected as a duplicate. The
authoritative rule sets live in
[`api/v1alpha1/operatorauthority.go`](../api/v1alpha1/operatorauthority.go)
as `GatewayPodTemplateRules` and `MetadataPodTemplateRules`.

## Instance Phases

| Phase | Meaning |
|-------|---------|
| `Provisioning` | Components are being deployed; not yet ready |
| `Ready` | Metadata service and gateway are healthy |
| `Degraded` | Was previously Ready, but one or more components became unhealthy |
| `Failed` | Terminal error requiring manual intervention (e.g., multiple accounts found in metadata) |

## Gateway sizing

See [gateway-sizing.md](gateway-sizing.md) for the full sizing guidance on replica count, memory limits, and the 2 MiB buffer constraint.

## Monitoring

```bash
kubectl get fi -n firebolt
```

```
NAME         PHASE   GATEWAY   METADATA   AGE
production   Ready   true      true       24h
```

Inspect details:

```bash
kubectl get fi production -n firebolt -o yaml
```

Key status fields: `phase`, `metadataReady`, `gatewayReady`, `metadataEndpoint`, `gatewayEndpoint`, `conditions` (`Ready`, `MetadataReady`, `GatewayReady`).

For full examples, see the [`examples/`](../examples/) directory.

For troubleshooting, see [troubleshooting.md](troubleshooting.md).
