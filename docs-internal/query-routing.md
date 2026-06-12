### Why no EndpointSlice gate

A previous design (removed in commits `be577f2` and `d6dce81`) had the Firebolt Operator wait in `Switching` for the cluster Service's EndpointSlice to contain at least one Ready endpoint. It was deleted when the cluster Service became headless: with kube-proxy out of the data path, K8s automatically excludes not-ready pods from the headless DNS A-record set, and the gate was redundant.

The same conclusion holds for the *symmetric* version of the gate: "wait until the EndpointSlice no longer references the **draining** generation before transitioning from `draining` to `cleaning`":

- The chain in [Graceful pod shutdown](#graceful-pod-shutdown) closes the race in the data plane: any late request on a draining pod gets a clean retriable 503 and the gateway recovers before the client.
- An EndpointSlice gate only shifts *when* SIGTERM fires (after the slice update vs. concurrent with it). It does not shrink the window where Envoy might still pick a draining host, which is bounded by the active-health-check interval, not by slice propagation.
- Reintroducing it costs an extra Watch + RBAC + reconcile-state field with no liveness improvement under the existing data-plane contract.

If you find yourself wanting to add such a gate to fix a 5xx during cutover, the right question is which of mechanisms 1-5 above is broken, not whether to bolt on a sixth.
