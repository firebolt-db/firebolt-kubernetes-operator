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

// Schemes a port-forward can advertise. SchemeUnknown prints a
// protocol-neutral endpoint instead of asserting a scheme the target may not
// accept.
const (
	SchemeHTTPS   = "https"
	SchemeHTTP    = "http"
	SchemeUnknown = ""
)

// GatewayServingScheme reports the scheme a port-forward to inst's gateway
// client listener should advertise: https while that listener serves TLS, http
// while it serves plaintext, SchemeUnknown while it serves neither.
//
// Read from status rather than spec: status.gatewayTLS is populated only once
// the listener serves that posture, and stays cleared for the whole fail-closed
// window while the posture is being tightened. One window still reports http
// early — disabling gateway TLS clears the status before the TLS-serving pods
// have drained.
func GatewayServingScheme(inst *v1alpha1.FireboltInstance) string {
	switch {
	case inst == nil:
		return SchemeUnknown
	case inst.Status.GatewayTLS != nil:
		return SchemeHTTPS
	case gatewayTLSRequested(inst):
		return SchemeUnknown
	default:
		return SchemeHTTP
	}
}

// EngineFleetServingScheme reports the scheme a port-forward to any engine's
// query listener should advertise, on the same three-valued contract as
// GatewayServingScheme. TLS is configured on the owning Instance, reached from
// an engine via spec.instanceRef.
//
// The answer is deliberately fleet-wide: status.engineTLS.reencrypting turns
// true only after every engine has rolled onto TLS and stays true until every
// engine has rolled back off, so both steady states are unambiguous and every
// mid-rollout state reports SchemeUnknown rather than a scheme that holds for
// only part of the fleet. A per-engine answer would need what the engine's
// active generation serves, which no status field exposes.
func EngineFleetServingScheme(inst *v1alpha1.FireboltInstance) string {
	switch {
	case inst == nil:
		return SchemeUnknown
	case engineTLSRequested(inst):
		if inst.Status.EngineTLS != nil && inst.Status.EngineTLS.Reencrypting {
			return SchemeHTTPS
		}
		return SchemeUnknown
	case inst.Status.EngineTLS != nil:
		return SchemeUnknown
	default:
		return SchemeHTTP
	}
}

func gatewayTLSRequested(inst *v1alpha1.FireboltInstance) bool {
	return inst.Spec.TLS != nil && inst.Spec.TLS.Gateway != nil && inst.Spec.TLS.Gateway.Enabled
}

func engineTLSRequested(inst *v1alpha1.FireboltInstance) bool {
	return inst.Spec.TLS != nil && inst.Spec.TLS.Engine != nil && inst.Spec.TLS.Engine.Enabled
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
