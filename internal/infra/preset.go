package infra

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// DefaultEnginePresetName is the conventional per-namespace
// FireboltEnginePreset object name, re-exported so CLI commands can
// default a name argument without importing the API package.
const DefaultEnginePresetName = v1alpha1.FireboltEnginePresetDefaultName

// EnginePresetSummary is a one-line view of a FireboltEnginePreset.
type EnginePresetSummary struct {
	Name         string
	BoundEngines int32
	Ready        *bool
}

// GetEnginePreset fetches a FireboltEnginePreset by name.
func (c *Client) GetEnginePreset(ctx context.Context, name string) (*v1alpha1.FireboltEnginePreset, error) {
	out, err := c.kubectl.getNamed(c.namespace, resourceEnginePreset, name).Capture(ctx)
	if err != nil {
		return nil, err
	}
	var obj v1alpha1.FireboltEnginePreset
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		return nil, fmt.Errorf("parsing FireboltEnginePreset %q: %w", name, err)
	}
	return &obj, nil
}

// ListEnginePreset lists FireboltEnginePreset in the namespace.
func (c *Client) ListEnginePreset(ctx context.Context) ([]EnginePresetSummary, error) {
	out, err := c.kubectl.get(c.namespace, resourceEnginePreset).Capture(ctx)
	if err != nil {
		return nil, err
	}
	var list v1alpha1.FireboltEnginePresetList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parsing FireboltEnginePreset list: %w", err)
	}
	summaries := make([]EnginePresetSummary, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		summaries = append(summaries, EnginePresetSummary{
			Name:         d.Name,
			BoundEngines: d.Status.BoundEngines,
			Ready:        readyFromConditions(d.Status.Conditions),
		})
	}
	return summaries, nil
}

// BuildEnginePreset constructs a typed FireboltEnginePreset. Callers
// apply it with kubectl; the type is the compile-time contract.
func BuildEnginePreset(namespace, name, serviceAccount string) *v1alpha1.FireboltEnginePreset {
	if name == "" {
		name = DefaultEnginePresetName
	}
	return &v1alpha1.FireboltEnginePreset{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "FireboltEnginePreset",
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.FireboltEnginePresetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{ServiceAccountName: serviceAccount},
			},
		},
	}
}
