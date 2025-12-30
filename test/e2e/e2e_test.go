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

// queryConfig is defined in query_config_light_test.go or query_config_heavy_test.go
// based on build tags. Run with -tags=e2e for light queries, -tags=e2e,heavy for heavy queries.

var _ = Describe("Core Operator", func() {
	BeforeEach(func() {
		GinkgoWriter.Printf("Running tests with query mode: %s\n", queryConfig.Mode)
	})
	// Test 1: Single node cluster lifecycle
	Describe("Single Node Cluster", Ordered, func() {
		var (
			clusterPrefix = "test-single" + queryConfig.Suffix
			clusterName   = "test-single" + queryConfig.Suffix + "-cluster"
			operator      *OperatorInstance
		)

		BeforeAll(func() {
			By("Starting operator for single node test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for single node test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should create a single node cluster, run queries, and clean up", func() {
			By("Creating cluster config with 1 replica")
			err := CreateClusterConfig(ctx, clusterName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 1, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable")
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running SELECT 42 query")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())

			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Deleting cluster config")
			err = DeleteClusterConfig(ctx, clusterName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 2: Scale up from 2 to 4 nodes with continuous queries
	Describe("Scale Up 2 to 4 Nodes", Ordered, func() {
		var (
			clusterPrefix = "test-scaleup" + queryConfig.Suffix
			clusterName   = "test-scaleup" + queryConfig.Suffix + "-cluster"
			operator      *OperatorInstance
			bgRunner      *BackgroundQueryRunner
		)

		BeforeAll(func() {
			By("Starting operator for scale up test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for scale up test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should scale up while maintaining query availability", func() {
			By("Creating cluster config with 2 replicas")
			err := CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable")
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running initial SELECT 42 query")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Starting background query runner")
			bgRunner = NewBackgroundQueryRunnerWithValidator(clusterName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Scaling up to 4 replicas")
			err = UpdateClusterReplicas(ctx, clusterName, 4)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for scaled cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 4, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable after scaling")
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running SELECT 42 query on scaled cluster")
			output, err = RunQuery(ctx, clusterName, queryConfig.Query)
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

			// Zero failures allowed - zero-downtime scaling must be achieved
			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during scale up")
			Expect(successes).To(BeNumerically(">", 0), "Should have some successful background queries")

			By("Deleting cluster config")
			err = DeleteClusterConfig(ctx, clusterName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 3: Scale down from 3 to 1 node with continuous queries
	Describe("Scale Down 3 to 1 Node", Ordered, func() {
		var (
			clusterPrefix = "test-scaledown" + queryConfig.Suffix
			clusterName   = "test-scaledown" + queryConfig.Suffix + "-cluster"
			operator      *OperatorInstance
			bgRunner      *BackgroundQueryRunner
		)

		BeforeAll(func() {
			By("Starting operator for scale down test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for scale down test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should scale down while maintaining query availability", func() {
			By("Creating cluster config with 3 replicas")
			err := CreateClusterConfig(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable")
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running initial SELECT 42 query")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Starting background query runner")
			bgRunner = NewBackgroundQueryRunnerWithValidator(clusterName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Scaling down to 1 replica")
			err = UpdateClusterReplicas(ctx, clusterName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for scaled cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 1, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable after scaling")
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running SELECT 42 query on scaled cluster")
			output, err = RunQuery(ctx, clusterName, queryConfig.Query)
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

			// Zero failures allowed - zero-downtime scaling must be achieved
			Expect(failures).To(Equal(int32(0)), "Background queries should not fail during scale down")
			Expect(successes).To(BeNumerically(">", 0), "Should have some successful background queries")

			By("Deleting cluster config")
			err = DeleteClusterConfig(ctx, clusterName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 4: Rapid config changes - only the last change should be applied
	Describe("Rapid Config Changes", Ordered, func() {
		var (
			clusterPrefix = "test-rapid" + queryConfig.Suffix
			clusterName   = "test-rapid" + queryConfig.Suffix + "-cluster"
			operator      *OperatorInstance
			bgRunner      *BackgroundQueryRunner
		)

		BeforeAll(func() {
			By("Starting operator for rapid changes test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for rapid changes test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should only apply the last config change when multiple rapid changes occur", func() {
			By("Creating cluster config with 2 replicas")
			err := CreateClusterConfig(ctx, clusterName, 2)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable")
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Starting background query runner")
			bgRunner = NewBackgroundQueryRunnerWithValidator(clusterName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Triggering scale up to 4 replicas")
			err = UpdateClusterReplicas(ctx, clusterName, 4)
			Expect(err).NotTo(HaveOccurred())

			By("Rapidly applying 15 config changes alternating between 3 and 1, ending with 1")
			// Once the operator takes in a config change, it snapshots the target
			// config into the status ConfigMap. Subsequent changes are queued as
			// "pending" and only the most recent pending change is kept.
			for i := 0; i < 15; i++ {
				replicas := 3
				if i%2 == 1 {
					replicas = 1
				}
				// Last change (i=14, even) would be 3, but we want to end with 1
				if i == 14 {
					replicas = 1
				}
				err = UpdateClusterReplicas(ctx, clusterName, replicas)
				Expect(err).NotTo(HaveOccurred())
				// No delay between changes - rapid succession
			}

			By("Waiting for scale up to 4 to complete first")
			// The operator should complete the 4-replica transition it already started
			err = WaitForClusterReady(ctx, clusterName, 4, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for final scale down to 1 node (last change applied)")
			// After the 4-replica transition completes, the operator picks up the
			// most recent pending change (1 replica) and executes that transition
			err = WaitForClusterReady(ctx, clusterName, 1, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster to be stable at 1 node")
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying cluster has exactly 1 node")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
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
			Expect(successes).To(BeNumerically(">", 0), "Should have some successful background queries")

			By("Deleting cluster config")
			err = DeleteClusterConfig(ctx, clusterName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 5: Harmonic minor scale - 1->2->3->2->1
	Describe("Harmonic Minor Scale", Ordered, func() {
		var (
			clusterPrefix = "test-harmonic" + queryConfig.Suffix
			clusterName   = "test-harmonic" + queryConfig.Suffix + "-cluster"
			operator      *OperatorInstance
			bgRunner      *BackgroundQueryRunner
		)

		BeforeAll(func() {
			By("Starting operator for harmonic scale test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for harmonic scale test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should scale up and down through 1->2->3->2->1 without downtime", func() {
			By("Creating cluster config with 1 replica")
			err := CreateClusterConfig(ctx, clusterName, 1)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 1, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable")
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Starting background query runner")
			bgRunner = NewBackgroundQueryRunnerWithValidator(clusterName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			// Scale up: 1 -> 2 -> 3
			for replicas := 2; replicas <= 3; replicas++ {
				By(fmt.Sprintf("Scaling up to %d replicas", replicas))
				err = UpdateClusterReplicas(ctx, clusterName, replicas)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForClusterReady(ctx, clusterName, replicas, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			// Scale down: 3 -> 2 -> 1
			for replicas := 2; replicas >= 1; replicas-- {
				By(fmt.Sprintf("Scaling down to %d replicas", replicas))
				err = UpdateClusterReplicas(ctx, clusterName, replicas)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForClusterReady(ctx, clusterName, replicas, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())

				err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Running final query to verify cluster health")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
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
			Expect(successes).To(BeNumerically(">", 0), "Should have some successful background queries")

			By("Deleting cluster config")
			err = DeleteClusterConfig(ctx, clusterName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 6: Image switching
	Describe("Image Switching", Ordered, func() {
		var (
			clusterPrefix = "test-image" + queryConfig.Suffix
			clusterName   = "test-image" + queryConfig.Suffix + "-cluster"
			newImageTag   = "latest"
			operator      *OperatorInstance
			bgRunner      *BackgroundQueryRunner
		)

		BeforeAll(func() {
			By("Starting operator for image switching test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for image switching test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should switch image without downtime", func() {
			By("Creating cluster config with 3 replicas")
			err := CreateClusterConfig(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for initial cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable")
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running initial SELECT 42 query")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Starting background query runner")
			bgRunner = NewBackgroundQueryRunnerWithValidator(clusterName, queryConfig.Query, queryConfig.Validator)
			bgRunner.Start(ctx)

			By("Switching to new image tag")
			err = UpdateClusterImageTag(ctx, clusterName, newImageTag)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster to complete image switch")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster to be stable after image switch")
			err = WaitForClusterStable(ctx, clusterName, clusterTransitionTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running SELECT 42 query after image switch")
			output, err = RunQuery(ctx, clusterName, queryConfig.Query)
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
			Expect(successes).To(BeNumerically(">", 0), "Should have some successful background queries")

			By("Deleting cluster config")
			err = DeleteClusterConfig(ctx, clusterName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all resources to be deleted")
			err = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// Test 7: Multiple clusters managed by same operator
	Describe("Multi Cluster Management", Ordered, func() {
		var (
			clusterPrefix = "test-multi" + queryConfig.Suffix
			clusterNames  = []string{
				"test-multi" + queryConfig.Suffix + "-cluster1",
				"test-multi" + queryConfig.Suffix + "-cluster2",
				"test-multi" + queryConfig.Suffix + "-cluster3",
			}
			clusterSizes = []int{1, 2, 3}
			operator     *OperatorInstance
			bgRunners    []*BackgroundQueryRunner
		)

		BeforeAll(func() {
			By("Starting operator for multi-cluster test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for multi-cluster test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should manage multiple clusters independently", func() {
			By("Creating 3 clusters of sizes 1, 2, 3")
			for i, name := range clusterNames {
				err := CreateClusterConfig(ctx, name, clusterSizes[i])
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all clusters to become ready")
			for i, name := range clusterNames {
				err := WaitForClusterReady(ctx, name, clusterSizes[i], clusterReadyTimeout)
				Expect(err).NotTo(HaveOccurred())
				err = WaitForClusterStable(ctx, name, clusterReadyTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Starting background query runner for each cluster")
			bgRunners = make([]*BackgroundQueryRunner, len(clusterNames))
			for i, name := range clusterNames {
				bgRunners[i] = NewBackgroundQueryRunnerWithValidator(name, queryConfig.Query, queryConfig.Validator)
				bgRunners[i].Start(ctx)
			}

			By("Scaling up each cluster by 1")
			for i, name := range clusterNames {
				newSize := clusterSizes[i] + 1
				err := UpdateClusterReplicas(ctx, name, newSize)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all clusters to finish scaling up")
			for i, name := range clusterNames {
				newSize := clusterSizes[i] + 1
				err := WaitForClusterReady(ctx, name, newSize, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
				err = WaitForClusterStable(ctx, name, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Scaling all clusters down to 1")
			for _, name := range clusterNames {
				err := UpdateClusterReplicas(ctx, name, 1)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all clusters to finish scaling down")
			for _, name := range clusterNames {
				err := WaitForClusterReady(ctx, name, 1, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
				err = WaitForClusterStable(ctx, name, clusterTransitionTimeout)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Stopping all background query runners and verifying no failures")
			for i, runner := range bgRunners {
				runner.Stop()
				successes, failures := runner.GetStats()
				fmt.Fprintf(GinkgoWriter, "Cluster %s: %d successes, %d failures\n", clusterNames[i], successes, failures)
				if failures > 0 {
					runner.PrintFailureSummary()
				}
				Expect(failures).To(Equal(int32(0)), fmt.Sprintf("Cluster %s should have no query failures", clusterNames[i]))
				Expect(successes).To(BeNumerically(">", 0), fmt.Sprintf("Cluster %s should have some successful queries", clusterNames[i]))
			}

			By("Deleting all cluster configs")
			for _, name := range clusterNames {
				err := DeleteClusterConfig(ctx, name)
				Expect(err).NotTo(HaveOccurred())
			}

			By("Waiting for all resources to be deleted")
			for _, name := range clusterNames {
				err := WaitForResourcesDeleted(ctx, name, resourceCleanupTimeout)
				Expect(err).NotTo(HaveOccurred())
			}
		})
	})

	// Test 8: Recreate rollout strategy - no drain wait
	Describe("Recreate Rollout Strategy", Ordered, func() {
		var (
			clusterPrefix = "test-recreate" + queryConfig.Suffix
			clusterName   = "test-recreate" + queryConfig.Suffix + "-cluster"
			operator      *OperatorInstance
		)

		BeforeAll(func() {
			By("Starting operator for recreate rollout test")
			var err error
			operator, err = StartOperator(clusterPrefix)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			By("Stopping operator for recreate rollout test")
			if operator != nil {
				operator.Stop()
			}
		})

		It("should transition without waiting for drain when rollout is 'recreate'", func() {
			By("Creating cluster config with 2 replicas and recreate rollout")
			err := CreateClusterConfigWithRollout(ctx, clusterName, 2, "recreate")
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster to become ready")
			err = WaitForClusterReady(ctx, clusterName, 2, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for cluster status to be stable")
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Running one-shot query to verify cluster works")
			output, err := RunQuery(ctx, clusterName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Initial query result validation failed")

			By("Scaling to 3 replicas - should transition quickly without drain wait")
			err = UpdateClusterReplicas(ctx, clusterName, 3)
			Expect(err).NotTo(HaveOccurred())

			By("Polling query until successful with 2 minute timeout")
			deadline := time.Now().Add(2 * time.Minute)
			var querySuccess bool
			for time.Now().Before(deadline) {
				output, err := RunQuery(ctx, clusterName, queryConfig.Query)
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

			By("Verifying cluster is stable with 3 replicas")
			err = WaitForClusterReady(ctx, clusterName, 3, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())
			err = WaitForClusterStable(ctx, clusterName, clusterReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting cluster config")
			err = DeleteClusterConfig(ctx, clusterName)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for resources to be deleted")
			err = WaitForResourcesDeleted(ctx, clusterName, resourceCleanupTimeout)
			Expect(err).NotTo(HaveOccurred())
		})
	})

})
