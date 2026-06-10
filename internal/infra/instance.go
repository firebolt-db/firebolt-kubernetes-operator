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
