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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Each second-level Describe owns its own FireboltInstance + engine so specs
// can run in parallel without stepping on each other's instance mutations.

var _ = Describe("FireboltInstance Lifecycle", func() {
	Describe("Metadata Image Switch", Ordered, func() {
		var (
			instanceName = "inst-md-switch"
			engineName   = "test-md-switch-engine"
			clientPod    = "client-md-switch"
			lc           *TestInstanceLifecycle
		)

		BeforeAll(func() {
			By("Setting up FireboltInstance for metadata image switch test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())

			By("Creating a 2-replica engine for gateway query routing")
			Expect(CreateEngine(ctx, instanceName, engineName, 2)).To(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)).To(Succeed())

			By("Verifying initial query via gateway succeeds")
			output, err := RunQueryViaGateway(ctx, clientPod, instanceName, engineName, LightQuery)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(LightQueryValidator(result)).To(BeTrue(), "Initial gateway query should return 42")
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		AfterEach(func() {
			By("Restoring metadata image to original tag")
			err := UpdateInstanceMetadataImage(ctx, instanceName, pensieveTag)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for instance to stabilize after restore")
			err = WaitForInstanceReady(ctx, instanceName, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should switch metadata to a new image tag", func() {
			By(fmt.Sprintf("Updating metadata image to tag %s", newPensieveTag))
			err := UpdateInstanceMetadataImage(ctx, instanceName, newPensieveTag)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for metadata deployment to roll out new image")
			err = WaitForInstanceMetadataImage(ctx, instanceName, newPensieveTag, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for instance to return to Ready")
			err = WaitForInstanceReady(ctx, instanceName, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engines still respond via gateway after metadata switch")
			output, err := RunQueryViaGateway(ctx, clientPod, instanceName, engineName, LightQuery)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(LightQueryValidator(result)).To(BeTrue(), "Query via gateway should return 42 after metadata switch")
		})
	})

	Describe("Gateway Scaling", Ordered, func() {
		var (
			instanceName = "inst-gw-scale"
			engineName   = "test-gw-scale-engine"
			clientPod    = "client-gw-scale"
			lc           *TestInstanceLifecycle
			bgRunner     *GatewayBackgroundQueryRunner
		)

		BeforeAll(func() {
			By("Setting up FireboltInstance for gateway scaling test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())

			By("Creating a 2-replica engine for gateway query routing")
			Expect(CreateEngine(ctx, instanceName, engineName, 2)).To(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)).To(Succeed())
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		AfterEach(func() {
			if bgRunner != nil {
				bgRunner.Stop()
				bgRunner = nil
			}

			By("Restoring gateway replicas to 1")
			err := UpdateInstanceGatewayReplicas(ctx, instanceName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for instance to stabilize after restore")
			err = WaitForGatewayReplicas(ctx, instanceName, 1, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
		})

		It("should scale gateway 1 -> 3 -> 1 without query downtime", func() {
			By("Starting background queries through gateway")
			bgRunner = NewGatewayBackgroundQueryRunner(clientPod, instanceName, engineName, LightQuery)
			bgRunner.Start(ctx)

			time.Sleep(3 * time.Second)

			By("Scaling gateway to 3 replicas")
			err := UpdateInstanceGatewayReplicas(ctx, instanceName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for 3 gateway replicas to be ready")
			err = WaitForGatewayReplicas(ctx, instanceName, 3, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(3 * time.Second)

			By("Scaling gateway back to 1 replica")
			err = UpdateInstanceGatewayReplicas(ctx, instanceName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for 1 gateway replica to be ready")
			err = WaitForGatewayReplicas(ctx, instanceName, 1, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			time.Sleep(3 * time.Second)

			By("Stopping background queries and checking results")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Gateway scaling: successes=%d failures=%d\n", successes, failures)
			bgRunner.PrintFailureSummary()

			Expect(successes).To(BeNumerically(">", 0), "Should have had successful queries")
			Expect(failures).To(Equal(int32(0)), "Gateway scaling should cause zero query failures")
			bgRunner = nil
		})
	})
})
