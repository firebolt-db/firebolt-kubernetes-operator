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

// FireboltEnginePresetDefaultName is the conventional metadata.name for
// the single FireboltEnginePreset object in a namespace. The operator
// selects by "the one Preset in the namespace" rather than this name;
// the constant exists so samples, docs, and clients agree on a name.
const FireboltEnginePresetDefaultName = "firebolt"

// FireboltEnginePresetSpec is the ambient, namespace-level engine
// overlay. Every FireboltEngine in the same namespace merges these
// fields underneath its own spec and above any referenced
// FireboltEngineClass:
//
//	engine spec > FireboltEnginePreset > FireboltEngineClass > operator default
//
// v1 admits at most one FireboltEnginePreset per namespace. The
// object is not selected by engines: customers keep referencing a
// class name (or no class). The conventional object name is
// FireboltEnginePresetDefaultName ("firebolt").
//
// The carried fields are the namespace-resolved identifiers and
// config fragments that are shared by every engine in the namespace
// (service account, credential env, storage, customEngineConfig).
// SKU-shaped settings (resources, instance type, rollout, autoStop,
// uiSidecar) stay on FireboltEngineClass.
type FireboltEnginePresetSpec struct {
	// Template is the pod-template fragment merged under every engine
	// in this namespace. See FireboltEngineClassSpec.Template for the
	// operator-owned path rejection set — the same
	// ValidateOperatorOwnedPodTemplate rules apply here.
	//
	// The CRD schema for template.metadata is patched post-controller-gen
	// (scripts/patch-crd-template-metadata.py) so labels and annotations
	// survive structural-schema pruning.
	Template corev1.PodTemplateSpec `json:"template"`

	// Storage is the default per-pod data-volume configuration for
	// engines in this namespace that do not declare a storage backend
	// of their own. An engine that sets any backend on spec.storage
	// owns its storage wholesale; otherwise this value applies, then
	// the referenced class, then the operator default (emptyDir).
	// +kubebuilder:default={}
	// +optional
	Storage EngineStorageSpec `json:"storage,omitempty"`

	// CustomEngineConfig is deep-merged into the rendered config.yaml
	// after the class config and before the engine config: operator
	// defaults, then class, then this Preset object, then the engine
	// (engine keys win on conflict). Operator-owned paths are stripped
	// before the merge, as they are from the class and the engine.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:validation:Type=object
	// +optional
	CustomEngineConfig *apiextensionsv1.JSON `json:"customEngineConfig,omitempty"`
}

// FireboltEnginePresetStatus is the observed state of a
// FireboltEnginePreset object.
type FireboltEnginePresetStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BoundEngines counts FireboltEngines in the same namespace. Preset
	// is ambient — every engine in the namespace merges it — so this
	// count is the namespace engine count, not a named-ref count. The
	// deletion webhook and the reconciler's deletion-guard finalizer
	// re-list live rather than trusting this cached value.
	// +optional
	BoundEngines int32 `json:"boundEngines,omitempty"`

	// Conditions surface the Preset object's high-level state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FireboltEnginePresetConditionReady is the top-level roll-up
// condition: True when spec.template is admissible. The validating
// webhook normally rejects offending specs at admission; the
// condition is defense in depth for objects admitted under an older
// operator with a narrower rejection set.
const FireboltEnginePresetConditionReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=firengp
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.boundEngines`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FireboltEnginePreset is a namespaced ambient overlay merged under
// every FireboltEngine in the same namespace. Engines do not reference
// it by name. v1 selects the single object in the namespace; the
// conventional name is "firebolt".
//
// It is namespaced because the template carries namespace-resolved
// identifiers (ServiceAccount names, Secret / ConfigMap references)
// that Kubernetes resolves in the engine's own namespace.
type FireboltEnginePreset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FireboltEnginePresetSpec   `json:"spec,omitempty"`
	Status FireboltEnginePresetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FireboltEnginePresetList contains a list of FireboltEnginePreset.
type FireboltEnginePresetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FireboltEnginePreset `json:"items"`
}
