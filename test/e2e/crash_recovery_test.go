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
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/firebolt-analytics/core-operator/internal/controller"
)

var _ = Describe("Crash Recovery", Ordered, func() {
	AfterAll(func() {
		controller.ClearAllCrashPoints()
	})

	Describe("Phase: Creating - Initial Deployment", Ordered, func() {
		var (
			clusterPrefix = "test-crash-create" + queryConfig.Suffix
			clusterName   = clusterPrefix + "-cluster"
			operator      *OperatorInstance
		)

		AfterEach(func() {
			controller.ClearAllCrashPoints()
			if operator != nil {
				operator.Stop()
				operator = nil
			}
			_ = DeleteClusterConfig(ctx, clusterName)
			_ = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
		})

		It("should recover from crash after ConfigMap created", func() {
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashAfterCoreConfigMapCreated, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after core ConfigMap created\n")
			})

			By("Starting operator")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Creating cluster config")
			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterReadyTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Releasing blocked reconcile")
			close(restartCh)

			By("Stopping operator (simulating crash)")
			operator.Stop()
			operator = nil

			By("Restarting operator")
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - cluster becomes ready")
			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})

		It("should recover from crash after StatefulSet created", func() {
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashAfterStatefulSetCreated, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after StatefulSet created\n")
			})

			By("Starting operator")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Creating cluster config")
			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterReadyTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Releasing blocked reconcile")
			close(restartCh)

			By("Stopping operator (simulating crash)")
			operator.Stop()
			operator = nil

			By("Restarting operator")
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - cluster becomes ready")
			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})

		It("should recover from crash before status update to switching", func() {
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashBeforeCreatingToSwitching, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: before creating-to-switching status update\n")
			})

			By("Starting operator")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Creating cluster config")
			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterReadyTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Releasing blocked reconcile")
			close(restartCh)

			By("Stopping operator (simulating crash)")
			operator.Stop()
			operator = nil

			By("Restarting operator")
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - cluster becomes ready")
			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})
	})

	Describe("Phase: Switching - Scale Transition", Ordered, func() {
		var (
			clusterPrefix = "test-crash-switch" + queryConfig.Suffix
			clusterName   = clusterPrefix + "-cluster"
			operator      *OperatorInstance
		)

		AfterEach(func() {
			controller.ClearAllCrashPoints()
			if operator != nil {
				operator.Stop()
				operator = nil
			}
			_ = DeleteClusterConfig(ctx, clusterName)
			_ = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
		})

		It("should recover from crash after service selector update (CRITICAL)", func() {
			By("Creating initial cluster")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the scale transition")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashAfterServiceSelectorUpdate, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after service selector update (traffic already switched!)\n")
			})

			By("Triggering scale up")
			err = UpdateClusterReplicas(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterTransitionTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Releasing blocked reconcile")
			close(restartCh)

			By("Stopping operator (simulating crash)")
			operator.Stop()
			operator = nil

			By("Restarting operator")
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - cluster becomes ready with new size")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})

		It("should recover from crash before switching status update", func() {
			By("Creating initial cluster")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the scale transition")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashBeforeSwitchingStatusUpdate, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: before switching status update\n")
			})

			By("Triggering scale up")
			err = UpdateClusterReplicas(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterTransitionTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Releasing blocked reconcile")
			close(restartCh)

			By("Stopping operator (simulating crash)")
			operator.Stop()
			operator = nil

			By("Restarting operator")
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - cluster becomes ready with new size")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})
	})

	Describe("Phase: Cleaning - After Drain", Ordered, func() {
		var (
			clusterPrefix = "test-crash-clean" + queryConfig.Suffix
			clusterName   = clusterPrefix + "-cluster"
			operator      *OperatorInstance
		)

		AfterEach(func() {
			controller.ClearAllCrashPoints()
			if operator != nil {
				operator.Stop()
				operator = nil
			}
			_ = DeleteClusterConfig(ctx, clusterName)
			_ = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
		})

		It("should recover from crash after StatefulSet deleted", func() {
			By("Creating initial cluster")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the cleaning phase")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashAfterStatefulSetDeleted, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after StatefulSet deleted\n")
			})

			By("Triggering scale up")
			err = UpdateClusterReplicas(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterTransitionTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Releasing blocked reconcile")
			close(restartCh)

			By("Stopping operator (simulating crash)")
			operator.Stop()
			operator = nil

			By("Restarting operator")
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - cluster becomes stable")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})

		It("should recover from crash before status update to stable", func() {
			By("Creating initial cluster")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the final phase")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashBeforeCleaningToStable, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: before cleaning-to-stable status update\n")
			})

			By("Triggering scale up")
			err = UpdateClusterReplicas(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterTransitionTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Releasing blocked reconcile")
			close(restartCh)

			By("Stopping operator (simulating crash)")
			operator.Stop()
			operator = nil

			By("Restarting operator")
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - cluster becomes stable")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})
	})

	Describe("Availability During Crash", Ordered, func() {
		var (
			clusterPrefix = "test-crash-avail" + queryConfig.Suffix
			clusterName   = clusterPrefix + "-cluster"
			operator      *OperatorInstance
		)

		AfterEach(func() {
			controller.ClearAllCrashPoints()
			if operator != nil {
				operator.Stop()
				operator = nil
			}
			_ = DeleteClusterConfig(ctx, clusterName)
			_ = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
		})

		It("should maintain query availability when crash occurs after service selector update", func() {
			By("Creating initial cluster")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			err = CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Starting background queries")
			bgRunner := NewBackgroundQueryRunnerWithValidator(clusterName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			// Let some successful queries accumulate
			time.Sleep(3 * time.Second)

			By("Setting crash point for the scale transition")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(clusterName, controller.CrashAfterServiceSelectorUpdate, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit during availability test\n")
			})

			By("Triggering scale up")
			err = UpdateClusterReplicas(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterTransitionTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Stopping operator (simulating crash) - queries should continue to new generation")
			operator.Stop()
			operator = nil

			// Queries should continue working because the service selector was already updated
			time.Sleep(3 * time.Second)

			By("Releasing blocked reconcile and restarting")
			close(restartCh)
			time.Sleep(time.Second)

			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for recovery")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Stopping background queries and checking results")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Background queries: %d successes, %d failures\n", successes, failures)

			if failures > 0 {
				bgRunner.PrintFailureSummary()
			}

			Expect(failures).To(Equal(int32(0)), "Background queries should not fail - service selector was updated before crash")
			Expect(successes).To(BeNumerically(">", 0), "Should have successful queries")
		})
	})
})
