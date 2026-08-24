package infra

import (
	"strings"
	"testing"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

func TestBuildEngineDefaults_TypedKindAndConventionalName(t *testing.T) {
	d := BuildEngineDefaults("firebolt", "", "engine-sa")
	if d.Kind != "FireboltEngineDefaults" {
		t.Errorf("Kind = %q, want FireboltEngineDefaults", d.Kind)
	}
	if d.Name != v1alpha1.FireboltEngineDefaultsDefaultName {
		t.Errorf("Name = %q, want %q", d.Name, v1alpha1.FireboltEngineDefaultsDefaultName)
	}
	if d.Spec.Template.Spec.ServiceAccountName != "engine-sa" {
		t.Errorf("serviceAccountName = %q, want engine-sa", d.Spec.Template.Spec.ServiceAccountName)
	}
	raw, err := yaml.Marshal(d)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "kind: FireboltEngineDefaults") {
		t.Errorf("marshaled YAML missing kind:\n%s", text)
	}
	if !strings.Contains(text, "serviceAccountName: engine-sa") {
		t.Errorf("marshaled YAML missing serviceAccountName:\n%s", text)
	}
}
