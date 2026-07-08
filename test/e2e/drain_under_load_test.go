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
	"sync"
	"sync/atomic"
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

// keepPodBusy loops the compute-bound query back-to-back against one pod IP
// until stop is closed. Two workers so the busy signal has no gaps between
// consecutive queries (kubectl-exec startup leaves ~0.5s holes with one).
func keepPodBusy(ctx context.Context, clientPod, podIP string, stop <-chan struct{}) (wait func() (succeeded, failed int64)) {
	var wg sync.WaitGroup
	var succeeded, failed atomic.Int64
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := RunQueryAgainstPodIP(ctx, clientPod, podIP, computeBoundQuery); err != nil {
					failed.Add(1)
				} else {
					succeeded.Add(1)
				}
			}
		}()
	}
	return func() (int64, int64) {
		wg.Wait()
		return succeeded.Load(), failed.Load()
	}
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
			stop := make(chan struct{})
			waitForLoad := keepPodBusy(ctx, clientPod, oldPod.Status.PodIP, stop)

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
			Consistently(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseDraining)),
					"draining generation was released while its pod had queries in flight")
				g.Expect(engine.Status.DrainingGeneration).NotTo(BeNil())

				pod, err := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, oldPod.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "old-generation pod disappeared mid-drain")
				g.Expect(pod.DeletionTimestamp).To(BeNil(), "old-generation pod is terminating while busy")
			}, drainHoldWindow, 1*time.Second).Should(Succeed())

			By("Stopping the query load")
			close(stop)
			succeeded, failed := waitForLoad()
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
