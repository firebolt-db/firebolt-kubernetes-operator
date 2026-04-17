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
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

// InstanceInfo holds the multi-engine endpoint and account ID resolved from
// the FireboltInstance in the engine's namespace. These are injected into the
// engine ConfigMap so engine nodes can connect to the metadata service.
type InstanceInfo struct {
	MetadataEndpoint string
	AccountID        string
}

// computeEngineReconcile determines what resources need to be created, updated,
// or deleted based on the engine spec, its current status, and the observed
// cluster state. It does not perform any I/O.
func computeEngineReconcile(
	spec *computev1alpha1.FireboltEngineSpec,
	status *computev1alpha1.FireboltEngineStatus,
	current EngineState,
	engineName string,
	engineNamespace string,
	metadataGeneration int64,
	instanceInfo InstanceInfo,
) EngineReconcileResult {
	result := EngineReconcileResult{
		Status: *status.DeepCopy(),
	}

	switch status.Phase {
	case "", computev1alpha1.PhaseStable:
		computeStable(spec, &result, current, engineName, engineNamespace, metadataGeneration)
	case computev1alpha1.PhaseCreating:
		computeCreating(spec, &result, current, engineName, engineNamespace, instanceInfo)
	case computev1alpha1.PhaseSwitching:
		computeSwitching(&result, current, engineName, engineNamespace)
	case computev1alpha1.PhaseDraining:
		computeDraining(spec, &result, current)
	case computev1alpha1.PhaseCleaning:
		computeCleaning(&result, current)
	default:
		result.Status.Phase = computev1alpha1.PhaseStable
		result.Requeue = true
	}

	now := metav1.Now()
	result.Status.LastReconciled = &now
	return result
}

// computeStable handles the stable phase: detects spec drift or missing
// resources and starts a new blue-green transition if needed, or fixes
// in-place drift for ConfigMaps and service selectors.
//
// When a new generation is needed, computeStable only writes the intent
// to status (Phase=Creating, bumped CurrentGeneration) and requeues.
// Resource creation is deferred to computeCreating on the next reconcile.
// This ensures the status update is persisted before any resources are
// created, preventing leaked resources if the operator crashes between
// resource creation and the status write.
//
// Invariant: Phase=Stable implies ActiveGeneration >= 0. The controller's
// top-level Reconcile initializes Phase=Creating and ActiveGeneration=-1
// on first sight of the engine, so computeStable never runs with a
// negative ActiveGeneration. Violating this invariant is a programming
// error elsewhere in the state machine, not a recoverable state.
func computeStable(
	spec *computev1alpha1.FireboltEngineSpec,
	r *EngineReconcileResult,
	current EngineState,
	engineName string,
	engineNamespace string,
	metadataGeneration int64,
) {
	status := &r.Status

	if status.ActiveGeneration < 0 {
		panic(fmt.Sprintf(
			"BUG: computeStable reached with ActiveGeneration=%d; Phase=Stable invariant violated",
			status.ActiveGeneration,
		))
	}

	gen := status.ActiveGeneration

	// ConfigMap drift is not checked independently here. All mutable fields
	// that affect the ConfigMap (replicas, MetadataEndpointOverride) are
	// covered by stsMatchesSpec, which triggers a new generation. The
	// remaining ConfigMap inputs (instanceInfo) are effectively immutable.
	needsNewGen := current.CurrentSTS == nil ||
		!stsMatchesSpec(current.CurrentSTS, spec) ||
		current.CurrentHeadlessSvc == nil

	if needsNewGen {
		newGen := status.CurrentGeneration + 1
		status.CurrentGeneration = newGen
		status.Phase = computev1alpha1.PhaseCreating
		r.Requeue = true
		return
	}

	if current.ClusterService == nil {
		r.EnsureClusterSvc = buildClusterService(engineName, engineNamespace, gen)
	} else if current.ClusterServiceTargetGen != gen {
		svcCopy := current.ClusterService.DeepCopy()
		if svcCopy.Spec.Selector == nil {
			svcCopy.Spec.Selector = make(map[string]string)
		}
		svcCopy.Spec.Selector[LabelGeneration] = strconv.Itoa(gen)
		r.EnsureClusterSvc = svcCopy
	}

	status.Phase = computev1alpha1.PhaseStable
	status.ObservedGeneration = metadataGeneration
	r.RequeueAfter = 30 * time.Second
}

// computeCreating ensures all resources for the new generation exist and
// waits for pods to become ready before transitioning to switching.
//
// If the spec changed since the current generation was created (e.g. replica
// count changed), we abandon the in-progress generation and start a fresh
// one. Patching a live STS doesn't restart pods, leaving them with a stale
// config (wrong node list) that causes a permanent readiness deadlock.
//
// Ordering invariant: the spec-drift check MUST run before consulting
// current.CurrentPodsReady. getEngineState reads pod readiness against the
// latest engine.Spec.Replicas (see checkPodsReady) while the observed STS
// may still reflect the old replica count; if drift is not handled first,
// a stale CurrentPodsReady=false would block us on a generation that is
// already doomed to be abandoned, deadlocking the rollout until the next
// spec mutation.
//
// Double-bump case: when the user mutates the spec multiple times while a
// generation is still being created, this branch can fire repeatedly and
// increment CurrentGeneration twice (or more) without ever reaching
// PhaseSwitching. This is intentional — each bump creates a clean
// generation rather than patching in-place — but it means CurrentGeneration
// can grow faster than the number of successful rollouts. ObservedGeneration
// is only advanced once computeStable accepts a generation as stable, so
// external observers relying on ObservedGeneration (not CurrentGeneration)
// see a consistent view.
func computeCreating(
	spec *computev1alpha1.FireboltEngineSpec,
	r *EngineReconcileResult,
	current EngineState,
	engineName string,
	engineNamespace string,
	instanceInfo InstanceInfo,
) {
	status := &r.Status
	gen := status.CurrentGeneration

	// Must be checked before CurrentPodsReady; see "Ordering invariant" above.
	if current.CurrentSTS != nil && !stsMatchesSpec(current.CurrentSTS, spec) {
		r.DeleteResources = append(r.DeleteResources, current.CurrentSTS)
		if current.CurrentHeadlessSvc != nil {
			r.DeleteResources = append(r.DeleteResources, current.CurrentHeadlessSvc)
		}
		if current.CurrentConfigMap != nil {
			r.DeleteResources = append(r.DeleteResources, current.CurrentConfigMap)
		}
		status.CurrentGeneration++
		r.Requeue = true
		return
	}

	buildGenResources(spec, r, engineName, engineNamespace, gen, instanceInfo)

	if current.ClusterService == nil {
		r.EnsureClusterSvc = buildClusterService(engineName, engineNamespace, gen)
	}

	if !current.CurrentPodsReady {
		r.RequeueAfter = 5 * time.Second
		return
	}

	status.Phase = computev1alpha1.PhaseSwitching
	r.Requeue = true
}

// computeSwitching updates the cluster service selector to point to the new
// generation, waits for the endpoints controller to propagate ready addresses,
// then transitions to draining (if there's an old generation) or stable (if
// this is the initial deployment).
//
// Because the cluster Service is headless, flipping its selector to the new
// generation immediately changes the set of A records returned for the
// Service hostname. Kubernetes excludes not-ready pods from the headless
// endpoint set automatically, and the new generation's pods only reach this
// phase once their readiness probe already passes. No separate
// endpoint-readiness gate is therefore required.
func computeSwitching(
	r *EngineReconcileResult,
	current EngineState,
	engineName string,
	engineNamespace string,
) {
	status := &r.Status
	gen := status.CurrentGeneration

	if current.ClusterServiceTargetGen != gen {
		if current.ClusterService != nil {
			svcCopy := current.ClusterService.DeepCopy()
			if svcCopy.Spec.Selector == nil {
				svcCopy.Spec.Selector = make(map[string]string)
			}
			svcCopy.Spec.Selector[LabelGeneration] = strconv.Itoa(gen)
			r.EnsureClusterSvc = svcCopy
		} else {
			r.EnsureClusterSvc = buildClusterService(engineName, engineNamespace, gen)
		}
		r.Requeue = true
		return
	}

	oldGen := status.ActiveGeneration
	status.ActiveGeneration = gen

	if oldGen >= 0 && oldGen != gen {
		status.DrainingGeneration = &oldGen
		status.Phase = computev1alpha1.PhaseDraining
	} else {
		status.Phase = computev1alpha1.PhaseStable
	}
	r.Requeue = true
}

// computeDraining waits for the old generation's pods to finish serving
// queries before transitioning to cleaning. The read layer (getEngineState)
// already sets DrainingPodsDrained=true when the rollout strategy is
// "recreate" or drainCheckEnabled is false.
func computeDraining(
	spec *computev1alpha1.FireboltEngineSpec,
	r *EngineReconcileResult,
	current EngineState,
) {
	status := &r.Status

	if status.DrainingGeneration == nil {
		status.Phase = computev1alpha1.PhaseStable
		r.Requeue = true
		return
	}

	if !current.DrainingPodsDrained {
		r.RequeueAfter = getDrainCheckInterval(spec)
		return
	}

	status.Phase = computev1alpha1.PhaseCleaning
	r.Requeue = true
}

// computeCleaning deletes the old generation's StatefulSet, headless service,
// and ConfigMap, then transitions to stable.
func computeCleaning(
	r *EngineReconcileResult,
	current EngineState,
) {
	status := &r.Status

	if status.DrainingGeneration == nil {
		status.Phase = computev1alpha1.PhaseStable
		r.Requeue = true
		return
	}

	var toDelete []client.Object
	if current.DrainingSTS != nil {
		toDelete = append(toDelete, current.DrainingSTS)
	}
	if current.DrainingHeadlessSvc != nil {
		toDelete = append(toDelete, current.DrainingHeadlessSvc)
	}
	if current.DrainingConfigMap != nil {
		toDelete = append(toDelete, current.DrainingConfigMap)
	}

	if len(toDelete) > 0 {
		r.DeleteResources = append(r.DeleteResources, toDelete...)
	}

	status.DrainingGeneration = nil
	status.Phase = computev1alpha1.PhaseStable
	r.Requeue = true
}

func buildGenResources(
	spec *computev1alpha1.FireboltEngineSpec,
	r *EngineReconcileResult,
	engineName string,
	engineNamespace string,
	gen int,
	instanceInfo InstanceInfo,
) {
	r.EnsureConfigMap = buildConfigMap(spec, engineName, engineNamespace, gen, instanceInfo)
	r.EnsureHeadlessSvc = buildHeadlessService(engineName, engineNamespace, gen)
	r.EnsureStatefulSet = buildStatefulSet(spec, engineName, engineNamespace, gen)
}

func buildConfigMap(spec *computev1alpha1.FireboltEngineSpec, engineName, namespace string, gen int, instanceInfo InstanceInfo) *corev1.ConfigMap {
	name := genResourceName(engineName, gen, SuffixConfig)
	headlessSvcName := genResourceName(engineName, gen, SuffixHL)
	stsName := genResourceName(engineName, gen, "")

	nodes := make([]map[string]string, spec.Replicas)
	for i := int32(0); i < spec.Replicas; i++ {
		podName := fmt.Sprintf("%s-%d", stsName, i)
		host := fmt.Sprintf("%s.%s.%s.svc", podName, headlessSvcName, namespace)
		nodes[i] = map[string]string{"host": host}
	}

	metadataEndpoint := instanceInfo.MetadataEndpoint
	if spec.MetadataEndpointOverride != nil {
		metadataEndpoint = *spec.MetadataEndpointOverride
	}

	innerConfig := map[string]interface{}{
		"account_id":                instanceInfo.AccountID,
		"account_name":              "default-account",
		"organization_id":           "01KP98J0000000000000000000",
		"organization_name":         "default-org",
		"engine_id":                 engineName,
		"engine_name":               engineName,
		"cluster_id":                "default-cluster",
		"multi_engine_endpoint":     metadataEndpoint,
		"multi_engine_mode_enabled": true,
		"logger_formatting":         "json",
		"logger_use_files":          false,
	}

	coreConfig := map[string]interface{}{
		"config": innerConfig,
		"nodes":  nodes,
	}

	configJSON, err := json.MarshalIndent(coreConfig, "", "  ")
	if err != nil {
		panic(fmt.Sprintf("BUG: failed to marshal config.json: %v", err))
	}

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Data: map[string]string{
			"config.json": string(configJSON),
		},
	}
}

func buildHeadlessService(engineName, namespace string, gen int) *corev1.Service {
	name := genResourceName(engineName, gen, SuffixHL)
	labels := map[string]string{
		LabelEngine:     engineName,
		LabelGeneration: strconv.Itoa(gen),
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 labels,
			Ports:                    GetServicePorts(),
		},
	}
}

func buildStatefulSet(spec *computev1alpha1.FireboltEngineSpec, engineName, namespace string, gen int) *appsv1.StatefulSet {
	name := genResourceName(engineName, gen, "")
	headlessSvcName := genResourceName(engineName, gen, SuffixHL)
	coreConfigName := genResourceName(engineName, gen, SuffixConfig)

	labels := map[string]string{
		LabelEngine:     engineName,
		LabelGeneration: strconv.Itoa(gen),
	}

	annotations := map[string]string{}
	if spec.MetadataEndpointOverride != nil {
		annotations[AnnotationMetadataOverride] = *spec.MetadataEndpointOverride
	}

	image := DefaultEngineImage
	pullPolicy := corev1.PullIfNotPresent
	if spec.Image != nil {
		image = fmt.Sprintf("%s:%s", spec.Image.Repository, spec.Image.Tag)
		if spec.Image.PullPolicy != "" {
			pullPolicy = spec.Image.PullPolicy
		}
	}

	gracePeriod := getTerminationGracePeriod(spec)
	preStopScript := BuildEnginePreStopScript(gracePeriod)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         headlessSvcName,
			Replicas:            &spec.Replicas,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:                  spec.NodeSelector,
					Tolerations:                   spec.Tolerations,
					TerminationGracePeriodSeconds: &gracePeriod,
					Containers: []corev1.Container{
						{
							Name:            ContainerNameEngine,
							Image:           image,
							ImagePullPolicy: pullPolicy,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    spec.Resources.CPU,
									corev1.ResourceMemory: spec.Resources.Memory,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    spec.Resources.CPU,
									corev1.ResourceMemory: spec.Resources.Memory,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "POD_INDEX",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.labels['apps.kubernetes.io/pod-index']",
										},
									},
								},
							},
							Ports:   GetContainerPorts(),
							Command: []string{"/bin/bash", "-c"},
							Args:    []string{strings.TrimSpace(EngineStartupScript)},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "nodes-config",
									MountPath: ConfigMountPath,
									SubPath:   "config.json",
									ReadOnly:  true,
								},
							},
							Lifecycle: &corev1.Lifecycle{
								PreStop: &corev1.LifecycleHandler{
									Exec: &corev1.ExecAction{
										Command: []string{"/bin/bash", "-c", preStopScript},
									},
								},
							},
							ReadinessProbe: &corev1.Probe{
								InitialDelaySeconds: 1,
								PeriodSeconds:       3,
								TimeoutSeconds:      3,
								FailureThreshold:    6,
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: HealthReadyPath,
										Port: intstr.FromInt(HealthPort),
									},
								},
							},
							LivenessProbe: &corev1.Probe{
								InitialDelaySeconds: 2,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								FailureThreshold:    6,
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: HealthLivePath,
										Port: intstr.FromInt(HealthPort),
									},
								},
							},
							StartupProbe: &corev1.Probe{
								PeriodSeconds:    5,
								TimeoutSeconds:   3,
								FailureThreshold: 30,
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: HealthLivePath,
										Port: intstr.FromInt(HealthPort),
									},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "nodes-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: coreConfigName,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// buildClusterService builds the per-engine routing Service. It is a
// first-class entry point for engine traffic used by two kinds of callers:
//
//  1. The in-instance Envoy gateway, which resolves the Service hostname at
//     request time (via the dynamic_forward_proxy filter).
//  2. External clients that want to do their own connection-level load
//     balancing — they resolve the Service's A records and pick an endpoint
//     IP directly, without going through a VIP.
//
// To make both paths work correctly we expose this Service as headless
// (ClusterIP=None). Kubernetes then serves the Service hostname as a set of
// A records, one per endpoint pod IP. This has two consequences that are
// central to the zero-downtime behavior the gateway depends on:
//
//   - kube-proxy is removed from the data path. Requests go client -> pod IP
//     directly, avoiding the well-known terminating-endpoint race where a
//     SYN is still DNAT'd to a pod that has already closed its listener.
//
//   - PublishNotReadyAddresses is false, so only endpoints whose pod-level
//     readiness probe passes appear in DNS. A not-ready pod is excluded
//     from the A-record set automatically; conversely a ready pod appears
//     as soon as the readiness probe passes, without waiting for an
//     endpoints-controller propagation we have to gate on.
//
// The selector keeps `LabelGeneration` so the blue-green cutover mechanism
// is preserved: atomically flipping the selector from the draining
// generation to the new generation swaps the DNS A-record set over to the
// new pod IPs.
func buildClusterService(engineName, namespace string, gen int) *corev1.Service {
	name := engineName + SuffixService
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				LabelEngine: engineName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeClusterIP,
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: false,
			Selector: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
			Ports: GetServicePorts(),
		},
	}
}

func getDrainCheckInterval(spec *computev1alpha1.FireboltEngineSpec) time.Duration {
	if spec.DrainCheckInterval != nil {
		return spec.DrainCheckInterval.Duration
	}
	return DefaultDrainCheckInterval
}

// getTerminationGracePeriod returns the TGPS value to stamp on the engine
// StatefulSet's pod template: the spec override when set, otherwise the
// operator's default. Defaulting is done here (not relying on the kubebuilder
// default alone) so unit tests that construct a FireboltEngineSpec literal
// get the same value as the cluster-loaded CRs.
func getTerminationGracePeriod(spec *computev1alpha1.FireboltEngineSpec) int64 {
	if spec.TerminationGracePeriodSeconds != nil {
		return *spec.TerminationGracePeriodSeconds
	}
	return DefaultTerminationGracePeriodSeconds
}

// stsMatchesSpec returns true if the StatefulSet matches all mutable fields
// in the engine spec. A mismatch triggers a new blue-green generation.
func stsMatchesSpec(sts *appsv1.StatefulSet, spec *computev1alpha1.FireboltEngineSpec) bool {
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != spec.Replicas {
		return false
	}
	podSpec := sts.Spec.Template.Spec
	if len(podSpec.Containers) == 0 {
		return false
	}
	container := podSpec.Containers[0]

	expectedImage := DefaultEngineImage
	expectedPullPolicy := corev1.PullIfNotPresent
	if spec.Image != nil {
		expectedImage = fmt.Sprintf("%s:%s", spec.Image.Repository, spec.Image.Tag)
		if spec.Image.PullPolicy != "" {
			expectedPullPolicy = spec.Image.PullPolicy
		}
	}
	if container.Image != expectedImage {
		return false
	}
	if container.ImagePullPolicy != expectedPullPolicy {
		return false
	}

	if !container.Resources.Requests.Cpu().Equal(spec.Resources.CPU) ||
		!container.Resources.Requests.Memory().Equal(spec.Resources.Memory) {
		return false
	}

	if !reflect.DeepEqual(podSpec.NodeSelector, spec.NodeSelector) {
		return false
	}

	if !reflect.DeepEqual(podSpec.Tolerations, spec.Tolerations) {
		return false
	}

	expectedGracePeriod := getTerminationGracePeriod(spec)
	if podSpec.TerminationGracePeriodSeconds == nil ||
		*podSpec.TerminationGracePeriodSeconds != expectedGracePeriod {
		return false
	}

	stsOverride := sts.Annotations[AnnotationMetadataOverride]
	specOverride := ""
	if spec.MetadataEndpointOverride != nil {
		specOverride = *spec.MetadataEndpointOverride
	}
	if stsOverride != specOverride {
		return false
	}

	return true
}
