package infra

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// EngineDefaultsSummary is a one-line view of a FireboltEngineDefaults.
type EngineDefaultsSummary struct {
	Name         string
	BoundEngines int32
	Ready        *bool
}

// GetEngineDefaults fetches a FireboltEngineDefaults by name.
func (c *Client) GetEngineDefaults(ctx context.Context, name string) (*v1alpha1.FireboltEngineDefaults, error) {
	out, err := c.kubectl.getNamed(c.namespace, resourceEngineDefaults, name).Capture(ctx)
	if err != nil {
		return nil, err
	}
	var obj v1alpha1.FireboltEngineDefaults
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return nil, fmt.Errorf("parsing FireboltEngineDefaults %q: %w", name, err)
	}
	return &obj, nil
}

// ListEngineDefaults lists FireboltEngineDefaults in the namespace.
func (c *Client) ListEngineDefaults(ctx context.Context) ([]EngineDefaultsSummary, error) {
	out, err := c.kubectl.get(c.namespace, resourceEngineDefaults).Capture(ctx)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.FireboltEngineDefaultsList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing FireboltEngineDefaults list: %w", err)
	}
	summaries := make([]EngineDefaultsSummary, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		summaries = append(summaries, EngineDefaultsSummary{
			Name:         d.Name,
			BoundEngines: d.Status.BoundEngines,
			Ready:        readyFromConditions(d.Status.Conditions),
		})
	}
	return summaries, nil
}

// BuildEngineDefaults constructs a typed FireboltEngineDefaults. Callers
// apply it with kubectl; the type is the compile-time contract.
func BuildEngineDefaults(namespace, name, serviceAccount string) *v1alpha1.FireboltEngineDefaults {
	if name == "" {
		name = v1alpha1.FireboltEngineDefaultsDefaultName
	}
	return &v1alpha1.FireboltEngineDefaults{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "FireboltEngineDefaults",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.FireboltEngineDefaultsSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{ServiceAccountName: serviceAccount},
			},
		},
	}
}
