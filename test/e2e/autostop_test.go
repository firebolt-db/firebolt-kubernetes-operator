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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// First end-to-end coverage for autoStop. Everything below rides the same
// query-liveness scrape as the drain check (running+suspended gauges via
// ApiserverProxy), so this spec exercises the operator's idleness signal
// against a real engine: continuous load must pin the engine at
// activeReplicas past the idle timeout, quiet must scale it to
// idleReplicas=0 (phase stopped), and a stopped engine must stay stopped
// while nothing is asking for it. Wake itself is not covered here — see the
// comment on the final step for why.
const (
	// autoStopIdleTimeout / autoStopPollInterval are aggressive so the
	// idle scale-down lands within a test-friendly window. The busy-hold
	// window is deliberately > idleTimeout: if activity did not refresh
	// the idle clock, the engine would scale down mid-load and the hold
	// assertion would catch it.
	autoStopIdleTimeout  = 25 * time.Second
	autoStopPollInterval = 5 * time.Second
	autoStopBusyHold     = 40 * time.Second
	// autoStopScaleTimeout bounds quiet -> stopped (idle clock + poll
	// cadence + status/pod teardown) and wake -> stable.
	autoStopScaleTimeout = 180 * time.Second
)

var _ = Describe("Firebolt Engine AutoStop", func() {
	Describe("AutoStop Under Load", Ordered, func() {
		var (
			instanceName = "inst-autostop" + queryConfig.Suffix
			engineName   = "test-autostop" + queryConfig.Suffix + "-engine"
			clientPod    = "client-autostop" + queryConfig.Suffix
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
			By("Cleaning up autoStop test")
			defer TeardownTestInstance(ctx, lc)
			DeleteClientPod(ctx, clientPod)
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})

		It("should hold replicas under load, stop when idle, and wake on request", func() {
			By("Creating engine with autoStop enabled")
			idleReplicas := int32(0)
			Expect(CreateEngineWithAutoStop(ctx, instanceName, engineName, 1, &computev1alpha1.AutoStopSpec{
				Enabled:        true,
				ActiveReplicas: 1,
				IdleReplicas:   &idleReplicas,
				IdleTimeout:    &metav1.Duration{Duration: autoStopIdleTimeout},
				PollInterval:   &metav1.Duration{Duration: autoStopPollInterval},
			})).To(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())

			By("Resolving the engine pod, to sample its query gauges during the hold")
			_, activeGen, err := GetEngineGeneration(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())
			enginePods, err := EnginePodsForGeneration(ctx, engineName, activeGen)
			Expect(err).NotTo(HaveOccurred())
			Expect(enginePods).To(HaveLen(1))
			Expect(enginePods[0].Status.PodIP).NotTo(BeEmpty())

			By("Keeping the engine busy past the idle timeout")
			// The load loop runs inside the client pod (see keepURLBusy). It used
			// to be two goroutines each paying a kubectl-exec round trip per
			// query, which leaves holes where nothing is running on the engine —
			// and autoStop is entitled to scale down in one of them, so the hold
			// below failed on correct behaviour. Same defect as the drain spec had.
			stopLoad := keepURLBusy(ctx, clientPod, engineServiceQueryURL(engineName), computeBoundQuery)
			DeferCleanup(func() { stopLoad() })

			// The gauge is sampled alongside the assertion so a failure can say
			// whether the premise held. It does not weaken anything: the replica
			// check is unconditional, so any scale-down fails the spec whether or
			// not a sample caught the engine busy.
			var held loadHoldTracker
			Consistently(func(g Gomega) {
				held.sample(ctx, clientPod, enginePods[0].Status.PodIP)

				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(engine.Spec.Replicas).To(Equal(int32(1)),
					"%s (autoStopReason=%s)", held.failure("scaled a busy engine down"),
					engine.Status.AutoStopReason)
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseStable)))
			}, autoStopBusyHold, 2*time.Second).Should(Succeed())

			// A hold that never saw the engine busy proved nothing, and would
			// otherwise be indistinguishable from a real one.
			Expect(held.samples).NotTo(BeEmpty(),
				"no usable gauge sample was taken during the busy hold (%d scrape attempts "+
					"failed), so the engine was never observed busy", held.scrapeErrors)
			Expect(held.idle).To(BeZero(),
				"the engine went idle during the busy hold (%d of %d samples read 0); the hold "+
					"proved nothing even though it passed", held.idle, len(held.samples))

			succeeded, failed := stopLoad()
			GinkgoWriter.Printf("Busy-hold load: %d succeeded, %d failed\n", succeeded, failed)
			Expect(succeeded).To(BeNumerically(">", 0),
				"no query completed (%d failed); the busy-hold assertion proved nothing", failed)

			By("Waiting for the idle engine to scale down to zero")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(engine.Spec.Replicas).To(Equal(int32(0)),
					"autoStop did not scale the idle engine down (reason=%s, lastActivity=%v)",
					engine.Status.AutoStopReason, engine.Status.LastActivityTime)
			}, autoStopScaleTimeout, pollInterval).Should(Succeed())
			Expect(WaitForEnginePhase(ctx, engineName, computev1alpha1.PhaseStopped, clusterTransitionTimeout)).To(Succeed())

			// Wake-on-zero is NOT covered here, and deliberately not
			// faked. It needs the wake-agent sidecar running in the
			// gateway pod, which needs the operator image present in the
			// Kind registry — and scripts/load-e2e-images.sh only pulls
			// third-party images; the operator itself runs in-process in
			// this suite. Asserting a wake without the sidecar would
			// either test nothing or test a stub.
			//
			// The real path is covered instead by
			// scripts/ci/verify-wake-on-zero.sh, run from the test-helm
			// job: that installs the chart, so the sidecar runs from the
			// actual operator image inside the gateway pod, where Envoy's
			// loopback call can reach it.
			By("Verifying the stopped engine stays stopped without wake demand")
			Consistently(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(engine.Spec.Replicas).To(Equal(int32(0)),
					"a stopped engine scaled up with no wake demand (reason=%s)",
					engine.Status.AutoStopReason)
			}, 15*time.Second, 3*time.Second).Should(Succeed())

			By("Deleting engine")
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})
	})
})
