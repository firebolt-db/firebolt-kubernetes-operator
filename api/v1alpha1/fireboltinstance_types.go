/*
Copyright 2025.

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

const (
	InstancePhaseProvisioning InstancePhase = "Provisioning"
	InstancePhaseReady        InstancePhase = "Ready"
	InstancePhaseDegraded     InstancePhase = "Degraded"
	InstancePhaseFailed       InstancePhase = "Failed"
)

// AuthMode defines the authentication mode for the Firebolt Instance.
// +kubebuilder:validation:Enum=disabled;native;openid
type AuthMode string

const (
	AuthModeDisabled AuthMode = "disabled"
	AuthModeNative   AuthMode = "native"
	AuthModeOpenID   AuthMode = "openid"
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
// The operator deploys Metadata using an embedded Helm chart, so these fields
// map to chart values rather than raw Kubernetes pod specs.
type MetadataSpec struct {
	ComponentSpec `json:",inline"`

	// Postgres configures the external PostgreSQL connection.
	// If nil, the operator deploys an internal PostgreSQL instance.
	// +optional
	Postgres *PostgresSpec `json:"postgres,omitempty"`

	// EngineRegistration enables registration of Engine objects in Pensieve for SQL-level RBAC.
	// +kubebuilder:default=false
	// +optional
	EngineRegistration bool `json:"engineRegistration,omitempty"`
}

// GatewaySpec configures the gateway component.
// The operator deploys the gateway using an embedded Helm chart.
type GatewaySpec struct {
	ComponentSpec `json:",inline"`
}

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
type AuthSpec struct {
	// Mode is the authentication mode.
	Mode AuthMode `json:"mode"`

	// OIDC configures OpenID Connect. Required when mode is "openid".
	// +optional
	OIDC *OIDCSpec `json:"oidc,omitempty"`
}

// FireboltInstanceSpec defines the desired state of a Firebolt Instance.
type FireboltInstanceSpec struct {
	// Metadata configures the metadata service.
	Metadata MetadataSpec `json:"metadata"`

	// Gateway configures the query routing gateway.
	Gateway GatewaySpec `json:"gateway"`

	// Auth configures authentication. If nil, authentication is disabled.
	// +optional
	Auth *AuthSpec `json:"auth,omitempty"`
}

// FireboltInstanceStatus defines the observed state of a Firebolt Instance.
type FireboltInstanceStatus struct {
	// Phase is the current lifecycle phase of the Instance.
	// +optional
	Phase InstancePhase `json:"phase,omitempty"`

	// AccountID is the metadata account identifier, resolved during first
	// reconciliation and reused thereafter.
	// +optional
	AccountID string `json:"accountId,omitempty"`

	// MetadataReady indicates whether the metadata service is healthy.
	// +optional
	MetadataReady bool `json:"metadataReady,omitempty"`

	// MetadataEndpoint is the resolved Service address.
	// The Engine reconciler uses this to write engine ConfigMaps.
	// +optional
	MetadataEndpoint string `json:"metadataEndpoint,omitempty"`

	// GatewayReady indicates whether the gateway is healthy.
	// +optional
	GatewayReady bool `json:"gatewayReady,omitempty"`

	// GatewayEndpoint is the resolved gateway Service address.
	// +optional
	GatewayEndpoint string `json:"gatewayEndpoint,omitempty"`

	// EngineCount is the total number of Engine CRs in this namespace.
	// +optional
	EngineCount int32 `json:"engineCount,omitempty"`

	// ReadyEngines is the number of engines in a Ready state.
	// +optional
	ReadyEngines int32 `json:"readyEngines,omitempty"`

	// Conditions represent the latest available observations of the Instance's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=fi;inst
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Gateway",type=boolean,JSONPath=`.status.gatewayReady`
// +kubebuilder:printcolumn:name="Metadata",type=boolean,JSONPath=`.status.metadataReady`
// +kubebuilder:printcolumn:name="Engines",type=integer,JSONPath=`.status.engineCount`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyEngines`
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
