/*
Copyright 2026 Firebolt Analytics.

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
	"os"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// examples/instance-auth-tls.yaml is the reference an operator copies an auth
// block from. Nothing in CI applies it — it needs cert-manager, an issuer, a
// Secret and a reachable provider — so without this its auth block can drift
// out of admissibility unnoticed, handing whoever copies it a rejection.
// ValidateAuth is the whole of what the webhook checks, and it is exactly the
// part of the file no CI job reaches.
func TestAuthExample_PassesAdmissionValidation(t *testing.T) {
	const path = "../../examples/instance-auth-tls.yaml"

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var instance *computev1alpha1.FireboltInstance
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		if !strings.Contains(doc, "kind: FireboltInstance") {
			continue
		}
		var parsed computev1alpha1.FireboltInstance
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("parse the FireboltInstance document in %s: %v", path, err)
		}
		instance = &parsed
		break
	}
	if instance == nil {
		t.Fatalf("%s declares no FireboltInstance", path)
	}
	if instance.Spec.Auth == nil {
		t.Fatalf("%s is the auth reference but declares no spec.auth", path)
	}

	// The example is the reference for OIDC too, so it must keep showing both
	// ways a provider names its servers — a flat discoveryURL and a
	// target/exchange pair — or the shape this test guards is only half of one.
	var flat, twoHop int
	for i := range instance.Spec.Auth.OIDC.Providers {
		switch p := &instance.Spec.Auth.OIDC.Providers[i]; {
		case p.Target != nil && p.Exchange != nil:
			twoHop++
		case p.DiscoveryURL != "":
			flat++
		}
	}
	if flat == 0 || twoHop == 0 {
		t.Errorf("providers show %d flat and %d two-hop; want at least one of each", flat, twoHop)
	}

	for _, err := range computev1alpha1.ValidateAuth(instance) {
		t.Errorf("spec.auth would be rejected at admission: %v", err)
	}
}
