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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilptr "k8s.io/utils/ptr"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("FireboltEngine autoscaling admission validation", func() {
	const ns = "default"

	mkEngine := func(name string, as *computev1alpha1.AutoscalingSpec) *computev1alpha1.FireboltEngine {
		return &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: computev1alpha1.FireboltEngineSpec{
				InstanceRef: "any-instance",
				Replicas:    1,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
				Autoscaling: as,
			},
		}
	}

	tryCreate := func(eng *computev1alpha1.FireboltEngine) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := k8sClient.Create(ctx, eng)
		if err == nil {
			// Don't leak resources between specs; CEL doesn't gate deletes.
			_ = k8sClient.Delete(context.Background(), eng)
		}
		return err
	}

	It("rejects minReplicas > maxReplicas", func() {
		err := tryCreate(mkEngine("autoscale-bad", &computev1alpha1.AutoscalingSpec{
			Enabled:     true,
			MaxReplicas: 2,
			MinReplicas: utilptr.To(int32(5)),
		}))
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("maxreplicas must be >= minreplicas"))
	})

	It("accepts minReplicas == maxReplicas", func() {
		Expect(tryCreate(mkEngine("autoscale-eq", &computev1alpha1.AutoscalingSpec{
			Enabled:     true,
			MaxReplicas: 3,
			MinReplicas: utilptr.To(int32(3)),
		}))).To(Succeed())
	})

	It("accepts minReplicas < maxReplicas", func() {
		Expect(tryCreate(mkEngine("autoscale-ok", &computev1alpha1.AutoscalingSpec{
			Enabled:     true,
			MaxReplicas: 5,
			MinReplicas: utilptr.To(int32(2)),
		}))).To(Succeed())
	})

	It("accepts minReplicas omitted (defaults to 0)", func() {
		Expect(tryCreate(mkEngine("autoscale-default", &computev1alpha1.AutoscalingSpec{
			Enabled:     true,
			MaxReplicas: 3,
		}))).To(Succeed())
	})
})
