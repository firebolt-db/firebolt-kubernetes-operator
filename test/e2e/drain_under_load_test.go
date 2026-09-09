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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
)

// The rollout must retain an old generation while a query accepted through the
// gateway is still running. Direct Pod access is used only to observe metrics.
// ApiserverProxy scraping lets the in-process operator observe Kind Pods.
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

			By("Staging the replacement before the routing cutover")
			staged := make(chan struct{})
			resume := controller.SetCrashPoint(engineName, controller.CrashBeforeCreatingToSwitching, func() {
				close(staged)
			})
			var resumeOnce sync.Once
			release := func() { resumeOnce.Do(func() { close(resume) }) }
			DeferCleanup(release)
			DeferCleanup(func() { controller.ClearCrashPointsForEngine(engineName) })

			By("Triggering a blue-green rollout via a no-op toleration")
			Expect(UpdateEngineScheduling(ctx, engineName, nil, []corev1.Toleration{{
				Key:      drainRolloutTolerationKey,
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			}}, nil)).To(Succeed())

			Eventually(staged, rolloutToDrainingTimeout).Should(BeClosed(),
				"replacement must be ready before starting the held query")

			By("Admitting one long query through the gateway to the old generation")
			type queryResult struct {
				body string
				err  error
			}
			finished := make(chan queryResult, 1)
			go func() {
				body, queryErr := execCurlQueryWithDeadline(ctx, clientPod,
					gatewayLoadQueryURL(instanceName, engineName)+"&enable_internal_functions=true",
					"SELECT sleep(45)", 65)
				finished <- queryResult{body: body, err: queryErr}
			}()
			waitForLoadInFlight(ctx, clientPod, oldPod.Status.PodIP)
			Consistently(finished, time.Second).ShouldNot(Receive(),
				"the held query must remain in flight before cutover")
			release()

			By("Waiting for the rollout to reach draining on the old generation")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseDraining)))
				g.Expect(engine.Status.DrainingGeneration).NotTo(BeNil())
				g.Expect(*engine.Status.DrainingGeneration).To(Equal(activeGen))
			}, 20*time.Second, time.Second).Should(Succeed())

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

			By("Serving fresh gateway queries while the old query finishes")
			_, err = RunQueryViaGateway(ctx, clientPod, instanceName, engineName, "SELECT 1")
			Expect(err).NotTo(HaveOccurred())
			var heldResult queryResult
			Eventually(finished, 65*time.Second).Should(Receive(&heldResult))
			Expect(heldResult.err).NotTo(HaveOccurred(), "the query admitted before cutover must complete")
			ExpectSleepResult(heldResult.body)

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
