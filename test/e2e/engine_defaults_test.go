//go:build e2e
// +build e2e

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

package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("FireboltEngineDefaults merge", Ordered, func() {
	var (
		instanceName = "inst-defaults" + queryConfig.Suffix
		engineName   = "test-defaults" + queryConfig.Suffix + "-engine"
		defaultsName = computev1alpha1.FireboltEngineDefaultsDefaultName
		lc           *TestInstanceLifecycle
	)
	RegisterFailedSpecPodLogDump(&instanceName, &engineName)

	BeforeAll(func() {
		By("Setting up FireboltInstance for Defaults merge")
		var err error
		lc, err = SetupTestInstance(ctx, instanceName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("Cleaning up Defaults merge test")
		defer TeardownTestInstance(ctx, lc)
		Expect(DeleteEngine(ctx, engineName)).To(Succeed())
		Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		_ = DeleteFireboltEngineDefaults(ctx, defaultsName)
	})

	It("fails closed until Defaults exists, then rolls on a Defaults edit", func() {
		By("Creating an engine that requires FireboltEngineDefaults")
		Expect(CreateEngineWithRequireDefaults(ctx, instanceName, engineName, 1)).To(Succeed())

		By("Waiting for Ready=False/FireboltEngineDefaultsRequired")
		Expect(WaitForEngineReadyCondition(ctx, engineName, metav1.ConditionFalse, "FireboltEngineDefaultsRequired", generationSweepTimeout)).To(Succeed())

		By("Creating FireboltEngineDefaults")
		Expect(CreateFireboltEngineDefaults(ctx, defaultsName, "default")).To(Succeed())

		By("Waiting for the engine to become Ready")
		Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
		Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())

		By("Editing FireboltEngineDefaults service account")
		Expect(UpdateFireboltEngineDefaultsServiceAccount(ctx, defaultsName, "defaults-sa")).To(Succeed())

		By("Waiting for the engine to leave PhaseStable")
		Expect(WaitForEnginePhaseChange(ctx, engineName, computev1alpha1.PhaseStable, clusterTransitionTimeout)).To(Succeed())
	})
})
