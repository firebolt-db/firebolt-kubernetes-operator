/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestBuildEnvoyConfigYAMLParses guards two invariants:
//  1. The emitted Envoy static config is valid YAML (catches any
//     fmt.Sprintf mis-escaping, e.g. Lua patterns containing `%`).
//  2. The config is a pure function of the FireboltInstance. It must not
//     contain anything engine-specific, otherwise every engine lifecycle event
//     would regenerate the ConfigMap and force a gateway rollout.
func TestBuildEnvoyConfigYAMLParses(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	}
	got := buildEnvoyConfigYAML(inst)

	var out map[string]any
	if err := yaml.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("emitted envoy config is not valid YAML: %v\n---\n%s", err, got)
	}

	// Namespace must be baked into the :authority rewrite.
	if !strings.Contains(got, "-service.ns-1.svc.cluster.local:3473") {
		t.Errorf("emitted config does not contain the expected per-namespace :authority rewrite; got:\n%s", got)
	}

	// Engine-set independence: sentinel substrings that would indicate we
	// are still templating per-engine content.
	for _, forbidden := range []string{"advanced_engines", "advancedMode"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("emitted config contains %q; the gateway must be engine-agnostic", forbidden)
		}
	}
}

// TestBuildEnvoyConfigYAMLStableAcrossInstances ensures two different
// namespaces produce configs that differ only in the namespace-derived
// authority rewrite, not in any other structural way.
func TestBuildEnvoyConfigYAMLStableAcrossInstances(t *testing.T) {
	a := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns-a"},
	})
	b := buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns-b"},
	})
	// Replacing the namespace-specific fragment in b should yield a.
	normalised := strings.ReplaceAll(b, "-service.ns-b.svc.cluster.local:3473", "-service.ns-a.svc.cluster.local:3473")
	if normalised != a {
		t.Fatal("configs differ in more than the namespace-scoped authority rewrite; a and b should be structurally identical")
	}
}
