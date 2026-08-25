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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// The one-FireboltEnginePreset-per-namespace invariant is enforced by a
// CRD CEL rule pinning metadata.name to "firebolt": name uniqueness then
// caps a namespace at one object. The rule is baked into the CRD and runs
// inside the apiserver, so it holds even when the validating webhook is
// disabled (the shipped Helm default). This suite runs against envtest
// with the CRD bases applied and NO webhook installed (see suite_test.go),
// so every rejection below is the API server alone.
var _ = Describe("FireboltEnginePreset fixed-name singleton (CEL, webhook-free)", func() {
	const ns = "default"
	ctx := context.Background()

	mkPreset := func(name string) *computev1alpha1.FireboltEnginePreset {
		return &computev1alpha1.FireboltEnginePreset{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: computev1alpha1.FireboltEnginePresetSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{ServiceAccountName: "engine-sa"},
				},
			},
		}
	}

	It("rejects a FireboltEnginePreset named anything but 'firebolt'", func() {
		err := k8sClient.Create(ctx, mkPreset("shadow-preset"))
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(),
			"expected a schema/CEL Invalid rejection, got: %v", err)
		Expect(err.Error()).To(ContainSubstring("must be named 'firebolt'"))
	})

	It("accepts the 'firebolt' object and refuses a second by name uniqueness", func() {
		preset := mkPreset(computev1alpha1.FireboltEnginePresetDefaultName)
		Expect(k8sClient.Create(ctx, preset)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), preset) }()

		dup := mkPreset(computev1alpha1.FireboltEnginePresetDefaultName)
		err := k8sClient.Create(ctx, dup)
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(),
			"expected AlreadyExists for a duplicate 'firebolt' object, got: %v", err)
	})
})
