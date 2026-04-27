# Monitoring

This document describes how the Firebolt operator exposes Prometheus metrics for the components it manages.

## Metrics endpoints

| Component | Port | Name | Path | What it exposes |
|---|---|---|---|---|
| Engine pods | 9090 | `metrics` | `/metrics` | `firebolt_running_queries`, `firebolt_suspended_queries`, and other engine-internal gauges |
| Gateway pods (Envoy) | 9090 (default) | `metrics` | `/stats/prometheus` | Envoy connection, request, and cluster stats |
| Operator pod | Configurable via `metrics.bindAddress` | `https` or `http` | `/metrics` | controller-runtime reconciliation, workqueue, REST client, and Go runtime metrics |

The gateway metrics port defaults to 9090 and is configurable per FireboltInstance CR via `spec.gateway.metricsPort`. Metadata pods do not currently expose a Prometheus metrics endpoint.

### Operator metrics mode

The operator metrics endpoint mode is controlled by two Helm values:

| Mode | `metrics.secure` | `metrics.bindAddress` | Port name | Scheme |
|---|---|---|---|---|
| HTTPS (default) | `true` | `:8443` | `https` | `https` with authn/authz and self-signed TLS |
| HTTP | `false` | `:8080` | `http` | plain `http` |

The operator PodMonitor template automatically adapts its port reference, scheme, bearer token, and TLS configuration based on `metrics.secure`.

## Scraping with Prometheus

The operator Helm chart ships optional `PodMonitor` resources (one per component type) that can be enabled via `values.yaml`:

```yaml
podMonitor:
  engines:
    enabled: true
  gateway:
    enabled: true
  operator:
    enabled: true
  allNamespaces: false   # set true when the operator watches all namespaces
```

Each PodMonitor uses label selectors to match the relevant pods:

- **Engines**: `firebolt.io/engine` (exists) -- matches all engine pods regardless of engine name
- **Gateway**: `firebolt.io/component=gateway`
- **Operator**: `control-plane=controller-manager` + chart selector labels

When `allNamespaces` is true, `namespaceSelector.any: true` is added so pods in any namespace are discovered. This does not apply to the operator PodMonitor since the operator always runs in the release namespace.

### Per-instance monitoring

The chart-level PodMonitors apply uniform scrape configuration to all instances in scope. If you need per-instance control (different intervals, selective enablement, custom relabelings), disable the chart-level PodMonitors and deploy your own alongside each FireboltInstance or FireboltEngine CR. The label selectors to use are:

- Engine pods: `firebolt.io/engine: <engine-name>`
- Gateway pods: `firebolt.io/instance: <instance-name>`, `firebolt.io/component: gateway`

## Architecture decisions

### Helm chart templates, not operator reconciliation

PodMonitor resources are shipped as Helm templates, not created by the operator's Go reconciliation loop. This follows the dominant industry pattern (used by cert-manager, Strimzi, FoundationDB operator, and others). CloudNativePG tried the operator-managed approach and deprecated it in v1.26 because:

- It creates a hard dependency on the Prometheus Operator CRDs — the operator fails to reconcile on clusters where the CRDs are not installed.
- The operator overwrites user customizations (scrape intervals, relabelings, TLS config) on every reconcile.
- It adds RBAC complexity for `monitoring.coreos.com` resources.
- Platform teams want full ownership of their monitoring configuration.

### Gateway stats listener

The Envoy admin interface (port 9901) is bound to `127.0.0.1` and must stay that way. It exposes mutation endpoints (`POST /healthcheck/fail`, `POST /quitquitquit`) that the preStop hook depends on for graceful shutdown. Binding admin to `0.0.0.0` would allow any pod in the cluster to drain or kill gateway pods.

Instead, a separate read-only stats listener is added on the metrics port (default 9090). This listener proxies only `/stats/prometheus` from the admin interface via an internal static cluster, exposing no mutation endpoints.

### Consistent metrics port

All components default to port 9090 named `metrics` via `ComponentSpec.MetricsPort`. This means PodMonitors can always reference `port: metrics` without knowing the actual port number. The port is overridable per FireboltInstance CR for non-standard binaries.

### Cross-namespace support

When the operator watches all namespaces (`watchNamespace` is empty), engine, gateway, and metadata pods may live in namespaces other than the operator's. Setting `podMonitor.allNamespaces: true` adds `namespaceSelector.any: true` to the PodMonitors so Prometheus discovers pods across all namespaces.
