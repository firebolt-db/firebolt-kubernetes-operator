package infra

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// InstanceSummary is a one-line view of a FireboltInstance for `instance list`.
type InstanceSummary struct {
	Name  string
	Phase string
	Ready *bool
}

// ListInstanceObjects lists the FireboltInstance objects in the namespace. It
// backs the summary view (ListInstances) from a single kubectl get.
func (c *Client) ListInstanceObjects(ctx context.Context) ([]v1alpha1.FireboltInstance, error) {
	out, err := c.kubectl.get(c.namespace, resourceInstance).Capture(ctx)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.FireboltInstanceList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing FireboltInstance list: %w", err)
	}
	return list.Items, nil
}

// GetInstance fetches a single FireboltInstance by name (one `kubectl get`).
func (c *Client) GetInstance(ctx context.Context, name string) (*v1alpha1.FireboltInstance, error) {
	out, err := c.kubectl.getNamed(c.namespace, resourceInstance, name).Capture(ctx)
	if err != nil {
		return nil, err
	}
	var inst v1alpha1.FireboltInstance
	if err := json.Unmarshal([]byte(out), &inst); err != nil {
		return nil, fmt.Errorf("parsing FireboltInstance %q: %w", name, err)
	}
	return &inst, nil
}

// GatewayTLSEnabled reports whether inst terminates TLS on its Envoy gateway's
// client-facing listener — so a port-forward to the gateway Service speaks
// https, not http. Gateway TLS terminates on the same forwarded port (there is
// no separate plaintext port), so this is the sole determinant of the scheme.
func GatewayTLSEnabled(inst *v1alpha1.FireboltInstance) bool {
	return inst != nil && inst.Spec.TLS != nil && inst.Spec.TLS.Gateway != nil && inst.Spec.TLS.Gateway.Enabled
}

// EngineTLSEnabled reports whether inst's engines terminate TLS on their query
// listener — so a port-forward to an engine Service speaks https, not http.
// Engine TLS replaces the plaintext listener on the same port, so this alone
// determines the scheme. TLS lives on the owning Instance (engines carry no TLS
// spec), reached via FireboltEngine.Spec.InstanceRef.
func EngineTLSEnabled(inst *v1alpha1.FireboltInstance) bool {
	return inst != nil && inst.Spec.TLS != nil && inst.Spec.TLS.Engine != nil && inst.Spec.TLS.Engine.Enabled
}

// ListInstances lists FireboltInstances in the namespace as one-line summaries.
func (c *Client) ListInstances(ctx context.Context) ([]InstanceSummary, error) {
	instances, err := c.ListInstanceObjects(ctx)
	if err != nil {
		return nil, err
	}
	summaries := make([]InstanceSummary, 0, len(instances))
	for i := range instances {
		inst := &instances[i]
		summaries = append(summaries, InstanceSummary{
			Name:  inst.Name,
			Phase: string(inst.Status.Phase),
			Ready: readyFromConditions(inst.Status.Conditions),
		})
	}
	return summaries, nil
}

// ListInstancesScript renders the get that backs ListInstances (--print-commands).
func (c *Client) ListInstancesScript() string {
	return c.kubectl.get(c.namespace, resourceInstance).Render()
}
