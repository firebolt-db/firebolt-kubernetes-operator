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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

// getEngineState reads all cluster resources related to this engine: StatefulSets,
// Services, ConfigMaps, pod readiness, and drain status.
func (r *FireboltEngineReconciler) getEngineState(ctx context.Context, engine *computev1alpha1.FireboltEngine) (EngineState, error) {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)
	state := EngineState{
		ClusterServiceTargetGen: -1,
	}

	engineName := engine.Name
	ns := engine.Namespace
	status := &engine.Status

	currentGen := status.CurrentGeneration

	var drainingGen = -1
	if status.DrainingGeneration != nil {
		drainingGen = *status.DrainingGeneration
	}

	if currentGen >= 0 {
		state.CurrentSTS = r.getStatefulSet(ctx, engineName, ns, currentGen)
		state.CurrentConfigMap = r.getConfigMap(ctx, engineName, ns, currentGen)
		state.CurrentHeadlessSvc = r.getHeadlessService(ctx, engineName, ns, currentGen)

		if state.CurrentSTS != nil {
			ready, count, err := r.checkPodsReady(ctx, engine, currentGen, int(engine.Spec.Replicas))
			if err != nil {
				return state, fmt.Errorf("checkPodsReady (gen %d): %w", currentGen, err)
			}
			state.CurrentPodsReady = ready
			state.CurrentPodCount = count
		}
	}

	if drainingGen >= 0 && drainingGen != currentGen {
		state.DrainingSTS = r.getStatefulSet(ctx, engineName, ns, drainingGen)
		state.DrainingConfigMap = r.getConfigMap(ctx, engineName, ns, drainingGen)
		state.DrainingHeadlessSvc = r.getHeadlessService(ctx, engineName, ns, drainingGen)

		drainCheckDisabled := engine.Spec.DrainCheckEnabled != nil && !*engine.Spec.DrainCheckEnabled
		skipDrain := state.DrainingSTS == nil ||
			engine.Spec.Rollout == computev1alpha1.RolloutRecreate ||
			drainCheckDisabled

		if skipDrain {
			state.DrainingPodsDrained = true
		} else {
			drained, err := r.checkDrainComplete(ctx, engine, drainingGen)
			if err != nil {
				return state, fmt.Errorf("checkDrainComplete (gen %d): %w", drainingGen, err)
			}
			state.DrainingPodsDrained = drained
		}
	}

	clusterSvcName := engineName + SuffixService
	clusterSvc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: clusterSvcName, Namespace: ns}, clusterSvc); err != nil {
		if !apierrors.IsNotFound(err) {
			return state, fmt.Errorf("failed to get cluster service: %w", err)
		}
		log.Info("Cluster service not found", "name", clusterSvcName)
	} else {
		state.ClusterService = clusterSvc
		if genStr, ok := clusterSvc.Spec.Selector[LabelGeneration]; ok {
			g, err := strconv.Atoi(genStr)
			if err != nil {
				return state, fmt.Errorf("parsing %s label %q on service %s: %w", LabelGeneration, genStr, clusterSvcName, err)
			}
			state.ClusterServiceTargetGen = g
		}
		log.Info("Cluster service state",
			"name", clusterSvcName,
			"targetGen", state.ClusterServiceTargetGen,
			"selectorLabels", clusterSvc.Spec.Selector,
		)

		epSlices := &discoveryv1.EndpointSliceList{}
		if err := r.List(ctx, epSlices, client.InNamespace(ns), client.MatchingLabels{
			discoveryv1.LabelServiceName: clusterSvcName,
		}); err != nil {
			return state, fmt.Errorf("listing endpoint slices for service %s: %w", clusterSvcName, err)
		}
		for i := range epSlices.Items {
			for _, ep := range epSlices.Items[i].Endpoints {
				if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
					state.ClusterServiceEndpointsReady = true
					break
				}
			}
			if state.ClusterServiceEndpointsReady {
				break
			}
		}
	}

	return state, nil
}

func (r *FireboltEngineReconciler) getStatefulSet(ctx context.Context, engineName, ns string, gen int) *appsv1.StatefulSet {
	name := genResourceName(engineName, gen, "")
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sts); err != nil {
		return nil
	}
	return sts
}

func (r *FireboltEngineReconciler) getConfigMap(ctx context.Context, engineName, ns string, gen int) *corev1.ConfigMap {
	name := genResourceName(engineName, gen, SuffixConfig)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm); err != nil {
		return nil
	}
	return cm
}

func (r *FireboltEngineReconciler) getHeadlessService(ctx context.Context, engineName, ns string, gen int) *corev1.Service {
	name := genResourceName(engineName, gen, SuffixHL)
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, svc); err != nil {
		return nil
	}
	return svc
}

func (r *FireboltEngineReconciler) checkPodsReady(ctx context.Context, engine *computev1alpha1.FireboltEngine, gen int, expectedReplicas int) (bool, int, error) {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name, "generation", gen)

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(engine.Namespace), client.MatchingLabels{
		LabelEngine:     engine.Name,
		LabelGeneration: strconv.Itoa(gen),
	}); err != nil {
		return false, 0, fmt.Errorf("failed to list pods: %w", err)
	}

	count := len(podList.Items)
	if count != expectedReplicas {
		log.Info("Waiting for pods", "have", count, "want", expectedReplicas)
		return false, count, nil
	}

	notReady := 0
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			notReady++
			continue
		}
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			notReady++
		}
	}

	if notReady > 0 {
		log.Info("Pods not ready", "notReady", notReady, "total", count)
		return false, count, nil
	}

	log.Info("All pods ready", "count", count)
	return true, count, nil
}

func (r *FireboltEngineReconciler) checkDrainComplete(ctx context.Context, engine *computev1alpha1.FireboltEngine, gen int) (bool, error) {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name, "drainingGeneration", gen)

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(engine.Namespace), client.MatchingLabels{
		LabelEngine:     engine.Name,
		LabelGeneration: strconv.Itoa(gen),
	}); err != nil {
		return false, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(podList.Items) == 0 {
		log.Info("No draining pods found, drain complete")
		return true, nil
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		drained, err := r.isPodDrained(ctx, pod)
		if err != nil {
			log.Info("Drain check failed for pod", "pod", pod.Name, "error", err)
			return false, nil
		}

		if !drained {
			log.Info("Pod still draining", "pod", pod.Name)
			return false, nil
		}
		log.Info("Pod drained", "pod", pod.Name)
	}

	log.Info("All pods drained", "count", len(podList.Items))
	return true, nil
}

func (r *FireboltEngineReconciler) isPodDrained(ctx context.Context, pod *corev1.Pod) (bool, error) {
	if r.Clientset == nil || r.RestConfig == nil {
		return false, errors.New("clientset or rest config not initialized")
	}

	cmd := []string{
		"fb",
		"--core",
		"--no-spinner",
		"--concise",
		"--label", "firebolt-k8s-operator-drain-check",
		"--extra", "access_internal_system_tables=1",
		"--format", "JSON_Compact",
		"--command", DrainCheckSQL,
	}

	req := r.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		Param("container", ContainerNameEngine).
		Param("command", cmd[0])

	for _, c := range cmd[1:] {
		req = req.Param("command", c)
	}
	req = req.Param("stdout", "true").
		Param("stderr", "true")

	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return false, fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return false, fmt.Errorf("exec failed: %w, stderr: %s", err, stderr.String())
	}

	output := stdout.String()
	var response DrainCheckResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return false, fmt.Errorf("failed to parse drain check response: %w, output: %s", err, output)
	}

	if len(response.Errors) > 0 {
		return false, fmt.Errorf("drain check query returned error: %s", response.Errors[0].Description)
	}

	if len(response.Data) == 0 || len(response.Data[0]) == 0 {
		return false, fmt.Errorf("drain check response missing data field: %s", output)
	}

	count := response.Data[0][0]
	return count == "0", nil
}
