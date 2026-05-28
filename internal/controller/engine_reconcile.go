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

// InstanceInfo holds the multi-engine endpoint and instance ID (a ULID)
// resolved from the FireboltInstance in the engine's namespace. These are
// injected into the engine ConfigMap so engine nodes can connect to the
// metadata service and stamp the correct identity.
type InstanceInfo struct {
	MetadataEndpoint string
	InstanceID       string
}

// EngineClassInfo carries the resolved EngineClass template plus a
// content hash. The template is merged underneath the operator-built
// pod spec in buildStatefulSet so engines can inherit shared pod-level
// settings (serviceAccountName, nodeSelector, tolerations, pod
// annotations, affinity, sidecars, init containers) without restating
// them per engine. The hash is stamped on the StatefulSet as
// AnnotationEngineClassHash and consumed by stsMatchesSpec to detect
// drift when either spec.engineClassRef or the referenced class's
// spec.template content changes.
//
// A nil *EngineClassInfo means the engine has no engineClassRef set;
// the merge is a no-op and the hash annotation is absent.
type EngineClassInfo struct {
	// Name is the class name copied from engine.spec.engineClassRef.
	Name string

	// Template is the EngineClass.spec.template ready to merge.
	Template *corev1.PodTemplateSpec

	// Hash is a stable content hash of Template. Used as the value of
	// AnnotationEngineClassHash on the rendered StatefulSet so drift is
	// detected by stsMatchesSpec when the class spec changes.
	Hash string
}

// newEngineClassInfo wraps an EngineClass into an EngineClassInfo for
// downstream consumers. Returns nil when ec is nil so callers can pass
// the result through unconditionally.
func newEngineClassInfo(ec *computev1alpha1.EngineClass) *EngineClassInfo {
	if ec == nil {
		return nil
	}
	raw, err := json.Marshal(ec.Spec.Template)
	hash := ""
	if err == nil {
		hash = contentHash(string(raw))
	}
	tmpl := ec.Spec.Template.DeepCopy()
	return &EngineClassInfo{
		Name:     ec.Name,
		Template: tmpl,
		Hash:     hash,
	}
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
	classInfo *EngineClassInfo,
) EngineReconcileResult {
	result := EngineReconcileResult{
		Status: *status.DeepCopy(),
	}

	switch status.Phase {
	case "", computev1alpha1.PhaseStable, computev1alpha1.PhaseStopped:
		computeStable(spec, &result, current, engineName, engineNamespace, metadataGeneration, instanceInfo, classInfo)
	case computev1alpha1.PhaseCreating:
		computeCreating(spec, &result, current, engineName, engineNamespace, instanceInfo, classInfo)
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
	classInfo *EngineClassInfo,
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
	if current.CurrentSTS == nil || !stsMatchesSpec(current.CurrentSTS, spec, classInfo) {
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

	// Always rebuild from buildClusterService instead of DeepCopying the
	// live Service. With Server-Side Apply, the desired object passed to
	// client.Patch(Apply) must have metadata.managedFields == nil; a
	// DeepCopy of a live object carries the apiserver-populated
	// managedFields and the apply is rejected with
	// "metadata.managedFields must be nil". buildClusterService produces
	// a fresh object owning exactly the fields the operator manages
	// (selector, ports, ClusterIPNone, PublishNotReadyAddresses); SSA
	// preserves any foreign-managed fields a user added separately.
	if current.ClusterService == nil || current.ClusterServiceTargetGen != gen {
		r.EnsureClusterSvc = buildClusterService(engineName, engineNamespace, gen)
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
	classInfo *EngineClassInfo,
) {
	status := &r.Status
	gen := status.CurrentGeneration

	// Must be checked before CurrentPodsReady; see "Ordering invariant" above.
	if current.CurrentSTS != nil && !stsMatchesSpec(current.CurrentSTS, spec, classInfo) {
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

	buildGenResources(spec, r, engineName, engineNamespace, gen, instanceInfo, classInfo)

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
		// See computeStable for why we rebuild via buildClusterService
		// instead of DeepCopying the live Service: SSA Apply rejects a
		// desired object whose metadata.managedFields is non-nil, and a
		// DeepCopy of the live object always carries managedFields.
		r.EnsureClusterSvc = buildClusterService(engineName, engineNamespace, gen)
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
	classInfo *EngineClassInfo,
) {
	r.EnsureConfigMap = buildConfigMap(spec, engineName, engineNamespace, gen, instanceInfo)
	r.EnsureHeadlessSvc = buildHeadlessService(engineName, engineNamespace, gen)
	r.EnsureStatefulSet = buildStatefulSet(spec, engineName, engineNamespace, gen, classInfo)
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
	shutdownWait := engineShutdownWaitSeconds(gracePeriod)
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
	// organization_name); InstanceInfo.InstanceID already carries the
	// FireboltInstance's ULID for this purpose.
	coreConfig := map[string]interface{}{
		"schema_version": EngineConfigSchemaVersion,
		"instance": map[string]interface{}{
			"id":   instanceInfo.InstanceID,
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

// enginePodAnnotations returns the pod-template annotations to set on an
// engine StatefulSet. User-provided PodAnnotations are passed through
// verbatim, then any operator-managed annotations are layered on top so
// they always win against user collisions. Returns nil when neither
// source contributes an entry, to keep the rendered pod template
// byte-identical to its pre-feature shape (avoids a spurious StatefulSet
// rewrite on upgrade).
//
// The operator currently sets no annotations on the pod template itself
// (only on the StatefulSet's own ObjectMeta), so this helper passes
// PodAnnotations through unchanged today. The structure exists so that
// any future operator-owned pod-template annotation can be added in one
// place without re-opening the override-protection question.
func enginePodAnnotations(spec *computev1alpha1.FireboltEngineSpec) map[string]string {
	if len(spec.PodAnnotations) == 0 {
		return nil
	}
	annotations := make(map[string]string, len(spec.PodAnnotations))
	for k, v := range spec.PodAnnotations {
		annotations[k] = v
	}
	return annotations
}

func buildStatefulSet(spec *computev1alpha1.FireboltEngineSpec, engineName, namespace string, gen int, classInfo *EngineClassInfo) *appsv1.StatefulSet {
	name := genResourceName(engineName, gen, "")
	headlessSvcName := genResourceName(engineName, gen, SuffixHL)
	coreConfigName := genResourceName(engineName, gen, SuffixConfig)

	labels := map[string]string{
		LabelEngine:     engineName,
		LabelGeneration: strconv.Itoa(gen),
	}

	podLabels := effectivePodLabels(spec, engineName, gen, classInfo)
	podAnnotations := effectivePodAnnotations(spec, classInfo)

	annotations := map[string]string{}
	if spec.MetadataEndpointOverride != nil {
		annotations[AnnotationMetadataOverride] = *spec.MetadataEndpointOverride
	}
	if h := customEngineConfigHash(spec); h != "" {
		annotations[AnnotationCustomEngineConfigHash] = h
	}
	if classInfo != nil {
		annotations[AnnotationEngineClassHash] = classInfo.Hash
	}

	// Engine container image flows from the referenced EngineClass's
	// containers[name=="engine"].image when set, falling back to the
	// operator's embedded default.
	image, pullPolicy := effectiveEngineImage(classInfo)

	gracePeriod := getTerminationGracePeriod(spec)
	podSecurityContext := effectivePodSecurityContext(spec, classInfo)
	containerSecurityContext := effectiveEngineContainerSecurityContext(spec, classInfo)

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
	volumeMounts = append(volumeMounts, engineClassEngineVolumeMounts(classInfo)...)
	engineEnv := []corev1.EnvVar{
		{
			Name: computev1alpha1.EnginePodIndexEnvKey,
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
			Name:  computev1alpha1.EngineAwsEC2MetadataClientEnabledEnvKey,
			Value: "true",
		},
		// Selects the firebolt-core code path inside the unified
		// `firebolt` binary (packdb FB-914): the operator-rendered
		// config (config.yaml at the data-dir root) is honored as-is
		// and not rewritten at startup.
		{
			Name:  computev1alpha1.EngineCoreModeEnvKey,
			Value: "1",
		},
	}
	engineEnv = append(engineEnv, engineClassEngineEnv(classInfo)...)
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
	podVolumes = appendClassPodVolumes(podVolumes, classInfo)

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
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            effectiveServiceAccountName(spec, classInfo),
					NodeSelector:                  effectiveNodeSelector(spec, classInfo),
					Tolerations:                   effectiveTolerations(spec, classInfo),
					Affinity:                      effectiveAffinity(spec, classInfo),
					InitContainers:                effectiveInitContainers(spec, classInfo),
					TerminationGracePeriodSeconds: &gracePeriod,
					SecurityContext:               podSecurityContext,
					// Suppress legacy Docker-link env vars (`<SVC>_SERVICE_HOST`,
					// `<SVC>_PORT=tcp://...`, etc.) the kubelet would otherwise
					// inject for every Service in the namespace. DNS is the
					// real service-discovery channel here; the auto-injected
					// vars are dead weight that also risks colliding with
					// firebolt-core's own config keys (cf. floci's
					// `FLOCI_PORT` collision in FB-1215).
					EnableServiceLinks: boolPtr(false),
					ImagePullSecrets:   effectiveImagePullSecrets(classInfo),
					Containers: append([]corev1.Container{
						{
							Name:            computev1alpha1.EngineContainerName,
							Image:           image,
							ImagePullPolicy: pullPolicy,
							SecurityContext: containerSecurityContext,
							Resources:       effectiveEngineResources(spec, classInfo),
							Env:             engineEnv,
							EnvFrom:         engineClassEngineEnvFrom(classInfo),
							Ports:           GetContainerPorts(),
							Command:         []string{"/bin/bash", "-c"},
							Args:            []string{strings.TrimSpace(EngineStartupScript)},
							VolumeMounts:    volumeMounts,
							Lifecycle:       effectiveEngineLifecycle(classInfo),
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
					}, engineClassSidecars(classInfo)...),
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

// engineShutdownWaitSeconds returns the engine's post-SIGTERM in-flight-query
// budget: the pod grace period minus EngineShutdownMarginSeconds so the engine
// exits before the kubelet escalates to SIGKILL, floored at 1s. A single clamp
// keeps the result monotonic non-decreasing in gracePeriod.
func engineShutdownWaitSeconds(gracePeriod int64) int64 {
	shutdownWait := gracePeriod - int64(EngineShutdownMarginSeconds)
	if shutdownWait < 1 {
		shutdownWait = 1
	}
	return shutdownWait
}

// enginePodServiceAccountName returns the ServiceAccount name stamped on engine
// pods, or "" when unset (namespace default).
func enginePodServiceAccountName(spec *computev1alpha1.FireboltEngineSpec) string {
	if spec.ServiceAccountName == nil || *spec.ServiceAccountName == "" {
		return ""
	}
	return *spec.ServiceAccountName
}

// effectiveServiceAccountName resolves the SA stamped on the engine pod
// template. Precedence: engine spec > EngineClass template > "" (default
// namespace SA). The same helper is used by buildStatefulSet and by
// stsMatchesSpec so the rendered value and the drift comparator stay
// aligned.
func effectiveServiceAccountName(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) string {
	if sa := enginePodServiceAccountName(spec); sa != "" {
		return sa
	}
	if classInfo != nil && classInfo.Template != nil && classInfo.Template.Spec.ServiceAccountName != "" {
		return classInfo.Template.Spec.ServiceAccountName
	}
	return ""
}

// effectiveNodeSelector merges the class template's nodeSelector with the
// engine spec's. Engine keys win on conflict. Nil-safe: returns nil when
// neither contributes anything, so a freshly built STS with no scheduling
// hints compares equal to the engine pod spec on read-back.
func effectiveNodeSelector(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) map[string]string {
	var classSel map[string]string
	if classInfo != nil && classInfo.Template != nil {
		classSel = classInfo.Template.Spec.NodeSelector
	}
	if len(classSel) == 0 && len(spec.NodeSelector) == 0 {
		return nil
	}
	out := make(map[string]string, len(classSel)+len(spec.NodeSelector))
	for k, v := range classSel {
		out[k] = v
	}
	for k, v := range spec.NodeSelector {
		out[k] = v
	}
	return out
}

// effectiveTolerations concatenates the class template's tolerations with
// the engine spec's. Class tolerations come first so the engine spec's
// order is preserved when callers iterate the slice; equivalence with
// stsMatchesSpec is by-element via reflect.DeepEqual so the order does
// not produce false drift.
func effectiveTolerations(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) []corev1.Toleration {
	var classTols []corev1.Toleration
	if classInfo != nil && classInfo.Template != nil {
		classTols = classInfo.Template.Spec.Tolerations
	}
	if len(classTols) == 0 && len(spec.Tolerations) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, 0, len(classTols)+len(spec.Tolerations))
	out = append(out, classTols...)
	out = append(out, spec.Tolerations...)
	return out
}

// effectiveAffinity returns the engine spec's affinity when set, else the
// class template's. Affinity is not field-merged: the value is a deeply
// nested struct (node / pod / pod-anti) whose semantics depend on every
// term agreeing, so partial overrides are dangerous. Whoever sets the
// field owns the whole tree.
func effectiveAffinity(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) *corev1.Affinity {
	if spec.Affinity != nil {
		return spec.Affinity
	}
	if classInfo != nil && classInfo.Template != nil {
		return classInfo.Template.Spec.Affinity
	}
	return nil
}

// effectivePodLabels merges the operator-built labels with class template
// labels and engine spec.PodLabels. Precedence: operator-built (reserved
// keys) > engine spec.PodLabels > class template labels. The two reserved
// keys (LabelEngine, LabelGeneration) are non-overridable so the
// blue-green selector machinery cannot be detached by a user typo.
func effectivePodLabels(spec *computev1alpha1.FireboltEngineSpec, engineName string, gen int, classInfo *EngineClassInfo) map[string]string {
	labels := map[string]string{
		LabelEngine:     engineName,
		LabelGeneration: strconv.Itoa(gen),
	}
	if classInfo != nil && classInfo.Template != nil {
		for k, v := range classInfo.Template.Labels {
			if k == LabelEngine || k == LabelGeneration {
				continue
			}
			labels[k] = v
		}
	}
	for k, v := range spec.PodLabels {
		if k == LabelEngine || k == LabelGeneration {
			continue
		}
		labels[k] = v
	}
	return labels
}

// effectivePodAnnotations merges class template annotations with engine
// spec.PodAnnotations. Engine annotations win on conflict. Returns nil
// when neither side contributes; mirrors enginePodAnnotations's existing
// nil-return so stsMatchesSpec equality stays clean.
func effectivePodAnnotations(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) map[string]string {
	var classAnno map[string]string
	if classInfo != nil && classInfo.Template != nil {
		classAnno = classInfo.Template.Annotations
	}
	if len(classAnno) == 0 && len(spec.PodAnnotations) == 0 {
		return nil
	}
	out := make(map[string]string, len(classAnno)+len(spec.PodAnnotations))
	for k, v := range classAnno {
		out[k] = v
	}
	for k, v := range spec.PodAnnotations {
		out[k] = v
	}
	return out
}

// effectiveEngineImage resolves the engine container image. If the class
// template carries a non-empty image on the engine container (name
// EngineContainerName), it wins; otherwise the operator's embedded
// default is used. The EngineClass webhook does not reject image on the
// engine container, so this is the documented user-extension point.
func effectiveEngineImage(classInfo *EngineClassInfo) (image string, pullPolicy corev1.PullPolicy) {
	defaultImage := resolveImageRef(nil, DefaultEngineRepository, DefaultEngineTag)
	defaultPullPolicy := resolveImagePullPolicy(nil)
	if classInfo == nil || classInfo.Template == nil {
		return defaultImage, defaultPullPolicy
	}
	for i := range classInfo.Template.Spec.Containers {
		c := &classInfo.Template.Spec.Containers[i]
		if c.Name != computev1alpha1.EngineContainerName {
			continue
		}
		if c.Image != "" {
			defaultImage = c.Image
		}
		if c.ImagePullPolicy != "" {
			defaultPullPolicy = c.ImagePullPolicy
		}
		return defaultImage, defaultPullPolicy
	}
	return defaultImage, defaultPullPolicy
}

// engineClassSidecars returns the sidecar containers (every container
// whose name is not EngineContainerName) from the class template. The
// EngineClass webhook leaves these fully user-owned, so the result is
// passed through verbatim — image, command, ports, env, mounts.
func engineClassSidecars(classInfo *EngineClassInfo) []corev1.Container {
	if classInfo == nil || classInfo.Template == nil {
		return nil
	}
	out := make([]corev1.Container, 0, len(classInfo.Template.Spec.Containers))
	for i := range classInfo.Template.Spec.Containers {
		c := classInfo.Template.Spec.Containers[i]
		if c.Name == computev1alpha1.EngineContainerName {
			continue
		}
		copied := *c.DeepCopy()
		// Stamp API-server defaults so the built pod template matches
		// what stsMatchesSpec will read back. Without this, every
		// reconcile sees the API-server-filled imagePullPolicy /
		// terminationMessage* as drift and rolls a fresh blue-green
		// generation forever.
		applyContainerAPIServerDefaults(&copied)
		out = append(out, copied)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// engineClassInitContainers returns the init containers from the class
// template, deep-copied so subsequent mutations of the result do not
// reach back into classInfo.
func engineClassInitContainers(classInfo *EngineClassInfo) []corev1.Container {
	if classInfo == nil || classInfo.Template == nil {
		return nil
	}
	if len(classInfo.Template.Spec.InitContainers) == 0 {
		return nil
	}
	out := make([]corev1.Container, len(classInfo.Template.Spec.InitContainers))
	for i := range classInfo.Template.Spec.InitContainers {
		out[i] = *classInfo.Template.Spec.InitContainers[i].DeepCopy()
	}
	return out
}

// effectiveInitContainers concatenates the EngineClass template's init
// containers and the engine spec's init containers, in that order, so
// shared bootstrap steps in the class run before engine-specific ones.
// Mirrors the class-then-spec precedence used by effectiveTolerations.
func effectiveInitContainers(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) []corev1.Container {
	classICs := engineClassInitContainers(classInfo)
	specICs := engineInitContainers(spec)
	if len(classICs) == 0 && len(specICs) == 0 {
		return nil
	}
	out := make([]corev1.Container, 0, len(classICs)+len(specICs))
	out = append(out, classICs...)
	out = append(out, specICs...)
	return out
}

// classEngineContainer returns the engine container (name=EngineContainerName)
// from the class template, or nil if no class is bound or the class
// template does not define one. The validating webhook guarantees at
// most one such container.
func classEngineContainer(classInfo *EngineClassInfo) *corev1.Container {
	if classInfo == nil || classInfo.Template == nil {
		return nil
	}
	for i := range classInfo.Template.Spec.Containers {
		c := &classInfo.Template.Spec.Containers[i]
		if c.Name == computev1alpha1.EngineContainerName {
			return c
		}
	}
	return nil
}

// effectiveEngineResources resolves the resources stamped on the engine
// container. Precedence: engine spec wins if it carries any
// requests / limits / claims; otherwise the class's engine container
// fills in. Whole-struct ownership (not per-resource-key merging) so a
// partial spec override does not silently inherit the rest from the
// class.
func effectiveEngineResources(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) corev1.ResourceRequirements {
	if computev1alpha1.HasContainerResources(spec.Resources) {
		return *spec.Resources.DeepCopy()
	}
	if c := classEngineContainer(classInfo); c != nil && computev1alpha1.HasContainerResources(c.Resources) {
		return *c.Resources.DeepCopy()
	}
	return corev1.ResourceRequirements{}
}

// engineClassEngineEnv returns the env vars declared on the class's
// engine container, deep-copied for downstream mutation safety. The
// validating webhook rejects reserved keys (POD_INDEX,
// FB_AWS_EC2_METADATA_CLIENT_ENABLED, FIREBOLT_CORE_MODE) so the result can be
// appended verbatim to the operator-injected list.
func engineClassEngineEnv(classInfo *EngineClassInfo) []corev1.EnvVar {
	c := classEngineContainer(classInfo)
	if c == nil || len(c.Env) == 0 {
		return nil
	}
	out := make([]corev1.EnvVar, len(c.Env))
	for i := range c.Env {
		c.Env[i].DeepCopyInto(&out[i])
	}
	return out
}

// engineClassEngineEnvFrom returns the envFrom entries declared on the
// class's engine container, deep-copied. Class-only — engine spec has no
// envFrom field.
func engineClassEngineEnvFrom(classInfo *EngineClassInfo) []corev1.EnvFromSource {
	c := classEngineContainer(classInfo)
	if c == nil || len(c.EnvFrom) == 0 {
		return nil
	}
	out := make([]corev1.EnvFromSource, len(c.EnvFrom))
	for i := range c.EnvFrom {
		c.EnvFrom[i].DeepCopyInto(&out[i])
	}
	return out
}

// engineClassEngineVolumeMounts returns the additional volumeMounts
// declared on the class's engine container. Entries whose name collides
// with an operator-reserved volume ("nodes-config", DataVolumeName) are
// skipped: the operator owns those mount paths and a class override
// would silently break startup (config volume) or data persistence
// (data volume).
func engineClassEngineVolumeMounts(classInfo *EngineClassInfo) []corev1.VolumeMount {
	c := classEngineContainer(classInfo)
	if c == nil || len(c.VolumeMounts) == 0 {
		return nil
	}
	out := make([]corev1.VolumeMount, 0, len(c.VolumeMounts))
	for i := range c.VolumeMounts {
		name := c.VolumeMounts[i].Name
		if name == "nodes-config" || name == DataVolumeName {
			continue
		}
		out = append(out, *c.VolumeMounts[i].DeepCopy())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// effectiveEngineContainerSecurityContext resolves the container-level
// SecurityContext on the engine container. Precedence: engine spec >
// class > operator-stamped hardened default. Whole-struct ownership: a
// non-nil spec or class value replaces the default wholesale so a
// partial override does not silently inherit unrelated fields.
//
// The hardened default (drop ALL capabilities, non-root UID/GID 3473,
// no privilege escalation) matches the sibling firebolt-instance-helm
// chart's engine StatefulSet, so an engine migrated between deployment
// paths keeps the same security posture.
func effectiveEngineContainerSecurityContext(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) *corev1.SecurityContext {
	if spec.SecurityContext != nil {
		return spec.SecurityContext.DeepCopy()
	}
	if c := classEngineContainer(classInfo); c != nil && c.SecurityContext != nil {
		return c.SecurityContext.DeepCopy()
	}
	return defaultEngineContainerSecurityContext()
}

// effectiveEngineLifecycle returns the Lifecycle hooks for the engine
// container, taken from the class. Class-only — engine spec has no
// Lifecycle field.
func effectiveEngineLifecycle(classInfo *EngineClassInfo) *corev1.Lifecycle {
	c := classEngineContainer(classInfo)
	if c == nil || c.Lifecycle == nil {
		return nil
	}
	return c.Lifecycle.DeepCopy()
}

// effectiveImagePullSecrets returns the pod-level imagePullSecrets from
// the class template, deep-copied. Class-only — engine spec has no
// equivalent. Typical use is to authenticate sidecar pulls from a
// private registry.
func effectiveImagePullSecrets(classInfo *EngineClassInfo) []corev1.LocalObjectReference {
	if classInfo == nil || classInfo.Template == nil {
		return nil
	}
	refs := classInfo.Template.Spec.ImagePullSecrets
	if len(refs) == 0 {
		return nil
	}
	out := make([]corev1.LocalObjectReference, len(refs))
	copy(out, refs)
	return out
}

// appendClassPodVolumes appends the class's pod-level volumes to the
// operator-rendered list. Volume names already present in operator are
// skipped: the operator owns DataVolumeName and "nodes-config" and a
// class redefinition would break those mounts.
func appendClassPodVolumes(operator []corev1.Volume, classInfo *EngineClassInfo) []corev1.Volume {
	if classInfo == nil || classInfo.Template == nil {
		return operator
	}
	classVols := classInfo.Template.Spec.Volumes
	if len(classVols) == 0 {
		return operator
	}
	reserved := make(map[string]struct{}, len(operator))
	for i := range operator {
		reserved[operator[i].Name] = struct{}{}
	}
	out := make([]corev1.Volume, 0, len(operator)+len(classVols))
	out = append(out, operator...)
	for i := range classVols {
		if _, ok := reserved[classVols[i].Name]; ok {
			continue
		}
		out = append(out, *classVols[i].DeepCopy())
	}
	return out
}

// effectivePodSecurityContext resolves the pod-level PodSecurityContext.
// Precedence: engine spec > class > operator default. Operator defaults
// (FSGroup, FSGroupChangePolicy) are stamped on top of whichever side
// won so the engine can write to the data volume and kubelet skips the
// recursive chown on every mount, regardless of how much (or little)
// context the user supplied.
func effectivePodSecurityContext(spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) *corev1.PodSecurityContext {
	var psc *corev1.PodSecurityContext
	switch {
	case spec.PodSecurityContext != nil:
		psc = spec.PodSecurityContext.DeepCopy()
	case classInfo != nil && classInfo.Template != nil && classInfo.Template.Spec.SecurityContext != nil:
		psc = classInfo.Template.Spec.SecurityContext.DeepCopy()
	default:
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
// When a top-level section (e.g. `instance`, `engine`) is not a JSON object
// the whole key is dropped: deepMergeJSON would otherwise replace the
// operator-built section wholesale with the user's scalar, losing every
// authoritative key.
//
// The owned-path set lives in
// computev1alpha1.OperatorOwnedEngineConfigPaths so that every templating
// surface (this function, the EngineClass webhook) reads from one
// declaration.
func stripProtectedEngineConfigPaths(m map[string]interface{}) {
	for _, owned := range computev1alpha1.OperatorOwnedEngineConfigPaths {
		if owned.Section == "" {
			for _, k := range owned.Keys {
				delete(m, k)
			}
			continue
		}
		if sub, ok := m[owned.Section].(map[string]interface{}); ok {
			for _, k := range owned.Keys {
				delete(sub, k)
			}
			continue
		}
		delete(m, owned.Section)
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

const defaultTerminationMessagePath = "/dev/termination-log"

// engineInitContainers returns a deep copy of spec.InitContainers for the pod
// template, or nil when unset so nil and empty compare equal in stsMatchesSpec.
// API-server defaults (imagePullPolicy, terminationMessagePath/Policy) are
// applied so a freshly-built STS compares equal to the same object read back
// from the API.
func engineInitContainers(spec *computev1alpha1.FireboltEngineSpec) []corev1.Container {
	if len(spec.InitContainers) == 0 {
		return nil
	}
	out := make([]corev1.Container, len(spec.InitContainers))
	for i := range spec.InitContainers {
		spec.InitContainers[i].DeepCopyInto(&out[i])
		applyContainerAPIServerDefaults(&out[i])
	}
	return out
}

// applyContainerAPIServerDefaults stamps the fields the API server fills
// in on every container at create time. Applied identically to init and
// regular containers (kubelet treats them the same way for these
// fields), so callers building either kind from user input pass the
// container through this helper to match what a read-back will show.
func applyContainerAPIServerDefaults(c *corev1.Container) {
	if c.TerminationMessagePath == "" {
		c.TerminationMessagePath = defaultTerminationMessagePath
	}
	if c.TerminationMessagePolicy == "" {
		c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = resolveContainerImagePullPolicy(c.Image, "")
	}
}

// normalizeContainer returns a deep copy of c with API-server-applied
// defaults stamped. Comparison-time helper used by containersEqualAfterDefaults.
func normalizeContainer(c *corev1.Container) corev1.Container {
	out := *c.DeepCopy()
	applyContainerAPIServerDefaults(&out)
	return out
}

// containersEqualAfterDefaults compares two container slices for drift,
// applying the same API-server defaults the kubelet would stamp so
// read-back from the API does not spuriously differ from the user spec.
// Used for both init containers and sidecars — the defaults are
// container-kind-agnostic.
func containersEqualAfterDefaults(actual, desired []corev1.Container) bool {
	if len(actual) == 0 && len(desired) == 0 {
		return true
	}
	if len(actual) != len(desired) {
		return false
	}
	for i := range desired {
		if !reflect.DeepEqual(normalizeContainer(&actual[i]), normalizeContainer(&desired[i])) {
			return false
		}
	}
	return true
}

// getEngineContainerSecurityContext is the no-class shorthand around
// effectiveEngineContainerSecurityContext kept solely for test fixtures
// (makeSTS / makeEmptyDirSTS) that pre-date the EngineClass merge layer.
// Production code paths use effectiveEngineContainerSecurityContext
// directly; do not introduce new callers of this wrapper.
func getEngineContainerSecurityContext(spec *computev1alpha1.FireboltEngineSpec) *corev1.SecurityContext {
	return effectiveEngineContainerSecurityContext(spec, nil)
}

// defaultEngineContainerSecurityContext returns the hardened
// container-level SecurityContext applied to the engine container when
// no user value is supplied. Mirrors the firebolt-instance-helm chart's
// engine StatefulSet (UID/GID 3473): non-root, no extra capabilities,
// no privilege escalation, writable root filesystem (the engine writes
// ephemeral files at startup; persistent state lives on the data PVC
// mounted separately).
func defaultEngineContainerSecurityContext() *corev1.SecurityContext {
	runAsUser := DefaultEngineUID
	runAsGroup := DefaultEngineGID
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(false),
		RunAsNonRoot:             boolPtr(true),
		RunAsUser:                &runAsUser,
		RunAsGroup:               &runAsGroup,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
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

// annotationsEqual compares two annotation maps for equality, treating a
// nil map as identical to an empty map. ObjectMeta.Annotations is
// json:"annotations,omitempty", so a round-trip through the API server
// can flip an empty map to nil or vice versa; reflect.DeepEqual would
// then report drift where there is none.
func annotationsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// stsMatchesSpec returns true if the StatefulSet matches all mutable fields
// in the engine spec. A mismatch triggers a new blue-green generation.
// classInfo carries the resolved EngineClass template (nil when the engine
// has no engineClassRef set); when present its hash is compared against
// AnnotationEngineClassHash and the merged pod-template fields drive the
// drift checks, so an in-place edit to the class spec or a flip to a
// different class produces a clean blue-green roll.
func stsMatchesSpec(sts *appsv1.StatefulSet, spec *computev1alpha1.FireboltEngineSpec, classInfo *EngineClassInfo) bool {
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != spec.Replicas {
		return false
	}
	podSpec := sts.Spec.Template.Spec
	if len(podSpec.Containers) == 0 {
		return false
	}
	container := podSpec.Containers[0]

	expectedImage, expectedPullPolicy := effectiveEngineImage(classInfo)
	if container.Image != expectedImage {
		return false
	}
	if container.ImagePullPolicy != expectedPullPolicy {
		return false
	}

	if !resourceRequirementsEqual(container.Resources, effectiveEngineResources(spec, classInfo)) {
		return false
	}

	if !reflect.DeepEqual(podSpec.NodeSelector, effectiveNodeSelector(spec, classInfo)) {
		return false
	}

	if !reflect.DeepEqual(podSpec.Tolerations, effectiveTolerations(spec, classInfo)) {
		return false
	}

	if !reflect.DeepEqual(podSpec.Affinity, effectiveAffinity(spec, classInfo)) {
		return false
	}

	if !sidecarsMatch(podSpec.Containers, engineClassSidecars(classInfo)) {
		return false
	}

	// Check pod template labels. The StatefulSet has the base labels plus
	// any user-provided PodLabels (and any from the class template). We
	// reconstruct the expected merged set using the engine name and
	// generation read from the StatefulSet's own labels.
	engineName, hasEngineName := sts.Labels[LabelEngine]
	genStr, hasGenLabel := sts.Labels[LabelGeneration]
	if !hasEngineName || !hasGenLabel {
		return false
	}
	gen, err := strconv.Atoi(genStr)
	if err != nil {
		return false
	}
	expectedPodLabels := effectivePodLabels(spec, engineName, gen, classInfo)
	if !reflect.DeepEqual(sts.Spec.Template.Labels, expectedPodLabels) {
		return false
	}

	// Pod-template annotations follow the same drift rule: any add /
	// remove / value change against the merged class+user map forces a
	// new generation. effectivePodAnnotations returns nil when neither
	// side contributes anything, which compares equal to a freshly-built
	// StatefulSet whose pod template has no annotations.
	if !annotationsEqual(sts.Spec.Template.Annotations, effectivePodAnnotations(spec, classInfo)) {
		return false
	}

	if podSpec.ServiceAccountName != effectiveServiceAccountName(spec, classInfo) {
		return false
	}

	expectedGracePeriod := getTerminationGracePeriod(spec)
	if podSpec.TerminationGracePeriodSeconds == nil ||
		*podSpec.TerminationGracePeriodSeconds != expectedGracePeriod {
		return false
	}

	if !reflect.DeepEqual(podSpec.SecurityContext, effectivePodSecurityContext(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(container.SecurityContext, effectiveEngineContainerSecurityContext(spec, classInfo)) {
		return false
	}

	if !containersEqualAfterDefaults(podSpec.InitContainers, effectiveInitContainers(spec, classInfo)) {
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

	expectedClassHash := ""
	if classInfo != nil {
		expectedClassHash = classInfo.Hash
	}
	if sts.Annotations[AnnotationEngineClassHash] != expectedClassHash {
		return false
	}

	if !storageMatchesSpec(sts, spec) {
		return false
	}

	return true
}

// sidecarsMatch compares the user-owned sidecar containers (those whose
// name is not EngineContainerName) against the expected sidecars from the
// EngineClass template. The engine container itself is compared
// field-by-field by stsMatchesSpec; sidecars are passed through verbatim
// from the class so a structural DeepEqual on the slice is sufficient.
func sidecarsMatch(actual []corev1.Container, expected []corev1.Container) bool {
	gotSidecars := make([]corev1.Container, 0, len(actual))
	for i := range actual {
		if actual[i].Name == computev1alpha1.EngineContainerName {
			continue
		}
		gotSidecars = append(gotSidecars, actual[i])
	}
	// Defaults-aware comparison: the API server stamps imagePullPolicy
	// and terminationMessage* on every sidecar at create time, so a raw
	// reflect.DeepEqual against the class template (which is the user-
	// supplied form, without defaults) would report drift on every
	// reconcile and roll a fresh blue-green generation indefinitely.
	return containersEqualAfterDefaults(gotSidecars, expected)
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
