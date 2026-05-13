# FireboltInstance CRD Reference

## Spec Reference

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `spec.id` | No | (auto-generated ULID) | Stable unique identifier for the instance, used as the metadata account ID. Immutable once set. |
| `spec.metadata` | **Yes** | - | Metadata service configuration (can be empty `{}` for defaults) |
| `spec.metadata.postgres` | No | (internal) | External PostgreSQL connection. If omitted, the operator deploys an internal PostgreSQL StatefulSet |
| `spec.metadata.postgres.host` | Yes* | - | PostgreSQL hostname |
| `spec.metadata.postgres.port` | No | `5432` | PostgreSQL port |
| `spec.metadata.postgres.database` | Yes* | - | Database name |
| `spec.metadata.postgres.credentialsSecretRef.name` | Yes* | - | Secret with `username` and `password` keys |
| `spec.metadata.image` | No | (operator default) | Override the metadata service container image |
| `spec.metadata.replicas` | No | `1` | Number of metadata service pods (only `1` is currently supported) |
| `spec.metadata.resources` | No | (operator default) | CPU/memory for metadata service pods |
| `spec.metadata.nodeSelector` | No | - | Node selector for metadata service pods |
| `spec.gateway` | **Yes** | - | Envoy gateway proxy configuration (can be empty `{}` for defaults) |
| `spec.gateway.image` | No | `envoyproxy/envoy:v1.37.2` | Override the Envoy container image |
| `spec.gateway.replicas` | No | `2` | Number of gateway pods. See [sizing guidance](#gateway-sizing) -- replicas + memory must absorb both steady-state traffic and the retry amplification produced by the X-Firebolt-Drained shutdown path. |
| `spec.gateway.resources` | No | (operator default) | CPU/memory for gateway pods. See [sizing guidance](#gateway-sizing). |
| `spec.gateway.nodeSelector` | No | - | Node selector for gateway pods |
| `spec.auth` | No | disabled | Authentication configuration (not enforced yet; reserved for future engine-level auth) |
| `spec.auth.mode` | Yes* | - | `disabled`, `native`, or `openid` |
| `spec.auth.oidc` | Yes* | - | OIDC config (required when mode is `openid`) |

\* Required when the parent field is set.

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
