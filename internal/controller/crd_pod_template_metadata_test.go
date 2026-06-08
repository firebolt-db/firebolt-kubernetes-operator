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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// These specs pin the structural-schema contract for the three CR fields
// that embed corev1.PodTemplateSpec — FireboltEngineClass.spec.template,
// FireboltInstance.spec.gateway.template, FireboltInstance.spec.metadata.template.
//
// controller-gen renders the embedded ObjectMeta in those templates as a
// bare `type: object` with no declared properties. Without
// x-kubernetes-preserve-unknown-fields on that metadata sub-schema, the
// apiserver strips every key under template.metadata (labels,
// annotations, ...) at write time. A CR applied with those fields then
// round-trips as `metadata: {}`, which is invisible at the Go-type
// layer but produces an infinite drift loop against any GitOps
// controller (Argo CD, Flux) that keeps trying to re-apply the lost
// fields.
//
// scripts/patch-crd-template-metadata.py injects the marker after
// controller-gen runs (see the `manifests` target). These tests use the
// envtest apiserver (real structural-schema enforcement, unlike the
// fake client) so a future regression — controller-gen description
// change that breaks the patch regex, or someone dropping the patch
// step — fails here instead of in production.
var _ = Describe("CRD pod-template metadata round-trip", func() {
	const ns = "default"

	var (
		testCtx    context.Context
		testCancel context.CancelFunc
	)

	BeforeEach(func() {
		testCtx, testCancel = context.WithTimeout(context.Background(), 30*time.Second)
	})

	AfterEach(func() {
		testCancel()
	})

	wantLabels := map[string]string{"app": "packdb"}
	wantAnnotations := map[string]string{"karpenter.sh/do-not-disrupt": "true"}

	// A container is included to mirror real usage. It is not required by
	// the schema — scripts/patch-crd-template-required.py drops the embedded
	// PodSpec `required: [containers]`, and crd_template_required_test.go
	// covers the container-less case explicitly.
	stubContainers := []corev1.Container{{
		Name:  "engine",
		Image: "example/engine:latest",
	}}

	expectMetaSurvives := func(meta metav1.ObjectMeta) {
		GinkgoHelper()
		Expect(meta.Labels).To(HaveKeyWithValue("app", "packdb"),
			"template.metadata.labels was pruned by the apiserver — check that scripts/patch-crd-template-metadata.py ran during `make manifests` and injected x-kubernetes-preserve-unknown-fields on the embedded ObjectMeta")
		Expect(meta.Annotations).To(HaveKeyWithValue("karpenter.sh/do-not-disrupt", "true"),
			"template.metadata.annotations was pruned by the apiserver — check that scripts/patch-crd-template-metadata.py ran during `make manifests` and injected x-kubernetes-preserve-unknown-fields on the embedded ObjectMeta")
	}

	It("preserves labels and annotations on FireboltEngineClass.spec.template.metadata", func() {
		name := "class-meta-" + utilrand.String(6)
		class := &computev1alpha1.FireboltEngineClass{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: computev1alpha1.FireboltEngineClassSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels:      wantLabels,
						Annotations: wantAnnotations,
					},
					Spec: corev1.PodSpec{
						ServiceAccountName: "my-sa",
						Containers:         stubContainers,
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, class)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), class)
		})

		got := &computev1alpha1.FireboltEngineClass{}
		Expect(k8sClient.Get(testCtx, client.ObjectKeyFromObject(class), got)).To(Succeed())
		expectMetaSurvives(got.Spec.Template.ObjectMeta)
	})

	It("preserves labels and annotations on FireboltInstance gateway and metadata template metadata", func() {
		name := "inst-meta-" + utilrand.String(6)
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Gateway: computev1alpha1.GatewaySpec{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      wantLabels,
							Annotations: wantAnnotations,
						},
						Spec: corev1.PodSpec{Containers: stubContainers},
					},
				},
				Metadata: computev1alpha1.MetadataSpec{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels:      wantLabels,
							Annotations: wantAnnotations,
						},
						Spec: corev1.PodSpec{Containers: stubContainers},
					},
				},
			},
		}
		Expect(k8sClient.Create(testCtx, inst)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), inst)
		})

		got := &computev1alpha1.FireboltInstance{}
		Expect(k8sClient.Get(testCtx, client.ObjectKeyFromObject(inst), got)).To(Succeed())
		Expect(got.Spec.Gateway.Template).NotTo(BeNil())
		expectMetaSurvives(got.Spec.Gateway.Template.ObjectMeta)
		Expect(got.Spec.Metadata.Template).NotTo(BeNil())
		expectMetaSurvives(got.Spec.Metadata.Template.ObjectMeta)
	})
})
