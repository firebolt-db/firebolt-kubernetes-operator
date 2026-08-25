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

// Serial because FireboltEnginePreset is an ambient overlay merged under
// every engine in the namespace: each create/edit/delete here changes the
// effective pod template of all co-scheduled specs' engines and rolls them a
// new blue-green generation, breaking any spec that asserts phase stability
// or a stable pod IP. Serial also lets the AfterAll deletion complete instead
// of hanging on the deletion-guard finalizer while other engines exist.
var _ = Describe("FireboltEnginePreset merge", Ordered, Serial, func() {
	var (
		instanceName = "inst-defaults" + queryConfig.Suffix
		engineName   = "test-defaults" + queryConfig.Suffix + "-engine"
		presetName = computev1alpha1.FireboltEnginePresetDefaultName
		// editedServiceAccount is created by the spec before the Preset
		// edit references it; pods of the rolled generation run under it.
		editedServiceAccount = "preset-sa" + queryConfig.Suffix
		lc                   *TestInstanceLifecycle
	)
	RegisterFailedSpecPodLogDump(&instanceName, &engineName)

	BeforeAll(func() {
		By("Setting up FireboltInstance for Preset merge")
		var err error
		lc, err = SetupTestInstance(ctx, instanceName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("Cleaning up Preset merge test")
		defer TeardownTestInstance(ctx, lc)
		Expect(DeleteEngine(ctx, engineName)).To(Succeed())
		Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		_ = DeleteFireboltEnginePreset(ctx, presetName)
		_ = DeleteServiceAccount(ctx, editedServiceAccount)
	})

	It("fails closed until Preset exists, then rolls on a Preset edit", func() {
		By("Creating an engine that requires FireboltEnginePreset")
		Expect(CreateEngineWithRequirePreset(ctx, instanceName, engineName, 1)).To(Succeed())

		By("Waiting for Ready=False/FireboltEnginePresetRequired")
		Expect(WaitForEngineReadyCondition(ctx, engineName, metav1.ConditionFalse, "FireboltEnginePresetRequired", generationSweepTimeout)).To(Succeed())

		By("Creating FireboltEnginePreset")
		Expect(CreateFireboltEnginePreset(ctx, presetName, "default")).To(Succeed())

		By("Waiting for the engine to become Ready")
		Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
		Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())
		genBefore, _, err := GetEngineGeneration(ctx, engineName)
		Expect(err).NotTo(HaveOccurred())

		// The ServiceAccount must exist before the Preset edit points
		// pods at it, or the new generation could never be admitted and
		// the blue-green roll would wedge.
		By("Creating the edited ServiceAccount")
		Expect(CreateServiceAccount(ctx, editedServiceAccount)).To(Succeed())

		By("Editing FireboltEnginePreset service account")
		Expect(UpdateFireboltEnginePresetServiceAccount(ctx, presetName, editedServiceAccount)).To(Succeed())

		By("Waiting for the engine to leave PhaseStable")
		Expect(WaitForEnginePhaseChange(ctx, engineName, computev1alpha1.PhaseStable, clusterTransitionTimeout)).To(Succeed())

		By("Waiting for the roll to complete on a new generation")
		Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
		Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())
		genAfter, _, err := GetEngineGeneration(ctx, engineName)
		Expect(err).NotTo(HaveOccurred())
		Expect(genAfter).To(BeNumerically(">", genBefore),
			"a Preset spec edit must roll a new blue-green generation")

		By("Verifying the new generation carries the merged service account")
		sts, err := GetEngineGenerationStatefulSet(ctx, engineName, genAfter)
		Expect(err).NotTo(HaveOccurred())
		Expect(sts.Spec.Template.Spec.ServiceAccountName).To(Equal(editedServiceAccount))
	})
})
