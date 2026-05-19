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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EngineStorageSpec configures the per-pod data volume mounted into the
// engine container at the operator's data path (see the DataMountPath
// constant for the current value). Exactly one of PersistentVolumeClaim,
// EmptyDir, or HostPath should be set; if all three are nil the operator
// backs the mount with an emptyDir. Engine data at this mount is
// regenerable cache (authoritative state lives in the metadata service
// and the managed-table S3 bucket), so an ephemeral pod-local volume is
// the safe default and avoids a hard dependency on a dynamic-provisioner
// StorageClass. Opt into a per-pod PVC by setting persistentVolumeClaim
// (with or without explicit fields). The CEL rule below makes the
// three-way mutual exclusion an admission-time error rather than a
// silent "first non-nil wins" race.
// +kubebuilder:validation:XValidation:rule="(has(self.persistentVolumeClaim) ? 1 : 0) + (has(self.emptyDir) ? 1 : 0) + (has(self.hostPath) ? 1 : 0) <= 1",message="storage.persistentVolumeClaim, storage.emptyDir, and storage.hostPath are mutually exclusive"
type EngineStorageSpec struct {
	// PersistentVolumeClaim opts the engine into a per-pod
	// PersistentVolumeClaim synthesized by the StatefulSet controller
	// from a VolumeClaimTemplate. Set the field (even to an empty
	// struct) to enable the PVC backend; the kubebuilder defaults
	// (1Gi, ReadWriteOnce, cluster-default StorageClass) on
	// EnginePersistentVolumeClaimSpec fill in any omitted inner field.
	// When this field is nil the engine falls back to the default
	// emptyDir backend (see the EngineStorageSpec doc above).
	// +optional
	PersistentVolumeClaim *EnginePersistentVolumeClaimSpec `json:"persistentVolumeClaim,omitempty"`

	// EmptyDir backs the engine's data mount with a pod-scoped emptyDir
	// Volume. The mount holds regenerable cache only; authoritative
	// data lives in the metadata service and the managed-table S3
	// bucket (see EngineStorageSpec for the full picture), so volume
	// contents being lost on pod restart is normal engine behavior
	// rather than a durability regression.
	// +optional
	EmptyDir *EngineEmptyDirSpec `json:"emptyDir,omitempty"`

	// HostPath backs the engine's data mount with a directory on the
	// node filesystem. Pods are implicitly node-pinned (the directory only
	// exists where it was created); the path must pre-exist on every
	// node the engine pod could be scheduled onto, with permissions
	// readable by the engine UID. Use to expose instance-store NVMe on
	// the node.
	// +optional
	HostPath *EngineHostPathSpec `json:"hostPath,omitempty"`
}

// EnginePersistentVolumeClaimSpec configures the per-pod PersistentVolumeClaim
// the StatefulSet controller synthesizes from the VolumeClaimTemplate. Each
// field is optional; omit the whole struct to accept the operator's defaults.
type EnginePersistentVolumeClaimSpec struct {
	// Size is the requested capacity for each engine pod's PVC. Defaults to 1Gi.
	// +kubebuilder:default="1Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// AccessModes for the PVC. Defaults to [ReadWriteOnce], which matches the
	// per-pod ownership model used by the StatefulSet VolumeClaimTemplate.
	// +kubebuilder:default={ReadWriteOnce}
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// StorageClassName selects the StorageClass for the PVC. Leave nil to use
	// the cluster default. An empty string disables dynamic provisioning.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// EngineEmptyDirSpec configures an emptyDir-backed engine data volume.
type EngineEmptyDirSpec struct {
	// Medium is the storage medium backing the emptyDir. Leave empty
	// for the default node disk; set to "Memory" for a tmpfs-backed
	// volume sized against the pod's memory limit.
	// +kubebuilder:validation:Enum="";Memory
	// +optional
	Medium corev1.StorageMedium `json:"medium,omitempty"`

	// SizeLimit caps the volume's growth. Optional; nil means
	// unbounded (subject to node disk pressure or pod memory limits
	// for Memory medium).
	// +optional
	SizeLimit *resource.Quantity `json:"sizeLimit,omitempty"`
}

// EngineHostPathSpec configures a hostPath-backed engine data volume.
type EngineHostPathSpec struct {
	// Path is the absolute filesystem path on the host node.
	// +kubebuilder:validation:Pattern=`^/.*`
	Path string `json:"path"`

	// Type enforces a check on Path before mounting (Directory,
	// DirectoryOrCreate, …). See
	// https://kubernetes.io/docs/concepts/storage/volumes/#hostpath
	// for the full enum. Leave nil to skip the check (kubelet default).
	// +optional
	Type *corev1.HostPathType `json:"type,omitempty"`
}

// AutoscalingSpec configures replica autoscaling for a FireboltEngine.
//
// When Enabled is true the autoscaler owns spec.replicas: the controller will
// mutate spec.replicas between zero (or MinReplicas) and MaxReplicas based on
// the engine's observed query activity and the optional always-on Schedule.
// User edits to spec.replicas while autoscaling is enabled are converged on
// the next reconcile, so the user-facing "active" replica count is
// MaxReplicas.
//
// Activity is observed via the same Prometheus gauges
// (firebolt_running_queries + firebolt_suspended_queries) that drive the
// blue-green drain check, so a wake-up triggered by query traffic transitions
// out of MinReplicas without an additional probe protocol.
// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= (has(self.minReplicas) ? self.minReplicas : 0)",message="maxReplicas must be >= minReplicas"
type AutoscalingSpec struct {
	// Enabled turns the autoscaler on for this engine. Defaults to false.
	// +kubebuilder:default=false
	Enabled bool `json:"enabled"`

	// MaxReplicas is the replica count the autoscaler scales up to when the
	// engine is active (during a Schedule window or following a wake-up).
	// Required when Enabled is true.
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`

	// MinReplicas is the floor the autoscaler scales down to once IdleTimeout
	// elapses with no observed query activity. 0 enables scale-to-zero.
	// Defaults to 0.
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// IdleTimeout is how long the engine must observe zero in-flight and
	// suspended queries before the autoscaler scales down to MinReplicas.
	// Defaults to 30 minutes.
	// +kubebuilder:default="30m"
	// +optional
	IdleTimeout *metav1.Duration `json:"idleTimeout,omitempty"`

	// PollInterval is how often the autoscaler scrapes engine metrics to
	// re-evaluate idleness. Defaults to 1 minute.
	// +kubebuilder:default="1m"
	// +optional
	PollInterval *metav1.Duration `json:"pollInterval,omitempty"`

	// Schedule is an optional list of UTC time windows during which the
	// autoscaler holds the engine at MaxReplicas regardless of observed
	// activity. Useful for "always-on during business hours" policies.
	// +optional
	Schedule []ScheduleWindow `json:"schedule,omitempty"`
}

// ScheduleWindow is a recurring UTC time window during which the autoscaler
// keeps the engine pinned at MaxReplicas. End may be less than Start to
// express a window that crosses midnight (e.g. 22:00-02:00).
type ScheduleWindow struct {
	// Start is the window opening time in UTC, formatted "HH:MM".
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	Start string `json:"start"`

	// End is the window closing time in UTC, formatted "HH:MM". An End equal
	// to Start is treated as an empty window.
	// +kubebuilder:validation:Pattern=`^([01]\d|2[0-3]):[0-5]\d$`
	End string `json:"end"`

	// Days lists the UTC weekdays the window applies to. Empty means every
	// day. The window is anchored to the day on which Start falls; if End
	// crosses midnight the trailing portion still belongs to the listed day.
	// +optional
	Days []ScheduleDay `json:"days,omitempty"`
}

// ScheduleDay is a UTC weekday code used in ScheduleWindow.Days.
// +kubebuilder:validation:Enum=Mon;Tue;Wed;Thu;Fri;Sat;Sun
type ScheduleDay string

// RolloutStrategy defines how transitions between generations are handled.
// +kubebuilder:validation:Enum=graceful;recreate
type RolloutStrategy string

// RolloutGraceful and RolloutRecreate define the supported rollout strategies.
const (
	RolloutGraceful RolloutStrategy = "graceful"
	RolloutRecreate RolloutStrategy = "recreate"
)

// EnginePhase represents the current phase of the engine transition.
type EnginePhase string

// PhaseStable through PhaseStopped enumerate the lifecycle phases
// of a FireboltEngine during a blue-green rollout.
//
// PhaseStopped is a terminal phase reached when spec.replicas is 0.
// It is structurally identical to PhaseStable: the active generation
// exists (as an empty StatefulSet + headless Service + ConfigMap) and
// any spec drift triggers a new blue-green generation. The distinct
// name exists so kubectl get and GitOps tooling can tell a running
// engine apart from an intentionally parked one.
const (
	PhaseStable    EnginePhase = "stable"
	PhaseCreating  EnginePhase = "creating"
	PhaseSwitching EnginePhase = "switching"
	PhaseDraining  EnginePhase = "draining"
	PhaseCleaning  EnginePhase = "cleaning"
	PhaseStopped   EnginePhase = "stopped"
)

// Condition types for FireboltEngine.
const (
	// ConditionReady is the top-level roll-up condition: True only when
	// the engine is serving traffic on its active generation with all
	// replicas ready, the backing FireboltInstance is healthy, and no
	// transition is in progress. GitOps / ArgoCD-style tooling should
	// key off this condition rather than Phase to decide whether a
	// deployment has converged.
	//
	// Reasons for False:
	//   - Initializing     : status has not been populated yet
	//   - InstanceNotReady : ConditionInstanceReady is False
	//   - Rolling          : phase is Creating / Switching / Draining / Cleaning
	//   - PhaseFailed      : phase is Failed (terminal; human-gated recovery)
	//   - PodsNotReady     : phase is Stable but active-generation pods
	//                        have not yet reported Ready
	//   - Stopped          : phase is Stopped (spec.replicas is 0);
	//                        the engine is intentionally parked and
	//                        cannot serve traffic until replicas > 0
	ConditionReady = "Ready"

	// ConditionInstanceReady indicates whether the referenced FireboltInstance
	// has a populated metadata endpoint and instance ID.
	ConditionInstanceReady = "InstanceReady"
)

// FireboltEngineSpec defines the desired state of a Firebolt engine.
//
// The CEL rule freezes spec.metadataEndpointOverride once it has been set
// (or unset) at creation time. The field selects the cross-cluster metadata
// topology the engine's nodes bake into their on-disk configuration; letting
// users mutate it later would silently force a full blue-green rollout (it
// participates in stsMatchesSpec via AnnotationMetadataOverride) and would
// break the instanceInfo-drift invariant computeStable relies on to allow
// in-place recovery of a deleted ConfigMap (rebuild-at-same-gen produces
// byte-identical content only when all ConfigMap inputs are frozen).
// The wording mirrors the set-once pattern on FireboltInstanceSpec.ID,
// adapted for an optional pointer field via has(): absent-on-create may
// still transition to set, but any set value may not subsequently be
// changed or cleared.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.metadataEndpointOverride) || (has(self.metadataEndpointOverride) && self.metadataEndpointOverride == oldSelf.metadataEndpointOverride)",message="spec.metadataEndpointOverride is immutable once set"
type FireboltEngineSpec struct {
	// InstanceRef is the name of the FireboltInstance in the same namespace
	// that this engine depends on. The engine reconciler will not proceed
	// until the referenced instance has a populated metadata endpoint and
	// instance ID. This field is immutable after creation.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="instanceRef is immutable"
	InstanceRef string `json:"instanceRef"`

	// Replicas is the number of engine nodes. Set to 0 to stop the
	// engine: the operator tears down the active generation (honoring
	// spec.rollout for drain behavior) and leaves the CR in the
	// Stopped phase. Setting replicas back to a non-zero value resumes
	// the engine via a new blue-green generation.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// Image defines the container image to use.
	// If not specified, defaults to the engine image embedded in the operator binary.
	// +optional
	Image *ImageSpec `json:"image,omitempty"`

	// Resources defines the Kubernetes resource requests and limits for engine pods.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// DrainCheckEnabled controls whether the operator performs a SQL-based drain
	// check on old-generation pods during graceful rollouts. When false, the
	// operator skips directly to cleaning after switching traffic, without
	// verifying that in-flight queries have completed. Requires a running
	// node that can execute the drain-check query when enabled.
	// +kubebuilder:default=true
	// +optional
	DrainCheckEnabled *bool `json:"drainCheckEnabled,omitempty"`

	// DrainCheckInterval controls how often to check if old pods have finished serving queries.
	// Only used when drainCheckEnabled is true.
	// +optional
	DrainCheckInterval *metav1.Duration `json:"drainCheckInterval,omitempty"`

	// NodeSelector constrains which nodes the engine pods can be scheduled on.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Tolerations allow engine pods to be scheduled on tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// Affinity defines scheduling affinity rules for engine pods. Supports the
	// full Kubernetes corev1.Affinity shape: nodeAffinity, podAffinity, and
	// podAntiAffinity. Changes to this field trigger a new blue-green generation
	// (see stsMatchesSpec).
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// PodLabels are additional labels to apply to engine pods. The operator
	// reserves "firebolt.io/engine" and "firebolt.io/generation" for its own
	// use; user-provided values for these keys are silently ignored. All other
	// labels are passed through verbatim.
	//
	// Changes to this field trigger a new blue-green generation.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// PodAnnotations are additional annotations to apply to engine pods.
	// Operator-managed pod annotations always win over user-provided values
	// with the same key, so user input cannot accidentally drop or shadow
	// them. All other annotations are passed through verbatim.
	//
	// Changes to this field trigger a new blue-green generation.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// ServiceAccountName is the name of the ServiceAccount to run engine pods as.
	// If unset, StatefulSet pods use the namespace default ServiceAccount.
	// Changing this value triggers a new blue-green generation (see stsMatchesSpec).
	// +kubebuilder:validation:MinLength=1
	// +optional
	ServiceAccountName *string `json:"serviceAccountName,omitempty"`

	// Rollout strategy for transitions: "graceful" waits for drain, "recreate" deletes immediately.
	// +kubebuilder:default=graceful
	// +optional
	Rollout RolloutStrategy `json:"rollout,omitempty"`

	// MetadataEndpointOverride overrides the metadata endpoint for this engine.
	// If nil, the engine uses Instance.status.metadataEndpoint (with intra-cluster
	// topology-aware routing). Set this for cross-cluster scenarios where the engine
	// connects to a metadata service in a different cluster via private link.
	//
	// Immutable once set: see the struct-level CEL rule on FireboltEngineSpec.
	// +optional
	MetadataEndpointOverride *string `json:"metadataEndpointOverride,omitempty"`

	// Storage configures the per-pod data volume mounted into each engine
	// container. See EngineStorageSpec for the backend choice (emptyDir is
	// the default; persistentVolumeClaim or hostPath are opt-ins). Changes
	// to any field — including switching backends — force a new blue-green
	// generation, because the StatefulSet's VolumeClaimTemplates are
	// immutable and a backend swap necessarily reshapes the pod-template
	// data Volume.
	// +kubebuilder:default={}
	// +optional
	Storage EngineStorageSpec `json:"storage,omitempty"`

	// TerminationGracePeriodSeconds is the grace period given to engine pods
	// between SIGTERM and SIGKILL during termination. On SIGTERM the engine
	// waits up to (TerminationGracePeriodSeconds - 5s) for in-flight queries
	// to finish before exiting; Envoy's active health checks eject the pod
	// from the gateway load-balancer within ~1s of SIGTERM so no new queries
	// are routed to a draining pod.
	//
	// Defaults to 60 seconds. Raise it for workloads with analytical
	// queries that routinely exceed a minute; lower it for latency-bounded
	// workloads where quicker rollouts are preferable to query survival at
	// the tail.
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=1
	// +optional
	TerminationGracePeriodSeconds *int64 `json:"terminationGracePeriodSeconds,omitempty"`

	// CustomEngineConfig is a free-form object deep-merged into the rendered
	// engine config.yaml at the root. The rendered document follows the
	// structured schema consumed by `firebolt server` (top-level sections:
	// `auth`, `engine`, `instance`, `logging`, `schema_version`); keys
	// placed in customEngineConfig at the same paths are merged into the
	// corresponding sections, overriding the operator's defaults (e.g.
	// `logging.format`, `logging.level`, `auth.*`). Any additional sections
	// understood by the engine binary may be set at the root.
	//
	// The operator retains authority over identity, routing, and topology
	// paths. The following are silently stripped from user input before the
	// merge and cannot be overridden via this field:
	//
	//   - schema_version
	//   - instance.id
	//   - instance.type
	//   - instance.multi_engine (entire subtree, including metadata_endpoint)
	//   - engine.id
	//   - engine.nodes
	//   - engine.termination_grace_period
	//
	// Changes to this field trigger a new blue-green generation, since the
	// rendered config.yaml content changes.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	// +optional
	CustomEngineConfig *apiextensionsv1.JSON `json:"customEngineConfig,omitempty"`

	// PodSecurityContext sets pod-level security attributes stamped on the
	// engine pod template. The operator unconditionally applies an fsGroup
	// (3473) so the kernel chowns the per-pod data PVC for the engine
	// process; setting fsGroup here overrides that default. All other fields
	// are passed through verbatim.
	//
	// Changes to this field trigger a new blue-green generation.
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// SecurityContext sets container-level security attributes for the
	// engine container. The value is passed through verbatim; the operator
	// applies no defaults at the container scope.
	//
	// Changes to this field trigger a new blue-green generation.
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// InitContainers are additional init containers stamped on the engine pod
	// template before the engine container starts. Use this for node-local data
	// prep (for example chown on a hostPath volume at the data mount path).
	// Init containers must mount pod volumes explicitly (typically the "data"
	// volume at /firebolt-core/volume). The operator does not inject volume
	// mounts or mutate container definitions.
	//
	// Changes to this field trigger a new blue-green generation.
	// +optional
	// +listType=atomic
	InitContainers []corev1.Container `json:"initContainers,omitempty"`

	// Autoscaling configures automatic replica management for this engine.
	// When omitted or with Enabled=false, replicas is governed entirely by
	// the user. When enabled, the autoscaler owns spec.replicas (HPA-style)
	// and mutates it based on query activity and Schedule windows.
	// +optional
	Autoscaling *AutoscalingSpec `json:"autoscaling,omitempty"`
}

// FireboltEngineStatus defines the observed state of a Firebolt engine.
type FireboltEngineStatus struct {
	// ObservedGeneration is the metadata.generation that was last fully reconciled to stable.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CurrentGeneration is the latest generation number created.
	CurrentGeneration int `json:"currentGeneration"`

	// ActiveGeneration is the generation currently serving traffic.
	ActiveGeneration int `json:"activeGeneration"`

	// DrainingGeneration is the generation being drained, if any.
	// +optional
	DrainingGeneration *int `json:"drainingGeneration,omitempty"`

	// Phase is the current lifecycle phase of the engine.
	// +optional
	Phase EnginePhase `json:"phase,omitempty"`

	// LastReconciled is the timestamp of the last reconciliation.
	// +optional
	LastReconciled *metav1.Time `json:"lastReconciled,omitempty"`

	// LastActivityTime is the timestamp of the most recent autoscaler
	// observation that recorded in-flight or suspended queries. The
	// autoscaler scales down once now() - LastActivityTime exceeds
	// spec.autoscaling.idleTimeout. Cleared when autoscaling is disabled.
	// +optional
	LastActivityTime *metav1.Time `json:"lastActivityTime,omitempty"`

	// AutoscaledAt is the timestamp of the most recent autoscaler-driven
	// mutation of spec.replicas. Distinguishes autoscaler scale events from
	// user edits in audit trails.
	// +optional
	AutoscaledAt *metav1.Time `json:"autoscaledAt,omitempty"`

	// AutoscalerReason is a short token describing the most recent autoscaler
	// decision: "Idle", "ScheduleActive", "ActivityObserved", "ScrapeFailed",
	// "Disabled", "Stopped".
	// +optional
	AutoscalerReason string `json:"autoscalerReason,omitempty"`

	// Conditions represent the latest available observations of the engine's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fireng
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Generation",type=integer,JSONPath=`.status.activeGeneration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FireboltEngine is the Schema for the fireboltengines API.
type FireboltEngine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FireboltEngineSpec   `json:"spec,omitempty"`
	Status FireboltEngineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FireboltEngineList contains a list of FireboltEngine.
type FireboltEngineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FireboltEngine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FireboltEngine{}, &FireboltEngineList{})
}
