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
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// This spec is the consumer-side half of the query-liveness contract pinned
// by engine_metrics_test.go: with drainCheckEnabled=true, a blue-green
// rollout must HOLD the old generation in draining while queries are in
// flight on its pods, and release it promptly once they finish. Every other
// spec in the suite runs with the drain check off, so before this spec the
// busy-pod-blocks-cleanup behavior had no end-to-end coverage at all — a
// drain signal that always read "drained" (as engine builds before
// 2026-07-07 produced) passed the whole suite.
//
// The instance uses metricScrapeMode=ApiserverProxy because the in-process
// operator scrapes from the host, where kind pod IPs are unreachable.
const (
	// drainHoldWindow is how long the spec requires the draining phase to
	// persist while old-generation pods are busy. With drainCheckInterval=2s
	// this spans several consecutive drain probes, so a single spurious
	// "drained" reading cannot pass.
	drainHoldWindow = 15 * time.Second
	// drainReleaseTimeout bounds draining -> cleaning -> stable after the
	// load stops (the last in-flight query may still need to finish).
	drainReleaseTimeout = 120 * time.Second
	// rolloutToDrainingTimeout bounds creating -> switching -> draining for
	// the new generation (pod schedule + engine boot + readiness).
	rolloutToDrainingTimeout = 300 * time.Second

	// drainRolloutTolerationKey is the no-op toleration the spec adds to
	// the pod template purely to trigger a blue-green rollout. No node
	// carries a matching taint, so scheduling is unaffected.
	drainRolloutTolerationKey = "firebolt.io/e2e-drain-under-load"
)

// keepPodBusy keeps queries in flight against ONE pod, bypassing the cluster
// Service. That targeting is the point: the drain spec has to keep loading the
// OLD generation after the selector has flipped away from it, which is exactly
// the traffic the drain check exists to protect.
func keepPodBusy(ctx context.Context, clientPod, podIP string) (stop func() (succeeded, failed int64)) {
	return keepURLBusy(ctx, clientPod, enginePodQueryURL(podIP), computeBoundQuery)
}

var _ = Describe("Firebolt Engine Drain", func() {
	Describe("Drain Under Load", Ordered, func() {
		var (
			instanceName = "inst-drain" + queryConfig.Suffix
			engineName   = "test-drain" + queryConfig.Suffix + "-engine"
			clientPod    = "client-drain" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance with ApiserverProxy metric scraping")
			var err error
			lc, err = SetupTestInstanceWithScrapeMode(ctx, instanceName, computev1alpha1.MetricScrapeModeApiserverProxy)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up drain-under-load test")
			defer TeardownTestInstance(ctx, lc)
			DeleteClientPod(ctx, clientPod)
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})

		It("should hold the draining generation while queries are in flight and release it after", func() {
			By("Creating engine with the drain check enabled")
			Expect(CreateEngineWithDrainCheck(ctx, instanceName, engineName, 1)).To(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())

			By("Resolving the active-generation pod")
			_, activeGen, err := GetEngineGeneration(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())
			oldPods, err := EnginePodsForGeneration(ctx, engineName, activeGen)
			Expect(err).NotTo(HaveOccurred())
			Expect(oldPods).To(HaveLen(1))
			oldPod := oldPods[0]
			Expect(oldPod.Status.PodIP).NotTo(BeEmpty())

			By("Starting continuous queries against the old-generation pod")
			stopLoad := keepPodBusy(ctx, clientPod, oldPod.Status.PodIP)
			// A failing assertion below must still stop the in-pod loop (a loop
			// left running would keep the pod busy into the next spec) and must
			// still record the load evidence, which otherwise only the success
			// path printed. stopLoad is idempotent, so the explicit call on the
			// success path keeps its ordering and counts.
			DeferCleanup(func() {
				succeeded, failed := stopLoad()
				GinkgoWriter.Printf("Old-generation load (cleanup): %d succeeded, %d failed\n",
					succeeded, failed)
			})

			// The hold below is far enough from here that ramp-up cannot pollute
			// it — the rollout wait sits in between. Waiting anyway pins the
			// premise before the rollout is triggered, so a load loop that never
			// produced traffic fails here, with that as the reason, instead of
			// surfacing later as a drain that released "too early".
			By("Waiting for the load to actually reach the old-generation pod")
			waitForLoadInFlight(ctx, clientPod, oldPod.Status.PodIP)

			By("Triggering a blue-green rollout via a no-op toleration")
			Expect(UpdateEngineScheduling(ctx, engineName, nil, []corev1.Toleration{{
				Key:      drainRolloutTolerationKey,
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			}}, nil)).To(Succeed())

			By("Waiting for the rollout to reach draining on the old generation")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseDraining)))
				g.Expect(engine.Status.DrainingGeneration).NotTo(BeNil())
				g.Expect(*engine.Status.DrainingGeneration).To(Equal(activeGen))
			}, rolloutToDrainingTimeout, pollInterval).Should(Succeed())

			By("Verifying the busy old generation is held in draining")
			// The gauge is sampled alongside the phase so a failure can say
			// whether the premise held. Recording it does not weaken the
			// assertion: the phase check below is unconditional, so any release
			// fails the spec whether or not a sample caught the pod busy.
			var held loadHoldTracker
			Consistently(func(g Gomega) {
				held.sample(ctx, clientPod, oldPod.Status.PodIP)

				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseDraining)),
					held.failure("released the draining generation"))
				g.Expect(engine.Status.DrainingGeneration).NotTo(BeNil())

				pod, err := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, oldPod.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "old-generation pod disappeared mid-drain")
				g.Expect(pod.DeletionTimestamp).To(BeNil(), "old-generation pod is terminating while busy")
			}, drainHoldWindow, 1*time.Second).Should(Succeed())

			// Belt-and-braces on the premise for a run that PASSED: a hold that
			// never saw the pod busy proved nothing, and would otherwise be
			// indistinguishable from a real one.
			Expect(held.samples).NotTo(BeEmpty(),
				"no usable gauge sample was taken during the hold window (%d scrape attempts "+
					"failed), so the pod was never observed busy", held.scrapeErrors)
			Expect(held.idle).To(BeZero(),
				"the pod went idle during the hold window (%d of %d samples read 0); the hold "+
					"proved nothing even though it passed", held.idle, len(held.samples))

			By("Stopping the query load")
			succeeded, failed := stopLoad()
			GinkgoWriter.Printf("Old-generation load: %d succeeded, %d failed\n", succeeded, failed)
			Expect(succeeded).To(BeNumerically(">", 0),
				"no query completed against the old pod (%d failed); the hold assertion proved nothing", failed)

			By("Waiting for the drained generation to be released and cleaned")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseStable)))
				g.Expect(engine.Status.DrainingGeneration).To(BeNil())

				pods, err := EnginePodsForGeneration(ctx, engineName, activeGen)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods).To(BeEmpty(), "old-generation pods survived drain completion")
			}, drainReleaseTimeout, pollInterval).Should(Succeed())

			By("Verifying the new generation serves queries")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Deleting engine")
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})
	})
})
