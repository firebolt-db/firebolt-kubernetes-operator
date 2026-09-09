//go:build e2e

// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
)

func gatewayRoutingState(ctx context.Context, instance string) (routing.State, error) {
	cm, err := k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, routing.ConfigMapName(instance), metav1.GetOptions{})
	if err != nil {
		return routing.State{}, err
	}
	return routing.Decode([]byte(cm.Data[routing.DataKey]))
}

func gatewayRoutingPods(ctx context.Context, instance string) ([]corev1.Pod, error) {
	pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: controller.LabelInstance + "=" + instance + "," + controller.LabelComponent + "=gateway",
	})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// Reports are read through the API-server proxy because Kind Pod addresses are
// not necessarily reachable from the process running the E2E suite.
func gatewayRoutingReport(ctx context.Context, pod *corev1.Pod) (routing.Report, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, err := k8sClient.CoreV1().RESTClient().Get().Namespace(testNamespace).
		Resource("pods").Name(pod.Name + ":9903").SubResource("proxy").Suffix("routing").DoRaw(ctx)
	if err != nil {
		return routing.Report{}, err
	}
	var report routing.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return routing.Report{}, err
	}
	if report.PodUID != string(pod.UID) || report.SessionID == "" || report.Outstanding == nil {
		return routing.Report{}, fmt.Errorf("invalid routing report from Pod %s", pod.Name)
	}
	return report, nil
}

type routingQueryResult struct {
	body string
	err  error
}

func startGatewayRoutingSleep(ctx context.Context, clientPod, instance, engine string, seconds int) <-chan routingQueryResult {
	result := make(chan routingQueryResult, 1)
	go func() {
		body, err := execCurlQueryWithDeadline(ctx, clientPod,
			gatewayLoadQueryURL(instance, engine)+"&enable_internal_functions=true",
			fmt.Sprintf("SELECT sleep(%d)", seconds), seconds+15)
		result <- routingQueryResult{body: body, err: err}
	}()
	return result
}

func requireRoutingSleepSuccess(result routingQueryResult) {
	GinkgoHelper()
	Expect(result.err).NotTo(HaveOccurred(), "gateway-admitted query was interrupted")
	ExpectSleepResult(result.body)
}

// ExpectSleepResult asserts that a `SELECT sleep(n)` body carries the
// function's completion value. Engine builds differ on its type: some return
// the integer 0, others the boolean false. Either proves the query ran to
// completion, which is all these specs need from it.
func ExpectSleepResult(body string) {
	GinkgoHelper()
	value, err := ParseQueryResult(body)
	Expect(err).NotTo(HaveOccurred())
	Expect(value).To(SatisfyAny(BeNumerically("==", 0), BeFalse()),
		"sleep() should return 0 or false, got %T %v", value, value)
}

var _ = Describe("Gateway routing withdrawal", Ordered, func() {
	var (
		instanceName = "inst-routing" + queryConfig.Suffix
		engineName   = "routing" + queryConfig.Suffix + "-engine"
		clientPod    = "client-routing" + queryConfig.Suffix
		lc           *TestInstanceLifecycle
	)
	RegisterFailedSpecPodLogDump(&instanceName, &engineName)

	BeforeAll(func() {
		var err error
		lc, err = SetupTestInstance(ctx, instanceName)
		Expect(err).NotTo(HaveOccurred())
		Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		// The ordinary helper disables metric drain checks. This test must
		// exercise the routing gate independently of a positive query scrape.
		Expect(CreateEngine(ctx, instanceName, engineName, 1)).To(Succeed())
		Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
		Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())
	})

	AfterAll(func() {
		controller.ClearCrashPointsForEngine(engineName)
		Expect(DeleteEngine(ctx, engineName)).To(Succeed())
		Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		DeleteClientPod(ctx, clientPod)
		TeardownTestInstance(ctx, lc)
	})

	It("retains old admissions across reconciler restart and gateway replacement", func() {
		engine, err := GetEngine(ctx, engineName)
		Expect(err).NotTo(HaveOccurred())
		Expect(engine.Spec.DrainCheckEnabled).NotTo(BeNil())
		Expect(*engine.Spec.DrainCheckEnabled).To(BeFalse(), "metric drain must not mask a missing routing fence")

		By("Waiting for two distinct, durably registered Gateway sessions")
		var initial routing.State
		var originalPods []corev1.Pod
		Eventually(func(g Gomega) {
			initial, err = gatewayRoutingState(ctx, instanceName)
			g.Expect(err).NotTo(HaveOccurred())
			originalPods, err = gatewayRoutingPods(ctx, instanceName)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(originalPods).To(HaveLen(2))
			g.Expect(initial.Sessions).To(HaveLen(2))
			g.Expect(initial.Routes[engineName].Enabled).To(BeTrue())
			for i := range originalPods {
				pod := &originalPods[i]
				report, reportErr := gatewayRoutingReport(ctx, pod)
				g.Expect(reportErr).NotTo(HaveOccurred())
				g.Expect(report.Registered).To(BeTrue())
				g.Expect(report.SessionID).To(Equal(initial.Sessions[string(pod.UID)].ID))
			}
		}, 15*time.Second, time.Second).Should(Succeed())
		Expect(initial.Sessions[string(originalPods[0].UID)].ID).NotTo(Equal(initial.Sessions[string(originalPods[1].UID)].ID))
		oldRoute := initial.Routes[engineName]
		oldPods, err := EnginePodsForGeneration(ctx, engineName, oldRoute.Generation)
		Expect(err).NotTo(HaveOccurred())
		Expect(oldPods).To(HaveLen(1))
		oldPod := oldPods[0]

		By("Staging the ready replacement before publishing its route")
		staged := make(chan struct{})
		resume := controller.SetCrashPoint(engineName, controller.CrashBeforeCreatingToSwitching, func() { close(staged) })
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(resume) }) }
		DeferCleanup(release)
		Expect(UpdateEngineScheduling(ctx, engineName, nil, []corev1.Toleration{{
			Key: "firebolt.io/e2e-routing-withdrawal", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
		}}, nil)).To(Succeed())
		Eventually(staged, rolloutToDrainingTimeout).Should(BeClosed())

		By("Admitting a long query and identifying the Gateway that owns its permit")
		longQuery := startGatewayRoutingSleep(ctx, clientPod, instanceName, engineName, 45)
		var holder corev1.Pod
		var holderSession string
		Eventually(func(g Gomega) {
			var count uint64
			for i := range originalPods {
				pod := &originalPods[i]
				report, reportErr := gatewayRoutingReport(ctx, pod)
				g.Expect(reportErr).NotTo(HaveOccurred())
				active := report.Outstanding[oldRoute.Key()].Active
				count += active
				if active != 0 {
					holder, holderSession = *pod, report.SessionID
				}
			}
			g.Expect(count).To(Equal(uint64(1)))
		}, 10*time.Second, 200*time.Millisecond).Should(Succeed())
		waitForLoadInFlight(ctx, clientPod, oldPod.Status.PodIP)
		release()

		By("Observing the closed epoch with a live old permit")
		var replacementRoute routing.Route
		Eventually(func(g Gomega) {
			state, stateErr := gatewayRoutingState(ctx, instanceName)
			g.Expect(stateErr).NotTo(HaveOccurred())
			replacementRoute = state.Routes[engineName]
			g.Expect(replacementRoute.Generation).NotTo(Equal(oldRoute.Generation))
			retirement, exists := state.Retirements[oldRoute.Key()]
			g.Expect(exists).To(BeTrue())
			g.Expect(retirement.Holders).To(HaveLen(2))
			g.Expect(retirement.Holders[string(holder.UID)]).To(Equal(holderSession))
			report, reportErr := gatewayRoutingReport(ctx, &holder)
			g.Expect(reportErr).NotTo(HaveOccurred())
			g.Expect(report.AppliedRevision).To(BeNumerically(">=", retirement.RequiredRevision))
			g.Expect(report.Outstanding[oldRoute.Key()].Active).To(Equal(uint64(1)))
			pod, podErr := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, oldPod.Name, metav1.GetOptions{})
			g.Expect(podErr).NotTo(HaveOccurred())
			g.Expect(pod.DeletionTimestamp).To(BeNil())
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())

		By("Restarting the engine reconciler while withdrawal remains pending")
		lc.EngineOp.Stop()
		newQuery := startGatewayRoutingSleep(ctx, clientPod, instanceName, engineName, 6)
		Eventually(func(g Gomega) {
			var count uint64
			for i := range originalPods {
				pod := &originalPods[i]
				report, reportErr := gatewayRoutingReport(ctx, pod)
				g.Expect(reportErr).NotTo(HaveOccurred())
				count += report.Outstanding[replacementRoute.Key()].Active
			}
			g.Expect(count).To(Equal(uint64(1)), "new requests must be admitted on the replacement epoch")
			pods, podsErr := EnginePodsForGeneration(ctx, engineName, replacementRoute.Generation)
			g.Expect(podsErr).NotTo(HaveOccurred())
			g.Expect(pods).To(HaveLen(1))
			metrics, metricsErr := scrapeEnginePodMetrics(ctx, clientPod, pods[0].Status.PodIP)
			g.Expect(metricsErr).NotTo(HaveOccurred())
			active, present := parseActiveQueries(metrics)
			g.Expect(present).To(BeTrue())
			g.Expect(active).To(BeNumerically(">", 0), "the query must reach the replacement Engine Pod")
		}, 5*time.Second, 200*time.Millisecond).Should(Succeed())
		lc.EngineOp, err = StartOperator(instanceName)
		Expect(err).NotTo(HaveOccurred())
		var result routingQueryResult
		Eventually(newQuery, 10*time.Second).Should(Receive(&result))
		requireRoutingSleepSuccess(result)

		By("Replacing the Gateway that still owns the old-generation request")
		Expect(k8sClient.CoreV1().Pods(testNamespace).Delete(ctx, holder.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &holder.UID},
		})).To(Succeed())
		Eventually(func(g Gomega) {
			state, stateErr := gatewayRoutingState(ctx, instanceName)
			g.Expect(stateErr).NotTo(HaveOccurred())
			g.Expect(state.Sessions[string(holder.UID)].ID).To(Equal(holderSession), "replacement must not erase old liability")
			g.Expect(state.Retirements[oldRoute.Key()].Holders[string(holder.UID)]).To(Equal(holderSession))
			terminating, podErr := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, holder.Name, metav1.GetOptions{})
			g.Expect(podErr).NotTo(HaveOccurred())
			g.Expect(terminating.DeletionTimestamp).NotTo(BeNil())
			g.Expect(terminating.Finalizers).To(ContainElement("firebolt.io/gateway-routing"))
			report, reportErr := gatewayRoutingReport(ctx, terminating)
			g.Expect(reportErr).NotTo(HaveOccurred())
			g.Expect(report.Outstanding[oldRoute.Key()].Active).To(Equal(uint64(1)))
			pods, podsErr := gatewayRoutingPods(ctx, instanceName)
			g.Expect(podsErr).NotTo(HaveOccurred())
			var freshSession bool
			for i := range pods {
				pod := &pods[i]
				if _, original := initial.Sessions[string(pod.UID)]; original {
					continue
				}
				fresh, freshErr := gatewayRoutingReport(ctx, pod)
				if freshErr == nil && fresh.Registered && state.Sessions[string(pod.UID)].ID == fresh.SessionID {
					g.Expect(fresh.SessionID).NotTo(Equal(holderSession))
					g.Expect(fresh.Outstanding[oldRoute.Key()]).To(Equal(routing.Count{}))
					_, captured := state.Retirements[oldRoute.Key()].Holders[string(pod.UID)]
					g.Expect(captured).To(BeFalse(), "new session cannot join a closed epoch")
					freshSession = true
				}
			}
			g.Expect(freshSession).To(BeTrue(), "replacement Gateway must register its own Pod UID and session")
			retained, retainedErr := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, oldPod.Name, metav1.GetOptions{})
			g.Expect(retainedErr).NotTo(HaveOccurred())
			g.Expect(retained.DeletionTimestamp).To(BeNil(), "old Engine must remain alive without metric drain checks")
		}, 15*time.Second, 200*time.Millisecond).Should(Succeed())
		_, err = RunQueryViaGateway(ctx, clientPod, instanceName, engineName, "SELECT 1")
		Expect(err).NotTo(HaveOccurred())

		By("Completing accepted work and retiring the old generation and session")
		Eventually(longQuery, 60*time.Second).Should(Receive(&result))
		requireRoutingSleepSuccess(result)
		Eventually(func(g Gomega) {
			pods, podsErr := EnginePodsForGeneration(ctx, engineName, oldRoute.Generation)
			g.Expect(podsErr).NotTo(HaveOccurred())
			g.Expect(pods).To(BeEmpty())
			state, stateErr := gatewayRoutingState(ctx, instanceName)
			g.Expect(stateErr).NotTo(HaveOccurred())
			_, exists := state.Sessions[string(holder.UID)]
			g.Expect(exists).To(BeFalse())
			_, pending := state.Retirements[oldRoute.Key()]
			g.Expect(pending).To(BeFalse(), "completed retirement should be reclaimed")
			g.Expect(state.Routes[engineName].Generation).To(Equal(replacementRoute.Generation))
		}, 30*time.Second, time.Second).Should(Succeed())
		Expect(WaitForEngineStable(ctx, engineName, 15*time.Second)).To(Succeed())
	})
})
