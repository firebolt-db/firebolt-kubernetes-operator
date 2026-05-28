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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestUserGatewayServiceAccountName pins down the helper that decides
// whether the operator manages the gateway ServiceAccount and RBAC
// (returns "") or hands ownership over to the user (returns the
// user-supplied name). The behavior is load-bearing: when this
// returns non-empty, ensureGatewayResources skips ensureGatewayRBAC
// entirely; otherwise the operator creates the SA + Role + RoleBinding
// at the default name.
func TestUserGatewayServiceAccountName(t *testing.T) {
	mk := func(template *corev1.PodTemplateSpec) *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Gateway: computev1alpha1.GatewaySpec{Template: template},
			},
		}
	}

	t.Run("nil template returns empty", func(t *testing.T) {
		if got := userGatewayServiceAccountName(mk(nil)); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("empty template returns empty", func(t *testing.T) {
		if got := userGatewayServiceAccountName(mk(&corev1.PodTemplateSpec{})); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("template with empty serviceAccountName returns empty", func(t *testing.T) {
		inst := mk(&corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: ""}})
		if got := userGatewayServiceAccountName(inst); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("template with user serviceAccountName returns it verbatim", func(t *testing.T) {
		inst := mk(&corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "my-gateway-sa"}})
		if got := userGatewayServiceAccountName(inst); got != "my-gateway-sa" {
			t.Errorf("got %q, want my-gateway-sa", got)
		}
	})
}

// TestEffectiveGatewayPodTemplate_ServiceAccountFallback pins down
// the partner behavior on the pod-template side: when the user did
// not set spec.gateway.template.spec.serviceAccountName, the operator
// stamps gatewayServiceAccountName(instance.Name) on the rendered pod
// so it binds to the operator-managed SA. When the user did set it,
// the user value passes through and the operator skips RBAC creation
// (verified by TestUserGatewayServiceAccountName).
func TestEffectiveGatewayPodTemplate_ServiceAccountFallback(t *testing.T) {
	envoyYAML := "" // contents don't matter for SA assertion
	baseLabels := map[string]string{"firebolt.io/instance": "fb"}

	t.Run("operator-managed SA when user did not set it", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", contentHash(envoyYAML), baseLabels)
		want := gatewayServiceAccountName(inst.Name)
		if pt.Spec.ServiceAccountName != want {
			t.Errorf("ServiceAccountName = %q, want %q (operator-managed default)", pt.Spec.ServiceAccountName, want)
		}
	})

	t.Run("user SA passes through when set", func(t *testing.T) {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "default"},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Gateway: computev1alpha1.GatewaySpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{ServiceAccountName: "my-gateway-sa"},
					},
				},
			},
		}
		pt := effectiveGatewayPodTemplate(inst, "fb-gateway-config", contentHash(envoyYAML), baseLabels)
		if pt.Spec.ServiceAccountName != "my-gateway-sa" {
			t.Errorf("ServiceAccountName = %q, want my-gateway-sa", pt.Spec.ServiceAccountName)
		}
	})
}
