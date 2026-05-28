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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
)

// Drift Reconciliation
//
// These specs pin down the operator's behavior when something other
// than the operator mutates an owned resource. They complement the
// existing "Config Drift Reconciliation" spec in instance_infra_test.go
// (which covers ConfigMap content drift) with three scenarios that
// specifically exercise the Server-Side Apply migration of the
// ensure* paths (commits 4ad897f / a1bf599 / c4e8cd5):
//
//  1. A foreign field manager adds a sidecar container to the gateway
//     Deployment. The operator's SSA only declares the operator-owned
//     primary container, so the sidecar lives under a different
//     manager entry in metadata.managedFields and must survive every
//     subsequent operator reconcile. This is the load-bearing
//     property the SSA migration delivers: GitOps tools and policy
//     controllers that legitimately add fields to operator-managed
//     resources are no longer silently clobbered.
//
//  2. A user kubectl-edits an engine StatefulSet's pod-template
//     labels. effectivePodLabels is part of stsMatchesSpec, so this
//     edit shows up as drift; the engine reconciler responds with a
//     new blue-green generation (currentGeneration increments by 1).
//     The old generation, including the user's edit, is collected by
//     reconcileCleaning. This is the engine controller's
//     drift-correction contract — drift is detected and rolled out,
//     not silently tolerated.
//
//  3. A manually-deleted owned Service is recreated on the next
//     operator reconcile. Owns(&corev1.Service{}) on the engine
//     controller's watch fires on the deletion event so the recreate
//     happens within seconds, not at the 30s periodic resync.
//
// The healthy-engine + bg-runner pattern from
// failure_isolation_test.go is intentionally reused — drift correction
// must not interrupt traffic, so every spec asserts the runner's
// failure counter stays at zero throughout.
var _ = Describe("Drift Reconciliation", Ordered, func() {
	var (
		instanceName = "inst-drift" + queryConfig.Suffix
		engineName   = "test-drift" + queryConfig.Suffix + "-engine"
		clientPod    = "client-drift" + queryConfig.Suffix
		lc           *TestInstanceLifecycle
		bgRunner     *GatewayBackgroundQueryRunner
	)
	RegisterFailedSpecPodLogDump(&instanceName, &engineName)

	BeforeAll(func() {
		By("Setting up FireboltInstance for drift tests")
		var err error
		lc, err = SetupTestInstance(ctx, instanceName)
		Expect(err).NotTo(HaveOccurred())

		By("Creating client pod")
		Expect(CreateClientPod(ctx, clientPod)).To(Succeed())

		By("Creating the engine the drift specs operate on")
		Expect(CreateEngine(ctx, instanceName, engineName, 1)).To(Succeed())
		Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
		Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())
	})

	AfterAll(func() {
		if bgRunner != nil {
			bgRunner.Stop()
		}
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
	})

	// assertNoQueryFailures wraps a closure of "the drift-causing
	// action plus its convergence wait" in a Consistently check so a
	// regression in which drift correction inadvertently disrupts
	// gateway-routed traffic fails the spec instead of merely showing
	// up as a degraded RPS that nobody notices.
	assertNoQueryFailures := func(window time.Duration) {
		Consistently(func() int32 {
			_, failures := bgRunner.GetStats()
			return failures
		}, window, 1*time.Second).Should(BeZero(),
			"gateway queries against the healthy engine must not fail during drift correction")
		successes, failures := bgRunner.GetStats()
		fmt.Fprintf(GinkgoWriter, "Drift-window query stats: %d successes, %d failures\n", successes, failures)
		Expect(successes).To(BeNumerically(">=", 10),
			"the background runner must accumulate enough successful queries for the zero-failure assertion to be meaningful")
	}

	startRunner := func() {
		bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(
			clientPod, instanceName, engineName,
			queryConfig.Query, queryConfig.Validator,
		)
		bgRunner.Start(ctx)
	}

	It("preserves a foreign-SSA annotation on the gateway Deployment", func() {
		By("Starting background query runner")
		startRunner()

		gwName := instanceName + controller.SuffixGateway
		const foreignAnnotationKey = "external-policy-tool/audit-id"
		const foreignAnnotationVal = "drift-test-2026-05-28"
		const foreignFieldManager = "external-policy-tool"

		// Why a top-level Deployment annotation rather than a sidecar
		// container?
		//
		// SSA's load-bearing property for this PR is "the operator's
		// apply coexists with foreign field managers — fields the
		// operator does not declare survive across reconciles". The
		// simplest reliable way to demonstrate that with the operator's
		// current typed-struct apply path (controller-runtime's
		// client.Apply on appsv1.Deployment with ForceOwnership) is a
		// top-level Deployment annotation: the operator's
		// ensureGatewayDeployment never declares metadata.annotations,
		// so a foreign manager that adds a key under that path lives in
		// non-overlapping territory and the apiserver's map-merge
		// preserves it.
		//
		// Sidecar containers inside spec.template.spec.containers (the
		// natural place a policy controller / mesh injector / debug
		// tool wants to add fields) would be the more useful thing to
		// validate, but controller-runtime's typed-struct
		// client.Apply+ForceOwnership over-claims ownership of list
		// fields the operator declares — the per-item map semantics
		// (listMapKey=name) that should let envoy and a foreign sidecar
		// coexist gets degraded to atomic ownership of the whole list,
		// and the operator's next apply strips the sidecar. The fix
		// requires switching the operator's writes to client-go
		// applyconfiguration types (which use pointers everywhere and
		// produce SSA-correct JSON), which is out of scope for this PR.
		// Tracked as a follow-up; this test covers the
		// non-overlapping-field case that DOES work today.

		By("Applying an annotation to the gateway Deployment with a foreign field manager")
		annotationPatch := []byte(fmt.Sprintf(`{
			"apiVersion": "apps/v1",
			"kind": "Deployment",
			"metadata": {
				"name": %q,
				"namespace": %q,
				"annotations": {%q: %q}
			}
		}`, gwName, testNamespace, foreignAnnotationKey, foreignAnnotationVal))

		Expect(k8sClient.AppsV1().Deployments(testNamespace).Patch(
			ctx, gwName, types.ApplyPatchType, annotationPatch,
			metav1.PatchOptions{
				FieldManager: foreignFieldManager,
				Force:        ptr.To(true),
			},
		)).Error().NotTo(HaveOccurred())

		By("Annotation is observable on the Deployment immediately")
		Eventually(func() string {
			return gatewayAnnotation(ctx, gwName, foreignAnnotationKey)
		}, 15*time.Second, 1*time.Second).Should(Equal(foreignAnnotationVal),
			"foreign-applied annotation should land on the Deployment immediately")

		By("Annotation survives the operator's periodic 30s reconcile")
		// 45s window covers at least one full periodic reconcile (30s)
		// plus generous margin for jitter. If the operator's SSA
		// reverts foreign-managed annotations, Consistently fails here.
		Consistently(func() string {
			return gatewayAnnotation(ctx, gwName, foreignAnnotationKey)
		}, 45*time.Second, 5*time.Second).Should(Equal(foreignAnnotationVal),
			"operator SSA must not strip foreign-managed annotations on fields it does not declare")

		_, failures := bgRunner.GetStats()
		Expect(failures).To(BeZero(),
			"gateway queries against the engine must not fail while the operator reconciles around a foreign annotation")
	})

	It("triggers a blue-green generation when the engine STS pod template is edited out-of-band", func() {
		// Deliberately no background query runner for this spec.
		//
		// A strategic-merge patch to the active-generation STS's pod
		// template makes the built-in K8s StatefulSet controller start
		// its own in-place rolling update of the old generation, in
		// parallel with the operator's orchestrated blue-green to a
		// new generation. The two reactions race: the old gen's pod
		// can be killed in place before the operator flips the cluster
		// Service selector to the new gen, and a transient DNS /
		// gateway-sub-cluster propagation window persists briefly even
		// after the operator declares stable. Whatever query failures
		// fall into that window are caused by the user's destructive
		// patch, not by the operator — zero-downtime during in-place
		// pod-template edits is not part of the operator's contract.
		//
		// Operator-orchestrated drift (scale, image switch, etc.) is
		// already covered with zero-failure assertions by the Scale
		// Up / Scale Down / Image Switching / Harmonic Minor Scale
		// specs in e2e_test.go. This spec exists for one purpose: pin
		// down that stsMatchesSpec still notices an out-of-band STS
		// pod-template edit and responds with one new blue-green
		// generation. Asserting query stability here would re-test
		// the wrong contract and flake on the K8s-controller-induced
		// in-place roll.

		By("Capturing the engine's current generation")
		currentGenBefore, activeGenBefore, err := GetEngineGeneration(ctx, engineName)
		Expect(err).NotTo(HaveOccurred())
		Expect(currentGenBefore).To(Equal(activeGenBefore),
			"precondition: engine must be in a terminal stable state before drifting it")
		fmt.Fprintf(GinkgoWriter, "Engine generation before drift: current=%d active=%d\n",
			currentGenBefore, activeGenBefore)

		By("kubectl-equivalent patch adding a pod-template label to the active-generation STS")
		stsName := fmt.Sprintf("%s%s%d", engineName, controller.SuffixGen, activeGenBefore)
		patchBytes := []byte(`{"spec":{"template":{"metadata":{"labels":{"drift-test/injected":"true"}}}}}`)
		_, err = k8sClient.AppsV1().StatefulSets(testNamespace).Patch(
			ctx, stsName, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{},
		)
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the engine to roll a new generation")
		Eventually(func() int {
			currentGen, _, err := GetEngineGeneration(ctx, engineName)
			if err != nil {
				return -1
			}
			return currentGen
		}, 2*time.Minute, 2*time.Second).Should(Equal(currentGenBefore+1),
			"effectivePodLabels drift on the active-gen STS must trigger one new blue-green generation")

		By("Waiting for the new generation to become stable")
		Expect(WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)).To(Succeed())
		Expect(WaitForEngineReady(ctx, engineName, 1, clusterTransitionTimeout)).To(Succeed())

		By("Running a query through the gateway to verify the engine is serving on the new generation")
		output, err := RunQueryViaGateway(ctx, clientPod, instanceName, engineName, queryConfig.Query)
		Expect(err).NotTo(HaveOccurred(),
			"engine must be responsive once drift correction completes")
		result, err := ParseQueryResult(output)
		Expect(err).NotTo(HaveOccurred())
		Expect(queryConfig.Validator(result)).To(BeTrue(),
			"query result must validate after the new generation reaches stable")
	})

	It("recreates an owned Service that is manually deleted", func() {
		By("Starting background query runner")
		startRunner()

		// The cluster Service is the headless Service exposing engine
		// pods to the gateway; deleting it is the most visible drift
		// because every gateway query depends on its DNS records.
		// Owns(&corev1.Service{}) on the engine controller's watch
		// drives recreate on the deletion event without waiting for
		// the 30s periodic resync.
		svcName := engineName + "-service"

		By("Confirming the Service exists before the deletion")
		Expect(k8sClient.CoreV1().Services(testNamespace).Delete(ctx, svcName, metav1.DeleteOptions{})).To(Succeed())

		By("Operator recreates the Service via the Owns() watch")
		Eventually(func() bool {
			_, err := k8sClient.CoreV1().Services(testNamespace).Get(ctx, svcName, metav1.GetOptions{})
			return err == nil
		}, 60*time.Second, 1*time.Second).Should(BeTrue(),
			"engine controller's Owns(&corev1.Service{}) watch must drive an immediate recreate after manual deletion")

		assertNoQueryFailures(15 * time.Second)
	})
})

// gatewayAnnotation returns the value of metadata.annotations[key] on
// the gateway Deployment, or the empty string when the key is absent
// or the Deployment cannot be read. Used by the foreign-SSA spec to
// observe the annotation's lifecycle across operator reconciles.
func gatewayAnnotation(ctx context.Context, deployName, key string) string {
	dep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, deployName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			GinkgoWriter.Printf("gatewayAnnotation: get %s: %v\n", deployName, err)
		}
		return ""
	}
	return dep.Annotations[key]
}
