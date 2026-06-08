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

// These specs pin the structural-schema contract that an embedded
// corev1.PodTemplateSpec admits a template carrying pod-level fields but NO
// containers. controller-gen renders PodSpec's `containers` as required
// (`required: [containers]` under each template.spec), but our templates are
// fragments — the operator injects the engine / gateway / metadata container at
// StatefulSet / Deployment build time, so a CR legitimately sets only
// serviceAccountName / nodeSelector / pod labels (or an optional sidecar) with
// no container. Kubernetes 1.36 enforces the embedded required list on write,
// so without the fix the apiserver rejects such a CR with
// `spec.template.spec.containers: Required value`.
//
// scripts/patch-crd-template-required.py drops `containers` from those required
// lists after controller-gen (see the `manifests` target). These specs run
// against the envtest apiserver (real structural-schema enforcement, unlike the
// fake client) so dropping the patch step — or a controller-gen layout change
// that breaks its regex — fails here instead of in production.
var _ = Describe("CRD pod-template containers-optional contract", func() {
	const ns = "default"

	// bareTemplate is the shape the operator stores before injecting the
	// workload container: a pod-level field set, no containers.
	bareTemplate := func() *corev1.PodTemplateSpec {
		return &corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "my-sa"}}
	}

	tryCreate := func(obj client.Object) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := k8sClient.Create(ctx, obj)
		if err == nil {
			_ = k8sClient.Delete(context.Background(), obj)
		}
		return err
	}

	const hint = "apiserver rejected a container-less pod template — check scripts/patch-crd-template-required.py ran during `make manifests`"

	It("admits a FireboltEngine whose spec.template has no containers", func() {
		Expect(tryCreate(&computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-nocont-" + utilrand.String(6), Namespace: ns},
			Spec: computev1alpha1.FireboltEngineSpec{
				InstanceRef: "any-instance",
				Replicas:    1,
				Template:    bareTemplate(),
			},
		})).To(Succeed(), hint)
	})

	It("admits a FireboltEngineClass whose spec.template has no containers", func() {
		Expect(tryCreate(&computev1alpha1.FireboltEngineClass{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-nocont-" + utilrand.String(6), Namespace: ns},
			Spec: computev1alpha1.FireboltEngineClassSpec{
				Template: *bareTemplate(),
			},
		})).To(Succeed(), hint)
	})

	It("admits a FireboltInstance whose gateway and metadata templates have no containers", func() {
		Expect(tryCreate(&computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "tmpl-nocont-" + utilrand.String(6), Namespace: ns},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Gateway:  computev1alpha1.GatewaySpec{Template: bareTemplate()},
				Metadata: computev1alpha1.MetadataSpec{Template: bareTemplate()},
			},
		})).To(Succeed(), hint)
	})
})
