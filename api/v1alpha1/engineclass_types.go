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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EngineClassSpec defines the desired state of an EngineClass.
//
// Template is a Kubernetes PodTemplateSpec merged underneath an engine's
// own pod template when the engine references this class via
// spec.engineClassRef. Precedence on every field is:
//
//  1. operator defaults (lowest)
//  2. EngineClass template
//  3. FireboltEngine spec (highest)
//
// The operator owns a subset of the pod template (the engine container's
// image, command, args, ports, probes, reserved env keys; pod-level
// terminationGracePeriodSeconds, subdomain, hostname; metadata names; the
// firebolt.io/* label/annotation prefix). These paths are rejected by the
// EngineClass validating webhook at admission time so users see the
// failure immediately instead of via silent stripping. The authoritative
// rejection set is built from operatorauthority.go.
//
// Non-engine containers (sidecars) and additional init containers are
// fully user-owned: the webhook does not constrain their image, command,
// ports, or environment.
type EngineClassSpec struct {
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
}

// EngineClassStatus is the observed state of an EngineClass.
type EngineClassStatus struct {
	// ObservedGeneration is the metadata.generation last reconciled by the
	// EngineClass controller. Lets tooling detect stale status.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// BoundEngines counts the FireboltEngines in the same namespace that
	// reference this class via spec.engineClassRef. Surfaced for
	// visibility (printcolumn, kubectl describe); the EngineClass
	// deletion webhook does its own live List against the namespace
	// rather than trusting this cached value, so a class bound between
	// reconciler runs (status still at zero) is still protected from
	// deletion.
	// +optional
	BoundEngines int32 `json:"boundEngines,omitempty"`

	// Conditions surface the EngineClass's high-level state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// EngineClassConditionReady is the top-level roll-up condition on an
// EngineClass: True when the class is admissible and safe to bind. The
// validating webhook normally rejects offending specs at admission time;
// the condition is a defense in depth for classes admitted under an older
// operator with a narrower rejection set.
const EngineClassConditionReady = "Ready"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=firec
// +kubebuilder:printcolumn:name="Bound",type=integer,JSONPath=`.status.boundEngines`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EngineClass is a reusable pod-template fragment shared by multiple
// FireboltEngines in the same namespace. Engines reference an EngineClass
// by name through spec.engineClassRef and inherit its template,
// eliminating the need to repeat identical per-engine settings
// (serviceAccountName, nodeSelector, tolerations, pod annotations —
// including the cloud-provider IAM binding for kube2iam, IRSA, and Pod
// Identity).
//
// EngineClass is namespaced (not cluster-scoped like IngressClass /
// GatewayClass) because its template carries namespace-resolved
// identifiers — ServiceAccount names, Secret / ConfigMap / PVC volume
// references, and the per-tenant IAM annotations that the engine pod
// needs. Kubernetes resolves those names in the engine's own namespace
// at pod admission time, so the class and its consumer engines must
// live together. A cluster-scoped class with `serviceAccountName: foo`
// referenced from two namespaces would silently bind to two different
// ServiceAccounts (and possibly two different IAM roles) without
// admission catching the divergence.
type EngineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EngineClassSpec   `json:"spec,omitempty"`
	Status EngineClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EngineClassList contains a list of EngineClass.
type EngineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EngineClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&EngineClass{}, &EngineClassList{})
}
