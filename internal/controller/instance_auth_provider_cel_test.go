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
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// An OIDC provider must name the server clients authenticate at exactly
// once — as the flat discoveryURL or as target. packdb refuses to start on a
// provider that names it twice or not at all, which takes every engine in the
// Instance down, so the invariant is a CRD CEL rule and holds even with the
// validating webhook disabled (the shipped Helm default). This suite runs
// against envtest with the CRD bases applied and NO webhook installed (see
// suite_test.go), so every verdict below is the API server evaluating that
// rule alone.
var _ = Describe("FireboltInstance OIDC provider server choice (CEL, webhook-free)", func() {
	const ns = "default"
	ctx := context.Background()

	mkInstance := func(name string, provider computev1alpha1.OIDCProviderSpec) *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Auth: &computev1alpha1.AuthSpec{
					Enabled: true,
					Local: &computev1alpha1.LocalAuthSpec{
						Admin: computev1alpha1.AdminSpec{
							Password: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "admin-creds"},
								Key:                  "password",
							},
						},
						SigningKeys: &computev1alpha1.SigningKeyPolicy{
							CertManager: computev1alpha1.CertManagerSpec{
								IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
							},
						},
					},
					OIDC: &computev1alpha1.OIDCAuthSpec{
						Providers: []computev1alpha1.OIDCProviderSpec{provider},
					},
				},
			},
		}
	}

	target := func() *computev1alpha1.OIDCTargetSpec {
		return &computev1alpha1.OIDCTargetSpec{
			DiscoveryURL:            "https://idp.example.com/.well-known/openid-configuration",
			TokenEndpointAuthMethod: "client_secret_post",
		}
	}
	exchange := func() *computev1alpha1.OIDCExchangeSpec {
		return &computev1alpha1.OIDCExchangeSpec{
			DiscoveryURL: "https://exchange.example.com/.well-known/oauth-authorization-server",
		}
	}

	It("admits a two-hop provider naming target and exchange", func() {
		inst := mkInstance("cel-provider-two-hop", computev1alpha1.OIDCProviderSpec{
			Name:            "firehq",
			Target:          target(),
			Exchange:        exchange(),
			UsernameMapping: "{{ sub }}",
			RoleMapping: &computev1alpha1.RoleMappingSpec{
				Claim: "role",
				Map:   []computev1alpha1.RoleMappingEntrySpec{{Value: "admin", Role: "account_admin"}},
			},
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()
	})

	It("admits a flat-discoveryURL provider, the pre-existing single-server shape", func() {
		inst := mkInstance("cel-provider-flat", computev1alpha1.OIDCProviderSpec{
			Name:            "okta",
			DiscoveryURL:    "https://okta.example.com/.well-known/openid-configuration",
			UsernameMapping: "{{ email }}",
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()
	})

	It("rejects a provider setting both discoveryURL and target", func() {
		inst := mkInstance("cel-provider-both", computev1alpha1.OIDCProviderSpec{
			Name:            "firehq",
			DiscoveryURL:    "https://idp.example.com/.well-known/openid-configuration",
			Target:          target(),
			UsernameMapping: "{{ sub }}",
		})
		err := k8sClient.Create(ctx, inst)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(),
			"expected a schema/CEL Invalid rejection, got: %v", err)
	})

	It("rejects a provider naming no server, even when it sets an exchange", func() {
		inst := mkInstance("cel-provider-neither", computev1alpha1.OIDCProviderSpec{
			Name:            "firehq",
			Exchange:        exchange(),
			UsernameMapping: "{{ sub }}",
		})
		err := k8sClient.Create(ctx, inst)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(),
			"expected a schema/CEL Invalid rejection, got: %v", err)
	})

	It("admits a flat discoveryURL alongside an exchange, the two-hop shape without a pin", func() {
		inst := mkInstance("cel-provider-flat-exchange", computev1alpha1.OIDCProviderSpec{
			Name:            "firehq",
			DiscoveryURL:    "https://idp.example.com/.well-known/openid-configuration",
			Exchange:        exchange(),
			UsernameMapping: "{{ sub }}",
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()
	})

	It("admits a roleMapping that omits claim, leaving the engine to default it", func() {
		inst := mkInstance("cel-provider-no-claim", computev1alpha1.OIDCProviderSpec{
			Name:            "firehq",
			Target:          target(),
			Exchange:        exchange(),
			UsernameMapping: "{{ sub }}",
			RoleMapping: &computev1alpha1.RoleMappingSpec{
				Map: []computev1alpha1.RoleMappingEntrySpec{{Value: "admin", Role: "account_admin"}},
			},
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()
	})

	// The engine refuses a claim value naming two roles rather than guess an
	// order to apply them in, so the keyed list has to reject it here — the
	// validating webhook is disabled in the shipped chart and cannot.
	It("rejects a roleMapping repeating a claim value", func() {
		inst := mkInstance("cel-provider-dup-claim-value", computev1alpha1.OIDCProviderSpec{
			Name:            "firehq",
			Target:          target(),
			Exchange:        exchange(),
			UsernameMapping: "{{ sub }}",
			RoleMapping: &computev1alpha1.RoleMappingSpec{
				Claim: "role",
				Map: []computev1alpha1.RoleMappingEntrySpec{
					{Value: "admin", Role: "account_admin"},
					{Value: "admin", Role: "reader"},
				},
			},
		})
		err := k8sClient.Create(ctx, inst)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(),
			"expected a schema/CEL Invalid rejection, got: %v", err)
	})

	It("admits swapping a flat discoveryURL for a target in one update", func() {
		inst := mkInstance("cel-provider-flat-to-target", computev1alpha1.OIDCProviderSpec{
			Name:            "okta",
			DiscoveryURL:    "https://okta.example.com/.well-known/openid-configuration",
			UsernameMapping: "{{ email }}",
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		var cur computev1alpha1.FireboltInstance
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), &cur)).To(Succeed())
		cur.Spec.Auth.OIDC.Providers[0].DiscoveryURL = ""
		cur.Spec.Auth.OIDC.Providers[0].Target = target()
		Expect(k8sClient.Update(ctx, &cur)).To(Succeed())
	})

	It("rejects dropping a flat discoveryURL to leave the provider naming no server", func() {
		inst := mkInstance("cel-provider-drop-url", computev1alpha1.OIDCProviderSpec{
			Name:            "okta",
			DiscoveryURL:    "https://okta.example.com/.well-known/openid-configuration",
			UsernameMapping: "{{ email }}",
		})
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		var cur computev1alpha1.FireboltInstance
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), &cur)).To(Succeed())
		cur.Spec.Auth.OIDC.Providers[0].DiscoveryURL = ""
		err := k8sClient.Update(ctx, &cur)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(),
			"expected a schema/CEL Invalid rejection, got: %v", err)
	})
})
