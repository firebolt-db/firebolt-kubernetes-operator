//go:build e2e
// +build e2e

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

package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-analytics/core-operator/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validEngineSpec() computev1alpha1.FireboltEngineSpec {
	return computev1alpha1.FireboltEngineSpec{
		Replicas: 1,
		Image: computev1alpha1.ImageSpec{
			Repository: testImage,
			Tag:        testTag,
			PullPolicy: corev1.PullIfNotPresent,
		},
		Resources: computev1alpha1.ResourceRequirements{
			CPU:    resource.MustParse("100m"),
			Memory: resource.MustParse("1Gi"),
		},
		Rollout: computev1alpha1.RolloutGraceful,
	}
}

var _ = Describe("CRD Validation", func() {
	var cl client.Client

	BeforeEach(func() {
		var err error
		cl, err = getCRDClient()
		Expect(err).NotTo(HaveOccurred())
	})

	createEngine := func(name string, spec computev1alpha1.FireboltEngineSpec) error {
		engine := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: testNamespace,
			},
			Spec: spec,
		}
		return cl.Create(ctx, engine)
	}

	It("should accept a valid engine spec", func() {
		name := "valid-engine"
		err := createEngine(name, validEngineSpec())
		Expect(err).NotTo(HaveOccurred())

		defer func() {
			_ = cl.Delete(ctx, &computev1alpha1.FireboltEngine{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			})
		}()
	})

	DescribeTable("should reject invalid specs",
		func(name string, mutate func(*computev1alpha1.FireboltEngineSpec), expectedSubstring string) {
			spec := validEngineSpec()
			mutate(&spec)
			err := createEngine(fmt.Sprintf("invalid-%s", name), spec)
			Expect(err).To(HaveOccurred(), "Expected creation to be rejected")
			Expect(err.Error()).To(ContainSubstring(expectedSubstring),
				"Error should mention: %s, got: %v", expectedSubstring, err)
		},
		Entry("zero replicas", "zero-replicas",
			func(s *computev1alpha1.FireboltEngineSpec) { s.Replicas = 0 },
			"spec.replicas"),
		Entry("negative replicas", "neg-replicas",
			func(s *computev1alpha1.FireboltEngineSpec) { s.Replicas = -1 },
			"spec.replicas"),
		Entry("replicas over maximum", "over-max-replicas",
			func(s *computev1alpha1.FireboltEngineSpec) { s.Replicas = 101 },
			"spec.replicas"),
		Entry("empty image repository", "empty-repo",
			func(s *computev1alpha1.FireboltEngineSpec) { s.Image.Repository = "" },
			"spec.image.repository"),
		Entry("empty image tag", "empty-tag",
			func(s *computev1alpha1.FireboltEngineSpec) { s.Image.Tag = "" },
			"spec.image.tag"),
		Entry("invalid rollout strategy", "bad-rollout",
			func(s *computev1alpha1.FireboltEngineSpec) { s.Rollout = "bluegreen" },
			"spec.rollout"),
		Entry("invalid pull policy", "bad-pullpolicy",
			func(s *computev1alpha1.FireboltEngineSpec) { s.Image.PullPolicy = "Sometimes" },
			"spec.image.pullPolicy"),
	)
})
