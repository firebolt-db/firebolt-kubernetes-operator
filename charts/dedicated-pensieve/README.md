# Dedicated Pensieve Helm Chart

Helm chart for deploying Dedicated Pensieve Server - a lightweight metadata management service for PackDB Firebolt Core.

## Overview

Dedicated Pensieve is a gRPC-based metadata management service that uses PostgreSQL as its backend storage. This chart provides a production-ready deployment with **secure, zero-config-exposure credential management**.

## Key Security Features

✅ **ConfigMap contains ZERO credential information** (not even file paths!)  
✅ **Credentials flow through: Values → Secret → Files → ENV vars → Application**  
✅ **Compatible with External Secrets Operator (Vault, AWS, GCP, etc.)**  
✅ **Configurable mount paths (no hardcoded values)**  
✅ **Automatic pod restart on credential changes**

## Prerequisites

- Kubernetes 1.19+
- Helm 3.0+
- PostgreSQL 12+ (external, not included in this chart)
- Access to the Dedicated Pensieve container image

## Quick Start

### Development (Create Secret from Values)

```bash
helm install my-pensieve ./dedicated-pensieve \
  --set postgresql.host=postgres.example.com \
  --set postgresql.database=pensieve_db \
  --set postgresql.credentials.username=pensieve_user \
  --set postgresql.credentials.password=secure_password
```

### Production (Use Existing Secret)

```bash
# Option 1: Create secret manually
kubectl create secret generic my-postgres-creds \
  --from-literal=username=pensieve_user \
  --from-literal=password=secure_password

# Option 2: Or use External Secrets Operator (see below)

# Deploy with existing secret
helm install my-pensieve ./dedicated-pensieve \
  --set postgresql.host=postgres.prod.com \
  --set postgresql.database=pensieve_db \
  --set postgresql.credentials.existingSecret=my-postgres-creds
```

### Production with External Secrets Operator (Recommended)

```yaml
# Create ExternalSecret that pulls from your secret store
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: postgres-from-vault
spec:
  refreshInterval: 1h
  secretStoreRef:
    name: vault-backend
    kind: SecretStore
  target:
    name: dedicated-pensieve-postgres-creds
  data:
    - secretKey: username
      remoteRef:
        key: prod/dedicated-pensieve/postgres
        property: username
    - secretKey: password
      remoteRef:
        key: prod/dedicated-pensieve/postgres
        property: password
```

```bash
# Deploy referencing ESO-managed secret
helm install my-pensieve ./dedicated-pensieve \
  --set postgresql.host=postgres.prod.com \
  --set postgresql.database=pensieve_db \
  --set postgresql.credentials.existingSecret=dedicated-pensieve-postgres-creds
```

## Configuration

### Core Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Container image repository | `000000000000.dkr.ecr.us-east-1.amazonaws.com/dedicated-pensieve` |
| `image.tag` | Container image tag | `1.0.0-test` |
| `service.type` | Kubernetes service type | `ClusterIP` |
| `service.port` | Service port | `9090` |

### PostgreSQL Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.host` | PostgreSQL host | `""` |
| `postgresql.port` | PostgreSQL port | `5432` |
| `postgresql.database` | Database name | `""` |
| `postgresql.schema` | Database schema | `public` |

### Credential Management (NEW Design!)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.credentials.mountPath` | Where to mount secret in container | `/secrets/postgres` |
| `postgresql.credentials.existingSecret` | Use existing Kubernetes secret | `""` |
| `postgresql.credentials.username` | Username (only if no existingSecret) | `""` |
| `postgresql.credentials.password` | Password (only if no existingSecret) | `""` |

**How it works:**
- Credentials are mounted as files at `mountPath`
- ENV vars `POSTGRES_USERNAME_FILE` and `POSTGRES_PASSWORD_FILE` point to these files
- Application reads from files (not from config)
- **ConfigMap has ZERO credential information**

### Resource Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `512Mi` |
| `resources.limits.memory` | Memory limit | `1Gi` |

## Architecture

```
┌─────────────────────────────────────────┐
│          Kubernetes Cluster             │
│                                         │
│  ┌───────────────────────────────────┐  │
│  │  Dedicated Pensieve Pod           │  │
│  │                                   │  │
│  │  ENV VARS:                        │  │
│  │  POSTGRES_USERNAME_FILE=          │  │
│  │    /secrets/postgres/username     │◄─┼── ConfigMap (NO creds!)
│  │  POSTGRES_PASSWORD_FILE=          │  │
│  │    /secrets/postgres/password     │  │
│  │                                   │  │
│  │  FILES (mounted from Secret):     │  │
│  │  /secrets/postgres/               │◄─┼── Secret (encrypted)
│  │    ├─ username                    │  │
│  │    └─ password                    │  │
│  │                                   │  │
│  │  Connects to PostgreSQL ──────────┼──┼─► External PostgreSQL
│  │  on port 9090 (gRPC)              │  │
│  └───────────────────────────────────┘  │
│                                         │
│  Service (ClusterIP) Port: 9090         │
└─────────────────────────────────────────┘
```

## Credential Management Deep Dive

See [FLOW-EXPLAINED.md](./FLOW-EXPLAINED.md) for complete documentation.

### The Flow

```
values.yaml (username/password)
         ↓
   Secret (encrypted)
         ↓
   Mounted as files (/secrets/postgres/)
         ↓
   ENV vars point to files (POSTGRES_*_FILE)
         ↓
   Application reads from files
```

### Security Benefits

1. **ConfigMap Isolation**: ConfigMap has ZERO credential info (not even file paths!)
2. **Encrypted Storage**: Credentials only in Kubernetes Secret (encrypted at rest)
3. **File Permissions**: Mounted with 0400 (read-only for owner)
4. **ENV Var Safety**: ENV vars contain only file PATHS, not actual credentials
5. **Flexibility**: Mount path is configurable (no hardcoded paths)

## Examples

### Example 1: Development

```bash
helm install dev-pensieve ./dedicated-pensieve \
  --set postgresql.host=localhost \
  --set postgresql.database=dev_db \
  --set postgresql.credentials.username=dev_user \
  --set postgresql.credentials.password=dev_pass
```

### Example 2: Production with Custom Mount Path

```bash
helm install prod-pensieve ./dedicated-pensieve \
  --set postgresql.host=prod-postgres.aws.com \
  --set postgresql.database=prod_db \
  --set postgresql.credentials.mountPath=/var/run/secrets/db \
  --set postgresql.credentials.existingSecret=my-postgres-secret
```

### Example 3: Using Values File

```yaml
# values-prod.yaml (gitignored!)
postgresql:
  host: "prod-postgres.aws.com"
  database: "pensieve_prod"
  credentials:
    existingSecret: "prod-postgres-creds"
```

```bash
helm install prod-pensieve ./dedicated-pensieve -f values-prod.yaml
```

## Upgrading

### Updating Credentials

Pods automatically restart when credentials change (due to checksum annotations):

```bash
# Update secret
kubectl create secret generic my-postgres-creds \
  --from-literal=username=new_user \
  --from-literal=password=new_password \
  --dry-run=client -o yaml | kubectl apply -f -

# Pods will restart automatically
```

### Upgrading the Chart

```bash
helm upgrade my-pensieve ./dedicated-pensieve -f values.yaml
```

## Uninstallation

```bash
helm uninstall my-pensieve
```

**Note:** Secrets created by the chart will be removed. External PostgreSQL data is NOT affected.

## Troubleshooting

### Pod Not Starting

```bash
kubectl logs -l app.kubernetes.io/name=my-pensieve
kubectl describe pod <pod-name>
```

### Check ENV Variables

```bash
kubectl exec -it <pod-name> -- env | grep POSTGRES
```

### Check Mounted Files

```bash
kubectl exec -it <pod-name> -- ls -la /secrets/postgres/
kubectl exec -it <pod-name> -- cat /secrets/postgres/username
```

### Verify Secret

```bash
kubectl get secret my-pensieve-dedicated-pensieve-postgres-creds -o yaml
```

### Connection Issues

```bash
# Check PostgreSQL connectivity from pod
kubectl exec -it <pod-name> -- /bin/sh
# Try connecting to PostgreSQL host
```

## Testing

Run the automated test suite:

```bash
cd helm/dedicated-pensieve
./test-chart.sh
```

Tests validate:
- Helm lint passes
- ConfigMap has ZERO credential information
- ENV vars are correctly set
- Secret is created/referenced properly
- Volume mounts are configured
- File permissions are correct
- Checksum annotations work

## Documentation

- [FLOW-EXPLAINED.md](./FLOW-EXPLAINED.md) - Detailed design and architecture
- [values-secure-example.yaml](./values-secure-example.yaml) - Annotated example configuration
- [CREDENTIALS.md](./CREDENTIALS.md) - Comprehensive credential management guide
- [IMPLEMENTATION_SUMMARY.md](./IMPLEMENTATION_SUMMARY.md) - Technical implementation details

## Security Best Practices

1. **Never commit credentials to git** - Use separate values files or command-line overrides
2. **Use existingSecret in production** - Integrate with your secret management system
3. **Enable RBAC** - Restrict who can access secrets
4. **Rotate credentials regularly** - Update secrets and let pods restart automatically
5. **Use External Secrets Operator** - For enterprise deployments with Vault/AWS/GCP
6. **Enable encryption at rest** - Ensure Kubernetes cluster encrypts secrets
7. **Monitor secret access** - Set up audit logging

## Support

For issues and questions:
- Check [FLOW-EXPLAINED.md](./FLOW-EXPLAINED.md) for design details
- Review [CREDENTIALS.md](./CREDENTIALS.md) for security guidance
- Check application logs: `kubectl logs <pod-name>`
- Run tests: `./test-chart.sh`

## License

Copyright Firebolt Analytics
