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
	"fmt"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
)

// Each second-level Describe owns its own FireboltInstance and client pod so
// they run in parallel across Ginkgo procs. Crash recovery specs stop and
// restart the engine operator within a single It, so the instance operator is
// set up directly in BeforeAll and the engine operator is managed per-It.

var _ = Describe("Crash Recovery", func() {
	Describe("Phase: Creating - Initial Deployment", Ordered, func() {
		var (
			instanceName = "inst-crash-create" + queryConfig.Suffix
			engineName   = "test-crash-create" + queryConfig.Suffix + "-engine"
			instanceOp   *InstanceOperator
			operator     *OperatorInstance
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Starting instance operator for crash create tests")
			var err error
			instanceOp, err = StartInstanceOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating FireboltInstance")
			Expect(CreateInstance(ctx, instanceName, metadataImage, metadataTag)).To(Succeed())
			Expect(WaitForInstanceReady(ctx, instanceName, instanceReadyTimeout)).To(Succeed())
		})

		AfterAll(func() {
			_ = DeleteInstance(ctx, instanceName)
			if instanceOp != nil {
				instanceOp.Stop()
			}
		})

		AfterEach(func() {
			controller.ClearCrashPointsForEngine(engineName)
			// Delete before stopping the operator so the engine finalizer can be
			// processed. Stopping first would leave the engine stuck "being deleted"
			// and cause the next It block to get a 409 on CreateEngine.
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			if operator != nil {
				operator.Stop()
				operator = nil
			}
		})

		It("should recover from crash after ConfigMap created", func() {
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashAfterEngineConfigMapCreated, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after engine ConfigMap created\n")
			})

			By("Starting operator")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Creating engine")
			err = CreateEngine(ctx, instanceName, engineName, 1)
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

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - engine becomes stable")
			err = WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should recover from crash after StatefulSet created", func() {
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashAfterStatefulSetCreated, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after StatefulSet created\n")
			})

			By("Starting operator")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Creating engine")
			err = CreateEngine(ctx, instanceName, engineName, 1)
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

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - engine becomes stable")
			err = WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should recover from crash before status update to switching", func() {
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashBeforeCreatingToSwitching, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: before creating-to-switching status update\n")
			})

			By("Starting operator")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Creating engine")
			err = CreateEngine(ctx, instanceName, engineName, 1)
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

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - engine becomes stable")
			err = WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Phase: Switching - Scale Transition", Ordered, func() {
		var (
			instanceName = "inst-crash-switch" + queryConfig.Suffix
			engineName   = "test-crash-switch" + queryConfig.Suffix + "-engine"
			clientPod    = "client-crash-switch" + queryConfig.Suffix
			instanceOp   *InstanceOperator
			operator     *OperatorInstance
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Starting instance operator for crash switch tests")
			var err error
			instanceOp, err = StartInstanceOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating FireboltInstance")
			Expect(CreateInstance(ctx, instanceName, metadataImage, metadataTag)).To(Succeed())
			Expect(WaitForInstanceReady(ctx, instanceName, instanceReadyTimeout)).To(Succeed())
			By("Creating client pod for crash switch tests")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			_ = DeleteInstance(ctx, instanceName)
			if instanceOp != nil {
				instanceOp.Stop()
			}
		})

		AfterEach(func() {
			controller.ClearCrashPointsForEngine(engineName)
			// Delete before stopping the operator so the engine finalizer can be
			// processed. Stopping first would leave the engine stuck "being deleted"
			// and cause the next It block to get a 409 on CreateEngine.
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			if operator != nil {
				operator.Stop()
				operator = nil
			}
		})

		It("should recover from crash after service selector update (CRITICAL)", func() {
			By("Creating initial engine")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			err = CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the scale transition")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashAfterServiceSelectorUpdate, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after service selector update (traffic already switched!)\n")
			})

			By("Triggering scale up")
			err = UpdateEngineReplicas(ctx, engineName, 3)
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

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - engine becomes ready with new size")
			err = WaitForEngineReady(ctx, engineName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine works")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})

		It("should recover from crash before switching status update", func() {
			By("Creating initial engine")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			err = CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the scale transition")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashBeforeSwitchingStatusUpdate, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: before switching status update\n")
			})

			By("Triggering scale up")
			err = UpdateEngineReplicas(ctx, engineName, 3)
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

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - engine becomes ready with new size")
			err = WaitForEngineReady(ctx, engineName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine works")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})
	})

	Describe("Phase: Cleaning - After Drain", Ordered, func() {
		var (
			instanceName = "inst-crash-clean" + queryConfig.Suffix
			engineName   = "test-crash-clean" + queryConfig.Suffix + "-engine"
			clientPod    = "client-crash-clean" + queryConfig.Suffix
			instanceOp   *InstanceOperator
			operator     *OperatorInstance
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Starting instance operator for crash clean tests")
			var err error
			instanceOp, err = StartInstanceOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating FireboltInstance")
			Expect(CreateInstance(ctx, instanceName, metadataImage, metadataTag)).To(Succeed())
			Expect(WaitForInstanceReady(ctx, instanceName, instanceReadyTimeout)).To(Succeed())
			By("Creating client pod for crash clean tests")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			_ = DeleteInstance(ctx, instanceName)
			if instanceOp != nil {
				instanceOp.Stop()
			}
		})

		AfterEach(func() {
			controller.ClearCrashPointsForEngine(engineName)
			// Delete before stopping the operator so the engine finalizer can be
			// processed. Stopping first would leave the engine stuck "being deleted"
			// and cause the next It block to get a 409 on CreateEngine.
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			if operator != nil {
				operator.Stop()
				operator = nil
			}
		})

		It("should recover from crash after StatefulSet deleted", func() {
			By("Creating initial engine")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			err = CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the cleaning phase")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashAfterStatefulSetDeleted, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: after StatefulSet deleted\n")
			})

			By("Triggering scale up")
			err = UpdateEngineReplicas(ctx, engineName, 3)
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

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - engine becomes stable")
			err = WaitForEngineReady(ctx, engineName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine works")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})

		It("should recover from crash before status update to stable", func() {
			By("Creating initial engine")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			err = CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Setting crash point for the final phase")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashBeforeCleaningToTerminal, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit: before cleaning-to-stable status update\n")
			})

			By("Triggering scale up")
			err = UpdateEngineReplicas(ctx, engineName, 3)
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

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying recovery - engine becomes stable")
			err = WaitForEngineReady(ctx, engineName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine works")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue())
		})
	})

	Describe("Availability During Crash", Ordered, func() {
		var (
			instanceName = "inst-crash-avail" + queryConfig.Suffix
			engineName   = "test-crash-avail" + queryConfig.Suffix + "-engine"
			clientPod    = "client-crash-avail" + queryConfig.Suffix
			instanceOp   *InstanceOperator
			operator     *OperatorInstance
			bgRunner     *GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Starting instance operator for availability-during-crash test")
			var err error
			instanceOp, err = StartInstanceOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating FireboltInstance")
			Expect(CreateInstance(ctx, instanceName, metadataImage, metadataTag)).To(Succeed())
			Expect(WaitForInstanceReady(ctx, instanceName, instanceReadyTimeout)).To(Succeed())
			By("Creating client pod for availability-during-crash test")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			_ = DeleteInstance(ctx, instanceName)
			if instanceOp != nil {
				instanceOp.Stop()
			}
		})

		AfterEach(func() {
			if bgRunner != nil {
				bgRunner.Stop()
				bgRunner = nil
			}
			controller.ClearCrashPointsForEngine(engineName)
			if operator != nil {
				operator.Stop()
				operator = nil
			}
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
		})

		It("should maintain query availability when crash occurs after service selector update", func() {
			By("Creating initial engine")
			var err error
			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			err = CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Starting background queries")
			bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, engineName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			time.Sleep(3 * time.Second)

			By("Setting crash point for the scale transition")
			var crashHit atomic.Bool
			restartCh := controller.SetCrashPoint(engineName, controller.CrashAfterServiceSelectorUpdate, func() {
				crashHit.Store(true)
				fmt.Fprintf(GinkgoWriter, "Crash point hit during availability test\n")
			})

			By("Triggering scale up")
			err = UpdateEngineReplicas(ctx, engineName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash point to be hit")
			Eventually(func() bool {
				return crashHit.Load()
			}).WithTimeout(clusterTransitionTimeout).WithPolling(500 * time.Millisecond).Should(BeTrue())

			By("Stopping operator (simulating crash) - queries should continue to new generation")
			operator.Stop()
			operator = nil

			time.Sleep(3 * time.Second)

			By("Releasing blocked reconcile and restarting")
			close(restartCh)
			time.Sleep(time.Second)

			operator, err = StartOperator(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for recovery")
			err = WaitForEngineReady(ctx, engineName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
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
