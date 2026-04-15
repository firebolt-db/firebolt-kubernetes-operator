# Charts

The operator does not ship Helm chart sources. It fetches the following charts
at runtime from a local filesystem path or an OCI registry:

- **dedicated-pensieve** (metadata service)
- **core-gateway** — HTTP query routing gateway

Chart sources are configured via operator flags:

```
--metadata-chart-source=oci://ghcr.io/firebolt-db/dedicated-pensieve
--gateway-chart-source=oci://ghcr.io/firebolt-db/core-gateway
```

For local development, point to a local chart directory:

```
--metadata-chart-source=../dedicated-pensieve/helm
--gateway-chart-source=../core-gateway/helm
```
