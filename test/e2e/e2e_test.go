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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// queryConfig is defined in query_config_light_test.go or query_config_heavy_test.go
// based on build tags. Run with -tags=e2e for light queries, -tags=e2e,heavy for heavy queries.
//
// Every second-level Describe below owns its own FireboltInstance (via
// SetupTestInstance/TeardownTestInstance) so specs stay isolated and can run
// in parallel under --ginkgo.procs.

var _ = Describe("Firebolt Engine", func() {
	BeforeEach(func() {
		GinkgoWriter.Printf("Running tests with query mode: %s\n", queryConfig.Mode)
	})
	// Test 1: Single node engine lifecycle
	Describe("Single Node Engine", Ordered, func() {
		var (
			instanceName = "inst-single" + queryConfig.Suffix
			engineName   = "test-single" + queryConfig.Suffix + "-engine"
			clientPod    = "client-single" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for single node test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up single node test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should create a single node engine, run queries, and clean up", func() {
			By("Creating engine with 1 replica")
			err := CreateEngine(ctx, instanceName, engineName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running query")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())

			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 2: Scale up from 2 to 4 nodes with continuous queries
	Describe("Scale Up 2 to 4 Nodes", Ordered, func() {
		var (
			instanceName = "inst-scaleup" + queryConfig.Suffix
			engineName   = "test-scaleup" + queryConfig.Suffix + "-engine"
			clientPod    = "client-scaleup" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
			bgRunner     *GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for scale up test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up scale up test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should scale up while maintaining query availability", func() {
			By("Creating engine with 2 replicas")
			err := CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running initial query")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Starting background query runner")
			bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, engineName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Scaling up to 4 replicas")
			err = UpdateEngineReplicas(ctx, engineName, 4)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for scaled engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 4, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable after scaling")
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running query on scaled engine")
			output, err = RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err = ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Stopping background query runner")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Background queries: %d successes, %d failures\n", successes, failures)
			if failures > 0 {
				bgRunner.PrintFailureSummary()
			}

			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during scale up")
			Expect(successes).To(BeNumerically(">=", int32(minMeaningfulQueries)), "background runner did not accumulate enough samples for the zero-failure assertion to be meaningful")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 3: Scale down from 3 to 1 node with continuous queries
	Describe("Scale Down 3 to 1 Node", Ordered, func() {
		var (
			instanceName = "inst-scaledown" + queryConfig.Suffix
			engineName   = "test-scaledown" + queryConfig.Suffix + "-engine"
			clientPod    = "client-scaledown" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
			bgRunner     *GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for scale down test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up scale down test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should scale down while maintaining query availability", func() {
			By("Creating engine with 3 replicas")
			err := CreateEngine(ctx, instanceName, engineName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 3, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running initial query")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Starting background query runner")
			bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, engineName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Scaling down to 1 replica")
			err = UpdateEngineReplicas(ctx, engineName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for scaled engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 1, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable after scaling")
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running query on scaled engine")
			output, err = RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err = ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Stopping background query runner")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Background queries: %d successes, %d failures\n", successes, failures)
			if failures > 0 {
				bgRunner.PrintFailureSummary()
			}

			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during scale down")
			Expect(successes).To(BeNumerically(">=", int32(minMeaningfulQueries)), "background runner did not accumulate enough samples for the zero-failure assertion to be meaningful")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 4: Rapid config changes - only the last change should be applied
	// Serial because the rapid-change loop creates short-lived StatefulSets
	// (and therefore PVCs) faster than the kind cluster's scheduler and PV
	// controller can settle. Running concurrently with other Describes
	// pushed kube-scheduler's VolumeBinding plugin into 409-Conflict retries
	// on PreBind, leaving pods stuck in FailedScheduling and the engine in a
	// half-rolled-out state past the rapid-changes timeout. See FB-996.
	Describe("Rapid Config Changes", Ordered, Serial, func() {
		var (
			instanceName = "inst-rapid" + queryConfig.Suffix
			engineName   = "test-rapid" + queryConfig.Suffix + "-engine"
			clientPod    = "client-rapid" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
			bgRunner     *GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for rapid changes test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up rapid changes test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should only apply the last config change when multiple rapid changes occur", func() {
			rapidTimeout := 300 * time.Second

			By("Creating engine with 2 replicas")
			err := CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Starting background query runner")
			bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, engineName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Triggering scale up to 4 replicas")
			err = UpdateEngineReplicas(ctx, engineName, 4)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for scale up to 4 to complete")
			err = WaitForEngineReady(ctx, engineName, 4, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			// 7 rapid changes alternating 3↔1 (ending at 1) is enough to exercise
			// the operator's "abandon if creating, defer if draining/cleaning"
			// path multiple times. The original 15-change variant produced ~32
			// short-lived PVCs in <1s, which overran the kind cluster's
			// scheduler+PV-controller and stranded pods in FailedScheduling well
			// past the rapid-changes timeout. See FB-996.
			By("Rapidly applying 7 config changes alternating between 3 and 1, ending with 1")
			for i := 0; i < 7; i++ {
				replicas := 3
				if i%2 == 1 {
					replicas = 1
				}
				if i == 6 {
					replicas = 1
				}
				err = UpdateEngineReplicas(ctx, engineName, replicas)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for final scale down to 1 node (last change applied)")
			err = WaitForEngineReady(ctx, engineName, 1, rapidTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to be stable at 1 node")
			err = WaitForEngineStable(ctx, engineName, rapidTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine has exactly 1 node")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Stopping background query runner")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Background queries: %d successes, %d failures\n", successes, failures)
			if failures > 0 {
				bgRunner.PrintFailureSummary()
			}

			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during rapid changes")
			Expect(successes).To(BeNumerically(">=", int32(minMeaningfulQueries)), "background runner did not accumulate enough samples for the zero-failure assertion to be meaningful")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 5: Harmonic minor scale - 1->2->3->2->1
	Describe("Harmonic Minor Scale", Ordered, func() {
		var (
			instanceName = "inst-harmonic" + queryConfig.Suffix
			engineName   = "test-harmonic" + queryConfig.Suffix + "-engine"
			clientPod    = "client-harmonic" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
			bgRunner     *GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for harmonic scale test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up harmonic scale test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should scale up and down through 1->2->3->2->1 without downtime", func() {
			By("Creating engine with 1 replica")
			err := CreateEngine(ctx, instanceName, engineName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Starting background query runner")
			bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, engineName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			for replicas := 2; replicas <= 3; replicas++ {
				By(fmt.Sprintf("Scaling up to %d replicas", replicas))
				err = UpdateEngineReplicas(ctx, engineName, replicas)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForEngineReady(ctx, engineName, replicas, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			for replicas := 2; replicas >= 1; replicas-- {
				By(fmt.Sprintf("Scaling down to %d replicas", replicas))
				err = UpdateEngineReplicas(ctx, engineName, replicas)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForEngineReady(ctx, engineName, replicas, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Running final query to verify engine health")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Stopping background query runner")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Background queries: %d successes, %d failures\n", successes, failures)
			if failures > 0 {
				bgRunner.PrintFailureSummary()
			}

			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during harmonic scaling")
			Expect(successes).To(BeNumerically(">=", int32(minMeaningfulQueries)), "background runner did not accumulate enough samples for the zero-failure assertion to be meaningful")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 6: Image switching via EngineClass mutation.
	//
	// Image override moved from spec.image on FireboltEngine to
	// containers[engine].image on the referenced EngineClass (FB-1145).
	// The engine is created with spec.engineClassRef pointing at a
	// dedicated class; mutating the class's container image is the
	// canonical path for runtime version upgrades. The engine controller's
	// EngineClass watch fires immediately, stsMatchesSpec detects the
	// AnnotationEngineClassHash drift, and a clean blue-green rolls.
	Describe("Image Switching", Ordered, func() {
		var (
			instanceName = "inst-image" + queryConfig.Suffix
			engineName   = "test-image" + queryConfig.Suffix + "-engine"
			className    = "test-image" + queryConfig.Suffix + "-class"
			clientPod    = "client-image" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
			bgRunner     *GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for image switching test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up image switching test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			// EngineClass deletion is guarded by the EngineClassReconciler's
			// deletion-guard finalizer while any FireboltEngine in the
			// same namespace references the class (the gate uses a live
			// List, not status). The e2e harness runs the controllers
			// in-process and does not register the validating webhook,
			// so the finalizer is the only enforcement here. Clearing
			// the engine first (above) must drop the bound count to
			// zero, after which the next class reconcile removes the
			// finalizer and the delete completes.
			_ = DeleteEngineClass(ctx, className)
			TeardownTestInstance(ctx, lc)
		})

		It("should switch image without downtime", func() {
			By("Creating EngineClass with the initial image")
			err := CreateEngineClass(ctx, className, testImage+":"+testTag)
			Expect(err).NotTo(HaveOccurred())

			By("Creating engine with 3 replicas, referencing the EngineClass")
			err = CreateEngineWithClass(ctx, instanceName, engineName, 3, className)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 3, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running initial query")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Starting background query runner")
			bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, engineName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Switching the EngineClass to the new image tag")
			err = UpdateEngineClassImage(ctx, className, testImage+":"+newImageTag)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to leave the pre-mutation PhaseStable")
			// EngineClass watch enqueues this engine for re-reconcile;
			// stsMatchesSpec returns false on the new class hash and the
			// controller bumps currentGeneration. No FireboltEngine spec
			// edit is involved, so observedGeneration cannot be used as
			// the gate — wait for the engine to leave Stable instead.
			err = WaitForEnginePhaseChange(ctx, engineName, computev1alpha1.PhaseStable, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to complete image switch")
			err = WaitForEngineReady(ctx, engineName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to be stable after image switch")
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running query after image switch")
			output, err = RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err = ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Stopping background query runner")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Background queries: %d successes, %d failures\n", successes, failures)
			if failures > 0 {
				bgRunner.PrintFailureSummary()
			}

			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during image switch")
			Expect(successes).To(BeNumerically(">=", int32(minMeaningfulQueries)), "background runner did not accumulate enough samples for the zero-failure assertion to be meaningful")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 7: Multiple engines managed by same operator
	Describe("Multi Engine Management", Ordered, func() {
		var (
			instanceName = "inst-multi" + queryConfig.Suffix
			engineNames  = []string{
				"test-multi" + queryConfig.Suffix + "-engine1",
				"test-multi" + queryConfig.Suffix + "-engine2",
				"test-multi" + queryConfig.Suffix + "-engine3",
			}
			engineSizes = []int{1, 2, 3}
			clientPod   = "client-multi" + queryConfig.Suffix
			lc          *TestInstanceLifecycle
			bgRunners   []*GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDumpMulti(&instanceName, &engineNames)

		BeforeAll(func() {
			By("Setting up FireboltInstance for multi-engine test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up multi-engine test")
			for _, runner := range bgRunners {
				if runner != nil {
					runner.Stop()
				}
			}
			DeleteClientPod(ctx, clientPod)
			for _, name := range engineNames {
				_ = DeleteEngine(ctx, name)
				_ = WaitForResourcesDeleted(ctx, name, resourceCleanupTimeout)
			}
			TeardownTestInstance(ctx, lc)
		})

		It("should manage multiple engines independently", func() {
			By("Creating 3 engines of sizes 1, 2, 3")
			for i, name := range engineNames {
				err := CreateEngine(ctx, instanceName, name, engineSizes[i])
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all engines to become ready")
			for i, name := range engineNames {
				err := WaitForEngineReady(ctx, name, engineSizes[i], clusterReadyTimeout)
				Expect(err).NotTo(HaveOccurred())
				err = WaitForEngineStable(ctx, name, clusterReadyTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Starting background query runner for each engine")
			bgRunners = make([]*GatewayBackgroundQueryRunner, len(engineNames))
			for i, name := range engineNames {
				bgRunners[i] = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, name, queryConfig.Query, queryConfig.Validator)
				bgRunners[i].Start(ctx)
			}

			By("Scaling up each engine by 1")
			for i, name := range engineNames {
				newSize := engineSizes[i] + 1
				err := UpdateEngineReplicas(ctx, name, newSize)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all engines to finish scaling up")
			for i, name := range engineNames {
				newSize := engineSizes[i] + 1
				err := WaitForEngineReady(ctx, name, newSize, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
				err = WaitForEngineStable(ctx, name, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Scaling all engines down to 1")
			for _, name := range engineNames {
				err := UpdateEngineReplicas(ctx, name, 1)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all engines to finish scaling down")
			for _, name := range engineNames {
				err := WaitForEngineReady(ctx, name, 1, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
				err = WaitForEngineStable(ctx, name, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Stopping all background query runners and verifying no failures")
			for i, runner := range bgRunners {
				runner.Stop()
				successes, failures := runner.GetStats()
				fmt.Fprintf(GinkgoWriter, "Engine %s: %d successes, %d failures\n", engineNames[i], successes, failures)
				if failures > 0 {
					runner.PrintFailureSummary()
				}
				Expect(failures).To(Equal(int32(0)), fmt.Sprintf("Engine %s should have no query failures", engineNames[i]))
				Expect(successes).To(BeNumerically(">=", int32(minMeaningfulQueries)),
					fmt.Sprintf("engine %s background runner did not accumulate enough samples for the zero-failure assertion to be meaningful", engineNames[i]))
			}

			By("Deleting all engines")
			for _, name := range engineNames {
				err := DeleteEngine(ctx, name)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all resources to be deleted")
			for _, name := range engineNames {
				err := WaitForResourcesDeleted(ctx, name, resourceCleanupTimeout)
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

	// Test 8: Scale down restarts pods with updated config (config hash)
	Describe("Scale Down Config Restart", Ordered, func() {
		var (
			instanceName = "inst-cfghash" + queryConfig.Suffix
			engineName   = "test-cfghash" + queryConfig.Suffix + "-engine"
			clientPod    = "client-cfghash" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for config hash test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up config hash test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should restart pods when replica count changes so engine reads correct node list", func() {
			By("Creating engine with 3 replicas")
			err := CreateEngine(ctx, instanceName, engineName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Immediately scaling down to 1 replica before pods are ready")
			time.Sleep(2 * time.Second)
			err = UpdateEngineReplicas(ctx, engineName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to become ready with 1 replica")
			err = WaitForEngineReady(ctx, engineName, 1, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running query to verify the single-node engine is functional")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Post-scale-down query validation failed")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 9: Recreate rollout strategy - no drain wait
	Describe("Recreate Rollout Strategy", Ordered, func() {
		var (
			instanceName = "inst-recreate" + queryConfig.Suffix
			engineName   = "test-recreate" + queryConfig.Suffix + "-engine"
			clientPod    = "client-recreate" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for recreate rollout test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up recreate rollout test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should transition without waiting for drain when rollout is 'recreate'", func() {
			By("Creating engine with 2 replicas and recreate rollout")
			err := CreateEngineWithRollout(ctx, instanceName, engineName, 2, "recreate")
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running one-shot query to verify engine works")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Initial query result validation failed")

			By("Scaling to 3 replicas - should transition quickly without drain wait")
			err = UpdateEngineReplicas(ctx, engineName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Polling query until successful with 2 minute timeout")
			deadline := time.Now().Add(2 * time.Minute)
			var querySuccess bool
			for time.Now().Before(deadline) {
				output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
				if err == nil {
					result, parseErr := ParseQueryResult(output)
					if parseErr == nil && queryConfig.Validator(result) {
						querySuccess = true
						break
					}
				}
				time.Sleep(1 * time.Second)
			}
			Expect(querySuccess).To(BeTrue(), "Query should succeed within timeout after recreate rollout")

			By("Verifying engine is stable with 3 replicas")
			err = WaitForEngineReady(ctx, engineName, 3, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 10: Scale to zero - stop/start lifecycle via spec.replicas=0
	Describe("Scale To Zero", Ordered, func() {
		var (
			instanceName = "inst-stop" + queryConfig.Suffix
			engineName   = "test-stop" + queryConfig.Suffix + "-engine"
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for scale-to-zero test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Cleaning up scale-to-zero test")
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should scale from 2 to 0 to 2, surfacing PhaseStopped and Ready=Stopped", func() {
			By("Creating engine with 2 replicas (graceful rollout)")
			err := CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying running engine Service has 2 endpoints")
			Expect(WaitForEngineServiceEndpointCount(ctx, engineName, 2, clusterReadyTimeout)).To(Succeed())

			By("Scaling engine to 0 replicas (stop)")
			err = UpdateEngineReplicas(ctx, engineName, 0)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine Phase to become stopped")
			err = WaitForEnginePhase(ctx, engineName, computev1alpha1.PhaseStopped, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Ready condition is False with Reason=Stopped")
			err = WaitForEngineReadyCondition(ctx, engineName, metav1.ConditionFalse, "Stopped", clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine Service has 0 endpoints")
			Expect(WaitForEngineServiceEndpointCount(ctx, engineName, 0, clusterReadyTimeout)).To(Succeed())

			By("Scaling engine back to 2 replicas (resume)")
			err = UpdateEngineReplicas(ctx, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine to become ready again")
			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Ready condition is True with Reason=EngineReady")
			err = WaitForEngineReadyCondition(ctx, engineName, metav1.ConditionTrue, "EngineReady", clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine Service has 2 endpoints again")
			Expect(WaitForEngineServiceEndpointCount(ctx, engineName, 2, clusterReadyTimeout)).To(Succeed())

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 11: nodeSelector + tolerations + affinity mutated together
	// trigger one blue-green re-roll, propagate to the pod template, and
	// keep gateway queries error-free. Rules pin to kubernetes.io/os=linux
	// and an unused taint key so the test exercises propagation without
	// depending on cluster topology.
	Describe("Scheduling Fields Trigger Blue-Green", Ordered, func() {
		var (
			instanceName = "inst-sched" + queryConfig.Suffix
			engineName   = "test-sched" + queryConfig.Suffix + "-engine"
			clientPod    = "client-sched" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
			bgRunner     *GatewayBackgroundQueryRunner
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for scheduling-fields test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up scheduling-fields test")
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should re-roll the STS when nodeSelector, tolerations, and affinity are set together", func() {
			By("Creating engine with 2 replicas and no scheduling fields")
			err := CreateEngine(ctx, instanceName, engineName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial engine to become ready")
			err = WaitForEngineReady(ctx, engineName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for engine status to be stable")
			err = WaitForEngineStable(ctx, engineName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Recording initial generation")
			initialGen, initialActiveGen, err := GetEngineGeneration(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())
			Expect(initialGen).To(Equal(initialActiveGen),
				"engine should be stable (current == active) before scheduling update")

			By("Running baseline query")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "baseline query result validation failed")

			By("Starting background query runner")
			bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(clientPod, instanceName, engineName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			nodeSelector := map[string]string{"kubernetes.io/os": "linux"}
			tolerations := []corev1.Toleration{{
				Key:      "firebolt.io/e2e-scheduling",
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			}}
			affinity := &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      "kubernetes.io/os",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"linux"},
							}},
						}},
					},
				},
			}

			By("Setting nodeSelector + tolerations + affinity in a single spec update")
			err = UpdateEngineScheduling(ctx, engineName, nodeSelector, tolerations, affinity)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for controller to observe the new spec (avoid racing the pre-mutation PhaseStable)")
			err = WaitForEngineSpecObserved(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for blue-green to complete (engine ready and stable on the new generation)")
			err = WaitForEngineReady(ctx, engineName, 2, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Asserting the engine actually re-rolled to a new generation")
			newGen, newActiveGen, err := GetEngineGeneration(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())
			Expect(newGen).To(BeNumerically(">", initialGen),
				"scheduling-field change must advance CurrentGeneration; was %d, still %d", initialGen, newGen)
			Expect(newActiveGen).To(Equal(newGen),
				"after stable, ActiveGeneration must equal CurrentGeneration")

			By("Verifying the live StatefulSet's pod template carries all three scheduling fields")
			// The controller transitions to stable immediately after issuing the
			// Delete on the old generation's STS; Kubernetes' foregroundDeletion
			// propagation then removes the object asynchronously once owned pods
			// are gone. Poll briefly so we don't race that GC window — STSs with
			// a non-nil DeletionTimestamp are mid-removal and don't count.
			var podSpec corev1.PodSpec
			Eventually(func(g Gomega) {
				stsList, err := k8sClient.AppsV1().StatefulSets(testNamespace).List(ctx, metav1.ListOptions{
					LabelSelector: fmt.Sprintf("firebolt.io/engine=%s", engineName),
				})
				g.Expect(err).NotTo(HaveOccurred())
				var active []int
				for i := range stsList.Items {
					if stsList.Items[i].DeletionTimestamp == nil {
						active = append(active, i)
					}
				}
				g.Expect(active).To(HaveLen(1),
					"after stable, exactly one active engine STS (the active generation) must remain")
				podSpec = stsList.Items[active[0]].Spec.Template.Spec
			}, 15*time.Second, pollInterval).Should(Succeed())
			Expect(podSpec.NodeSelector).To(Equal(nodeSelector),
				"nodeSelector must propagate to the live pod template")
			Expect(podSpec.Tolerations).To(Equal(tolerations),
				"tolerations must propagate to the live pod template")
			Expect(podSpec.Affinity).To(Equal(affinity),
				"affinity must propagate to the live pod template")

			By("Running query on the re-rolled engine")
			output, err = RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err = ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "post-reroll query result validation failed")

			By("Stopping background query runner")
			bgRunner.Stop()

			successes, failures := bgRunner.GetStats()
			fmt.Fprintf(GinkgoWriter, "Background queries: %d successes, %d failures\n", successes, failures)
			if failures > 0 {
				bgRunner.PrintFailureSummary()
			}

			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during scheduling-fields blue-green")
			Expect(successes).To(BeNumerically(">=", int32(minMeaningfulQueries)), "background runner did not accumulate enough samples for the zero-failure assertion to be meaningful")

			By("Deleting engine")
			err = DeleteEngine(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

})
