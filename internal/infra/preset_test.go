package infra

import (
	"strings"
	"testing"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

func TestBuildEnginePreset_TypedKindAndDefaultName(t *testing.T) {
	d := BuildEnginePreset("firebolt", "", "engine-sa")
	if d.Kind != "FireboltEnginePreset" {
		t.Errorf("Kind = %q, want FireboltEnginePreset", d.Kind)
	}
	if d.Name != v1alpha1.FireboltEnginePresetDefaultName {
		t.Errorf("Name = %q, want %q", d.Name, v1alpha1.FireboltEnginePresetDefaultName)
	}
	if d.Spec.Template.Spec.ServiceAccountName != "engine-sa" {
		t.Errorf("serviceAccountName = %q, want engine-sa", d.Spec.Template.Spec.ServiceAccountName)
	}
	raw, err := yaml.Marshal(d)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "kind: FireboltEnginePreset") {
		t.Errorf("marshaled YAML missing kind:\n%s", text)
	}
	if !strings.Contains(text, "serviceAccountName: engine-sa") {
		t.Errorf("marshaled YAML missing serviceAccountName:\n%s", text)
	}
}
