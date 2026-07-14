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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

// FireboltEngineClassInfo carries the resolved FireboltEngineClass template
// plus a content hash. The template is merged underneath the operator-built
// pod spec in buildStatefulSet so engines can inherit shared pod-level
// settings (serviceAccountName, nodeSelector, tolerations, pod
// annotations, affinity, sidecars, init containers) without restating
// them per engine. The hash is stamped on the StatefulSet as
// AnnotationEngineClassHash and consumed by stsMatchesSpec to detect
// drift when either spec.engineClassRef or the referenced class's
// spec.template content changes.
//
// A nil *FireboltEngineClassInfo means the engine has no engineClassRef set;
// the merge is a no-op and the hash annotation is absent.
type FireboltEngineClassInfo struct {
	// Name is the class name copied from engine.spec.engineClassRef.
	Name string

	// Template is the FireboltEngineClass.spec.template ready to merge.
	Template *corev1.PodTemplateSpec

	// UISidecar, Storage, CustomEngineConfig, Rollout, DrainCheckEnabled,
	// DrainCheckInterval, and AutoStop carry the class's defaults for the
	// matching engine settings. They are consumed by the effective* helpers,
	// each of which resolves engine-if-set → class-if-set → operator default.
	// Copied verbatim from the class spec.
	UISidecar          *bool
	Storage            computev1alpha1.EngineStorageSpec
	CustomEngineConfig *apiextensionsv1.JSON
	Rollout            computev1alpha1.RolloutStrategy
	DrainCheckEnabled  *bool
	DrainCheckInterval *metav1.Duration
	AutoStop           *computev1alpha1.AutoStopSpec

	// Hash is a stable content hash of Template. Used as the value of
	// AnnotationEngineClassHash on the rendered StatefulSet so drift is
	// detected by stsMatchesSpec when the class template changes. Storage
	// and CustomEngineConfig drift is detected through their own
	// effective-aware comparators (storageMatchesSpec, customEngineConfigHash)
	// rather than this hash; Rollout / DrainCheckEnabled / DrainCheckInterval /
	// AutoStop do not reshape the StatefulSet and are read live, so they
	// are not hashed.
	Hash string
}

// newFireboltEngineClassInfo wraps a FireboltEngineClass into a
// FireboltEngineClassInfo for downstream consumers. Returns nil when ec is nil
// so callers can pass the result through unconditionally.
func newFireboltEngineClassInfo(ec *computev1alpha1.FireboltEngineClass) *FireboltEngineClassInfo {
	if ec == nil {
		return nil
	}
	raw, err := json.Marshal(ec.Spec.Template)
	hash := ""
	if err == nil {
		hash = contentHash(string(raw))
	}
	tmpl := ec.Spec.Template.DeepCopy()
	return &FireboltEngineClassInfo{
		Name:               ec.Name,
		Template:           tmpl,
		UISidecar:          ec.Spec.UISidecar,
		Storage:            *ec.Spec.Storage.DeepCopy(),
		CustomEngineConfig: ec.Spec.CustomEngineConfig.DeepCopy(),
		Rollout:            ec.Spec.Rollout,
		DrainCheckEnabled:  ec.Spec.DrainCheckEnabled,
		DrainCheckInterval: ec.Spec.DrainCheckInterval,
		AutoStop:           ec.Spec.AutoStop.DeepCopy(),
		Hash:               hash,
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
	classInfo *FireboltEngineClassInfo,
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
		computeDraining(spec, &result, current, classInfo)
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
	classInfo *FireboltEngineClassInfo,
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
		r.EnsureConfigMap = buildConfigMap(spec, engineName, engineNamespace, gen, instanceInfo, classInfo)
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
	classInfo *FireboltEngineClassInfo,
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
	classInfo *FireboltEngineClassInfo,
) {
	status := &r.Status

	if status.DrainingGeneration == nil {
		status.Phase = terminalPhase(spec)
		r.Requeue = true
		return
	}

	if !current.DrainingPodsDrained {
		r.RequeueAfter = getDrainCheckInterval(spec, classInfo)
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
	classInfo *FireboltEngineClassInfo,
) {
	r.EnsureConfigMap = buildConfigMap(spec, engineName, engineNamespace, gen, instanceInfo, classInfo)
	r.EnsureHeadlessSvc = buildHeadlessService(engineName, engineNamespace, gen)
	r.EnsureStatefulSet = buildStatefulSet(spec, engineName, engineNamespace, gen, classInfo)
}

func buildConfigMap(spec *computev1alpha1.FireboltEngineSpec, engineName, namespace string, gen int, instanceInfo InstanceInfo, classInfo *FireboltEngineClassInfo) *corev1.ConfigMap {
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
	// `DB::Config::ApplicationConfig`). The user contribution
	// (effectiveCustomEngineConfig: the referenced class's customEngineConfig
	// deep-merged underneath the engine's own) is merged into this at the
	// root, so users may add/override keys in any top-level section (auth,
	// engine, instance, logging) and an engine key wins over the same key on
	// the class. Operator-authoritative paths are stripped from each user
	// layer before the merge (see stripProtectedEngineConfigPaths) so they
	// cannot be overridden — silently, to keep the same spec portable across
	// operator versions even if the protected set evolves.
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

	deepMergeJSON(coreConfig, effectiveCustomEngineConfig(spec, classInfo))

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
	src := engineTemplate(spec).Annotations
	if len(src) == 0 {
		return nil
	}
	annotations := make(map[string]string, len(src))
	for k, v := range src {
		annotations[k] = v
	}
	return annotations
}

func buildStatefulSet(spec *computev1alpha1.FireboltEngineSpec, engineName, namespace string, gen int, classInfo *FireboltEngineClassInfo) *appsv1.StatefulSet {
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
	if h := customEngineConfigHash(spec, classInfo); h != "" {
		annotations[AnnotationCustomEngineConfigHash] = h
	}
	if classInfo != nil {
		annotations[AnnotationEngineClassHash] = classInfo.Hash
	}

	// Engine container image flows from the engine's own spec.template's
	// "engine" container image when set, falling back to the referenced
	// FireboltEngineClass's containers[name=="engine"].image, then to
	// the operator's embedded default.
	image, pullPolicy := effectiveEngineImage(spec, classInfo)

	gracePeriod := getTerminationGracePeriod(spec)
	podSecurityContext := effectivePodSecurityContext(spec, classInfo)
	containerSecurityContext := effectiveEngineContainerSecurityContext(spec, classInfo)

	volumeMounts := buildEngineContainerVolumeMounts(spec, classInfo)
	engineEnv := buildEngineContainerEnv(spec, classInfo)
	// The "data" volume backing /var/lib/firebolt is either a per-pod
	// PVC (the default; the StatefulSet controller synthesizes the pod
	// Volume from the VolumeClaimTemplate) or a node-local emptyDir /
	// hostPath that we add to pod.spec.volumes explicitly.
	// storage resolves engine-if-set → class-if-set → default emptyDir, so
	// an engine that omits a backend inherits the class's.
	storage := effectiveStorage(spec, classInfo)
	var (
		volumeClaimTemplates []corev1.PersistentVolumeClaim
		extraDataVolume      *corev1.Volume
	)
	switch resolveStorageBackend(storage) {
	case BackendEmptyDir:
		// storage.EmptyDir may be nil when resolveStorageBackend
		// fell through to the default (no backend set); render a
		// bare emptyDir in that case.
		var ed computev1alpha1.EngineEmptyDirSpec
		if storage.EmptyDir != nil {
			ed = *storage.EmptyDir
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
		hp := storage.HostPath
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
		pvc := resolvePersistentVolumeClaimDefaults(storage.PersistentVolumeClaim)
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
			Name: "engine-config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: coreConfigName,
					},
				},
			},
		},
		{Name: computev1alpha1.EngineRuntimeVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if extraDataVolume != nil {
		podVolumes = append(podVolumes, *extraDataVolume)
	}
	podVolumes = appendUserPodVolumes(podVolumes, spec, classInfo)

	// Optional built-in engine web UI sidecar (engine/class uiSidecar: true).
	// effectiveSidecarsWithUI is the single source of truth for the sidecar
	// set — stsMatchesSpec uses it too, so the injected container does not
	// read back as drift. Both the engine-web container name and the
	// nginx-writable-dir volume are operator-owned (the validating webhook
	// reserves the container name; operatorOwnedPodVolumeNames reserves the
	// volume), so a user template can never collide with them and injection
	// is unconditional once the feature resolves to enabled.
	sidecars := effectiveSidecarsWithUI(spec, classInfo)
	if effectiveUISidecarEnabled(spec, classInfo) {
		podVolumes = append(podVolumes, buildEngineWebWritableVolume())
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
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            effectiveServiceAccountName(spec, classInfo),
					NodeSelector:                  effectiveNodeSelector(spec, classInfo),
					Tolerations:                   effectiveTolerations(spec, classInfo),
					Affinity:                      effectiveAffinity(spec, classInfo),
					TopologySpreadConstraints:     effectiveTopologySpreadConstraints(spec, classInfo),
					PriorityClassName:             effectivePriorityClassName(spec, classInfo),
					RuntimeClassName:              effectiveRuntimeClassName(spec, classInfo),
					DNSPolicy:                     effectiveDNSPolicy(spec, classInfo),
					DNSConfig:                     effectiveDNSConfig(spec, classInfo),
					SchedulerName:                 effectiveSchedulerName(spec, classInfo),
					PreemptionPolicy:              effectivePreemptionPolicy(spec, classInfo),
					ReadinessGates:                effectiveReadinessGates(spec, classInfo),
					ResourceClaims:                effectivePodResourceClaims(spec, classInfo),
					HostAliases:                   effectiveHostAliases(spec, classInfo),
					OS:                            effectivePodOS(spec, classInfo),
					Overhead:                      effectivePodOverhead(spec, classInfo),
					InitContainers:                effectiveInitContainers(spec, classInfo),
					TerminationGracePeriodSeconds: &gracePeriod,
					SecurityContext:               podSecurityContext,
					// Suppress legacy Docker-link env vars (`<SVC>_SERVICE_HOST`,
					// `<SVC>_PORT=tcp://...`, etc.) the kubelet would otherwise
					// inject for every Service in the namespace. DNS is the
					// real service-discovery channel here; the auto-injected
					// vars are dead weight that also risks colliding with
					// firebolt-core's own config keys.
					EnableServiceLinks: boolPtr(false),
					ImagePullSecrets:   effectiveImagePullSecrets(spec, classInfo),
					Containers: append([]corev1.Container{
						{
							Name:                     computev1alpha1.EngineContainerName,
							Image:                    image,
							ImagePullPolicy:          pullPolicy,
							SecurityContext:          containerSecurityContext,
							Resources:                effectiveEngineResources(spec, classInfo),
							Env:                      engineEnv,
							EnvFrom:                  effectiveEngineEnvFrom(spec, classInfo),
							Ports:                    GetContainerPorts(),
							Command:                  []string{"/bin/bash", "-c"},
							Args:                     []string{strings.TrimSpace(EngineStartupScript)},
							VolumeMounts:             volumeMounts,
							Lifecycle:                effectiveEngineLifecycle(spec, classInfo),
							WorkingDir:               effectiveEngineWorkingDir(spec, classInfo),
							TerminationMessagePath:   effectiveEngineTerminationMessagePath(spec, classInfo),
							TerminationMessagePolicy: effectiveEngineTerminationMessagePolicy(spec, classInfo),
							VolumeDevices:            effectiveEngineVolumeDevices(spec, classInfo),
							ResizePolicy:             effectiveEngineResizePolicy(spec, classInfo),
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
					}, sidecars...),
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

// getDrainCheckInterval resolves the drain-poll interval: engine-if-set →
// FireboltEngineClass-if-set → operator default (DefaultDrainCheckInterval).
func getDrainCheckInterval(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) time.Duration {
	if spec.DrainCheckInterval != nil {
		return spec.DrainCheckInterval.Duration
	}
	if classInfo != nil && classInfo.DrainCheckInterval != nil {
		return classInfo.DrainCheckInterval.Duration
	}
	return DefaultDrainCheckInterval
}

// getTerminationGracePeriod returns the TGPS value to stamp on the
// engine StatefulSet's pod template. Always the operator's default —
// TGPS is operator-owned end-to-end: the engine pod template
// validator rejects user input on
// spec.template.spec.terminationGracePeriodSeconds (matching the
// rejection on the class template), so there is no user surface to
// consult here. Kept as a function (rather than inlining
// the constant) so a future change that surfaces the knob lands in
// one place.
func getTerminationGracePeriod(_ *computev1alpha1.FireboltEngineSpec) int64 {
	return DefaultTerminationGracePeriodSeconds
}

// engineTemplate returns the user-supplied pod template, or an empty
// PodTemplateSpec when spec.Template is nil. Returning a non-nil
// pointer everywhere lets callers read pod-spec / object-meta fields
// without nil-guarding at every site. The empty PodTemplateSpec is a
// throwaway local value — callers MUST NOT retain it beyond the
// expression.
func engineTemplate(spec *computev1alpha1.FireboltEngineSpec) *corev1.PodTemplateSpec {
	if spec.Template != nil {
		return spec.Template
	}
	return &corev1.PodTemplateSpec{}
}

// engineSpecContainer returns the engine container (name=
// EngineContainerName) from the engine's own template, or nil when
// no template or no engine container is declared. Different from
// classEngineContainer (which reads from the merged-class side).
func engineSpecContainer(spec *computev1alpha1.FireboltEngineSpec) *corev1.Container {
	return computev1alpha1.EngineContainerInTemplate(spec.Template)
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

// enginePodServiceAccountName returns the ServiceAccount name the user
// declared on spec.template.spec.serviceAccountName, or "" when not
// declared (namespace default).
func enginePodServiceAccountName(spec *computev1alpha1.FireboltEngineSpec) string {
	return engineTemplate(spec).Spec.ServiceAccountName
}

// effectiveServiceAccountName resolves the SA stamped on the engine pod
// template. Precedence: engine spec > FireboltEngineClass template > ""
// (default namespace SA). The same helper is used by buildStatefulSet and by
// stsMatchesSpec so the rendered value and the drift comparator stay
// aligned.
func effectiveServiceAccountName(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) string {
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
func effectiveNodeSelector(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) map[string]string {
	var classSel map[string]string
	if classInfo != nil && classInfo.Template != nil {
		classSel = classInfo.Template.Spec.NodeSelector
	}
	engineSel := engineTemplate(spec).Spec.NodeSelector
	if len(classSel) == 0 && len(engineSel) == 0 {
		return nil
	}
	out := make(map[string]string, len(classSel)+len(engineSel))
	for k, v := range classSel {
		out[k] = v
	}
	for k, v := range engineSel {
		out[k] = v
	}
	return out
}

// effectiveTolerations concatenates the class template's tolerations with
// the engine spec's. Class tolerations come first so the engine spec's
// order is preserved when callers iterate the slice; equivalence with
// stsMatchesSpec is by-element via reflect.DeepEqual so the order does
// not produce false drift.
func effectiveTolerations(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.Toleration {
	var classTols []corev1.Toleration
	if classInfo != nil && classInfo.Template != nil {
		classTols = classInfo.Template.Spec.Tolerations
	}
	engineTols := engineTemplate(spec).Spec.Tolerations
	if len(classTols) == 0 && len(engineTols) == 0 {
		return nil
	}
	out := make([]corev1.Toleration, 0, len(classTols)+len(engineTols))
	out = append(out, classTols...)
	out = append(out, engineTols...)
	return out
}

// effectiveAffinity returns the engine spec's affinity when set, else the
// class template's. Affinity is not field-merged: the value is a deeply
// nested struct (node / pod / pod-anti) whose semantics depend on every
// term agreeing, so partial overrides are dangerous. Whoever sets the
// field owns the whole tree.
//
// The return value is a deep copy of whichever side won so callers can
// freely mutate it (e.g. buildStatefulSet stamping it onto a fresh STS
// that is later JSON-serialized) without aliasing live spec state into
// the rendered StatefulSet — effectiveTolerations /
// effectiveNodeSelector copy for the same reason.
func effectiveAffinity(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *corev1.Affinity {
	if a := engineTemplate(spec).Spec.Affinity; a != nil {
		return a.DeepCopy()
	}
	if classInfo != nil && classInfo.Template != nil && classInfo.Template.Spec.Affinity != nil {
		return classInfo.Template.Spec.Affinity.DeepCopy()
	}
	return nil
}

// effectiveTopologySpreadConstraints concatenates the class template's
// topologySpreadConstraints with the engine template's, class first.
// Constraints are independently evaluated by the scheduler, so
// concatenation matches the engine controller's other slice-typed
// fields (tolerations, init containers, sidecars).
func effectiveTopologySpreadConstraints(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.TopologySpreadConstraint {
	var classTSC []corev1.TopologySpreadConstraint
	if classInfo != nil && classInfo.Template != nil {
		classTSC = classInfo.Template.Spec.TopologySpreadConstraints
	}
	engineTSC := engineTemplate(spec).Spec.TopologySpreadConstraints
	if len(classTSC) == 0 && len(engineTSC) == 0 {
		return nil
	}
	out := make([]corev1.TopologySpreadConstraint, 0, len(classTSC)+len(engineTSC))
	for i := range classTSC {
		out = append(out, *classTSC[i].DeepCopy())
	}
	for i := range engineTSC {
		out = append(out, *engineTSC[i].DeepCopy())
	}
	return out
}

// effectivePriorityClassName picks the engine template's value when set,
// else the class template's, else "". Whole-string ownership.
func effectivePriorityClassName(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) string {
	if s := engineTemplate(spec).Spec.PriorityClassName; s != "" {
		return s
	}
	if classInfo != nil && classInfo.Template != nil {
		return classInfo.Template.Spec.PriorityClassName
	}
	return ""
}

// effectiveRuntimeClassName picks the engine template's value when set,
// else the class template's, else nil.
func effectiveRuntimeClassName(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *string {
	if s := engineTemplate(spec).Spec.RuntimeClassName; s != nil {
		v := *s
		return &v
	}
	if classInfo != nil && classInfo.Template != nil && classInfo.Template.Spec.RuntimeClassName != nil {
		v := *classInfo.Template.Spec.RuntimeClassName
		return &v
	}
	return nil
}

// effectiveDNSPolicy picks the engine template's value when set, else
// the class template's, else "" (which causes the apiserver to default
// to ClusterFirst).
func effectiveDNSPolicy(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) corev1.DNSPolicy {
	if p := engineTemplate(spec).Spec.DNSPolicy; p != "" {
		return p
	}
	if classInfo != nil && classInfo.Template != nil {
		return classInfo.Template.Spec.DNSPolicy
	}
	return ""
}

// effectiveDNSConfig picks the engine template's value when set, else
// the class template's, else nil. Whole-struct ownership: the
// nameservers / searches / options trio is generally authored as a
// unit and a partial merge would surprise.
func effectiveDNSConfig(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *corev1.PodDNSConfig {
	if c := engineTemplate(spec).Spec.DNSConfig; c != nil {
		return c.DeepCopy()
	}
	if classInfo != nil && classInfo.Template != nil && classInfo.Template.Spec.DNSConfig != nil {
		return classInfo.Template.Spec.DNSConfig.DeepCopy()
	}
	return nil
}

// storageBackendSet reports whether an EngineStorageSpec selects a
// concrete data-volume backend. Used as the "is set" predicate for
// effectiveStorage: an engine that names any backend owns its storage
// wholesale, while a bare/empty spec falls through to the class.
func storageBackendSet(s computev1alpha1.EngineStorageSpec) bool {
	return s.PersistentVolumeClaim != nil || s.EmptyDir != nil || s.HostPath != nil
}

// effectiveStorage resolves the engine's data-volume configuration: the
// engine's own spec.storage when it names a backend, else the class's,
// else the engine's (empty) value — which resolveStorageBackend turns
// into the default emptyDir. Backend selection is whole-struct so the
// EngineStorageSpec mutual-exclusion invariant is never violated by a
// cross-layer mix (e.g. engine emptyDir + class PVC). Consumed by both
// buildStatefulSet and storageMatchesSpec so the rendered value and the
// drift comparator stay aligned.
func effectiveStorage(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) computev1alpha1.EngineStorageSpec {
	if storageBackendSet(spec.Storage) {
		return spec.Storage
	}
	if classInfo != nil && storageBackendSet(classInfo.Storage) {
		return classInfo.Storage
	}
	return spec.Storage
}

// effectiveCustomEngineConfig returns the merged user contribution to the
// rendered config.yaml: the class's customEngineConfig deep-merged
// underneath the engine's, with operator-owned paths stripped from each
// layer first (so neither side can override identity / routing /
// topology). The result excludes the operator-rendered base; callers
// merge it on top of that base. Returns an empty map when neither side
// sets config. Consumed by both buildConfigMap and customEngineConfigHash.
func effectiveCustomEngineConfig(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) map[string]interface{} {
	merged := map[string]interface{}{}
	if classInfo != nil {
		mergeStrippedConfig(merged, classInfo.CustomEngineConfig)
	}
	mergeStrippedConfig(merged, spec.CustomEngineConfig)
	return merged
}

// mergeStrippedConfig unmarshals a customEngineConfig payload, strips the
// operator-owned paths, and deep-merges it into dst. A nil/empty payload
// or an unmarshal failure is a no-op: Schemaless+Type=object on the CRD
// constrains valid input to a JSON object, so a failure means the
// apiserver admitted something it should have rejected and skipping the
// merge is the conservative choice.
func mergeStrippedConfig(dst map[string]interface{}, raw *apiextensionsv1.JSON) {
	if raw == nil || len(raw.Raw) == 0 {
		return
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw.Raw, &parsed); err != nil {
		return
	}
	stripProtectedEngineConfigPaths(parsed)
	deepMergeJSON(dst, parsed)
}

// effectiveRollout resolves the rollout strategy: engine-if-set →
// class-if-set → operator default (graceful). The CRD no longer defaults
// the engine field, so an unset engine value is the empty string and
// falls through here.
func effectiveRollout(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) computev1alpha1.RolloutStrategy {
	if spec.Rollout != "" {
		return spec.Rollout
	}
	if classInfo != nil && classInfo.Rollout != "" {
		return classInfo.Rollout
	}
	return computev1alpha1.RolloutGraceful
}

// effectiveDrainCheckEnabled resolves the drain-check toggle:
// engine-if-set → class-if-set → operator default (true).
func effectiveDrainCheckEnabled(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) bool {
	if spec.DrainCheckEnabled != nil {
		return *spec.DrainCheckEnabled
	}
	if classInfo != nil && classInfo.DrainCheckEnabled != nil {
		return *classInfo.DrainCheckEnabled
	}
	return true
}

// effectiveUISidecarEnabled resolves whether the built-in engine web UI sidecar
// should be injected: engine-if-set → class-if-set → operator default
// (false). Unlike the drain-check default this is off, matching the
// firebolt-instance-helm chart's uiSidecar: false default.
func effectiveUISidecarEnabled(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) bool {
	if spec.UISidecar != nil {
		return *spec.UISidecar
	}
	if classInfo != nil && classInfo.UISidecar != nil {
		return *classInfo.UISidecar
	}
	return false
}

// effectiveAutoStop resolves the autoStop policy whole-struct:
// engine-if-set → class-if-set → nil (autoStop disabled). An engine
// that sets spec.autoStop owns the entire policy; the class value is
// not field-merged.
func effectiveAutoStop(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *computev1alpha1.AutoStopSpec {
	if spec.AutoStop != nil {
		return spec.AutoStop
	}
	if classInfo != nil && classInfo.AutoStop != nil {
		return classInfo.AutoStop
	}
	return nil
}

// effectiveSchedulerName picks the engine template's value when set,
// else the class template's, else "" (default scheduler).
func effectiveSchedulerName(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) string {
	if s := engineTemplate(spec).Spec.SchedulerName; s != "" {
		return s
	}
	if classInfo != nil && classInfo.Template != nil {
		return classInfo.Template.Spec.SchedulerName
	}
	return ""
}

// effectivePreemptionPolicy picks the engine template's value when set,
// else the class template's, else nil.
func effectivePreemptionPolicy(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *corev1.PreemptionPolicy {
	if p := engineTemplate(spec).Spec.PreemptionPolicy; p != nil {
		v := *p
		return &v
	}
	if classInfo != nil && classInfo.Template != nil && classInfo.Template.Spec.PreemptionPolicy != nil {
		v := *classInfo.Template.Spec.PreemptionPolicy
		return &v
	}
	return nil
}

// effectiveReadinessGates concatenates class then engine, mirroring
// tolerations. Readiness gates are independently evaluated and adding
// one cannot subtract a sibling's contribution, so concatenation is
// the natural merge.
func effectiveReadinessGates(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.PodReadinessGate {
	var classGates []corev1.PodReadinessGate
	if classInfo != nil && classInfo.Template != nil {
		classGates = classInfo.Template.Spec.ReadinessGates
	}
	engineGates := engineTemplate(spec).Spec.ReadinessGates
	if len(classGates) == 0 && len(engineGates) == 0 {
		return nil
	}
	out := make([]corev1.PodReadinessGate, 0, len(classGates)+len(engineGates))
	out = append(out, classGates...)
	out = append(out, engineGates...)
	return out
}

// effectivePodResourceClaims concatenates pod-level ResourceClaims
// from the class template and the engine template, class first. This
// pre-merges the pod claim list the engine container's
// volumeDevices / resources.Claims may reference by name.
func effectivePodResourceClaims(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.PodResourceClaim {
	var classClaims []corev1.PodResourceClaim
	if classInfo != nil && classInfo.Template != nil {
		classClaims = classInfo.Template.Spec.ResourceClaims
	}
	engineClaims := engineTemplate(spec).Spec.ResourceClaims
	if len(classClaims) == 0 && len(engineClaims) == 0 {
		return nil
	}
	out := make([]corev1.PodResourceClaim, 0, len(classClaims)+len(engineClaims))
	for i := range classClaims {
		out = append(out, *classClaims[i].DeepCopy())
	}
	for i := range engineClaims {
		out = append(out, *engineClaims[i].DeepCopy())
	}
	return out
}

// effectiveHostAliases concatenates host aliases from both sides,
// class first. /etc/hosts entries are independently meaningful.
func effectiveHostAliases(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.HostAlias {
	var classAliases []corev1.HostAlias
	if classInfo != nil && classInfo.Template != nil {
		classAliases = classInfo.Template.Spec.HostAliases
	}
	engineAliases := engineTemplate(spec).Spec.HostAliases
	if len(classAliases) == 0 && len(engineAliases) == 0 {
		return nil
	}
	out := make([]corev1.HostAlias, 0, len(classAliases)+len(engineAliases))
	for i := range classAliases {
		out = append(out, *classAliases[i].DeepCopy())
	}
	for i := range engineAliases {
		out = append(out, *engineAliases[i].DeepCopy())
	}
	return out
}

// effectivePodOS picks the engine template's value when set, else
// the class template's, else nil. Whole-struct ownership.
func effectivePodOS(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *corev1.PodOS {
	if o := engineTemplate(spec).Spec.OS; o != nil {
		return o.DeepCopy()
	}
	if classInfo != nil && classInfo.Template != nil && classInfo.Template.Spec.OS != nil {
		return classInfo.Template.Spec.OS.DeepCopy()
	}
	return nil
}

// effectivePodOverhead picks the engine template's value when set,
// else the class template's, else nil. Overhead is a ResourceList
// keyed by ResourceName; whole-map ownership keeps it consistent with
// how the engine container's Resources field is merged (engine wins
// wholesale, no per-key merge).
func effectivePodOverhead(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) corev1.ResourceList {
	if o := engineTemplate(spec).Spec.Overhead; len(o) > 0 {
		out := make(corev1.ResourceList, len(o))
		for k, v := range o {
			out[k] = v.DeepCopy()
		}
		return out
	}
	if classInfo != nil && classInfo.Template != nil && len(classInfo.Template.Spec.Overhead) > 0 {
		out := make(corev1.ResourceList, len(classInfo.Template.Spec.Overhead))
		for k, v := range classInfo.Template.Spec.Overhead {
			out[k] = v.DeepCopy()
		}
		return out
	}
	return nil
}

// effectivePodLabels merges the operator-built labels with class template
// labels and engine spec.PodLabels. Precedence: operator-built (reserved
// keys) > engine spec.PodLabels > class template labels. The two reserved
// keys (LabelEngine, LabelGeneration) are non-overridable so the
// blue-green selector machinery cannot be detached by a user typo.
func effectivePodLabels(spec *computev1alpha1.FireboltEngineSpec, engineName string, gen int, classInfo *FireboltEngineClassInfo) map[string]string {
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
	for k, v := range engineTemplate(spec).Labels {
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
func effectivePodAnnotations(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) map[string]string {
	var classAnno map[string]string
	if classInfo != nil && classInfo.Template != nil {
		classAnno = classInfo.Template.Annotations
	}
	engineAnno := engineTemplate(spec).Annotations
	if len(classAnno) == 0 && len(engineAnno) == 0 {
		return nil
	}
	out := make(map[string]string, len(classAnno)+len(engineAnno))
	for k, v := range classAnno {
		out[k] = v
	}
	for k, v := range engineAnno {
		out[k] = v
	}
	return out
}

// effectiveEngineImage resolves the engine container image and pull
// policy. Precedence: engine spec.template's "engine" container >
// class template's "engine" container > operator default. Image and
// ImagePullPolicy are tracked independently so a user can override
// just one (e.g. set ImagePullPolicy=Always without redeclaring the
// image, or vice versa).
func effectiveEngineImage(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) (image string, pullPolicy corev1.PullPolicy) {
	image = resolveImageRef(nil, DefaultEngineRepository, DefaultEngineTag)
	if c := classEngineContainer(classInfo); c != nil && c.Image != "" {
		image = c.Image
	}
	if c := engineSpecContainer(spec); c != nil && c.Image != "" {
		image = c.Image
	}
	pullPolicy = engineExplicitPullPolicy(spec, classInfo)
	if pullPolicy == "" {
		pullPolicy = resolveWorkloadImagePullPolicy(image)
	}
	return image, pullPolicy
}

// engineExplicitPullPolicy returns the pull policy explicitly set on the
// "engine" container (engine spec.template first, then class template), or
// "" when neither sets one so callers can apply their own default.
func engineExplicitPullPolicy(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) corev1.PullPolicy {
	if c := engineSpecContainer(spec); c != nil && c.ImagePullPolicy != "" {
		return c.ImagePullPolicy
	}
	if c := classEngineContainer(classInfo); c != nil && c.ImagePullPolicy != "" {
		return c.ImagePullPolicy
	}
	return ""
}

// effectiveEngineWebPullPolicy resolves the injected UI sidecar's image pull
// policy: engine-if-set → class-if-set → the Kubernetes tag-based default
// for the sidecar image. DefaultEngineWebImage is tracked at the mutable
// :latest alias, so that default resolves to Always: caching it under
// IfNotPresent would pin every node to whatever :latest it cached first and
// UI image fixes would never reach existing clusters.
func effectiveEngineWebPullPolicy(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) corev1.PullPolicy {
	if p := engineExplicitPullPolicy(spec, classInfo); p != "" {
		return p
	}
	return resolveContainerImagePullPolicy(DefaultEngineWebImage, "")
}

// effectiveSidecars merges sidecar containers (every container whose
// name is not EngineContainerName) from both the class template and
// the engine spec's template, in that order. Class sidecars run
// first, engine sidecars appended after. API-server defaults
// (imagePullPolicy, terminationMessage*) are stamped on each entry
// so stsMatchesSpec doesn't see drift on read-back.
func effectiveSidecars(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.Container {
	out := make([]corev1.Container, 0)
	if classInfo != nil && classInfo.Template != nil {
		out = appendSidecars(out, classInfo.Template.Spec.Containers)
	}
	out = appendSidecars(out, engineTemplate(spec).Spec.Containers)
	if len(out) == 0 {
		return nil
	}
	return out
}

// appendSidecars deep-copies every container in src whose name is
// not EngineContainerName, stamps API-server defaults so the result
// compares equal on read-back, and appends them to dst.
func appendSidecars(dst []corev1.Container, src []corev1.Container) []corev1.Container {
	for i := range src {
		if src[i].Name == computev1alpha1.EngineContainerName {
			continue
		}
		copied := *src[i].DeepCopy()
		applyContainerAPIServerDefaults(&copied)
		dst = append(dst, copied)
	}
	return dst
}

// effectiveSidecarsWithUI returns effectiveSidecars plus the built-in engine
// UI sidecar when the feature resolves to enabled (engine→class→false).
// buildStatefulSet and stsMatchesSpec MUST resolve the sidecar set through
// this one helper, or the injected UI container would read back from the API
// as perpetual drift and roll a new blue-green generation on every reconcile.
// No collision guard is needed: EngineWebContainerName is reserved by the
// validating webhook, so a user container can never share the name.
func effectiveSidecarsWithUI(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.Container {
	sidecars := effectiveSidecars(spec, classInfo)
	if effectiveUISidecarEnabled(spec, classInfo) {
		sidecars = append(sidecars, buildEngineWebSidecar(effectiveEngineWebPullPolicy(spec, classInfo)))
	}
	return sidecars
}

// buildEngineWebSidecar returns the built-in engine web UI sidecar container
// injected when an engine or its class sets uiSidecar: true. It mirrors the
// firebolt-instance-helm chart's UI container (renamed to the operator-owned
// EngineWebContainerName): an nginx-based UI pointed at the local engine
// (EngineWebBackendURL) over loopback, listening on EngineWebPort, with a
// hardened securityContext (read-only root FS, drop ALL, runs as the engine
// UID/GID) and nginx's writable paths backed by an emptyDir. API-server
// defaults are stamped so a read-back of the rendered StatefulSet does not
// look like drift (see containersEqualAfterDefaults). pullPolicy comes from
// effectiveEngineWebPullPolicy.
//
// The readiness probe makes pod Ready mean "the UI is actually serving":
// without it a sidecar counts ready the instant its process starts, so a
// sidecar that crashes right after startup opens a transient all-ready
// window that the blue-green promotion gate (checkPodsReady, a single-shot
// snapshot) can observe and promote on.
func buildEngineWebSidecar(pullPolicy corev1.PullPolicy) corev1.Container {
	runAsUser := DefaultEngineWebD
	runAsGroup := DefaultEngineGID
	c := corev1.Container{
		Name:            computev1alpha1.EngineWebContainerName,
		Image:           DefaultEngineWebImage,
		ImagePullPolicy: pullPolicy,
		Env: []corev1.EnvVar{
			{Name: "FIREBOLT_CORE_URL", Value: EngineWebBackendURL},
		},
		Ports: []corev1.ContainerPort{
			{Name: EngineWebPortName, ContainerPort: EngineWebPort, Protocol: corev1.ProtocolTCP},
		},
		ReadinessProbe: &corev1.Probe{
			InitialDelaySeconds: 1,
			PeriodSeconds:       3,
			TimeoutSeconds:      2,
			FailureThreshold:    3,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/",
					Port: intstr.FromInt(int(EngineWebPort)),
				},
			},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
			RunAsUser:                &runAsUser,
			RunAsGroup:               &runAsGroup,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: EngineWebWritableVolumeName, MountPath: "/var/tmp"},
			{Name: EngineWebWritableVolumeName, MountPath: "/var/cache/nginx"},
			{Name: EngineWebWritableVolumeName, MountPath: "/var/run/nginx"},
		},
	}
	applyContainerAPIServerDefaults(&c)
	return c
}

// buildEngineWebWritableVolume returns the emptyDir backing nginx's writable
// paths in the read-only-rootfs engine web UI sidecar.
func buildEngineWebWritableVolume() corev1.Volume {
	return corev1.Volume{
		Name:         EngineWebWritableVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// engineClassInitContainers returns the init containers from the class
// template, deep-copied so subsequent mutations of the result do not
// reach back into classInfo.
func engineClassInitContainers(classInfo *FireboltEngineClassInfo) []corev1.Container {
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

// effectiveInitContainers concatenates the FireboltEngineClass template's init
// containers and the engine spec's init containers, in that order, so
// shared bootstrap steps in the class run before engine-specific ones.
// Mirrors the class-then-spec precedence used by effectiveTolerations.
func effectiveInitContainers(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.Container {
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
func classEngineContainer(classInfo *FireboltEngineClassInfo) *corev1.Container {
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
func effectiveEngineResources(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) corev1.ResourceRequirements {
	if c := engineSpecContainer(spec); c != nil && computev1alpha1.HasContainerResources(c.Resources) {
		return *c.Resources.DeepCopy()
	}
	if c := classEngineContainer(classInfo); c != nil && computev1alpha1.HasContainerResources(c.Resources) {
		return *c.Resources.DeepCopy()
	}
	return corev1.ResourceRequirements{}
}

// effectiveEngineEnv concatenates env vars declared on the engine
// container of both the class template and the engine spec's
// template — class first, engine after. Reserved keys (POD_INDEX,
// FB_AWS_EC2_METADATA_CLIENT_ENABLED, FIREBOLT_CORE_MODE) on either
// surface are rejected before this runs: the validating webhook gates
// them at admission, and the always-on controller backstops re-run the
// same rules every reconcile (the engine template via
// validateEngineTemplate, the class template via the class's
// Ready=False/OperatorOwnedFieldSet condition read in
// resolveFireboltEngineClassInfo). A rejected engine never reaches the
// renderer, so the result is safe to append to the operator-injected
// env list without further filtering.
func effectiveEngineEnv(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.EnvVar {
	var out []corev1.EnvVar
	if c := classEngineContainer(classInfo); c != nil {
		for i := range c.Env {
			var v corev1.EnvVar
			c.Env[i].DeepCopyInto(&v)
			out = append(out, v)
		}
	}
	if c := engineSpecContainer(spec); c != nil {
		for i := range c.Env {
			var v corev1.EnvVar
			c.Env[i].DeepCopyInto(&v)
			out = append(out, v)
		}
	}
	return out
}

// effectiveEngineEnvFrom concatenates envFrom entries on the engine
// container from both class and engine templates, class first.
func effectiveEngineEnvFrom(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.EnvFromSource {
	var out []corev1.EnvFromSource
	if c := classEngineContainer(classInfo); c != nil {
		for i := range c.EnvFrom {
			var v corev1.EnvFromSource
			c.EnvFrom[i].DeepCopyInto(&v)
			out = append(out, v)
		}
	}
	if c := engineSpecContainer(spec); c != nil {
		for i := range c.EnvFrom {
			var v corev1.EnvFromSource
			c.EnvFrom[i].DeepCopyInto(&v)
			out = append(out, v)
		}
	}
	return out
}

// effectiveEngineVolumeMounts concatenates additional volumeMounts on
// the engine container from both class and engine templates, class
// first. Entries whose name collides with an operator-reserved volume
// ("engine-config", DataVolumeName) are skipped on both sides: the
// operator owns those mount paths and a user override would silently
// break startup (config volume) or data persistence (data volume).
func effectiveEngineVolumeMounts(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.VolumeMount {
	out := make([]corev1.VolumeMount, 0)
	if c := classEngineContainer(classInfo); c != nil {
		out = appendUserVolumeMounts(out, c.VolumeMounts)
	}
	if c := engineSpecContainer(spec); c != nil {
		out = appendUserVolumeMounts(out, c.VolumeMounts)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// appendUserVolumeMounts deep-copies entries from src into dst,
// skipping any that name an operator-reserved volume.
func appendUserVolumeMounts(dst, src []corev1.VolumeMount) []corev1.VolumeMount {
	for i := range src {
		name := src[i].Name
		if name == "engine-config" || name == DataVolumeName {
			continue
		}
		dst = append(dst, *src[i].DeepCopy())
	}
	return dst
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
func effectiveEngineContainerSecurityContext(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *corev1.SecurityContext {
	if c := engineSpecContainer(spec); c != nil && c.SecurityContext != nil {
		return c.SecurityContext.DeepCopy()
	}
	if c := classEngineContainer(classInfo); c != nil && c.SecurityContext != nil {
		return c.SecurityContext.DeepCopy()
	}
	return defaultEngineContainerSecurityContext()
}

// buildEngineContainerVolumeMounts returns the volumeMounts stamped
// on the rendered engine container: operator-owned mounts first
// (data, config, runtime), then class then engine user-supplied mounts
// in that order (excluding any that try to redefine an operator-owned
// name). Shared between buildStatefulSet and the drift comparator so the
// order and filtering stays consistent across both paths.
func buildEngineContainerVolumeMounts(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.VolumeMount {
	// ConfigMountPath (config.yaml) sits inside DataMountPath, so the data
	// volume is listed first: the engine then reads the operator-rendered
	// config.yaml as a read-only overlay on top of the writable data volume.
	// This slice order is for readability only -- the container runtime
	// (containerd, CRI-O, Docker) sorts mounts by destination-path depth, so
	// the shallower DataMountPath is mounted before the deeper ConfigMountPath
	// regardless of the order here.
	out := []corev1.VolumeMount{
		{
			Name:      DataVolumeName,
			MountPath: DataMountPath,
		},
		{
			Name:      "engine-config",
			MountPath: ConfigMountPath,
			SubPath:   ConfigFileName,
			ReadOnly:  true,
		},
		{
			Name:      computev1alpha1.EngineRuntimeVolumeName,
			MountPath: "/run/firebolt",
		},
	}
	return append(out, effectiveEngineVolumeMounts(spec, classInfo)...)
}

// buildEngineContainerEnv returns the env stamped on the rendered
// engine container: operator-injected vars first (POD_INDEX,
// FB_AWS_EC2_METADATA_CLIENT_ENABLED, FIREBOLT_CORE_MODE), then class
// then engine user-supplied entries in that order. Shared between
// buildStatefulSet (write path) and engineContainerExtraFieldsMatch
// (drift comparator) so a future env injection or reordering lands
// in both at once.
func buildEngineContainerEnv(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.EnvVar {
	out := []corev1.EnvVar{
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
		// `firebolt` binary: the operator-rendered config
		// (config.yaml at the data-dir root) is honored as-is and
		// not rewritten at startup.
		{
			Name:  computev1alpha1.EngineCoreModeEnvKey,
			Value: "1",
		},
	}
	return append(out, effectiveEngineEnv(spec, classInfo)...)
}

// effectiveEngineLifecycle resolves the Lifecycle hooks stamped on
// the engine container. Engine wins if set, else class, else nil.
// Whole-struct ownership (no merge of preStop / postStart).
func effectiveEngineLifecycle(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *corev1.Lifecycle {
	if c := engineSpecContainer(spec); c != nil && c.Lifecycle != nil {
		return c.Lifecycle.DeepCopy()
	}
	if c := classEngineContainer(classInfo); c != nil && c.Lifecycle != nil {
		return c.Lifecycle.DeepCopy()
	}
	return nil
}

// effectiveEngineWorkingDir picks the engine container's WorkingDir
// from engine template if set, else class, else "" (image default).
func effectiveEngineWorkingDir(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) string {
	if c := engineSpecContainer(spec); c != nil && c.WorkingDir != "" {
		return c.WorkingDir
	}
	if c := classEngineContainer(classInfo); c != nil {
		return c.WorkingDir
	}
	return ""
}

// effectiveEngineTerminationMessagePath picks the engine container's
// TerminationMessagePath from engine template if set, else class,
// else "" (apiserver defaults to /dev/termination-log).
func effectiveEngineTerminationMessagePath(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) string {
	if c := engineSpecContainer(spec); c != nil && c.TerminationMessagePath != "" {
		return c.TerminationMessagePath
	}
	if c := classEngineContainer(classInfo); c != nil {
		return c.TerminationMessagePath
	}
	return ""
}

// effectiveEngineTerminationMessagePolicy picks the engine container's
// TerminationMessagePolicy from engine template if set, else class,
// else "" (apiserver defaults to File).
func effectiveEngineTerminationMessagePolicy(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) corev1.TerminationMessagePolicy {
	if c := engineSpecContainer(spec); c != nil && c.TerminationMessagePolicy != "" {
		return c.TerminationMessagePolicy
	}
	if c := classEngineContainer(classInfo); c != nil {
		return c.TerminationMessagePolicy
	}
	return ""
}

// effectiveEngineVolumeDevices concatenates engine-container
// volumeDevices from class and engine template, class first, mirroring
// the volumeMounts merge pattern. Names colliding with operator-owned
// volumes are not filtered here because the operator does not mount
// block devices; user input is passed through verbatim.
func effectiveEngineVolumeDevices(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.VolumeDevice {
	var classDevs, engineDevs []corev1.VolumeDevice
	if c := classEngineContainer(classInfo); c != nil {
		classDevs = c.VolumeDevices
	}
	if c := engineSpecContainer(spec); c != nil {
		engineDevs = c.VolumeDevices
	}
	if len(classDevs) == 0 && len(engineDevs) == 0 {
		return nil
	}
	out := make([]corev1.VolumeDevice, 0, len(classDevs)+len(engineDevs))
	for i := range classDevs {
		out = append(out, *classDevs[i].DeepCopy())
	}
	for i := range engineDevs {
		out = append(out, *engineDevs[i].DeepCopy())
	}
	return out
}

// effectiveEngineResizePolicy concatenates engine-container
// resizePolicy entries from class and engine template, class first.
// ResizePolicy entries are keyed by ResourceName and Kubernetes
// itself doesn't deduplicate them; the operator preserves what the
// user wrote so the failing pair (if any) surfaces at admission.
func effectiveEngineResizePolicy(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.ContainerResizePolicy {
	var classRP, engineRP []corev1.ContainerResizePolicy
	if c := classEngineContainer(classInfo); c != nil {
		classRP = c.ResizePolicy
	}
	if c := engineSpecContainer(spec); c != nil {
		engineRP = c.ResizePolicy
	}
	if len(classRP) == 0 && len(engineRP) == 0 {
		return nil
	}
	out := make([]corev1.ContainerResizePolicy, 0, len(classRP)+len(engineRP))
	out = append(out, classRP...)
	out = append(out, engineRP...)
	return out
}

// effectiveImagePullSecrets concatenates pod-level imagePullSecrets
// from the class template and the engine template, class first.
// Typical use is to authenticate sidecar / init-container pulls from
// a private registry. Returns nil when neither side contributes.
func effectiveImagePullSecrets(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.LocalObjectReference {
	var classRefs []corev1.LocalObjectReference
	if classInfo != nil && classInfo.Template != nil {
		classRefs = classInfo.Template.Spec.ImagePullSecrets
	}
	engineRefs := engineTemplate(spec).Spec.ImagePullSecrets
	if len(classRefs) == 0 && len(engineRefs) == 0 {
		return nil
	}
	out := make([]corev1.LocalObjectReference, 0, len(classRefs)+len(engineRefs))
	out = append(out, classRefs...)
	out = append(out, engineRefs...)
	return out
}

// appendUserPodVolumes appends pod-level volumes from both the class
// template and the engine template to the operator-rendered list, in
// that order. Volume names matching an operator-owned name are skipped.
//
// The reserved set is the union of (a) every name in the operator-built
// `operator` slice and (b) the static operator-owned name list. The
// static list catches DataVolumeName on the PVC backend, where the data
// volume comes from spec.volumeClaimTemplates rather than from
// spec.template.spec.volumes — without the static list, a user volume
// named "data" would slip through admission and collide with the PVC-
// synthesized data volume at pod creation time, leaving pods Pending.
func appendUserPodVolumes(operator []corev1.Volume, spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) []corev1.Volume {
	reserved := operatorOwnedPodVolumeNames()
	for i := range operator {
		reserved[operator[i].Name] = struct{}{}
	}
	out := make([]corev1.Volume, 0, len(operator))
	out = append(out, operator...)
	if classInfo != nil && classInfo.Template != nil {
		out = appendUserVolumesFrom(out, classInfo.Template.Spec.Volumes, reserved)
	}
	out = appendUserVolumesFrom(out, engineTemplate(spec).Spec.Volumes, reserved)
	return out
}

// operatorOwnedPodVolumeNames returns the static set of pod-volume
// names the operator owns end-to-end. A user-supplied volume colliding
// with one of these is silently dropped by appendUserPodVolumes. The
// set is fixed (not derived from runtime state) so the PVC and
// emptyDir/hostPath storage backends share the same protection — on
// the PVC backend the data volume comes from VolumeClaimTemplates and
// is therefore not present in the rendered pod-level Volumes list,
// making the runtime-derived reservation insufficient.
func operatorOwnedPodVolumeNames() map[string]struct{} {
	return map[string]struct{}{
		"engine-config":             {},
		DataVolumeName:              {},
		EngineWebWritableVolumeName: {},
	}
}

// appendUserVolumesFrom deep-copies entries from src into dst,
// skipping any whose name is in the reserved set.
func appendUserVolumesFrom(dst, src []corev1.Volume, reserved map[string]struct{}) []corev1.Volume {
	for i := range src {
		if _, taken := reserved[src[i].Name]; taken {
			continue
		}
		dst = append(dst, *src[i].DeepCopy())
	}
	return dst
}

// effectivePodSecurityContext resolves the pod-level PodSecurityContext.
// Precedence: engine spec > class > operator default. Operator defaults
// (FSGroup, FSGroupChangePolicy) are stamped on top of whichever side
// won so the engine can write to the data volume and kubelet skips the
// recursive chown on every mount, regardless of how much (or little)
// context the user supplied.
func effectivePodSecurityContext(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) *corev1.PodSecurityContext {
	var psc *corev1.PodSecurityContext
	switch {
	case engineTemplate(spec).Spec.SecurityContext != nil:
		psc = engineTemplate(spec).Spec.SecurityContext.DeepCopy()
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
// authoritative key. A section whose only keys were operator-owned is also
// dropped once those keys are removed, so a config that names nothing but
// protected paths reduces to the empty map — it contributes nothing to the
// merge and leaves no trace in the customEngineConfig hash (no spurious
// generation).
//
// The owned-path set lives in
// computev1alpha1.OperatorOwnedEngineConfigPaths so that every templating
// surface (this function, the FireboltEngineClass webhook) reads from one
// declaration.
func stripProtectedEngineConfigPaths(m map[string]interface{}) {
	for _, owned := range computev1alpha1.OperatorOwnedEngineConfigPaths {
		if owned.Section == "" {
			for _, k := range owned.Keys {
				delete(m, k)
			}
			continue
		}
		sub, ok := m[owned.Section].(map[string]interface{})
		if !ok {
			delete(m, owned.Section)
			continue
		}
		for _, k := range owned.Keys {
			delete(sub, k)
		}
		if len(sub) == 0 {
			delete(m, owned.Section)
		}
	}
}

// customEngineConfigHash returns a stable hash of the effective user
// contribution to the engine config.yaml — the referenced class's
// customEngineConfig deep-merged underneath the engine's, with operator-
// owned paths already stripped (see effectiveCustomEngineConfig). Stamped
// onto the engine StatefulSet so stsMatchesSpec detects drift from either
// layer. Returns "" when neither side contributes anything post-strip,
// keeping the annotation absent and the rendered pod byte-identical to an
// engine with no custom config.
func customEngineConfigHash(spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) string {
	merged := effectiveCustomEngineConfig(spec, classInfo)
	if len(merged) == 0 {
		return ""
	}
	// Marshal through the canonical map form so semantically equal payloads
	// (whitespace, key order) hash to the same value and don't trigger
	// spurious generations.
	canonical, err := json.Marshal(merged)
	if err != nil {
		return contentHash(fmt.Sprintf("%v", merged))
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
	return effectivePodSecurityContext(spec, nil)
}

const defaultTerminationMessagePath = "/dev/termination-log"

// engineInitContainers returns a deep copy of the engine spec
// template's initContainers, or nil when none are declared so nil
// and empty compare equal in stsMatchesSpec. API-server defaults
// (imagePullPolicy, terminationMessagePath/Policy) are applied so a
// freshly-built STS compares equal to the same object read back from
// the API.
func engineInitContainers(spec *computev1alpha1.FireboltEngineSpec) []corev1.Container {
	src := engineTemplate(spec).Spec.InitContainers
	if len(src) == 0 {
		return nil
	}
	out := make([]corev1.Container, len(src))
	for i := range src {
		src[i].DeepCopyInto(&out[i])
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
	applyProbeAPIServerDefaults(c.LivenessProbe)
	applyProbeAPIServerDefaults(c.ReadinessProbe)
	applyProbeAPIServerDefaults(c.StartupProbe)
	if c.Lifecycle != nil {
		applyHandlerHTTPGetAPIServerDefaults(c.Lifecycle.PostStart)
		applyHandlerHTTPGetAPIServerDefaults(c.Lifecycle.PreStop)
	}
}

// applyProbeAPIServerDefaults stamps the probe fields the API server fills in
// at create time (SetDefaults_Probe + SetDefaults_HTTPGetAction), so a probe
// that omits them does not read back from the API as spurious drift.
func applyProbeAPIServerDefaults(p *corev1.Probe) {
	if p == nil {
		return
	}
	if p.TimeoutSeconds == 0 {
		p.TimeoutSeconds = 1
	}
	if p.PeriodSeconds == 0 {
		p.PeriodSeconds = 10
	}
	if p.SuccessThreshold == 0 {
		p.SuccessThreshold = 1
	}
	if p.FailureThreshold == 0 {
		p.FailureThreshold = 3
	}
	applyHTTPGetAPIServerDefaults(p.HTTPGet)
}

// applyHandlerHTTPGetAPIServerDefaults stamps the HTTPGet defaults on a
// lifecycle handler (SetDefaults_HTTPGetAction applies to those too).
func applyHandlerHTTPGetAPIServerDefaults(h *corev1.LifecycleHandler) {
	if h == nil {
		return
	}
	applyHTTPGetAPIServerDefaults(h.HTTPGet)
}

// applyHTTPGetAPIServerDefaults mirrors SetDefaults_HTTPGetAction.
func applyHTTPGetAPIServerDefaults(g *corev1.HTTPGetAction) {
	if g == nil {
		return
	}
	if g.Path == "" {
		g.Path = "/"
	}
	if g.Scheme == "" {
		g.Scheme = corev1.URISchemeHTTP
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
// (makeSTS / makeEmptyDirSTS) that pre-date the FireboltEngineClass merge layer.
// Production code paths use effectiveEngineContainerSecurityContext
// directly; do not introduce new callers of this wrapper.
func getEngineContainerSecurityContext(spec *computev1alpha1.FireboltEngineSpec) *corev1.SecurityContext {
	return effectiveEngineContainerSecurityContext(spec, nil)
}

// defaultEngineContainerSecurityContext returns the hardened
// container-level SecurityContext applied to the engine container when
// no user value is supplied. Mirrors the firebolt-instance-helm chart's
// engine StatefulSet (UID/GID 3473): non-root, no extra capabilities,
// no privilege escalation, read-only root filesystem; engine runtime
// writes go to the data volume at /var/lib/firebolt and an emptyDir at
// /run/firebolt.
func defaultEngineContainerSecurityContext() *corev1.SecurityContext {
	runAsUser := DefaultEngineWebD
	runAsGroup := DefaultEngineGID
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   boolPtr(true),
		RunAsNonRoot:             boolPtr(true),
		RunAsUser:                &runAsUser,
		RunAsGroup:               &runAsGroup,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// engineContainerResources returns the engine container's Resources
// as declared on spec.template (the no-class shorthand around
// effectiveEngineResources). Used by test fixtures that build an
// expected STS to compare against the buildStatefulSet output.
func engineContainerResources(spec *computev1alpha1.FireboltEngineSpec) corev1.ResourceRequirements {
	return effectiveEngineResources(spec, nil)
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

// dnsPolicyEqual treats apiserver-defaulted DNSPolicy values
// (ClusterFirst when hostNetwork is off, ClusterFirstWithHostNet
// when on) as equivalent to "" so an engine that does not set the
// field compares equal to the same StatefulSet read back from the
// apiserver. Without this tolerance, stsMatchesSpec returns false on
// its own buildStatefulSet output once the apiserver fills in the
// default, and the reconciler rolls a fresh blue-green generation on
// every loop forever.
func dnsPolicyEqual(actual, expected corev1.DNSPolicy) bool {
	if expected == "" {
		return actual == "" || actual == corev1.DNSClusterFirst || actual == corev1.DNSClusterFirstWithHostNet
	}
	return actual == expected
}

// schedulerNameEqual treats the apiserver-defaulted "default-scheduler"
// as equivalent to "" so an engine that does not pick a scheduler
// compares equal to the same StatefulSet read back. Same false-drift
// rationale as dnsPolicyEqual.
func schedulerNameEqual(actual, expected string) bool {
	if expected == "" {
		return actual == "" || actual == corev1.DefaultSchedulerName
	}
	return actual == expected
}

// stringPtrsEqual compares two *string by value, treating nil and ""
// (or two nils, or two empty pointers) as equal. corev1.PodSpec.RuntimeClassName
// is a *string, and an apiserver round-trip can flip a "" pointer back
// to nil; reflect.DeepEqual would report drift on that flip.
func stringPtrsEqual(a, b *string) bool {
	av, bv := "", ""
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}

// preemptionPolicyEqual is the *corev1.PreemptionPolicy sibling of
// stringPtrsEqual. The kubelet defaults a nil PreemptionPolicy to
// PreemptLowerPriority on read-back via the apiserver in some
// versions; comparing by value-or-empty keeps stsMatchesSpec from
// rolling a fresh generation on that no-op default flip.
func preemptionPolicyEqual(a, b *corev1.PreemptionPolicy) bool {
	av, bv := corev1.PreemptionPolicy(""), corev1.PreemptionPolicy("")
	if a != nil {
		av = *a
	}
	if b != nil {
		bv = *b
	}
	return av == bv
}

// overheadEqual compares two ResourceList overhead maps. nil and
// empty are treated as identical (matches what the apiserver does on
// json:omitempty round-trip), and each Quantity is compared with the
// resource.Quantity Cmp method so semantic equivalents (e.g. "1Gi"
// vs "1024Mi") do not produce false drift.
func overheadEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if av.Cmp(bv) != 0 {
			return false
		}
	}
	return true
}

// engineContainerExtraFieldsMatch is the drift comparator for engine-
// container fields the operator passes through from the engine
// template (with the FireboltEngineClass template's value as a
// fallback): Env, EnvFrom, VolumeMounts, Lifecycle, WorkingDir,
// TerminationMessagePath/Policy, VolumeDevices, ResizePolicy. Image, ImagePullPolicy, Resources,
// SecurityContext have their own per-field comparisons in
// stsMatchesSpec because they precede the field-by-field block by a
// long history of regression tests; the rest live here so adding a
// new container field requires touching one helper.
//
// Env and VolumeMounts are compared against the same recipes
// buildStatefulSet uses (buildEngineContainerEnv,
// buildEngineContainerVolumeMounts) so the operator-injected entries
// don't produce false drift on read-back.
func engineContainerExtraFieldsMatch(c *corev1.Container, spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) bool {
	if !reflect.DeepEqual(c.Env, buildEngineContainerEnv(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(c.EnvFrom, effectiveEngineEnvFrom(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(c.VolumeMounts, buildEngineContainerVolumeMounts(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(c.Lifecycle, effectiveEngineLifecycle(spec, classInfo)) {
		return false
	}
	if c.WorkingDir != effectiveEngineWorkingDir(spec, classInfo) {
		return false
	}
	if !engineTerminationMessagePathEqual(c.TerminationMessagePath, effectiveEngineTerminationMessagePath(spec, classInfo)) {
		return false
	}
	if !engineTerminationMessagePolicyEqual(c.TerminationMessagePolicy, effectiveEngineTerminationMessagePolicy(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(c.VolumeDevices, effectiveEngineVolumeDevices(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(c.ResizePolicy, effectiveEngineResizePolicy(spec, classInfo)) {
		return false
	}
	return true
}

// engineTerminationMessagePathEqual treats the apiserver-defaulted
// "/dev/termination-log" as equivalent to "" so an engine that does
// not override the field compares equal to the same container read
// back from the apiserver.
func engineTerminationMessagePathEqual(actual, expected string) bool {
	if expected == "" {
		return actual == "" || actual == defaultTerminationMessagePath
	}
	return actual == expected
}

// engineTerminationMessagePolicyEqual is the policy sibling of
// engineTerminationMessagePathEqual: "" and File compare equal.
func engineTerminationMessagePolicyEqual(actual, expected corev1.TerminationMessagePolicy) bool {
	if expected == "" {
		return actual == "" || actual == corev1.TerminationMessageReadFile
	}
	return actual == expected
}

// extraPodSpecFieldsMatch is the drift comparator for the pod-spec
// fields the operator passes through verbatim from the engine template
// (with the FireboltEngineClass template's value as a fallback).
// Extracted out of stsMatchesSpec to keep the latter's cyclomatic
// complexity reasonable as the set of honored pod-template fields
// grows.
func extraPodSpecFieldsMatch(podSpec *corev1.PodSpec, spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) bool {
	if !reflect.DeepEqual(podSpec.TopologySpreadConstraints, effectiveTopologySpreadConstraints(spec, classInfo)) {
		return false
	}
	if podSpec.PriorityClassName != effectivePriorityClassName(spec, classInfo) {
		return false
	}
	if !stringPtrsEqual(podSpec.RuntimeClassName, effectiveRuntimeClassName(spec, classInfo)) {
		return false
	}
	if !dnsPolicyEqual(podSpec.DNSPolicy, effectiveDNSPolicy(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(podSpec.DNSConfig, effectiveDNSConfig(spec, classInfo)) {
		return false
	}
	if !schedulerNameEqual(podSpec.SchedulerName, effectiveSchedulerName(spec, classInfo)) {
		return false
	}
	if !preemptionPolicyEqual(podSpec.PreemptionPolicy, effectivePreemptionPolicy(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(podSpec.ReadinessGates, effectiveReadinessGates(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(podSpec.ResourceClaims, effectivePodResourceClaims(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(podSpec.HostAliases, effectiveHostAliases(spec, classInfo)) {
		return false
	}
	if !reflect.DeepEqual(podSpec.OS, effectivePodOS(spec, classInfo)) {
		return false
	}
	if !overheadEqual(podSpec.Overhead, effectivePodOverhead(spec, classInfo)) {
		return false
	}
	return true
}

// stsMatchesSpec returns true if the StatefulSet matches all mutable fields
// in the engine spec. A mismatch triggers a new blue-green generation.
// classInfo carries the resolved FireboltEngineClass template (nil when the engine
// has no engineClassRef set); when present its hash is compared against
// AnnotationEngineClassHash and the merged pod-template fields drive the
// drift checks, so an in-place edit to the class spec or a flip to a
// different class produces a clean blue-green roll.
func stsMatchesSpec(sts *appsv1.StatefulSet, spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) bool {
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != spec.Replicas {
		return false
	}
	podSpec := sts.Spec.Template.Spec
	if len(podSpec.Containers) == 0 {
		return false
	}
	container := podSpec.Containers[0]

	expectedImage, expectedPullPolicy := effectiveEngineImage(spec, classInfo)
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

	if !extraPodSpecFieldsMatch(&podSpec, spec, classInfo) {
		return false
	}

	if !sidecarsMatch(podSpec.Containers, effectiveSidecarsWithUI(spec, classInfo)) {
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

	if !engineContainerExtraFieldsMatch(&container, spec, classInfo) {
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

	if sts.Annotations[AnnotationCustomEngineConfigHash] != customEngineConfigHash(spec, classInfo) {
		return false
	}

	expectedClassHash := ""
	if classInfo != nil {
		expectedClassHash = classInfo.Hash
	}
	if sts.Annotations[AnnotationEngineClassHash] != expectedClassHash {
		return false
	}

	if !storageMatchesSpec(sts, spec, classInfo) {
		return false
	}

	return true
}

// sidecarsMatch compares the user-owned sidecar containers (those whose
// name is not EngineContainerName) against the expected sidecars from the
// FireboltEngineClass template. The engine container itself is compared
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
// engine's resolved storage spec. The spec is resolved through effectiveStorage
// (engine-if-set → class-if-set → default emptyDir), the same helper
// buildStatefulSet renders from, so a class storage change is detected as drift
// and the rendered value and comparator never diverge. VolumeClaimTemplates are
// immutable on a STS, so any drift (resizing, switching access modes or storage
// class) must trigger a new blue-green generation.
func storageMatchesSpec(sts *appsv1.StatefulSet, spec *computev1alpha1.FireboltEngineSpec, classInfo *FireboltEngineClassInfo) bool {
	dataVol := findDataPodVolume(sts)
	storage := effectiveStorage(spec, classInfo)
	switch resolveStorageBackend(storage) {
	case BackendEmptyDir:
		if len(sts.Spec.VolumeClaimTemplates) != 0 {
			return false
		}
		if dataVol == nil || dataVol.EmptyDir == nil {
			return false
		}
		// storage.EmptyDir may be nil when resolveStorageBackend
		// fell through to the default (no backend set); compare
		// against a bare EngineEmptyDirSpec{} in that case.
		var ed computev1alpha1.EngineEmptyDirSpec
		if storage.EmptyDir != nil {
			ed = *storage.EmptyDir
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
		hp := storage.HostPath
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
		pvc := resolvePersistentVolumeClaimDefaults(storage.PersistentVolumeClaim)
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
// BackendEmptyDir: engine data at /var/lib/firebolt is regenerable
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
