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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// The minimum auto-stop poll interval and drain-check interval are CRD CEL
// rules, so the API server refuses a shorter value with the validating webhook
// disabled (the shipped Helm default). This suite runs against envtest with
// the CRD bases applied and no webhook installed (see suite_test.go), so every
// rejection below is the API server alone.
var _ = Describe("Engine poll and drain-check interval minimums (CEL, webhook-free)", func() {
	const ns = "default"
	ctx := context.Background()

	tooShort := &metav1.Duration{Duration: time.Nanosecond}
	atMinimum := func(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }

	mkEngine := func(name string, mutate func(*computev1alpha1.FireboltEngineSpec)) *computev1alpha1.FireboltEngine {
		e := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "interval-cel-inst", Replicas: 1},
		}
		mutate(&e.Spec)
		return e
	}
	mkClass := func(name string, mutate func(*computev1alpha1.FireboltEngineClassSpec)) *computev1alpha1.FireboltEngineClass {
		c := &computev1alpha1.FireboltEngineClass{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: computev1alpha1.FireboltEngineClassSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "engine-sa"}},
			},
		}
		mutate(&c.Spec)
		return c
	}
	expectInvalid := func(err error, msg string) {
		GinkgoHelper()
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a schema/CEL Invalid rejection, got: %v", err)
		Expect(err.Error()).To(ContainSubstring(msg))
	}

	It("rejects an Engine auto-stop poll interval below the minimum", func() {
		err := k8sClient.Create(ctx, mkEngine("interval-cel-poll", func(s *computev1alpha1.FireboltEngineSpec) {
			s.AutoStop = &computev1alpha1.AutoStopSpec{Enabled: true, ActiveReplicas: 1, PollInterval: tooShort}
		}))
		expectInvalid(err, "pollInterval must be at least 1s")
	})

	It("rejects an Engine drain-check interval below the minimum", func() {
		err := k8sClient.Create(ctx, mkEngine("interval-cel-drain", func(s *computev1alpha1.FireboltEngineSpec) {
			s.DrainCheckInterval = tooShort
		}))
		expectInvalid(err, "drainCheckInterval must be at least 1s")
	})

	It("rejects the same intervals on a FireboltEngineClass", func() {
		err := k8sClient.Create(ctx, mkClass("interval-cel-class-poll", func(s *computev1alpha1.FireboltEngineClassSpec) {
			s.AutoStop = &computev1alpha1.AutoStopSpec{Enabled: true, ActiveReplicas: 1, PollInterval: tooShort}
		}))
		expectInvalid(err, "pollInterval must be at least 1s")

		err = k8sClient.Create(ctx, mkClass("interval-cel-class-drain", func(s *computev1alpha1.FireboltEngineClassSpec) {
			s.DrainCheckInterval = tooShort
		}))
		expectInvalid(err, "drainCheckInterval must be at least 1s")
	})

	It("accepts both intervals at the minimum", func() {
		engine := mkEngine("interval-cel-ok", func(s *computev1alpha1.FireboltEngineSpec) {
			s.AutoStop = &computev1alpha1.AutoStopSpec{
				Enabled:        true,
				ActiveReplicas: 1,
				PollInterval:   atMinimum(computev1alpha1.MinAutoStopPollInterval),
			}
			s.DrainCheckInterval = atMinimum(computev1alpha1.MinDrainCheckInterval)
		})
		Expect(k8sClient.Create(ctx, engine)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), engine) })

		class := mkClass("interval-cel-class-ok", func(s *computev1alpha1.FireboltEngineClassSpec) {
			s.DrainCheckInterval = atMinimum(computev1alpha1.MinDrainCheckInterval)
		})
		Expect(k8sClient.Create(ctx, class)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), class) })
	})
})
