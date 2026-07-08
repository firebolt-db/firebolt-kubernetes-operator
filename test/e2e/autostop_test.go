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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
)

// First end-to-end coverage for autoStop. Everything below rides the same
// query-liveness scrape as the drain check (running+suspended gauges via
// ApiserverProxy), so this spec exercises the operator's idleness signal
// against a real engine: continuous load must pin the engine at
// activeReplicas past the idle timeout, quiet must scale it to
// idleReplicas=0 (phase stopped), and a fresh gateway wake-up annotation
// must bring it back.
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

			By("Keeping the engine busy past the idle timeout")
			// Two workers so consecutive queries overlap and every autoStop
			// poll observes in-flight work.
			stop := make(chan struct{})
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
						if _, err := RunQuery(ctx, clientPod, engineName, computeBoundQuery); err != nil {
							failed.Add(1)
						} else {
							succeeded.Add(1)
						}
					}
				}()
			}

			Consistently(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(engine.Spec.Replicas).To(Equal(int32(1)),
					"autoStop scaled a busy engine down (reason=%s)", engine.Status.AutoStopReason)
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseStable)))
			}, autoStopBusyHold, 2*time.Second).Should(Succeed())

			close(stop)
			wg.Wait()
			GinkgoWriter.Printf("Busy-hold load: %d succeeded, %d failed\n", succeeded.Load(), failed.Load())
			Expect(succeeded.Load()).To(BeNumerically(">", 0),
				"no query completed (%d failed); the busy-hold assertion proved nothing", failed.Load())

			By("Waiting for the idle engine to scale down to zero")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(engine.Spec.Replicas).To(Equal(int32(0)),
					"autoStop did not scale the idle engine down (reason=%s, lastActivity=%v)",
					engine.Status.AutoStopReason, engine.Status.LastActivityTime)
			}, autoStopScaleTimeout, pollInterval).Should(Succeed())
			Expect(WaitForEnginePhase(ctx, engineName, computev1alpha1.PhaseStopped, clusterTransitionTimeout)).To(Succeed())

			By("Stamping a wake-up request the way the gateway does")
			Expect(AnnotateEngine(ctx, engineName, controller.AnnotationWakeRequested,
				time.Now().UTC().Format(time.RFC3339))).To(Succeed())

			By("Waiting for the engine to wake back up to activeReplicas")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(engine.Spec.Replicas).To(Equal(int32(1)),
					"wake request was not honored (reason=%s)", engine.Status.AutoStopReason)
			}, autoStopScaleTimeout, pollInterval).Should(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())

			By("Verifying the woken engine serves queries")
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
