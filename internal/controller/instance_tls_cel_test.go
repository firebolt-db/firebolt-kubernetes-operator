/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// FB-896 #5: the engine TLS issuerRef must be immutable while engine TLS stays
// enabled, enforced by a field-scoped CEL transition rule on the CRD itself so
// it holds even when the validating webhook is disabled (the shipped Helm
// default). This suite runs against envtest with the CRD bases applied and NO
// webhook installed (see suite_test.go), so every rejection below is the API
// server evaluating the CEL rule alone — not the webhook.
var _ = Describe("FireboltInstance engine TLS issuerRef immutability (CEL, webhook-free)", func() {
	const ns = "default"
	ctx := context.Background()

	engineTLS := func(issuer string) *computev1alpha1.TLSListenerSpec {
		return &computev1alpha1.TLSListenerSpec{
			Enabled: true,
			CertManager: &computev1alpha1.CertManagerSpec{
				IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: issuer, Kind: "ClusterIssuer"},
			},
		}
	}
	mkInstance := func(name string, engine, gateway *computev1alpha1.TLSListenerSpec) *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       computev1alpha1.FireboltInstanceSpec{TLS: &computev1alpha1.TLSSpec{Engine: engine, Gateway: gateway}},
		}
	}

	// mutateWithRetry re-fetches the instance and applies mutate, retrying on
	// the optimistic-concurrency conflict the running controller can cause
	// (finalizer/status writes). It returns the first non-conflict Update
	// result — which is the CEL verdict we want to assert.
	mutateWithRetry := func(name string, mutate func(*computev1alpha1.FireboltInstance)) error {
		var result error
		Eventually(func() bool {
			var cur computev1alpha1.FireboltInstance
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cur); err != nil {
				result = err
				return true
			}
			mutate(&cur)
			result = k8sClient.Update(ctx, &cur)
			return !apierrors.IsConflict(result)
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
		return result
	}

	It("rejects changing the engine issuerRef while engine TLS stays enabled", func() {
		inst := mkInstance("cel-engine-issuer-frozen", engineTLS("ca-a"), nil)
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		err := mutateWithRetry(inst.Name, func(cur *computev1alpha1.FireboltInstance) {
			cur.Spec.TLS.Engine.CertManager.IssuerRef.Name = "ca-b"
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("issuerRef is immutable"))
	})

	It("allows changing the engine issuerRef in the same update that disables engine TLS", func() {
		inst := mkInstance("cel-engine-issuer-disable", engineTLS("ca-a"), nil)
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		// Disabling breaks the continuously-enabled precondition, so a new
		// issuer for a later re-enable (fresh certs, no overlap) is permitted.
		Expect(mutateWithRetry(inst.Name, func(cur *computev1alpha1.FireboltInstance) {
			cur.Spec.TLS.Engine.Enabled = false
			cur.Spec.TLS.Engine.CertManager.IssuerRef.Name = "ca-b"
		})).To(Succeed())
	})

	It("allows changing the engine key size while enabled (only the issuer is frozen)", func() {
		inst := mkInstance("cel-engine-size-mutable", engineTLS("ca-a"), nil)
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		Expect(mutateWithRetry(inst.Name, func(cur *computev1alpha1.FireboltInstance) {
			cur.Spec.TLS.Engine.CertManager.Size = 256
		})).To(Succeed())
	})

	It("does NOT freeze the gateway issuerRef (the rule is engine-scoped)", func() {
		inst := mkInstance("cel-gateway-issuer-mutable", nil, engineTLS("ca-a"))
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		// The gateway shares TLSListenerSpec/CertManagerSpec but carries no
		// immutability rule, so changing its issuer while enabled must succeed.
		Expect(mutateWithRetry(inst.Name, func(cur *computev1alpha1.FireboltInstance) {
			cur.Spec.TLS.Gateway.CertManager.IssuerRef.Name = "ca-b"
		})).To(Succeed())
	})
})
