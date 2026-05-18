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
// The per-component conditions (MetadataReady, GatewayReady) surface the
// outcome of each ensure step in Reconcile. They flip to False with a
// descriptive Reason whenever the corresponding sub-reconciler returns an
// error, which replaces the previous behavior of logging-and-requeueing-
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

	// InstanceConditionMetadataReady reports whether the metadata
	// Deployment's resources were applied successfully and its pods
	// are reporting Ready. The operator does not track a separate
	// PostgresReady condition: postgres is brought up alongside
	// metadata in the same reconcile pass, and the metadata pod's
	// connection-retry surfaces a missing or unreachable database in
	// THIS condition's Reason/Message.
	InstanceConditionMetadataReady = "MetadataReady"

	// InstanceConditionGatewayReady reports whether the Envoy gateway
	// Deployment's resources were applied successfully and its pods
	// are reporting Ready.
	InstanceConditionGatewayReady = "GatewayReady"
)

// PostgresSpec configures an external PostgreSQL connection for the metadata service.
//
// The string fields below are interpolated into the XML config the operator
// renders for the metadata service (see buildMetadataConfigXML). The
// controller XML-escapes user input at render time, but the patterns here
// reject XML metacharacters at admission time as defense-in-depth so a
// malformed CR is rejected at apply rather than producing a config that
// only works because the controller scrubs it (FB-1163).
type PostgresSpec struct {
	// Host is the PostgreSQL server hostname or IP. Allowed characters are
	// letters, digits, ".", "-", ":", "[", and "]" (the last three for IPv6
	// literals like "[::1]"). XML metacharacters are rejected at admission
	// time to prevent injection into the rendered metadata config.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9.\-:\[\]]+$`
	Host string `json:"host"`

	// Port is the PostgreSQL server port.
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// Database is the PostgreSQL database name. Allowed characters are
	// letters, digits, "_", ".", and "-". XML metacharacters are rejected
	// at admission time to prevent injection into the rendered metadata
	// config.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.\-]+$`
	Database string `json:"database"`

	// Schema is the PostgreSQL schema used by the metadata service.
	// Defaults to "public". Allowed characters are letters, digits, "_",
	// ".", and "-". XML metacharacters are rejected at admission time to
	// prevent injection into the rendered metadata config.
	// +kubebuilder:default=public
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.\-]+$`
	// +optional
	Schema string `json:"schema,omitempty"`

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
//
// The Envoy `per_connection_buffer_limit_bytes` is intentionally NOT
// exposed here. The operator hard-codes it (see GatewayPerConnectionBufferLimitBytes
// in instance_gateway.go) because it sits at the center of multiple
// correctness invariants — retry coverage for the X-Firebolt-Drained
// shutdown fence, gateway memory budget under concurrent load — that
// the operator owns end-to-end. A user-tunable knob would invite
// settings that silently break the zero-downtime contract or OOM the
// gateway pod. If this trade-off needs to be revisited, raise it in
// the architecture doc rather than re-adding a field.
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

// MetricScrapeMode selects how the operator reaches engine pods to scrape
// the Prometheus /metrics endpoint that backs the drain probe and the
// autoscaler activity poll.
// +kubebuilder:validation:Enum=PodIP;ApiserverProxy
type MetricScrapeMode string

// MetricScrapeModePodIP and MetricScrapeModeApiserverProxy enumerate the
// supported scrape transports. PodIP is the production default; see the
// docstring on FireboltInstanceSpec.MetricScrapeMode for the rationale.
const (
	// MetricScrapeModePodIP makes the operator open a plain HTTP
	// connection from the controller pod directly to the engine pod IP
	// on MetricsPort. Requires the operator to run in-cluster where pod
	// IPs are routable. This is the default because it is what every
	// standard in-cluster scraper (Prometheus, OpenTelemetry, kube-
	// state-metrics) does and because EKS / kubeadm node security
	// groups do not allow apiserver->node:9090 by default.
	MetricScrapeModePodIP MetricScrapeMode = "PodIP"

	// MetricScrapeModeApiserverProxy routes the scrape through the
	// Kubernetes apiserver pods/proxy subresource. Works without a
	// network path from the operator process to the pod (e.g. when the
	// operator runs out-of-cluster via `make run`) and is RBAC-gated by
	// pods/proxy. Requires the cluster security group to allow
	// apiserver->node on MetricsPort, which is NOT the default on EKS;
	// pick this mode only when you have explicitly opened that hole.
	MetricScrapeModeApiserverProxy MetricScrapeMode = "ApiserverProxy"
)

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

	// MetricScrapeMode selects the transport the operator uses to scrape
	// the Prometheus /metrics endpoint exposed by engine pods. The
	// resulting samples drive the drain probe (which gates blue-green
	// cutover until firebolt_running_queries + firebolt_suspended_queries
	// reach zero) and the autoscaler activity poll. Both probes use
	// whatever value is set here; the field is read fresh on every
	// scrape so it can be flipped without restarting the controller.
	//
	// Defaults to PodIP: the controller pod opens a direct HTTP
	// connection to the engine pod IP on MetricsPort. This is the
	// pattern every standard in-cluster scraper uses (Prometheus
	// PodMonitor, metrics-server, OpenTelemetry, kube-state-metrics)
	// and it does not depend on apiserver->node SG rules that EKS /
	// kubeadm do not open by default. ApiserverProxy is an opt-in
	// fallback for clusters where the controller cannot reach pod IPs
	// (out-of-cluster `make run`, or networks that explicitly forbid
	// node-to-node on MetricsPort while allowing the apiserver to
	// proxy); it costs extra apiserver request load and widens the
	// blast radius of pods/proxy RBAC, so opt in only when the default
	// is structurally infeasible.
	// +kubebuilder:default=PodIP
	// +optional
	MetricScrapeMode MetricScrapeMode `json:"metricScrapeMode,omitempty"`
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
