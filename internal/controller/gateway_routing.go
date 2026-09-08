// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const gatewayRoutingFinalizer = "firebolt.io/gateway-routing"

var errGatewayWithdrawalPending = errors.New("gateway withdrawal pending")

// A missing or unreachable session is a pending withdrawal, never an empty one.
var gatewayReportClient = &http.Client{Timeout: 2 * time.Second}

func readRoutingState(ctx context.Context, reader client.Reader, namespace, instance string) (*corev1.ConfigMap, routing.State, error) {
	cm := &corev1.ConfigMap{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: routing.ConfigMapName(instance)}, cm); err != nil {
		return nil, routing.State{}, err
	}
	state, err := routing.Decode([]byte(cm.Data[routing.DataKey]))
	return cm, state, err
}

func mutateRoutingState(ctx context.Context, c client.Client, reader client.Reader, namespace, instance string, mutate func(*routing.State) (bool, error)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, state, err := readRoutingState(ctx, reader, namespace, instance)
		if err != nil {
			return err
		}
		changed, err := mutate(&state)
		if err != nil || !changed {
			return err
		}
		data, err := routing.Encode(state)
		if err != nil {
			return err
		}
		cm.Data[routing.DataKey] = string(data)
		return c.Update(ctx, cm)
	})
}

func (r *FireboltInstanceReconciler) routingReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *FireboltInstanceReconciler) ensureGatewayRouting(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	current := &computev1alpha1.FireboltInstance{}
	if err := r.routingReader().Get(ctx, client.ObjectKeyFromObject(instance), current); err != nil {
		return err
	}
	if current.UID != instance.UID {
		return errors.New("instance UID changed before routing initialization")
	}
	if err := r.cleanupOrphanGatewayRouting(ctx, client.ObjectKeyFromObject(instance)); err != nil {
		return err
	}
	cm, state, err := readRoutingState(ctx, r.routingReader(), instance.Namespace, instance.Name)
	if apierrors.IsNotFound(err) {
		// Initialization must precede every gateway. Recreating lost coordination
		// while a gateway can still hold cached authority would erase its liability.
		pods := &corev1.PodList{}
		if err := r.routingReader().List(ctx, pods, client.InNamespace(instance.Namespace), client.MatchingLabels(instanceLabels(instance.Name, "gateway"))); err != nil {
			return err
		}
		if len(pods.Items) != 0 {
			return errors.New("routing state missing while gateway Pods exist")
		}
		data, err := routing.Encode(routing.NewState(string(instance.UID)))
		if err != nil {
			return err
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: routing.ConfigMapName(instance.Name), Namespace: instance.Namespace, Labels: instanceLabels(instance.Name, "routing"), Finalizers: []string{gatewayRoutingStateFinalizer}}, Data: map[string]string{routing.DataKey: string(data)}}
		if err := controllerutil.SetControllerReference(instance, cm, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if err != nil {
		return err
	}
	if err == nil && state.InstanceUID != string(instance.UID) {
		return errors.New("routing state belongs to another Instance UID")
	}
	if err == nil && controllerutil.AddFinalizer(cm, gatewayRoutingStateFinalizer) {
		if err := r.Update(ctx, cm); err != nil {
			return err
		}
	}
	if err := r.reclaimStoppedGatewaySessions(ctx, instance); err != nil {
		return err
	}
	if err := r.registerGatewaySessions(ctx, instance); err != nil {
		return err
	}
	return r.reclaimDeletedEngineRoutes(ctx, instance)
}

// A container termination status can describe a crash before an unreported
// restart. Require completed Pod teardown too, and reject synthetic status from
// a lost node: the control plane cannot establish process death there.
func gatewayEnvoyStopped(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp.IsZero() || (pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) || pod.Status.Reason == "NodeLost" {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.DisruptionTarget && condition.Status == corev1.ConditionTrue && condition.Reason == "DeletionByPodGC" {
			return false
		}
	}
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name == computev1alpha1.GatewayContainerName {
			stopped := status.State.Terminated
			return stopped != nil && stopped.Reason != "ContainerStatusUnknown" && stopped.Reason != "NodeLost" && !stopped.FinishedAt.IsZero()
		}
	}
	return false
}

func fetchGatewayReport(ctx context.Context, pod *corev1.Pod) (routing.Report, error) {
	var report routing.Report
	if pod.Status.PodIP == "" {
		return report, errors.New("gateway has no Pod IP")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(gatewayWakeAgentDemandPort)))+"/routing", http.NoBody)
	if err != nil {
		return report, err
	}
	resp, err := gatewayReportClient.Do(req)
	if err != nil {
		return report, err
	}
	defer func() {
		_ = resp.Body.Close() /* The bounded read has finished; closing releases the HTTP connection. */
	}()
	if resp.StatusCode != http.StatusOK {
		return report, fmt.Errorf("gateway report status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&report); err != nil {
		return report, err
	}
	if report.PodUID != string(pod.UID) || report.SessionID == "" || report.Outstanding == nil {
		return report, errors.New("invalid gateway session report")
	}
	return report, nil
}

func (r *FireboltInstanceReconciler) registerGatewaySessions(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	_, state, err := readRoutingState(ctx, r.routingReader(), instance.Namespace, instance.Name)
	if err != nil {
		return err
	}
	pods := &corev1.PodList{}
	if err := r.routingReader().List(ctx, pods, client.InNamespace(instance.Namespace), client.MatchingLabels(instanceLabels(instance.Name, "gateway"))); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if _, registered := state.Sessions[string(pod.UID)]; !registered {
			// A native agent can start even when Envoy's image cannot. Such a
			// Pod has no routing authority, and kubelet cannot provide a real
			// Envoy termination timestamp when deleting it. Wait for the process
			// before taking the evidence finalizer or registering its agent.
			running := false
			for i := range pod.Status.ContainerStatuses {
				status := &pod.Status.ContainerStatuses[i]
				if status.Name == computev1alpha1.GatewayContainerName && status.State.Running != nil {
					running = true
				}
			}
			if !running {
				continue
			}
		}
		// Persist the finalizer before registration grants any routing authority.
		if !controllerutil.ContainsFinalizer(pod, gatewayRoutingFinalizer) {
			controllerutil.AddFinalizer(pod, gatewayRoutingFinalizer)
			if err := r.Update(ctx, pod); err != nil {
				return err
			}
		}
		report, err := fetchRoutingReport(ctx, r.routingReader(), r.Clientset, pod)
		if err != nil {
			continue
		}
		replace := false
		if err := mutateRoutingState(ctx, r.Client, r.routingReader(), instance.Namespace, instance.Name, func(state *routing.State) (bool, error) {
			if session, ok := state.Sessions[string(pod.UID)]; ok && session.ID != report.SessionID {
				replace = true
				return false, nil
			}
			return routing.RegisterSession(state, string(pod.UID), report.SessionID)
		}); err != nil {
			return err
		}
		for _, count := range report.Outstanding {
			if count.Unknown > 0 {
				replace = true
			}
		}
		if replace {
			if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func collectGatewayReports(ctx context.Context, reader client.Reader, clientset *kubernetes.Clientset, namespace, instance string) (map[string]routing.Report, map[string]bool, error) {
	pods := &corev1.PodList{}
	if err := reader.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels(instanceLabels(instance, "gateway"))); err != nil {
		return nil, nil, err
	}
	reports := map[string]routing.Report{}
	stopped := map[string]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if gatewayEnvoyStopped(pod) {
			stopped[string(pod.UID)] = true
			continue
		}
		report, err := fetchRoutingReport(ctx, reader, clientset, pod)
		if err == nil {
			reports[string(pod.UID)] = report
		}
	}
	return reports, stopped, nil
}

// authorizeGenerationRemoval protects all callers of deletion, including GC.
// The authority record is read uncached immediately before the destructive call.
func (r *FireboltEngineReconciler) authorizeGenerationRemoval(ctx context.Context, engine *computev1alpha1.FireboltEngine, generation int) error {
	_, state, err := readRoutingState(ctx, r.sweepReader(), engine.Namespace, engine.Spec.InstanceRef)
	if err != nil {
		return fmt.Errorf("read gateway withdrawal state: %w", err)
	}
	// The retained record outlives Instance deletion until all independently
	// managed Engines are gone. Its closed proof also covers a replacement
	// Instance that is waiting for this record to be reclaimed.
	if closedGatewayRoutingProof(state) == nil {
		return nil
	}
	instance := &computev1alpha1.FireboltInstance{}
	if err := r.sweepReader().Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Spec.InstanceRef}, instance); err != nil {
		return err
	}
	if state.InstanceUID != string(instance.UID) {
		return errors.New("routing Instance UID mismatch")
	}
	for _, route := range state.Routes {
		if route.EngineUID == string(engine.UID) && route.Generation == generation && route.Enabled {
			return fmt.Errorf("%w: generation %d still has routing authority", errGatewayWithdrawalPending, generation)
		}
	}
	reports, stopped, err := collectGatewayReports(ctx, r.sweepReader(), r.Clientset, engine.Namespace, engine.Spec.InstanceRef)
	if err != nil {
		return err
	}
	for _, retirement := range state.Retirements {
		if retirement.Route.EngineUID == string(engine.UID) && retirement.Route.Generation == generation && !routing.CanRetire(retirement, reports, stopped) {
			return fmt.Errorf("%w: generation %d has outstanding gateway holders", errGatewayWithdrawalPending, generation)
		}
	}
	return nil
}

func (r *FireboltEngineReconciler) publishEngineRoute(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	gen := engine.Status.ActiveGeneration
	if engine.Status.Phase == computev1alpha1.PhaseSwitching {
		gen = engine.Status.CurrentGeneration
	} else if engine.Status.Phase != computev1alpha1.PhaseStable && engine.Status.Phase != computev1alpha1.PhaseStopped {
		return nil
	}
	if gen < 0 {
		return nil
	}
	live := &computev1alpha1.FireboltEngine{}
	if err := r.sweepReader().Get(ctx, client.ObjectKeyFromObject(engine), live); err != nil {
		return err
	}
	if live.UID != engine.UID || live.ResourceVersion != engine.ResourceVersion || !live.DeletionTimestamp.IsZero() {
		return errors.New("engine changed before routing publication")
	}
	instance := &computev1alpha1.FireboltInstance{}
	if err := r.sweepReader().Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: engine.Spec.InstanceRef}, instance); err != nil {
		return err
	}
	if !instance.DeletionTimestamp.IsZero() {
		return errors.New("instance is terminating")
	}

	sts := &appsv1.StatefulSet{}
	if err := r.sweepReader().Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: genResourceName(engine.Name, gen, "")}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		} // Preserve missing-StatefulSet recovery without inventing a route.
		return err
	}
	// A recreated Engine can reuse generation zero's resource name. Foreground
	// StatefulSet deletion retains the predecessor until its Pods are gone;
	// never give that retiring generation the replacement Engine's authority.
	if !sts.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(sts, engine) {
		return fmt.Errorf("%w: generation %d StatefulSet is terminating or belongs to another Engine", errGatewayWithdrawalPending, gen)
	}
	enabled := sts.Spec.Replicas == nil || *sts.Spec.Replicas > 0
	svc := buildClusterService(engine.Name, engine.Namespace, gen)
	svc.Name = routing.GenerationServiceName(string(engine.UID), gen)
	svc.Labels[LabelGeneration] = strconv.Itoa(gen)
	if err := r.ensureService(ctx, engine, svc); err != nil {
		return err
	}
	route := routing.Route{EngineUID: string(engine.UID), Generation: gen, Enabled: enabled, Authority: fmt.Sprintf("%s.%s.svc.cluster.local:%d", svc.Name, engine.Namespace, EngineHTTPQueryPort)}
	return mutateRoutingState(ctx, r.Client, r.sweepReader(), engine.Namespace, engine.Spec.InstanceRef, func(state *routing.State) (bool, error) {
		if state.InstanceUID != string(instance.UID) {
			return false, errors.New("routing Instance UID mismatch")
		}
		return routing.SetRoute(state, engine.Name, route)
	})
}

func (r *FireboltEngineReconciler) withdrawEngineRoute(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	return mutateRoutingState(ctx, r.Client, r.sweepReader(), engine.Namespace, engine.Spec.InstanceRef, func(state *routing.State) (bool, error) {
		// A retained Instance shutdown proof can name a deleted predecessor of
		// this Engine. Every Gateway has already stopped, so a newly created
		// same-name Engine needs no additional withdrawal before its deletion.
		if closedGatewayRoutingProof(*state) == nil {
			return false, nil
		}
		if route, exists := state.Routes[engine.Name]; exists && route.EngineUID != string(engine.UID) {
			return false, errors.New("routing Engine UID mismatch")
		}
		return routing.Withdraw(state, engine.Name)
	})
}

// Stop evidence is persisted before the Pod finalizer is removed. This lets a
// later operator recover safely after Kubernetes has collected the Pod object.
func (r *FireboltInstanceReconciler) reclaimStoppedGatewaySessions(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	pods := &corev1.PodList{}
	if err := r.routingReader().List(ctx, pods, client.InNamespace(instance.Namespace), client.MatchingLabels(instanceLabels(instance.Name, "gateway"))); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !gatewayEnvoyStopped(pod) {
			continue
		}
		if err := mutateRoutingState(ctx, r.Client, r.routingReader(), instance.Namespace, instance.Name, func(state *routing.State) (bool, error) {
			return routing.ForgetStoppedSession(state, string(pod.UID), true)
		}); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(pod, gatewayRoutingFinalizer) {
			controllerutil.RemoveFinalizer(pod, gatewayRoutingFinalizer)
			if err := r.Update(ctx, pod); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *FireboltInstanceReconciler) withdrawInstanceRoutes(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	if err := mutateRoutingState(ctx, r.Client, r.routingReader(), instance.Namespace, instance.Name, routing.CloseInstance); err != nil {
		return err
	}
	if err := r.reclaimStoppedGatewaySessions(ctx, instance); err != nil {
		return err
	}
	_, state, err := readRoutingState(ctx, r.routingReader(), instance.Namespace, instance.Name)
	if err != nil {
		return err
	}
	reports, stopped, err := collectGatewayReports(ctx, r.routingReader(), r.Clientset, instance.Namespace, instance.Name)
	if err != nil {
		return err
	}
	for _, retirement := range state.Retirements {
		if !routing.CanRetire(retirement, reports, stopped) {
			return errGatewayWithdrawalPending
		}
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace, Name: instance.Name + SuffixGateway}}
	if err := r.Delete(ctx, deployment); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	pods := &corev1.PodList{}
	if err := r.routingReader().List(ctx, pods, client.InNamespace(instance.Namespace), client.MatchingLabels(instanceLabels(instance.Name, "gateway"))); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	if len(pods.Items) > 0 {
		return errGatewayWithdrawalPending
	}
	_, state, err = readRoutingState(ctx, r.routingReader(), instance.Namespace, instance.Name)
	if err != nil {
		return err
	}
	return closedGatewayRoutingProof(state)
}

func (r *FireboltEngineReconciler) reclaimEngineRetirements(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	_, state, err := readRoutingState(ctx, r.sweepReader(), engine.Namespace, engine.Spec.InstanceRef)
	if err != nil {
		return err
	}
	complete := map[string]bool{}
	for key, retirement := range state.Retirements {
		if retirement.Route.EngineUID != string(engine.UID) {
			continue
		}
		sts := &appsv1.StatefulSet{}
		err := r.sweepReader().Get(ctx, types.NamespacedName{Namespace: engine.Namespace, Name: genResourceName(engine.Name, retirement.Route.Generation, "")}, sts)
		if apierrors.IsNotFound(err) {
			complete[key] = true
		} else if err != nil {
			return err
		}
	}
	if len(complete) == 0 {
		return nil
	}
	reports, stopped, err := collectGatewayReports(ctx, r.sweepReader(), r.Clientset, engine.Namespace, engine.Spec.InstanceRef)
	if err != nil {
		return err
	}
	return mutateRoutingState(ctx, r.Client, r.sweepReader(), engine.Namespace, engine.Spec.InstanceRef, func(state *routing.State) (bool, error) {
		changed := false
		for key := range complete {
			retirement, ok := state.Retirements[key]
			if !ok || !routing.CanRetire(retirement, reports, stopped) {
				continue
			}
			did, err := routing.CompleteRetirement(state, key, reports, stopped)
			if err != nil {
				return false, err
			}
			changed = changed || did
		}
		return changed, nil
	})
}

// Routing reports use the configured metrics transport, including API proxy
// access for an operator outside the Pod network. Both paths validate identity.
func fetchRoutingReport(ctx context.Context, reader client.Reader, clientset *kubernetes.Clientset, pod *corev1.Pod) (routing.Report, error) {
	instance := &computev1alpha1.FireboltInstance{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: pod.Labels[LabelInstance]}, instance); err != nil {
		return routing.Report{}, err
	}
	if instance.Spec.MetricScrapeMode != computev1alpha1.MetricScrapeModeApiserverProxy {
		return fetchGatewayReport(ctx, pod)
	}
	if clientset == nil {
		return routing.Report{}, errors.New("routing proxy client is not initialized")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stream, err := clientset.CoreV1().Pods(pod.Namespace).ProxyGet("http", pod.Name, strconv.Itoa(int(gatewayWakeAgentDemandPort)), "/routing", nil).Stream(ctx)
	if err != nil {
		return routing.Report{}, err
	}
	defer func() { _ = stream.Close() /* Release the completed bounded proxy response. */ }()
	data, err := io.ReadAll(io.LimitReader(stream, 4<<20))
	if err != nil {
		return routing.Report{}, err
	}
	var report routing.Report
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&report); err != nil {
		return report, err
	}
	if report.PodUID != string(pod.UID) || report.SessionID == "" || report.Outstanding == nil {
		return report, errors.New("invalid gateway session report")
	}
	return report, nil
}

// Engine finalization can finish before the next normal engine pass. The
// Instance owns cleanup of those records, including old UIDs after recreation.
func (r *FireboltInstanceReconciler) reclaimDeletedEngineRoutes(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	engines := &computev1alpha1.FireboltEngineList{}
	if err := r.routingReader().List(ctx, engines, client.InNamespace(instance.Namespace)); err != nil {
		return err
	}
	live := map[string]bool{}
	for i := range engines.Items {
		live[string(engines.Items[i].UID)] = true
	}
	_, state, err := readRoutingState(ctx, r.routingReader(), instance.Namespace, instance.Name)
	if err != nil {
		return err
	}
	needed := false
	for _, route := range state.Routes {
		if !live[route.EngineUID] {
			needed = true
		}
	}
	for _, retirement := range state.Retirements {
		if !live[retirement.Route.EngineUID] {
			needed = true
		}
	}
	if !needed {
		return nil
	}
	reports, stopped, err := collectGatewayReports(ctx, r.routingReader(), r.Clientset, instance.Namespace, instance.Name)
	if err != nil {
		return err
	}
	return mutateRoutingState(ctx, r.Client, r.routingReader(), instance.Namespace, instance.Name, func(state *routing.State) (bool, error) {
		// Read live UIDs after the CAS snapshot. A newly published route must not
		// be judged against a list captured before its Engine existed.
		engines := &computev1alpha1.FireboltEngineList{}
		if err := r.routingReader().List(ctx, engines, client.InNamespace(instance.Namespace)); err != nil {
			return false, err
		}
		live := map[string]bool{}
		for i := range engines.Items {
			live[string(engines.Items[i].UID)] = true
		}
		changed := false
		for name, route := range state.Routes {
			if !live[route.EngineUID] && route.Enabled {
				did, err := routing.Withdraw(state, name)
				if err != nil {
					return false, err
				}
				changed = changed || did
			}
		}
		for key, retirement := range state.Retirements {
			if live[retirement.Route.EngineUID] || !routing.CanRetire(retirement, reports, stopped) {
				continue
			}
			did, err := routing.CompleteRetirement(state, key, reports, stopped)
			if err != nil {
				return false, err
			}
			changed = changed || did
		}
		for name, route := range state.Routes {
			if live[route.EngineUID] {
				continue
			}
			pending := false
			for _, retirement := range state.Retirements {
				if retirement.Route.EngineUID == route.EngineUID {
					pending = true
				}
			}
			if pending {
				continue
			}
			did, err := routing.ForgetEngine(state, name)
			if err != nil {
				return false, err
			}
			changed = changed || did
		}
		return changed, nil
	})
}

// Waiting for a permit to finish is normal progress, not an API failure. Poll
// promptly so a long query does not leave retirement on exponential backoff.
func routingReconcileResult(result ctrl.Result, err error) (ctrl.Result, error) {
	if errors.Is(err, errGatewayWithdrawalPending) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return result, err
}
