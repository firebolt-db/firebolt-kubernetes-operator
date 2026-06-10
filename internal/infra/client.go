package infra

// Client is a namespace-scoped handle to a Kubernetes cluster running the
// Firebolt operator. All operations target the namespace it was constructed
// with; the Kubernetes context is whatever kubectl resolves on the host
// (the ambient --context/--kubeconfig, optionally overridden at construction).
type Client struct {
	namespace string
	kubectl   kubectl
}

// NewClient constructs a client scoped to namespace. A non-empty context or
// kubeconfig is threaded into every kubectl invocation as --context /
// --kubeconfig; empty values leave kubectl's ambient resolution untouched.
func NewClient(namespace, context, kubeconfig string) *Client {
	var global []string
	if context != "" {
		global = append(global, "--context", context)
	}
	if kubeconfig != "" {
		global = append(global, "--kubeconfig", kubeconfig)
	}
	return &Client{namespace: namespace, kubectl: kubectl{global: global}}
}
