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

package controller

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

// DrainProbeError indicates that the drain-readiness probe (pod /metrics
// scrape) could not tell the operator whether an old-generation pod has
// finished serving queries. It does NOT mean the engine itself is
// unhealthy: the active-generation pods may still be serving traffic
// normally. What it does mean is that the blue-green cannot proceed
// past PhaseDraining until the probe works again, because cutting over
// without a clean drain could abort in-flight queries.
//
// Reconcile uses errors.As to detect this and surface it as a
// user-facing ConditionReady=False with Reason=DrainCheckFailing, so a
// broken drain does not look like a silent infinite loop. The original
// error is still returned to controller-runtime so its exponential
// backoff applies to retries; there is no hidden RequeueAfter racing
// with the work-queue rate-limiter.
type DrainProbeError struct {
	Generation int
	PodName    string
	Err        error
}

func (e *DrainProbeError) Error() string {
	return fmt.Sprintf("drain probe failed on pod %s (gen %d): %v", e.PodName, e.Generation, e.Err)
}

func (e *DrainProbeError) Unwrap() error { return e.Err }

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
		var err error
		if state.CurrentSTS, err = r.getStatefulSet(ctx, engineName, ns, currentGen); err != nil {
			return state, err
		}
		if state.CurrentConfigMap, err = r.getConfigMap(ctx, engineName, ns, currentGen); err != nil {
			return state, err
		}
		if state.CurrentHeadlessSvc, err = r.getHeadlessService(ctx, engineName, ns, currentGen); err != nil {
			return state, err
		}

		if state.CurrentSTS != nil {
			allReady, total, ready, err := r.checkPodsReady(ctx, engine, currentGen, int(engine.Spec.Replicas))
			if err != nil {
				return state, fmt.Errorf("checkPodsReady (gen %d): %w", currentGen, err)
			}
			state.CurrentPodsReady = allReady
			state.CurrentPodTotal = total
			state.CurrentPodReady = ready
		}
	}

	if drainingGen >= 0 && drainingGen != currentGen {
		var err error
		if state.DrainingSTS, err = r.getStatefulSet(ctx, engineName, ns, drainingGen); err != nil {
			return state, err
		}
		if state.DrainingConfigMap, err = r.getConfigMap(ctx, engineName, ns, drainingGen); err != nil {
			return state, err
		}
		if state.DrainingHeadlessSvc, err = r.getHeadlessService(ctx, engineName, ns, drainingGen); err != nil {
			return state, err
		}

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
	}

	return state, nil
}

// These three getters differentiate between "resource absent" and "lookup
// failed for some other reason". Returning nil for any error would let a
// transient API failure (RBAC, connection reset, stale cache miss) be
// indistinguishable from NotFound, which in turn would cause computeStable
// to spuriously kick off a new blue-green generation because it interprets
// a nil STS/ConfigMap/HeadlessSvc as "missing — needs a fresh generation".
// We therefore return (obj, nil) on success, (nil, nil) only on NotFound,
// and propagate any other error to the caller so reconciliation retries.
func (r *FireboltEngineReconciler) getStatefulSet(ctx context.Context, engineName, ns string, gen int) (*appsv1.StatefulSet, error) {
	name := genResourceName(engineName, gen, "")
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get StatefulSet %s/%s: %w", ns, name, err)
	}
	return sts, nil
}

func (r *FireboltEngineReconciler) getConfigMap(ctx context.Context, engineName, ns string, gen int) (*corev1.ConfigMap, error) {
	name := genResourceName(engineName, gen, SuffixConfig)
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get ConfigMap %s/%s: %w", ns, name, err)
	}
	return cm, nil
}

func (r *FireboltEngineReconciler) getHeadlessService(ctx context.Context, engineName, ns string, gen int) (*corev1.Service, error) {
	name := genResourceName(engineName, gen, SuffixHL)
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get headless Service %s/%s: %w", ns, name, err)
	}
	return svc, nil
}

// checkPodsReady lists the pods for gen and returns (allReady, total, ready, err).
// allReady is true only when total == expectedReplicas AND every pod is
// PodRunning with PodReady=True. total is len(podList.Items) and ready is
// the subset satisfying the running+ready predicate; ready <= total always.
func (r *FireboltEngineReconciler) checkPodsReady(ctx context.Context, engine *computev1alpha1.FireboltEngine, gen int, expectedReplicas int) (allReady bool, total int, ready int, err error) {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name, "generation", gen)

	podList := &corev1.PodList{}
	if listErr := r.List(ctx, podList, client.InNamespace(engine.Namespace), client.MatchingLabels{
		LabelEngine:     engine.Name,
		LabelGeneration: strconv.Itoa(gen),
	}); listErr != nil {
		return false, 0, 0, fmt.Errorf("failed to list pods: %w", listErr)
	}

	total = len(podList.Items)
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
	}

	switch {
	case total != expectedReplicas:
		log.Info("Waiting for pods", "have", total, "want", expectedReplicas, "ready", ready)
		return false, total, ready, nil
	case ready < total:
		log.Info("Pods not ready", "ready", ready, "total", total)
		return false, total, ready, nil
	default:
		log.Info("All pods ready", "count", total)
		return true, total, ready, nil
	}
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
			// Surface the probe failure. See DrainProbeError - the
			// caller turns this into a ConditionReady=False with
			// Reason=DrainCheckFailing instead of silently looping.
			return false, &DrainProbeError{Generation: gen, PodName: pod.Name, Err: err}
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

// isPodDrained reports whether the engine pod has finished serving queries.
//
// It scrapes the pod's Prometheus /metrics endpoint via the Kubernetes
// pod-proxy subresource (not the pod IP directly), which:
//   - works whether the operator runs in-cluster or externally (make run / e2e),
//     because the request is routed through the API server;
//   - does not require pod-level exec (no fb CLI in the image, no SPDY);
//   - is covered by the same RBAC we already have on "pods/proxy".
//
// The signal we trust is firebolt_running_queries + firebolt_suspended_queries
// == 0. Both gauges are exported by the engine; suspended queries count
// queries that are idle waiting on a client but still holding a session,
// so we wait for those too before cutting the generation over.
//
// Errors (pod unreachable, metrics missing, scrape failure) are returned
// to the caller - checkDrainComplete wraps them as a DrainProbeError so
// Reconcile can surface them as ConditionReady=False with
// Reason=DrainCheckFailing. Returning (false, nil) here would make a
// broken /metrics endpoint look like an engine that just happens to
// still be busy, and the blue-green would stall silently forever.
func (r *FireboltEngineReconciler) isPodDrained(ctx context.Context, pod *corev1.Pod) (bool, error) {
	if r.Clientset == nil {
		return false, errors.New("clientset not initialized")
	}
	if pod.Status.Phase != corev1.PodRunning {
		return true, nil
	}

	raw, err := r.Clientset.CoreV1().RESTClient().Get().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(fmt.Sprintf("%s:%d", pod.Name, MetricsPort)).
		SubResource("proxy").
		Suffix(MetricsPath).
		DoRaw(ctx)
	if err != nil {
		return false, fmt.Errorf("scraping metrics from pod %s: %w", pod.Name, err)
	}

	running, runningOK := parsePrometheusGauge(raw, MetricRunningQueries)
	suspended, suspendedOK := parsePrometheusGauge(raw, MetricSuspendedQueries)
	if !runningOK || !suspendedOK {
		return false, fmt.Errorf(
			"drain metrics missing from pod %s (running=%t suspended=%t)",
			pod.Name, runningOK, suspendedOK,
		)
	}

	return running == 0 && suspended == 0, nil
}

// parsePrometheusGauge pulls a single gauge value out of a Prometheus text
// /metrics response. It returns (value, true) on success; (0, false) if the
// metric is missing, has no plain samples, or its value cannot be parsed.
//
// It only understands the subset of the exposition format we need: lines of
// the form "<name> <value>" (no labels). The two engine drain-check gauges
// are published without labels, so this is sufficient. If Core ever adds
// labels to these metrics we will need to revisit; for now a label-annotated
// sample is intentionally ignored by this parser (treated as "not found")
// so the drain check fails closed rather than silently matching a wrong
// series.
func parsePrometheusGauge(body []byte, name string) (int64, bool) {
	prefix := name + " "
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(line[len(prefix):])
		// Strip an optional trailing timestamp: "<value> [<ts>]".
		if idx := strings.IndexByte(rest, ' '); idx >= 0 {
			rest = rest[:idx]
		}
		// Prometheus gauges are float64; counters we care about are
		// integer-valued in practice but we parse as float and clamp to
		// avoid being tripped up by "3.0" vs "3" from exporters.
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, false
		}
		return int64(v), true
	}
	return 0, false
}
