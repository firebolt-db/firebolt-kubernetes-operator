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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FireboltEngineClassSpec defines the desired state of a FireboltEngineClass.
//
// Template is a Kubernetes PodTemplateSpec merged underneath an engine's
// own pod template when the engine references this class via
// spec.engineClassRef. Precedence on every field is:
//
//  1. operator defaults (lowest)
//  2. FireboltEngineClass template
//  3. FireboltEngine spec (highest)
//
// The operator owns a subset of the pod template (the engine container's
// image, command, args, ports, probes, reserved env keys; pod-level
// terminationGracePeriodSeconds, subdomain, hostname; metadata names; the
// firebolt.io/* label/annotation prefix). These paths are rejected by the
// FireboltEngineClass validating webhook at admission time so users see the
// failure immediately instead of via silent stripping. The authoritative
// rejection set is built from operatorauthority.go.
//
// Non-engine containers (sidecars) and additional init containers are
// fully user-owned: the webhook does not constrain their image, command,
// ports, or environment.
//
// Beyond Template, the class carries defaults for a subset of
// non-pod-template FireboltEngine settings (Storage, CustomEngineConfig,
// Rollout, DrainCheckEnabled, DrainCheckInterval, AutoStop). Each
// resolves engine-if-set → class-if-set → operator default: a referencing
// engine that sets the corresponding spec field owns it, the class value
// applies when the engine leaves it unset, and the operator default sits
// beneath both. This lets a platform team standardize storage, engine
// config, and rollout/autoStop policy across a fleet without repeating
// it on every FireboltEngine.
type FireboltEngineClassSpec struct {
	// Template is the pod template merged into engines that reference this
	// class. See the type-level doc for the precedence rules and the list
	// of operator-owned paths the validating webhook rejects.
	//
	// The CRD schema for template.metadata is patched post-controller-gen
	// (scripts/patch-crd-template-metadata.py, invoked by `make manifests`)
	// to set x-kubernetes-preserve-unknown-fields on the embedded
	// ObjectMeta. Without that injection, structural-schema pruning would
	// strip template.metadata.labels and template.metadata.annotations at
	// admission time and any GitOps controller re-applying them would
	// drift forever. A +kubebuilder:pruning:PreserveUnknownFields marker
	// on this Go field is not sufficient: controller-gen lands it on the
	// template schema, but the marker does not propagate into a child
	// that has its own typed sub-schema (metadata: {type: object}).
	Template corev1.PodTemplateSpec `json:"template"`

	// Storage is the default per-pod data-volume configuration for engines
	// that reference this class and do not declare a storage backend of
	// their own. An engine that sets any backend on spec.storage
	// (persistentVolumeClaim, emptyDir, or hostPath) owns its storage
	// wholesale and ignores this value; otherwise the class value applies,
	// falling through to the operator default (an emptyDir) when neither
	// side selects a backend. See EngineStorageSpec for the backend choice
	// and its mutual-exclusion rule, which this field inherits.
	// +kubebuilder:default={}
	// +optional
	Storage EngineStorageSpec `json:"storage,omitempty"`

	// CustomEngineConfig is deep-merged beneath each referencing engine's
	// own spec.customEngineConfig into the rendered config.yaml: operator
	// defaults first, then this class config, then the engine config on top
	// (engine keys win on conflict). Operator-owned paths (see
	// OperatorOwnedEngineConfigPaths) are stripped from this value before
	// the merge, exactly as they are from the engine's own config, so the
	// class cannot override identity, routing, or topology.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	// +optional
	CustomEngineConfig *apiextensionsv1.JSON `json:"customEngineConfig,omitempty"`

	// Rollout is the default rollout strategy for referencing engines that
	// leave spec.rollout unset. The operator default (graceful) applies when
	// neither the engine nor the class sets it; an empty value here means
	// the class does not override. The graceful/recreate enum is inherited
	// from RolloutStrategy.
	// +optional
	Rollout RolloutStrategy `json:"rollout,omitempty"`

	// DrainCheckEnabled is the default drain-check setting for referencing
	// engines that leave spec.drainCheckEnabled unset. The operator default
	// (true) applies when neither side sets it; nil here means the class
	// does not override.
	// +optional
	DrainCheckEnabled *bool `json:"drainCheckEnabled,omitempty"`

	// DrainCheckInterval is the default drain-poll interval for referencing
	// engines that leave spec.drainCheckInterval unset. The operator default
	// applies when neither side sets it; nil here means the class does not
	// override.
	// +optional
	DrainCheckInterval *metav1.Duration `json:"drainCheckInterval,omitempty"`

	// AutoStop is the default autoStop policy for referencing engines
	// that leave spec.autoStop unset. Resolution is whole-struct: an
	// engine that sets spec.autoStop owns the entire policy and this
	// value is not field-merged into it; when the engine omits it, the
	// class policy applies; when neither sets it, autoStop is disabled.
	// +optional
	AutoStop *AutoStopSpec `json:"autoStop,omitempty"`
}

// FireboltEngineClassStatus is the observed state of a FireboltEngineClass.
type FireboltEngineClassStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled by the
	// FireboltEngineClass controller. Lets tooling detect stale status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BoundEngines counts the FireboltEngines in the same namespace that
	// reference this class via spec.engineClassRef. Surfaced for
	// visibility (printcolumn, kubectl describe); both the FireboltEngineClass
	// deletion webhook and the reconciler's deletion-guard finalizer
	// do their own live List against the namespace rather than trusting
	// this cached value, so a class bound between reconciler runs
	// (status still at zero) is still protected from deletion.
	// +optional
	BoundEngines int32 `json:"boundEngines,omitempty"`

	// Conditions surface the FireboltEngineClass's high-level state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FireboltEngineClassConditionReady is the top-level roll-up condition on a
// FireboltEngineClass: True when the class is admissible and safe to bind. The
// validating webhook normally rejects offending specs at admission time;
// the condition is a defense in depth for classes admitted under an older
// operator with a narrower rejection set.
const FireboltEngineClassConditionReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=firengc
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.boundEngines`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FireboltEngineClass is a reusable pod-template fragment shared by multiple
// FireboltEngines in the same namespace. Engines reference a FireboltEngineClass
// by name through spec.engineClassRef and inherit its template,
// eliminating the need to repeat identical per-engine settings
// (serviceAccountName, nodeSelector, tolerations, pod annotations —
// including the cloud-provider IAM binding for kube2iam, IRSA, and Pod
// Identity).
//
// FireboltEngineClass is namespaced (not cluster-scoped like IngressClass /
// GatewayClass) because its template carries namespace-resolved
// identifiers — ServiceAccount names, Secret / ConfigMap / PVC volume
// references, and the per-tenant IAM annotations that the engine pod
// needs. Kubernetes resolves those names in the engine's own namespace
// at pod admission time, so the class and its consumer engines must
// live together. A cluster-scoped class with `serviceAccountName: foo`
// referenced from two namespaces would silently bind to two different
// ServiceAccounts (and possibly two different IAM roles) without
// admission catching the divergence.
type FireboltEngineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FireboltEngineClassSpec   `json:"spec,omitempty"`
	Status FireboltEngineClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FireboltEngineClassList contains a list of FireboltEngineClass.
type FireboltEngineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FireboltEngineClass `json:"items"`
}
