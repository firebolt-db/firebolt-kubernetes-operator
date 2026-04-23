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

// InstancePhase represents the lifecycle phase of a Firebolt Instance.
// +kubebuilder:validation:Enum=Provisioning;Ready;Degraded;Failed
type InstancePhase string

// InstancePhaseProvisioning through InstancePhaseFailed enumerate
// the lifecycle phases of a FireboltInstance.
const (
	InstancePhaseProvisioning InstancePhase = "Provisioning"
	InstancePhaseReady        InstancePhase = "Ready"
	InstancePhaseDegraded     InstancePhase = "Degraded"
	InstancePhaseFailed       InstancePhase = "Failed"
)

// Condition types for FireboltInstance.
//
// The per-component conditions (PostgresReady, MetadataReady, GatewayReady)
// surface the outcome of each ensure step in Reconcile. They flip to False
// with a descriptive Reason whenever the corresponding sub-reconciler returns
// an error, which replaces the previous behavior of logging-and-requeueing-
// silently. The roll-up InstanceConditionReady is False whenever any
// per-component condition is not True, carrying the first blocker's
// Reason/Message so `kubectl describe` shows the root cause without digging.
//
// These conditions are additive: the boolean Status.*Ready fields are
// kept for backward compatibility and for printcolumn display. The
// conditions carry the human-readable Reason/Message that booleans
// cannot.
const (
	// InstanceConditionReady is the top-level roll-up: True iff every
	// required per-component condition is True. GitOps tooling should
	// key off this condition rather than Phase, because Phase is a
	// summary enum derived from the same booleans and therefore
	// cannot distinguish "stuck on Postgres" from "stuck on gateway".
	InstanceConditionReady = "Ready"

	// InstanceConditionPostgresReady reports whether the metadata
	// PostgreSQL backend is reachable and has at least one ready
	// replica. For external Postgres, this also covers the
	// credential-secret preflight that checkExternalPostgresSecret
	// performs before the metadata Deployment is rolled out.
	InstanceConditionPostgresReady = "PostgresReady"

	// InstanceConditionMetadataReady reports whether the metadata
	// Deployment's resources were applied successfully and its pods
	// are reporting Ready. A pod that fails readiness because
	// Postgres is unreachable will flip THIS condition to False while
	// PostgresReady remains True, because the metadata pod owns the
	// DB connection error in its own status.
	InstanceConditionMetadataReady = "MetadataReady"

	// InstanceConditionAccountReady is kept for API backwards compatibility.
	// Account creation is now handled internally by Pensieve Dedicated;
	// the operator no longer sets this condition.
	InstanceConditionAccountReady = "AccountReady"

	// InstanceConditionGatewayReady reports whether the Envoy gateway
	// Deployment's resources were applied successfully and its pods
	// are reporting Ready.
	InstanceConditionGatewayReady = "GatewayReady"
)

// PostgresSpec configures an external PostgreSQL connection for the metadata service.
type PostgresSpec struct {
	// Host is the PostgreSQL server hostname or IP.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// Port is the PostgreSQL server port.
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// Database is the PostgreSQL database name.
	// +kubebuilder:validation:MinLength=1
	Database string `json:"database"`

	// CredentialsSecretRef references a Secret containing "username" and "password" keys.
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`
}

// MetadataSpec configures the metadata service.
// Only replicas=1 is currently supported; multi-replica metadata is not yet
// available. The CEL rule below enforces this at admission time, in addition
// to the Go-level check in the validating webhook (kept for defense-in-depth
// and to surface a clearer error message when the webhook is in the request path).
// +kubebuilder:validation:XValidation:rule="!has(self.replicas) || self.replicas == 1",message="metadata replicas must be 1"
type MetadataSpec struct {
	ComponentSpec `json:",inline"`

	// Postgres configures the external PostgreSQL connection.
	// If nil, the operator deploys an internal PostgreSQL instance.
	// +optional
	Postgres *PostgresSpec `json:"postgres,omitempty"`

	// EngineRegistration enables registration of Engine objects in the metadata service for SQL-level RBAC.
	// +kubebuilder:default=false
	// +optional
	EngineRegistration bool `json:"engineRegistration,omitempty"`
}

// GatewaySpec configures the gateway component.
type GatewaySpec struct {
	ComponentSpec `json:",inline"`
}

// AuthMode defines the authentication mode for the Firebolt Instance.
// +kubebuilder:validation:Enum=disabled;native;openid
type AuthMode string

// AuthModeDisabled through AuthModeOpenID enumerate the supported
// authentication modes.
const (
	AuthModeDisabled AuthMode = "disabled"
	AuthModeNative   AuthMode = "native"
	AuthModeOpenID   AuthMode = "openid"
)

// OIDCSpec configures OpenID Connect authentication.
type OIDCSpec struct {
	// IssuerURL is the OIDC provider's issuer URL.
	// +kubebuilder:validation:MinLength=1
	IssuerURL string `json:"issuerURL"`

	// ClientID is the OIDC client identifier.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`

	// ClientSecretRef references a Secret containing the OIDC client secret.
	// +optional
	ClientSecretRef *corev1.LocalObjectReference `json:"clientSecretRef,omitempty"`

	// ClaimMappings maps OIDC claims to Firebolt user attributes (e.g. {"username": "email"}).
	// +optional
	ClaimMappings map[string]string `json:"claimMappings,omitempty"`
}

// AuthSpec configures authentication for the Firebolt Instance.
// TODO: the operator does not enforce auth yet. This spec is persisted in
// the CRD so that it can later be propagated to engine node configuration
// (e.g. via ConfigMap or environment variables) to enable native or OIDC
// authentication at the engine level.
type AuthSpec struct {
	// Mode is the authentication mode.
	Mode AuthMode `json:"mode"`

	// OIDC configures OpenID Connect. Required when mode is "openid".
	// +optional
	OIDC *OIDCSpec `json:"oidc,omitempty"`
}

// FireboltInstanceSpec defines the desired state of a Firebolt Instance.
type FireboltInstanceSpec struct {
	// ID is a stable unique identifier for this instance, used as the metadata
	// account ID. If empty on creation, a ULID is generated automatically by
	// the defaulting webhook. Once set, this field is immutable.
	//
	// The CEL rule allows the one-time "" -> <ulid> transition because when
	// the mutating webhook is disabled (local dev, kind, some E2E setups),
	// the controller Reconcile has a fallback that generates an ID and
	// Updates the CR. A plain `self == oldSelf` would reject that Update at
	// admission time and leave the instance permanently stuck without an ID.
	// Once ID is non-empty, subsequent Updates are still blocked from
	// changing it.
	// +optional
	// +kubebuilder:validation:XValidation:rule="oldSelf == '' || self == oldSelf",message="spec.id is immutable once set"
	ID string `json:"id,omitempty"`

	// Metadata configures the metadata service.
	Metadata MetadataSpec `json:"metadata"`

	// Gateway configures the query routing gateway (Envoy proxy).
	Gateway GatewaySpec `json:"gateway"`

	// Auth configures authentication for engine nodes.
	// TODO: not enforced yet; will be propagated to engine configuration in a future release.
	// +optional
	Auth *AuthSpec `json:"auth,omitempty"`
}

// FireboltInstanceStatus defines the observed state of a Firebolt Instance.
type FireboltInstanceStatus struct {
	// Phase is the current lifecycle phase of the Instance.
	// +optional
	Phase InstancePhase `json:"phase,omitempty"`

	// MetadataReady indicates whether the metadata service is healthy.
	// +optional
	MetadataReady bool `json:"metadataReady,omitempty"`

	// MetadataEndpoint is the resolved Service address.
	// The Engine reconciler uses this to write engine ConfigMaps.
	// +optional
	MetadataEndpoint string `json:"metadataEndpoint,omitempty"`

	// AccountReady is kept for API backwards compatibility.
	// Account creation is now handled internally by Pensieve Dedicated.
	// +optional
	AccountReady bool `json:"accountReady,omitempty"`

	// GatewayReady indicates whether the gateway is healthy.
	// +optional
	GatewayReady bool `json:"gatewayReady,omitempty"`

	// GatewayEndpoint is the resolved gateway Service address.
	// +optional
	GatewayEndpoint string `json:"gatewayEndpoint,omitempty"`

	// Conditions represent the latest available observations of the Instance's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fire
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Gateway",type=boolean,JSONPath=`.status.gatewayReady`
// +kubebuilder:printcolumn:name="Metadata",type=boolean,JSONPath=`.status.metadataReady`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// FireboltInstance is the Schema for the fireboltinstances API.
type FireboltInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FireboltInstanceSpec   `json:"spec"`
	Status FireboltInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FireboltInstanceList contains a list of FireboltInstance.
type FireboltInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FireboltInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FireboltInstance{}, &FireboltInstanceList{})
}
