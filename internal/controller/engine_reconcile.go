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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// EngineConfigSchemaVersion is the schema_version stamped on the rendered
// config.yaml. FireboltCoreServer rejects unknown versions at startup, so
// bumping here requires a matching engine release.
const EngineConfigSchemaVersion = "1.0"

// ConfigFileName is the key under which the rendered engine configuration is
// stored in the engine ConfigMap and the filename mounted into the pod. It
// matches the path FireboltCoreServer looks for in its data-dir.
const ConfigFileName = "config.yaml"

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
	case "", computev1alpha1.PhaseStable, computev1alpha1.PhaseStopped:
		computeStable(spec, &result, current, engineName, engineNamespace, metadataGeneration, instanceInfo)
	case computev1alpha1.PhaseCreating:
		computeCreating(spec, &result, current, engineName, engineNamespace, instanceInfo)
	case computev1alpha1.PhaseSwitching:
		computeSwitching(spec, &result, current, engineName, engineNamespace)
	case computev1alpha1.PhaseDraining:
		computeDraining(spec, &result, current)
	case computev1alpha1.PhaseCleaning:
		computeCleaning(spec, &result, current)
	default:
		result.Status.Phase = terminalPhase(spec)
		result.Requeue = true
	}

	now := metav1.Now()
	result.Status.LastReconciled = &now
	return result
}

// terminalPhase returns the phase a reconciled engine should rest in
// once no transition is in progress. It is PhaseStopped when the spec
// asks for zero replicas (the user has parked the engine), PhaseStable
// otherwise. Every "I am done" write in the state machine funnels
// through this helper so the running/stopped distinction is made in
// exactly one place.
func terminalPhase(spec *computev1alpha1.FireboltEngineSpec) computev1alpha1.EnginePhase {
	if spec.Replicas == 0 {
		return computev1alpha1.PhaseStopped
	}
	return computev1alpha1.PhaseStable
}

// computeStable handles the stable phase: detects spec drift or missing
// resources. A missing or drifted StatefulSet starts a new blue-green
// transition; a missing ConfigMap or headless Service is re-materialized
// in place at the current generation (see comments inside).
//
// When a new generation is needed, computeStable only writes the intent
// to status (Phase=Creating, bumped CurrentGeneration) and requeues.
// Resource creation is deferred to computeCreating on the next reconcile.
// This ensures the status update is persisted before any resources are
// created, preventing leaked resources if the operator crashes between
// resource creation and the status write.
//
// Invariant: Phase=Stable or Phase=Stopped implies ActiveGeneration >= 0.
// The controller's top-level Reconcile initializes Phase=Creating and
// ActiveGeneration=-1 on first sight of the engine, so computeStable
// never runs with a negative ActiveGeneration. Violating this invariant
// is a programming error elsewhere in the state machine, not a
// recoverable state.
func computeStable(
	spec *computev1alpha1.FireboltEngineSpec,
	r *EngineReconcileResult,
	current EngineState,
	engineName string,
	engineNamespace string,
	metadataGeneration int64,
	instanceInfo InstanceInfo,
) {
	status := &r.Status

	if status.ActiveGeneration < 0 {
		panic(fmt.Sprintf(
			"BUG: computeStable reached with ActiveGeneration=%d; terminal-phase invariant violated",
			status.ActiveGeneration,
		))
	}

	gen := status.ActiveGeneration

	// A missing or spec-drifted StatefulSet is handled by bumping to a new
	// generation and starting a clean blue-green rollout. Patching a live
	// STS in-place to match spec drift doesn't restart pods, leaving them
	// with stale configuration, so we always roll forward via a new gen.
	//
	// ConfigMap content drift is not checked independently: all mutable
	// spec fields that affect the ConfigMap (replicas, MetadataEndpointOverride,
	// CustomEngineConfig) are covered by stsMatchesSpec, which triggers a new
	// generation. The remaining ConfigMap inputs (instanceInfo, engineName,
	// namespace, gen) are effectively immutable once the generation has been
	// created.
	if current.CurrentSTS == nil || !stsMatchesSpec(current.CurrentSTS, spec) {
		newGen := status.CurrentGeneration + 1
		status.CurrentGeneration = newGen
		status.Phase = computev1alpha1.PhaseCreating
		r.Requeue = true
		return
	}

	// Missing ConfigMap / headless service are recoverable in-place: both
	// are deterministic from (engineName, namespace, gen) plus the
	// already-verified stable spec, so rebuilding at the current generation
	// produces byte-identical content. Engine pods only read the ConfigMap
	// at startup, so re-materializing it has no effect on running pods but
	// unblocks any future restart (node drain, OOMKill, eviction) that
	// would otherwise get stuck in Pending on the projected-volume mount.
	// The headless service is likewise name/selector-deterministic and
	// its resurrection immediately restores intra-cluster pod DNS. No new
	// generation, no full rollout, no traffic switch.
	if current.CurrentConfigMap == nil {
		r.EnsureConfigMap = buildConfigMap(spec, engineName, engineNamespace, gen, instanceInfo)
		r.Requeue = true
	}
	if current.CurrentHeadlessSvc == nil {
		r.EnsureHeadlessSvc = buildHeadlessService(engineName, engineNamespace, gen)
		r.Requeue = true
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

	status.Phase = terminalPhase(spec)
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
	spec *computev1alpha1.FireboltEngineSpec,
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
		// DrainingPodsDrained is not stored in status; it is re-derived from live
		// pod metrics each reconcile, so no explicit reset is needed here.
		status.Phase = computev1alpha1.PhaseDraining
	} else {
		status.Phase = terminalPhase(spec)
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
		status.Phase = terminalPhase(spec)
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
// and ConfigMap, then transitions to stable (or stopped, if spec.replicas is 0).
func computeCleaning(
	spec *computev1alpha1.FireboltEngineSpec,
	r *EngineReconcileResult,
	current EngineState,
) {
	status := &r.Status

	if status.DrainingGeneration == nil {
		status.Phase = terminalPhase(spec)
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
	status.Phase = terminalPhase(spec)
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

	nodes := make([]map[string]interface{}, spec.Replicas)
	for i := int32(0); i < spec.Replicas; i++ {
		podName := fmt.Sprintf("%s-%d", stsName, i)
		host := fmt.Sprintf("%s.%s.%s.svc", podName, headlessSvcName, namespace)
		nodes[i] = map[string]interface{}{"host": host}
	}

	metadataEndpoint := instanceInfo.MetadataEndpoint
	if spec.MetadataEndpointOverride != nil {
		metadataEndpoint = *spec.MetadataEndpointOverride
	}

	gracePeriod := getTerminationGracePeriod(spec)
	// engine.termination_grace_period is the engine's post-SIGTERM budget
	// for draining in-flight queries (it maps to the legacy
	// shutdown_wait_unfinished inside the engine). Without a preStop hook,
	// SIGTERM arrives immediately; the engine uses this window before SIGKILL.
	// The margin covers container runtime teardown after the process exits.
	shutdownWait := gracePeriod - int64(EngineShutdownMarginSeconds)
	if shutdownWait < 1 {
		shutdownWait = gracePeriod - 1
	}
	if shutdownWait < 1 {
		shutdownWait = 1
	}
	// Canonical document seeded with operator-managed defaults, shaped to
	// the structured YAML schema consumed by FireboltCoreServer (packdb
	// `DB::Config::ApplicationConfig`). User input from spec.customEngineConfig
	// is deep-merged into this at the root, so users may add/override keys
	// in any top-level section (auth, engine, instance, logging). Operator-
	// authoritative paths are stripped from user input before the merge (see
	// stripProtectedEngineConfigPaths) so they cannot be overridden —
	// silently, to keep the same spec portable across operator versions even
	// if the protected set evolves.
	//
	// instance.id is a Ulid that the engine propagates to all four legacy
	// identity fields (account_id, account_name, organization_id,
	// organization_name); InstanceInfo.AccountID already carries the
	// instance's ULID for this purpose.
	coreConfig := map[string]interface{}{
		"schema_version": EngineConfigSchemaVersion,
		"instance": map[string]interface{}{
			"id":   instanceInfo.AccountID,
			"type": "multi_engine",
			"multi_engine": map[string]interface{}{
				"metadata_endpoint": metadataEndpoint,
			},
		},
		"engine": map[string]interface{}{
			"id":                       engineName,
			"nodes":                    nodes,
			"termination_grace_period": fmt.Sprintf("%ds", shutdownWait),
		},
		"logging": map[string]interface{}{
			"format": "json",
		},
	}

	if spec.CustomEngineConfig != nil && len(spec.CustomEngineConfig.Raw) > 0 {
		var custom map[string]interface{}
		// Schemaless+Type=object on the CRD constrains valid input to a
		// JSON object; any unmarshal failure here means the apiserver
		// admitted something it should have rejected, so silently
		// skipping the merge is the conservative choice.
		if err := json.Unmarshal(spec.CustomEngineConfig.Raw, &custom); err == nil {
			stripProtectedEngineConfigPaths(custom)
			deepMergeJSON(coreConfig, custom)
		}
	}

	configYAML, err := yaml.Marshal(coreConfig)
	if err != nil {
		panic(fmt.Sprintf("BUG: failed to marshal config.yaml: %v", err))
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
			ConfigFileName: string(configYAML),
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
	if h := customEngineConfigHash(spec); h != "" {
		annotations[AnnotationCustomEngineConfigHash] = h
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
	podSecurityContext := getEnginePodSecurityContext(spec)
	containerSecurityContext := getEngineContainerSecurityContext(spec)

	volumeMounts := []corev1.VolumeMount{
		{
			Name:      "nodes-config",
			MountPath: ConfigMountPath,
			SubPath:   ConfigFileName,
			ReadOnly:  true,
		},
		{
			Name:      DataVolumeName,
			MountPath: DataMountPath,
		},
	}
	// The "data" volume backing /firebolt-core/volume is either a per-pod
	// PVC (the default; the StatefulSet controller synthesizes the pod
	// Volume from the VolumeClaimTemplate) or a node-local emptyDir /
	// hostPath that we add to pod.spec.volumes explicitly.
	var (
		volumeClaimTemplates []corev1.PersistentVolumeClaim
		extraDataVolume      *corev1.Volume
	)
	switch resolveStorageBackend(spec.Storage) {
	case BackendEmptyDir:
		// spec.Storage.EmptyDir may be nil when resolveStorageBackend
		// fell through to the default (no backend set); render a
		// bare emptyDir in that case.
		var ed computev1alpha1.EngineEmptyDirSpec
		if spec.Storage.EmptyDir != nil {
			ed = *spec.Storage.EmptyDir
		}
		extraDataVolume = &corev1.Volume{
			Name: DataVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{
					Medium:    ed.Medium,
					SizeLimit: ed.SizeLimit,
				},
			},
		}
	case BackendHostPath:
		hp := spec.Storage.HostPath
		extraDataVolume = &corev1.Volume{
			Name: DataVolumeName,
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: hp.Path,
					Type: hp.Type,
				},
			},
		}
	case BackendPersistentVolumeClaim:
		pvc := resolvePersistentVolumeClaimDefaults(spec.Storage.PersistentVolumeClaim)
		volumeClaimTemplates = []corev1.PersistentVolumeClaim{{
			ObjectMeta: metav1.ObjectMeta{
				Name:   DataVolumeName,
				Labels: labels,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: pvc.AccessModes,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: pvc.Size,
					},
				},
				StorageClassName: pvc.StorageClassName,
			},
		}}
	default:
		// Unreachable while resolveStorageBackend only returns the wired
		// backends; future backends fill this in.
	}
	podVolumes := []corev1.Volume{
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
	}
	if extraDataVolume != nil {
		podVolumes = append(podVolumes, *extraDataVolume)
	}

	// Reclaim per-pod PVCs when the (old-generation) StatefulSet is deleted
	// during blue-green cleaning. WhenScaled stays Retain so a within-
	// generation scale-down does not silently drop a node's data.
	pvcRetentionPolicy := &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:                          headlessSvcName,
			Replicas:                             &spec.Replicas,
			PodManagementPolicy:                  appsv1.ParallelPodManagement,
			Selector:                             &metav1.LabelSelector{MatchLabels: labels},
			VolumeClaimTemplates:                 volumeClaimTemplates,
			PersistentVolumeClaimRetentionPolicy: pvcRetentionPolicy,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            enginePodServiceAccountName(spec),
					NodeSelector:                  spec.NodeSelector,
					Tolerations:                   spec.Tolerations,
					TerminationGracePeriodSeconds: &gracePeriod,
					SecurityContext:               podSecurityContext,
					Containers: []corev1.Container{
						{
							Name:            ContainerNameEngine,
							Image:           image,
							ImagePullPolicy: pullPolicy,
							SecurityContext: containerSecurityContext,
							Resources:       engineContainerResources(spec),
							Env: []corev1.EnvVar{
								{
									Name: "POD_INDEX",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											// Set explicitly so it matches the API-server-defaulted value on read-back.
											APIVersion: "v1",
											FieldPath:  "metadata.labels['apps.kubernetes.io/pod-index']",
										},
									},
								},
								// Allows the default AWS SDK EC2 metadata detection (required for IRSA).
								{
									Name:  "FIREBOLT_ALLOW_AWS_IRSA",
									Value: "true",
								},
								// Selects the firebolt-core code path inside the unified
								// `firebolt` binary (packdb FB-914): the operator-rendered
								// config (config.yaml at the data-dir root) is honored as-is
								// and not rewritten at startup.
								{
									Name:  "FIREBOLT_CORE_MODE",
									Value: "1",
								},
							},
							Ports:        GetContainerPorts(),
							Command:      []string{"/bin/bash", "-c"},
							Args:         []string{strings.TrimSpace(EngineStartupScript)},
							VolumeMounts: volumeMounts,
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
					Volumes: podVolumes,
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

// enginePodServiceAccountName returns the ServiceAccount name stamped on engine
// pods, or "" when unset (namespace default).
func enginePodServiceAccountName(spec *computev1alpha1.FireboltEngineSpec) string {
	if spec.ServiceAccountName == nil || *spec.ServiceAccountName == "" {
		return ""
	}
	return *spec.ServiceAccountName
}

// deepMergeJSON merges src into dst recursively. When both sides hold a
// nested JSON object (map[string]interface{}) at the same key, the merge
// recurses into it. For any other value type (arrays, primitives, mixed
// types), src wins and replaces whatever is at that key in dst. dst is
// mutated in place.
func deepMergeJSON(dst, src map[string]interface{}) {
	for k, srcVal := range src {
		if srcMap, ok := srcVal.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				deepMergeJSON(dstMap, srcMap)
				continue
			}
		}
		dst[k] = srcVal
	}
}

// stripProtectedEngineConfigPaths removes paths the operator manages from
// a user-supplied customEngineConfig payload. Stripping happens in place
// before the deep merge in buildConfigMap, and is silent — users keep the
// same spec portable across operator versions even if the protected set
// changes.
//
// When a top-level section (`instance`, `engine`) is not a JSON object the
// whole key is dropped: deepMergeJSON would otherwise replace the operator-
// built section wholesale with the user's scalar, losing every authoritative
// key.
func stripProtectedEngineConfigPaths(m map[string]interface{}) {
	delete(m, "schema_version")

	if instance, ok := m["instance"].(map[string]interface{}); ok {
		delete(instance, "id")
		delete(instance, "type")
		delete(instance, "multi_engine")
	} else {
		delete(m, "instance")
	}

	if engine, ok := m["engine"].(map[string]interface{}); ok {
		delete(engine, "id")
		delete(engine, "nodes")
		delete(engine, "termination_grace_period")
	} else {
		delete(m, "engine")
	}
}

// customEngineConfigHash returns a stable hash of spec.customEngineConfig
// suitable for stamping onto the engine StatefulSet so stsMatchesSpec can
// detect drift. Returns "" when no custom config is set.
func customEngineConfigHash(spec *computev1alpha1.FireboltEngineSpec) string {
	if spec.CustomEngineConfig == nil || len(spec.CustomEngineConfig.Raw) == 0 {
		return ""
	}
	// Re-marshal through map[string]interface{} so semantically equal
	// payloads (whitespace, key order) hash to the same value and don't
	// trigger spurious generations.
	var custom map[string]interface{}
	if err := json.Unmarshal(spec.CustomEngineConfig.Raw, &custom); err != nil {
		return contentHash(string(spec.CustomEngineConfig.Raw))
	}
	canonical, err := json.Marshal(custom)
	if err != nil {
		return contentHash(string(spec.CustomEngineConfig.Raw))
	}
	return contentHash(string(canonical))
}

// getEnginePodSecurityContext returns the resolved pod-level security context
// for engine pods. The operator's default fsGroup (3473) is applied when the
// user-supplied spec.PodSecurityContext leaves it unset; FSGroupChangePolicy
// defaults to OnRootMismatch so kubelet skips the recursive chown on every
// mount, keeping pod startup fast on restart. All other fields are
// pass-through. The result is independent of spec (deep-copied) so callers
// can mutate it safely.
func getEnginePodSecurityContext(spec *computev1alpha1.FireboltEngineSpec) *corev1.PodSecurityContext {
	var psc *corev1.PodSecurityContext
	if spec.PodSecurityContext != nil {
		psc = spec.PodSecurityContext.DeepCopy()
	} else {
		psc = &corev1.PodSecurityContext{}
	}
	if psc.FSGroup == nil {
		fsg := DefaultEngineFSGroup
		psc.FSGroup = &fsg
	}
	if psc.FSGroupChangePolicy == nil {
		policy := corev1.FSGroupChangeOnRootMismatch
		psc.FSGroupChangePolicy = &policy
	}
	return psc
}

// getEngineContainerSecurityContext returns the container-level security
// context for the engine container. Pure pass-through of spec.SecurityContext;
// the operator stamps no defaults at the container scope.
func getEngineContainerSecurityContext(spec *computev1alpha1.FireboltEngineSpec) *corev1.SecurityContext {
	if spec.SecurityContext == nil {
		return nil
	}
	return spec.SecurityContext.DeepCopy()
}

func engineContainerResources(spec *computev1alpha1.FireboltEngineSpec) corev1.ResourceRequirements {
	return *spec.Resources.DeepCopy()
}

func resourceRequirementsEqual(actual, desired corev1.ResourceRequirements) bool {
	return resourceListEqual(actual.Requests, desired.Requests) &&
		resourceListEqual(actual.Limits, desired.Limits) &&
		reflect.DeepEqual(actual.Claims, desired.Claims)
}

func resourceListEqual(actual, desired corev1.ResourceList) bool {
	if len(actual) != len(desired) {
		return false
	}
	for name, desiredQuantity := range desired {
		actualQuantity, ok := actual[name]
		if !ok || !actualQuantity.Equal(desiredQuantity) {
			return false
		}
	}
	return true
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

	if !resourceRequirementsEqual(container.Resources, engineContainerResources(spec)) {
		return false
	}

	if !reflect.DeepEqual(podSpec.NodeSelector, spec.NodeSelector) {
		return false
	}

	if !reflect.DeepEqual(podSpec.Tolerations, spec.Tolerations) {
		return false
	}

	if podSpec.ServiceAccountName != enginePodServiceAccountName(spec) {
		return false
	}

	expectedGracePeriod := getTerminationGracePeriod(spec)
	if podSpec.TerminationGracePeriodSeconds == nil ||
		*podSpec.TerminationGracePeriodSeconds != expectedGracePeriod {
		return false
	}

	if !reflect.DeepEqual(podSpec.SecurityContext, getEnginePodSecurityContext(spec)) {
		return false
	}
	if !reflect.DeepEqual(container.SecurityContext, getEngineContainerSecurityContext(spec)) {
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

	if sts.Annotations[AnnotationCustomEngineConfigHash] != customEngineConfigHash(spec) {
		return false
	}

	if !storageMatchesSpec(sts, spec) {
		return false
	}

	return true
}

// storageMatchesSpec reports whether the STS's VolumeClaimTemplates match the
// engine's resolved storage spec. VolumeClaimTemplates are immutable on a STS,
// so any drift (resizing, switching access modes or storage class) must
// trigger a new blue-green generation.
func storageMatchesSpec(sts *appsv1.StatefulSet, spec *computev1alpha1.FireboltEngineSpec) bool {
	dataVol := findDataPodVolume(sts)
	switch resolveStorageBackend(spec.Storage) {
	case BackendEmptyDir:
		if len(sts.Spec.VolumeClaimTemplates) != 0 {
			return false
		}
		if dataVol == nil || dataVol.EmptyDir == nil {
			return false
		}
		// spec.Storage.EmptyDir may be nil when resolveStorageBackend
		// fell through to the default (no backend set); compare
		// against a bare EngineEmptyDirSpec{} in that case.
		var ed computev1alpha1.EngineEmptyDirSpec
		if spec.Storage.EmptyDir != nil {
			ed = *spec.Storage.EmptyDir
		}
		if dataVol.EmptyDir.Medium != ed.Medium {
			return false
		}
		return quantityPtrEqual(dataVol.EmptyDir.SizeLimit, ed.SizeLimit)
	case BackendHostPath:
		if len(sts.Spec.VolumeClaimTemplates) != 0 {
			return false
		}
		if dataVol == nil || dataVol.HostPath == nil {
			return false
		}
		hp := spec.Storage.HostPath
		if dataVol.HostPath.Path != hp.Path {
			return false
		}
		return reflect.DeepEqual(dataVol.HostPath.Type, hp.Type)
	case BackendPersistentVolumeClaim:
		if dataVol != nil {
			return false
		}
		if len(sts.Spec.VolumeClaimTemplates) == 0 {
			return false
		}
		pvc := resolvePersistentVolumeClaimDefaults(spec.Storage.PersistentVolumeClaim)
		vct := sts.Spec.VolumeClaimTemplates[0]
		if vct.Name != DataVolumeName {
			return false
		}
		currentSize := vct.Spec.Resources.Requests[corev1.ResourceStorage]
		if !currentSize.Equal(pvc.Size) {
			return false
		}
		if !reflect.DeepEqual(vct.Spec.AccessModes, pvc.AccessModes) {
			return false
		}
		return reflect.DeepEqual(vct.Spec.StorageClassName, pvc.StorageClassName)
	default:
		return false
	}
}

// findDataPodVolume returns the pod-template Volume named DataVolumeName,
// or nil when only the VCT-synthesized PVC backs the mount.
func findDataPodVolume(sts *appsv1.StatefulSet) *corev1.Volume {
	for i := range sts.Spec.Template.Spec.Volumes {
		v := &sts.Spec.Template.Spec.Volumes[i]
		if v.Name == DataVolumeName {
			return v
		}
	}
	return nil
}

// quantityPtrEqual compares two *resource.Quantity by value, treating nil
// and a zero quantity as distinct (matches the kubelet's nil-vs-set
// semantics for EmptyDir.SizeLimit).
func quantityPtrEqual(a, b *resource.Quantity) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// StorageBackend identifies which of the EngineStorageSpec sibling pointers
// the operator should honor for a given FireboltEngine. The full enum is
// declared up front (BackendEmptyDir / BackendHostPath are wired in
// follow-up commits) so the switch infrastructure can settle separately
// from the per-backend wiring.
type StorageBackend string

// StorageBackend values, one per EngineStorageSpec sibling pointer.
const (
	BackendPersistentVolumeClaim StorageBackend = "PersistentVolumeClaim"
	BackendEmptyDir              StorageBackend = "EmptyDir"
	BackendHostPath              StorageBackend = "HostPath"
)

// resolveStorageBackend returns the effective backend for an
// EngineStorageSpec. If no sibling pointer is set we default to
// BackendEmptyDir: engine data at /firebolt-core/volume is regenerable
// cache (authoritative state lives in the metadata service and the
// managed-table S3 bucket), so an ephemeral pod-local volume is the
// safe default and avoids a hard dependency on a dynamic-provisioner
// StorageClass. FireboltEngines that want a durable PVC must opt in
// explicitly via spec.storage.persistentVolumeClaim. The CEL
// XValidation rule on EngineStorageSpec still guarantees at most one
// pointer is non-nil at admission time.
func resolveStorageBackend(s computev1alpha1.EngineStorageSpec) StorageBackend {
	switch {
	case s.PersistentVolumeClaim != nil:
		return BackendPersistentVolumeClaim
	case s.HostPath != nil:
		return BackendHostPath
	default:
		return BackendEmptyDir
	}
}

// resolvePersistentVolumeClaimDefaults returns the effective per-pod PVC
// configuration for the engine. nil input (no PersistentVolumeClaim sub-spec
// set, or the whole EngineStorageSpec omitted) is treated as "accept all
// defaults" — 1Gi, ReadWriteOnce, cluster-default StorageClass — which
// preserves the operator's behavior for FireboltEngines that don't
// configure storage explicitly. The kubebuilder defaults on
// EnginePersistentVolumeClaimSpec only fire when the parent sub-struct is
// present in the user's manifest, so the controller has to backfill them
// for the implicit-default case.
func resolvePersistentVolumeClaimDefaults(p *computev1alpha1.EnginePersistentVolumeClaimSpec) computev1alpha1.EnginePersistentVolumeClaimSpec {
	out := computev1alpha1.EnginePersistentVolumeClaimSpec{
		Size:        resource.MustParse(DefaultEngineStorageSize),
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
	}
	if p == nil {
		return out
	}
	if !p.Size.IsZero() {
		out.Size = p.Size
	}
	if len(p.AccessModes) > 0 {
		out.AccessModes = p.AccessModes
	}
	if p.StorageClassName != nil {
		out.StorageClassName = p.StorageClassName
	}
	return out
}
