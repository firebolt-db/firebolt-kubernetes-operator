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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/firebolt-analytics/firebolt-kubernetes-operator/internal/controller"
)

var _ = Describe("FireboltInstance Infrastructure", Ordered, func() {
	var (
		engineName = "test-infra-engine"
		clientPod  = "client-infra"
		operator   *OperatorInstance
	)

	BeforeAll(func() {
		By("Starting engine operator for infrastructure tests")
		var err error
		operator, err = StartOperator(engineName)
		Expect(err).NotTo(HaveOccurred())

		By("Creating client pod")
		Expect(CreateClientPod(ctx, clientPod)).To(Succeed())

		By("Creating a test engine for query validation")
		err = CreateEngine(ctx, engineName, 1)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for engine to be ready and stable")
		err = WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)
		Expect(err).NotTo(HaveOccurred())
		err = WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterAll(func() {
		By("Cleaning up infrastructure test engine")
		DeleteClientPod(ctx, clientPod)
		_ = DeleteEngine(ctx, engineName)
		_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
		if operator != nil {
			operator.Stop()
		}
	})

	Describe("Instance Resource Verification", func() {
		It("should create all expected sub-resources", func() {
			By("Verifying PostgreSQL StatefulSet")
			pgName := testInstance + controller.SuffixMetadataPG
			ss, err := k8sClient.AppsV1().StatefulSets(testNamespace).Get(ctx, pgName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(ss.Status.ReadyReplicas).To(Equal(int32(1)))
			Expect(ss.Labels[controller.LabelInstance]).To(Equal(testInstance))
			Expect(ss.Labels[controller.LabelComponent]).To(Equal("postgres"))

			By("Verifying PostgreSQL Service")
			_, err = k8sClient.CoreV1().Services(testNamespace).Get(ctx, pgName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying PostgreSQL credentials Secret")
			_, err = k8sClient.CoreV1().Secrets(testNamespace).Get(ctx, testInstance+controller.SuffixMetadataPostgresCreds, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Metadata Deployment")
			mdName := testInstance + controller.SuffixMetadataService
			mdDep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, mdName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(mdDep.Status.ReadyReplicas).To(Equal(int32(1)))
			Expect(mdDep.Labels[controller.LabelInstance]).To(Equal(testInstance))
			Expect(mdDep.Labels[controller.LabelComponent]).To(Equal("metadata"))

			By("Verifying Metadata ConfigMap")
			_, err = k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, mdName+"-config", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Metadata Service")
			_, err = k8sClient.CoreV1().Services(testNamespace).Get(ctx, mdName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Gateway Deployment")
			gwName := testInstance + controller.SuffixGateway
			gwDep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, gwName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			Expect(gwDep.Status.ReadyReplicas).To(BeNumerically(">=", int32(1)))
			Expect(gwDep.Labels[controller.LabelInstance]).To(Equal(testInstance))
			Expect(gwDep.Labels[controller.LabelComponent]).To(Equal("gateway"))

			By("Verifying Gateway ConfigMap")
			_, err = k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, gwName+"-config", metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Gateway Service")
			_, err = k8sClient.CoreV1().Services(testNamespace).Get(ctx, gwName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Gateway PodDisruptionBudget")
			_, err = k8sClient.PolicyV1().PodDisruptionBudgets(testNamespace).Get(ctx, gwName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("PostgreSQL Crash Recovery", func() {
		It("should recover when PG pod is deleted", func() {
			pgName := testInstance + controller.SuffixMetadataPG

			By("Deleting the PostgreSQL pod")
			pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s,%s=postgres", controller.LabelInstance, testInstance, controller.LabelComponent),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).NotTo(BeEmpty())
			err = k8sClient.CoreV1().Pods(testNamespace).Delete(ctx, pods.Items[0].Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash to propagate (ReadyReplicas drops to 0)")
			Eventually(func() int32 {
				ss, err := k8sClient.AppsV1().StatefulSets(testNamespace).Get(ctx, pgName, metav1.GetOptions{})
				if err != nil {
					return -1
				}
				return ss.Status.ReadyReplicas
			}, clusterReadyTimeout, pollInterval).Should(Equal(int32(0)))

			By("Waiting for StatefulSet to recover the pod")
			Eventually(func() bool {
				ss, err := k8sClient.AppsV1().StatefulSets(testNamespace).Get(ctx, pgName, metav1.GetOptions{})
				if err != nil {
					return false
				}
				return ss.Status.ReadyReplicas == 1
			}, clusterReadyTimeout, pollInterval).Should(BeTrue())

			By("Waiting for instance to return to Ready")
			err = WaitForInstanceReady(ctx, testInstance, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine still responds via gateway")
			Eventually(func() error {
				output, err := RunQueryViaGateway(ctx, clientPod, testInstance, engineName, LightQuery)
				if err != nil {
					return err
				}
				result, err := ParseQueryResult(output)
				if err != nil {
					return err
				}
				if !LightQueryValidator(result) {
					return fmt.Errorf("query result validation failed")
				}
				return nil
			}, clusterReadyTimeout, pollInterval).Should(Succeed())
		})
	})

	Describe("Metadata Crash Recovery", func() {
		It("should recover when metadata pod is deleted", func() {
			mdName := testInstance + controller.SuffixMetadataService

			By("Deleting the metadata pod")
			pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s,%s=metadata", controller.LabelInstance, testInstance, controller.LabelComponent),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).NotTo(BeEmpty())
			err = k8sClient.CoreV1().Pods(testNamespace).Delete(ctx, pods.Items[0].Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash to propagate (ReadyReplicas drops to 0)")
			Eventually(func() int32 {
				dep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, mdName, metav1.GetOptions{})
				if err != nil {
					return -1
				}
				return dep.Status.ReadyReplicas
			}, clusterReadyTimeout, pollInterval).Should(Equal(int32(0)))

			By("Waiting for Deployment to recover the pod")
			Eventually(func() bool {
				dep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, mdName, metav1.GetOptions{})
				if err != nil {
					return false
				}
				return dep.Status.ReadyReplicas == 1
			}, clusterReadyTimeout, pollInterval).Should(BeTrue())

			By("Waiting for instance to return to Ready")
			err = WaitForInstanceReady(ctx, testInstance, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying engine still responds via gateway")
			Eventually(func() error {
				output, err := RunQueryViaGateway(ctx, clientPod, testInstance, engineName, LightQuery)
				if err != nil {
					return err
				}
				result, err := ParseQueryResult(output)
				if err != nil {
					return err
				}
				if !LightQueryValidator(result) {
					return fmt.Errorf("query result validation failed")
				}
				return nil
			}, clusterReadyTimeout, pollInterval).Should(Succeed())
		})
	})

	Describe("Gateway Crash Recovery", func() {
		It("should recover when gateway pod is deleted", func() {
			gwName := testInstance + controller.SuffixGateway

			By("Deleting the gateway pod")
			pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s,%s=gateway", controller.LabelInstance, testInstance, controller.LabelComponent),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).NotTo(BeEmpty())
			err = k8sClient.CoreV1().Pods(testNamespace).Delete(ctx, pods.Items[0].Name, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for crash to propagate (ReadyReplicas drops to 0)")
			Eventually(func() int32 {
				dep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, gwName, metav1.GetOptions{})
				if err != nil {
					return -1
				}
				return dep.Status.ReadyReplicas
			}, clusterReadyTimeout, pollInterval).Should(Equal(int32(0)))

			By("Waiting for Deployment to recover the pod")
			Eventually(func() bool {
				dep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, gwName, metav1.GetOptions{})
				if err != nil {
					return false
				}
				return dep.Status.ReadyReplicas >= 1
			}, clusterReadyTimeout, pollInterval).Should(BeTrue())

			By("Waiting for instance to return to Ready")
			err = WaitForInstanceReady(ctx, testInstance, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying queries still route through gateway")
			Eventually(func() error {
				output, err := RunQueryViaGateway(ctx, clientPod, testInstance, engineName, LightQuery)
				if err != nil {
					return err
				}
				result, err := ParseQueryResult(output)
				if err != nil {
					return err
				}
				if !LightQueryValidator(result) {
					return fmt.Errorf("query result validation failed")
				}
				return nil
			}, clusterReadyTimeout, pollInterval).Should(Succeed())
		})
	})

	Describe("Instance Deletion Cleanup", func() {
		It("should garbage-collect all child resources", func() {
			sepInstance := "test-deletion-cleanup"

			By("Creating a separate instance")
			err := CreateInstance(ctx, sepInstance, pensieveImage, pensieveTag)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for the separate instance to become Ready")
			err = WaitForInstanceReady(ctx, sepInstance, instanceReadyTimeout)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the separate instance")
			err = DeleteInstance(ctx, sepInstance)
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for all child resources to be garbage-collected")
			selector := fmt.Sprintf("%s=%s", controller.LabelInstance, sepInstance)
			Eventually(func() int {
				total := 0
				if deps, err := k8sClient.AppsV1().Deployments(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector}); err == nil {
					total += len(deps.Items)
				}
				if sss, err := k8sClient.AppsV1().StatefulSets(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector}); err == nil {
					total += len(sss.Items)
				}
				if svcs, err := k8sClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector}); err == nil {
					total += len(svcs.Items)
				}
				if cms, err := k8sClient.CoreV1().ConfigMaps(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector}); err == nil {
					total += len(cms.Items)
				}
				if pdbs, err := k8sClient.PolicyV1().PodDisruptionBudgets(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector}); err == nil {
					total += len(pdbs.Items)
				}
				return total
			}, resourceCleanupTimeout, pollInterval).Should(Equal(0), "All child resources should be deleted")
		})
	})

	Describe("Config Drift Reconciliation", func() {
		It("should revert manual gateway ConfigMap changes", func() {
			gwConfigName := testInstance + controller.SuffixGateway + "-config"

			By("Reading the original ConfigMap data")
			original, err := k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, gwConfigName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			originalData := make(map[string]string)
			for k, v := range original.Data {
				originalData[k] = v
			}

			By("Patching the ConfigMap with invalid data")
			patched := original.DeepCopy()
			patched.Data["envoy.yaml"] = "corrupted: true\n"
			_, err = k8sClient.CoreV1().ConfigMaps(testNamespace).Update(ctx, patched, metav1.UpdateOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Waiting for the operator to reconcile the ConfigMap back")
			Eventually(func() string {
				cm, err := k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, gwConfigName, metav1.GetOptions{})
				if err != nil {
					return ""
				}
				return cm.Data["envoy.yaml"]
			}, 60*time.Second, pollInterval).Should(Equal(originalData["envoy.yaml"]),
				"Operator should revert the ConfigMap to its expected state")
		})
	})
})
