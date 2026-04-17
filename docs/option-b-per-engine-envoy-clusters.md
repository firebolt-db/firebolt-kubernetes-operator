# Option B: per-engine Envoy clusters with active health checks

This is a **forward-looking design sketch**, not a description of what the
operator does today. It is kept here so we can come back to it if/when the
current Option A (dynamic forward proxy + headless per-engine Service) shows
a limitation we care about.

## What Option A gives us today

- One static gateway config per FireboltInstance. The ConfigMap never
  depends on the set of engines, so engine create / delete / scale /
  blue-green events never trigger a gateway rollout.
- DNS-driven upstream selection via Envoy's `dynamic_forward_proxy`
  against each engine's headless Service.
- Pod readiness is reflected in DNS almost immediately (headless Service
  with `publishNotReadyAddresses: false`), so kube-proxy is not in the
  data path and there is no terminating-endpoint SYN race.
- Transport-level retries (`connect-failure`, `refused-stream`, `reset`)
  to hide the tiny endpoint-propagation window. 5xx are deliberately not
  retried.

## What Option A does not give us

1. **No L7 health checking.** Envoy picks a pod IP per request based on
   whatever DNS returned. If a pod is listening on port 3473 but is
   internally unhealthy in a way the readiness probe does not catch,
   Envoy will still send it requests until DNS drops it.
2. **No per-upstream circuit breaking / outlier detection.** DFP
   endpoints are ephemeral; we cannot keep a connection pool with
   failure stats per pod and eject misbehaving ones.
3. **Re-resolution granularity is DNS-wide, not per-endpoint.** We tune
   `dns_refresh_rate` and `host_ttl` globally; we cannot, for instance,
   react faster to a specific host that just returned a connection
   reset.
4. **No push-based endpoint updates.** DFP polls DNS; it does not
   subscribe to Kubernetes EndpointSlice changes, so we are always at
   most one DNS TTL behind reality.

## What Option B would look like

Move from "one DFP cluster that resolves any hostname" to "one Envoy
cluster per engine, discovered dynamically".

Two flavours:

### B1. Cluster-per-engine via static clusters, written by the operator

- The operator generates one Envoy cluster block per FireboltEngine,
  each of type `STRICT_DNS` against the engine's headless Service, with
  per-cluster `health_checks` (HTTP GET `/health/ready` on the engine
  health port) and `outlier_detection`.
- Route config uses `cluster_header: x-firebolt-engine` (or a Lua-set
  header) so requests are dispatched to the matching cluster without
  DNS rewriting at request time.
- Engine add/remove/scale events rewrite the gateway ConfigMap and
  trigger a gateway rollout — which is exactly what Option A was
  designed to avoid. Mitigations: SDS/ADS served from a file and
  `filesystem` xDS on the Envoy side so the container does not
  restart.

### B2. Cluster-per-engine via filesystem xDS (CDS/EDS from files)

- Gateway Envoy is configured once (statically) to read CDS and EDS
  from files in a mounted volume.
- The operator writes per-engine cluster/endpoint snapshots into that
  volume. Envoy picks them up via inotify without a restart.
- This preserves Option A's "gateway never rolls out on engine events"
  property while recovering the active-health-check and
  outlier-detection benefits.
- Cost: a small xDS-writer component (or a second controller loop)
  inside the operator, and a shared volume between the writer and
  the gateway pods.

## When would we actually do this?

Option B becomes interesting if any of the following turn into real
problems:

- We see engine pods that pass readiness but fail in flight, and we
  want the gateway to stop sending them traffic without waiting for the
  readiness probe to flip.
- We want per-pod circuit breaking (e.g. eject after N consecutive
  `reset`s within a window) rather than just per-request retries.
- We want to surface engine-level health state in gateway metrics
  without scraping each engine individually.
- We want to route based on something richer than the engine name
  (e.g. weighted routing during a canary) — this is awkward under DFP
  but natural under CDS.

## What to revisit before choosing

- Is DFP's DNS-refresh latency actually a problem in practice? We tuned
  it down to 1s; in measured runs the terminating-endpoint window is
  shorter than that, because the headless Service drops not-ready pods
  from DNS as soon as the readiness probe fails.
- Can we get most of the "stop hammering a bad pod" benefit by tightening
  the engine's own readiness probe and/or adding a `preStop` hook on
  engine pods? (See `docs/level-driven-reconciliation.md` for where the
  engine-pod `preStop` discussion lives.)

If the answer to both is "yes, and it's fine", Option A is enough. If
not, **B2 (filesystem xDS)** is the right target, because it keeps the
key Option A property — engine lifecycle events do not roll out the
gateway — while giving us real health-checked clusters.
