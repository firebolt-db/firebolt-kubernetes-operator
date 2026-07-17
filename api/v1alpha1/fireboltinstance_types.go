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

	// InstanceConditionAuthReady reports whether Instance-wide auth
	// provisioning (the admin credentials preflight and the JWT signing
	// keypair) has completed. True with reason "Disabled" when
	// spec.auth is unset or disabled. Unlike MetadataReady and
	// GatewayReady, this condition is deliberately NOT one of the
	// components setInstanceReadyRollup rolls up into
	// InstanceConditionReady: auth provisioning has no bearing on
	// whether the metadata service or gateway are usable, and engines
	// gate their own reconcile on Status.Auth directly rather than on
	// the top-level Ready condition.
	InstanceConditionAuthReady = "AuthReady"

	// InstanceConditionEngineTLSReady reports whether the engine-listener
	// TLS server certificate (spec.tls.engine) has been provisioned. True
	// with reason "Disabled" when spec.tls.engine is unset or disabled.
	// Rolled up into InstanceConditionReady (see setInstanceReadyRollup):
	// when engine TLS is requested the Instance must not report Ready until
	// the certificate is issued, so it never advertises a secure posture it
	// has not yet reached (the gateway would otherwise re-encrypt to engines
	// in plaintext during provisioning). Engines still gate their own
	// reconcile on Status.EngineTLS directly, not on this top-level condition.
	InstanceConditionEngineTLSReady = "EngineTLSReady"

	// InstanceConditionGatewayTLSReady reports whether the gateway's
	// client-facing (downstream) TLS server certificate (spec.tls.gateway)
	// has been provisioned. True with reason "Disabled" when
	// spec.tls.gateway is unset or disabled. Rolled up into
	// InstanceConditionReady (see setInstanceReadyRollup): when gateway TLS
	// is requested the Instance must not report Ready while the client-facing
	// listener would still be serving plaintext during provisioning. Distinct
	// from InstanceConditionGatewayReady (the Deployment's own rollout health).
	InstanceConditionGatewayTLSReady = "GatewayTLSReady"
)

// PostgresSpec configures an external PostgreSQL connection for the metadata service.
//
// The string fields below are interpolated into the XML config the operator
// renders for the metadata service (see buildMetadataConfigXML). The
// controller XML-escapes user input at render time, but the patterns here
// reject XML metacharacters at admission time as defense-in-depth so a
// malformed CR is rejected at apply rather than producing a config that
// only works because the controller scrubs it.
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
//
// Pod scheduling, image, resources, sidecars, init containers, volumes,
// imagePullSecrets, podSecurityContext, and labels / annotations are
// expressed via spec.metadata.template (a Kubernetes PodTemplateSpec).
// The FireboltInstance validating webhook rejects any input on that
// template that lands at a path the operator owns end-to-end: the
// dedicated-pensieve container's command / ports / probes / reserved
// env keys (POSTGRES_USERNAME_FILE / POSTGRES_PASSWORD_FILE) /
// reserved volume mounts (config / postgres-creds / tmp), and the
// pod-level terminationGracePeriodSeconds / subdomain / hostname /
// restartPolicy / activeDeadlineSeconds. See the
// MetadataPodTemplateRules ruleset in operatorauthority.go for the
// authoritative allowlist.
//
// Only replicas=1 is currently supported; multi-replica metadata is not yet
// available. The CEL rule below enforces this at admission time, in addition
// to the Go-level check in the validating webhook (kept for defense-in-depth
// and to surface a clearer error message when the webhook is in the request path).
// +kubebuilder:validation:XValidation:rule="!has(self.replicas) || self.replicas == 1",message="metadata replicas must be 1"
type MetadataSpec struct {
	// Replicas is the number of metadata pods. Pinned to 1 today by
	// the CEL rule above and the validating webhook; the surface is
	// kept on the spec for the day a multi-writer metadata story
	// lands.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Template is the pod template the operator merges with its
	// own-rendered metadata container, config volume, credentials
	// mount, probes, and pod-level securityContext to produce the
	// metadata Deployment's pod spec. Most users set only
	// template.spec.containers[name=="metadata"].image and
	// .resources, plus scheduling fields (nodeSelector / tolerations /
	// affinity / topologySpreadConstraints / priorityClassName).
	//
	// template.metadata is unpruned by a post-controller-gen patch (see
	// the matching note on FireboltEngineClassSpec.Template for the full
	// rationale).
	// +optional
	Template *corev1.PodTemplateSpec `json:"template,omitempty"`

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
// Pod scheduling, image, resources, sidecars, init containers, volumes,
// imagePullSecrets, podSecurityContext, and labels / annotations are
// expressed via spec.gateway.template (a Kubernetes PodTemplateSpec).
// The FireboltInstance validating webhook rejects any input on that
// template that lands at a path the operator owns end-to-end: the
// Envoy container's args / ports / probes / lifecycle preStop hook /
// reserved volume mounts (config-volume / tmp), and the pod-level
// terminationGracePeriodSeconds / subdomain / hostname / restartPolicy
// / activeDeadlineSeconds. See the GatewayPodTemplateRules ruleset in
// operatorauthority.go for the authoritative allowlist.
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
	// Replicas is the number of gateway pods. Defaults to 2 when nil.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// MetricsPort is the container port exposing Envoy's Prometheus
	// metrics endpoint. Defaults to 9090 if zero. The operator
	// stamps a corresponding "metrics" port entry on the container.
	// +kubebuilder:default=9090
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	MetricsPort int32 `json:"metricsPort,omitempty"`

	// Template is the pod template the operator merges with its
	// own-rendered Envoy container, config volume mount, hardcoded
	// probes, and preStop drain hook to produce the gateway
	// Deployment's pod spec. Most users set only
	// template.spec.containers[name=="envoy"].image and .resources,
	// plus scheduling fields (nodeSelector / tolerations / affinity /
	// topologySpreadConstraints / priorityClassName).
	//
	// template.metadata is unpruned by a post-controller-gen patch (see
	// the matching note on FireboltEngineClassSpec.Template for the full
	// rationale).
	// +optional
	Template *corev1.PodTemplateSpec `json:"template,omitempty"`
}

// CertManagerIssuerRef identifies the cert-manager Issuer or ClusterIssuer
// used to sign a Certificate the operator creates on the user's behalf.
// The operator never creates the Issuer itself — it must already exist —
// so a compromised operator cannot mint a new trust root, only leaf
// certificates under one the cluster administrator already trusts.
type CertManagerIssuerRef struct {
	// Name is the name of the Issuer or ClusterIssuer.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Kind is the referenced resource's kind. Issuer is namespaced (must
	// live in the same namespace as this FireboltInstance); ClusterIssuer
	// is cluster-scoped.
	// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
	// +kubebuilder:default=ClusterIssuer
	// +optional
	Kind string `json:"kind,omitempty"`
}

// CertManagerSpec describes how the operator provisions an X.509 keypair
// via a cert-manager Certificate. It is the only supported source of JWT
// signing-key material (see SigningKeyPolicy) — there is intentionally no
// bring-your-own-Secret alternative there, so every signing key is
// traceable to one issuer chain the cluster administrator configured. TLS
// listeners may instead bring their own Secret via TLSListenerSpec.SecretRef.
type CertManagerSpec struct {
	// IssuerRef references the cert-manager Issuer or ClusterIssuer that
	// signs the generated Certificate.
	IssuerRef CertManagerIssuerRef `json:"issuerRef"`

	// Algorithm is the private key algorithm.
	// +kubebuilder:validation:Enum=RSA;ECDSA
	// +kubebuilder:default=ECDSA
	// +optional
	Algorithm string `json:"algorithm,omitempty"`

	// Size is the private key size: RSA modulus bits (e.g. 2048, 4096) or
	// ECDSA curve size (256, 384, 521). Defaults to 384 (the P-384 curve,
	// matching the ECDSA algorithm default). The algorithm/size combination
	// is validated at admission (see validateCertManagerKey).
	// +kubebuilder:default=384
	// +optional
	Size int32 `json:"size,omitempty"`
}

// PasswordLoginPolicy controls whether password-based login is available
// to any authenticated user or only to the admin account. Mirrors
// packdb's instance.auth.password_login; meaningful only once OIDC is
// also configured (a native-only deployment always allows the admin to
// log in with a password).
// +kubebuilder:validation:Enum=admin_only;any_user
type PasswordLoginPolicy string

// PasswordLoginAdminOnly and PasswordLoginAnyUser enumerate packdb's
// password-login policies.
const (
	PasswordLoginAdminOnly PasswordLoginPolicy = "admin_only"
	PasswordLoginAnyUser   PasswordLoginPolicy = "any_user"
)

// AdminSpec configures the Instance admin account. packdb re-syncs this
// user's name and password from config on every engine startup.
type AdminSpec struct {
	// Name is the admin username. Defaults to "firebolt" — packdb's own
	// default — so omitting it matches engine behavior when auth is
	// first enabled.
	// +kubebuilder:default=firebolt
	// +optional
	Name string `json:"name,omitempty"`

	// Password references the Secret key holding the admin password.
	// Required when auth is enabled: the operator does not generate an
	// admin password, because a generated credential the user never sees
	// is not something they can use to log in. The referenced Secret is
	// mounted into every engine pod and rendered as
	// instance.auth.admin.password_file — never password_value — so the
	// plaintext password never appears in the rendered config.yaml or its
	// ConfigMap.
	Password corev1.SecretKeySelector `json:"password"`
}

// SigningKeyPolicy controls how the operator provisions and rotates the
// JWT signing keypair used by the embedded ("_local") authorization
// server on every engine. Signing keys are entirely operator-generated —
// the CRD does not accept user-supplied key material — because every
// engine in an Instance must share byte-identical signing_keys (packdb's
// SigningKeyManager validates tokens minted by any peer engine, looked up
// by kid, so every key any engine could have signed with must be present
// on every other engine), and an operator-generated Secret is the only
// way to guarantee that across a fleet.
//
// Every Certificate this policy produces has cert-manager auto-renew
// disabled: packdb reads signing keys only at process startup, so an
// uncoordinated renewal would make one engine sign with a key its peers
// can't yet validate. When RotationInterval is set, the operator performs
// its own coordinated rotation instead (see AuthStatus's doc comment for
// the two-phase promote/retire sequence), which is exactly the
// coordination an uncoordinated cert-manager renewal would be unable to
// provide.
type SigningKeyPolicy struct {
	// CertManager configures the cert-manager Certificate used to
	// generate the signing keypair.
	//
	// The key size is immutable once set (enforced by the API server, so it
	// holds even if the validating webhook is bypassed): packdb derives every
	// signing key's curve from the single global signingAlgorithm and cannot
	// serve two curves at once, so a size change cannot be migrated in place.
	// The rule is scoped to this signing-only field, leaving TLS listener key
	// sizes (which share CertManagerSpec) mutable. issuerRef/algorithm changes
	// are separately constrained by the signingAlgorithm compatibility check.
	// +kubebuilder:validation:XValidation:rule="self.size == oldSelf.size || oldSelf.size == 0",message="signing key size is immutable once set; recreate the instance to change it"
	CertManager CertManagerSpec `json:"certManager"`

	// RotationInterval, when set, enables operator-owned periodic
	// rotation: measured from the active key's CreatedAt, once this much
	// time has passed the operator mints a new key and promotes it via a
	// two-phase rollout that never opens a validation gap (see
	// AuthStatus's doc comment). Omit to keep a single, permanent,
	// non-rotating key, matching this operator's original behavior.
	//
	// RetainDuration must also be set whenever this is set.
	// +optional
	RotationInterval *metav1.Duration `json:"rotationInterval,omitempty"`

	// RetainDuration bounds how long a demoted key is kept in
	// signing_keys[] as validation-only after it stops signing new
	// tokens, before the operator drops it from every engine's rendered
	// config and deletes its Certificate/Secret. Must be at least
	// instance.auth.local.maxTokenAge (packdb default: 1 day) plus
	// however long a full engine fleet rollout realistically takes: a
	// token signed in the last instant the old key was active must fully
	// expire before the key that could validate it disappears.
	//
	// Required whenever RotationInterval is set; rejected otherwise (see
	// ValidateAuth).
	// +optional
	RetainDuration *metav1.Duration `json:"retainDuration,omitempty"`
}

// LocalAuthSpec configures the embedded ("_local") authorization server:
// packdb's native username/password login plus the JWT signing/validation
// parameters every engine uses regardless of whether OIDC is also
// configured. These fields are grouped together here for operator users
// even though packdb itself spreads them across
// instance.auth.{password_login,admin} and instance.auth.local.* — the
// operator maps between the two shapes at render time.
type LocalAuthSpec struct {
	// PasswordLogin controls whether password login is available to any
	// user or only the admin account. Defaults to admin_only (packdb's
	// own default).
	// +kubebuilder:default=admin_only
	// +optional
	PasswordLogin PasswordLoginPolicy `json:"passwordLogin,omitempty"`

	// Admin configures the Instance admin account. Required when auth is
	// enabled — packdb rejects a config with auth.enabled=true and no
	// admin block.
	Admin AdminSpec `json:"admin"`

	// SigningAlgorithm is the JWT signing algorithm used by the embedded
	// authorization server. Must be compatible with SigningKeys'
	// cert-manager key algorithm: the RS* family requires an RSA key, the
	// ES* family requires ECDSA. Defaults to ES384, matching the ECDSA
	// signing-key default.
	//
	// Immutable once set (enforced by the API server via the transition rule
	// below, so it holds even if the validating webhook is bypassed): packdb
	// exposes one global signing_algorithm and derives every key's curve from
	// it, so it can never hold two curves at once. Changing it in place would
	// roll engines onto a signing_algorithm their mounted key no longer matches
	// — an invalid JWKS / permanent startup wedge. Recreate the Instance (or
	// disable auth and drop spec.auth.local first) to change it.
	// +kubebuilder:validation:Enum=RS256;RS384;RS512;ES256;ES384;ES512
	// +kubebuilder:default=ES384
	// +kubebuilder:validation:XValidation:rule="self == oldSelf || oldSelf == ''",message="signingAlgorithm is immutable once set; recreate the instance to change the JWT signing algorithm"
	// +optional
	SigningAlgorithm string `json:"signingAlgorithm,omitempty"`

	// TokenExpiry is how long issued access tokens remain valid, as a Go
	// duration string (e.g. "1h"). Defaults to packdb's own default (1h)
	// when empty.
	// +optional
	TokenExpiry string `json:"tokenExpiry,omitempty"`

	// MaxTokenAge bounds how old a token's iat claim may be, independent
	// of TokenExpiry. Defaults to packdb's own default (1d) when empty.
	// +optional
	MaxTokenAge string `json:"maxTokenAge,omitempty"`

	// ClockSkewTolerance is the permitted clock drift when validating
	// time-based JWT claims. Defaults to packdb's own default (30s) when
	// empty.
	// +optional
	ClockSkewTolerance string `json:"clockSkewTolerance,omitempty"`

	// SigningKeys controls how the operator provisions the signing
	// keypair. Required when auth is enabled: packdb's own dev-autogen
	// fallback (used when signing_keys is empty) mints a different key
	// per engine process, which breaks cross-engine token validation in
	// any multi-engine deployment — exactly the topology this operator
	// always creates.
	// +optional
	SigningKeys *SigningKeyPolicy `json:"signingKeys,omitempty"`
}

// OIDCJWTSpec configures the JWT validation parameters shared by every
// OIDC provider on this Instance. Distinct from LocalAuthSpec's JWT
// fields: packdb's instance.auth.oidc.jwt has no token_expiry, because an
// OIDC provider issues its own tokens — the engine only validates them.
type OIDCJWTSpec struct {
	// ClockSkewTolerance is the permitted clock drift when validating
	// time-based claims on OIDC-issued tokens. Defaults to packdb's own
	// default (30s) when empty.
	// +optional
	ClockSkewTolerance string `json:"clockSkewTolerance,omitempty"`

	// MaxTokenAge bounds how old an OIDC token's iat claim may be.
	// Defaults to packdb's own default (1d) when empty.
	// +optional
	MaxTokenAge string `json:"maxTokenAge,omitempty"`
}

// JITProvisioningSpec controls whether a first-time OIDC login
// automatically creates a Firebolt user, and which roles that user
// receives.
type JITProvisioningSpec struct {
	// Enabled turns on just-in-time user provisioning for this provider.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// DefaultRoles lists the roles granted to an auto-provisioned user.
	// Defaults to ["public"] (packdb's own default) when empty.
	// +optional
	DefaultRoles []string `json:"defaultRoles,omitempty"`
}

// OIDCJWKSSpec configures caching of a provider's published JSON Web Key
// Set.
type OIDCJWKSSpec struct {
	// CacheTTL is how long a fetched JWKS document is cached before being
	// re-fetched, as a Go duration string. Defaults to packdb's own
	// default (1h) when empty.
	// +optional
	CacheTTL string `json:"cacheTTL,omitempty"`
}

// OIDCDiscoverySpec configures refresh of a provider's OpenID discovery
// document.
type OIDCDiscoverySpec struct {
	// RefreshInterval is how often the engine re-fetches the provider's
	// /.well-known/openid-configuration document, as a Go duration
	// string. Defaults to packdb's own default (1d) when empty.
	// +optional
	RefreshInterval string `json:"refreshInterval,omitempty"`
}

// OIDCProviderSpec configures one trusted OIDC identity provider. packdb
// validates bearer tokens against this provider's published keys — it is
// a JWT validator, not an OAuth2 client: there is no client ID/secret,
// redirect URI, or authorization-code flow here, because the engine never
// initiates a login. An external client (the Firebolt CLI, a BI tool)
// performs the OIDC flow itself and presents the resulting access token
// to the engine as a bearer token.
type OIDCProviderSpec struct {
	// Name is this provider's machine identifier, used in the
	// ?auth=<name> connection parameter and as the authorization server
	// name clients select. Must not start with "_" — that prefix is
	// reserved by packdb for Firebolt-managed authorization servers (the
	// embedded server is named "_local").
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[^_].*$`
	Name string `json:"name"`

	// Title is a human-readable label for this provider, shown in UIs.
	// Defaults to Name when empty.
	// +optional
	Title string `json:"title,omitempty"`

	// DiscoveryURL is the provider's OpenID Connect discovery endpoint
	// (typically ending in /.well-known/openid-configuration). Must be
	// an https:// URL — packdb requires TLS for every outbound OIDC
	// fetch except loopback.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^https://.+`
	DiscoveryURL string `json:"discoveryURL"`

	// Audience is the expected "aud" claim on tokens from this provider.
	// Defaults to the Instance's canonical issuer URL when empty
	// (packdb's own default).
	// +optional
	Audience string `json:"audience,omitempty"`

	// UsernameMapping is a Go-template string mapping token claims to the
	// Firebolt username; claims are interpolated with double-brace markers.
	// For example, the "email" claim on its own, or the "iss" and "sub"
	// claims joined with a pipe.
	// +kubebuilder:validation:MinLength=1
	UsernameMapping string `json:"usernameMapping"`

	// JITProvisioning controls automatic user creation on first login via
	// this provider.
	// +optional
	JITProvisioning *JITProvisioningSpec `json:"jitProvisioning,omitempty"`

	// JWKS configures caching of this provider's published key set.
	// +optional
	JWKS *OIDCJWKSSpec `json:"jwks,omitempty"`

	// Discovery configures refresh of this provider's OpenID discovery
	// document.
	// +optional
	Discovery *OIDCDiscoverySpec `json:"discovery,omitempty"`
}

// OIDCAuthSpec configures OpenID Connect bearer-token authentication:
// one or more trusted identity providers whose tokens engines accept
// alongside (or instead of) the embedded local authorization server.
type OIDCAuthSpec struct {
	// JWT configures validation parameters shared by every provider.
	// +optional
	JWT *OIDCJWTSpec `json:"jwt,omitempty"`

	// Providers lists the trusted OIDC identity providers. Must be
	// non-empty when OIDC is configured at all — packdb rejects a
	// present oidc block with an empty providers list.
	// +kubebuilder:validation:MinItems=1
	Providers []OIDCProviderSpec `json:"providers"`
}

// AuthSpec configures authentication for every engine in this Instance.
// Auth is an Instance-wide policy, not a per-Engine one: packdb's embedded
// authorization server on each engine both issues and validates JWTs, so
// every engine must run with byte-identical instance.auth.* — including
// the same signing keys — or a token minted by one engine fails
// validation on another. The operator enforces this by resolving AuthSpec
// once per Instance and rendering the result into every engine's
// config.yaml from that single source, never per-engine.
type AuthSpec struct {
	// Enabled turns on authentication for every engine in this Instance.
	// When false, engines run in packdb's unauthenticated mode and every
	// connection is treated as the admin. Local, OIDC, and
	// PreferredAuthorizationServer below are only meaningful when Enabled
	// is true; the validating webhook rejects setting them while Enabled
	// is false, matching packdb's own config validation (instance.auth's
	// admin and oidc fields must be absent when instance.auth.enabled is
	// false).
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// PreferredAuthorizationServer names the authorization server clients
	// should use by default when a connection doesn't select one
	// explicitly: either "_local" (the embedded server) or the Name of
	// one of the OIDC providers below. Advisory only; surfaced to clients
	// via /.well-known/firebolt. Must be unset when Enabled is false and,
	// when set, must name a configured server.
	// +optional
	PreferredAuthorizationServer string `json:"preferredAuthorizationServer,omitempty"`

	// Local configures the embedded ("_local") authorization server:
	// native username/password login and JWT signing. Required when
	// Enabled is true.
	// +optional
	Local *LocalAuthSpec `json:"local,omitempty"`

	// OIDC configures OpenID Connect bearer-token authentication against
	// one or more external identity providers.
	// +optional
	OIDC *OIDCAuthSpec `json:"oidc,omitempty"`
}

// TLSListenerSpec configures TLS termination for one operator-managed
// listener (the gateway's client-facing listener, or an engine's HTTP/
// Postgres-wire listeners). When Enabled, provide exactly one certificate
// source: CertManager (the operator provisions a cert-manager Certificate)
// or SecretRef (bring your own Secret). Unlike SigningKeyPolicy — where
// cert-manager is the only supported source — a TLS listener accepts a
// pre-issued Secret so certificates from a CA the cluster has no
// cert-manager integration with can still be used.
type TLSListenerSpec struct {
	// Enabled turns on TLS for this listener.
	// +kubebuilder:default=false
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// CertManager configures the cert-manager Certificate used to
	// provision this listener's server certificate. Provide exactly one of
	// CertManager or SecretRef when Enabled is true.
	// +optional
	CertManager *CertManagerSpec `json:"certManager,omitempty"`

	// SecretRef supplies a pre-existing Kubernetes Secret holding this
	// listener's certificate material instead of provisioning one via
	// cert-manager — for a certificate issued by a CA the cluster has no
	// cert-manager integration with. The Secret must carry "tls.crt" and
	// "tls.key" (the standard kubernetes.io/tls keys); an engine listener
	// additionally requires "ca.crt", since the gateway validates engine
	// certificates against it when re-encrypting upstream (see
	// EngineTLSStatus). The operator only reads this Secret; it never
	// creates, mutates, or garbage-collects it. Provide exactly one of
	// CertManager or SecretRef when Enabled is true.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// ClientCASecretRef enables mutual TLS on this listener: the server
	// requires a client certificate and verifies it against the "ca.crt"
	// bundle in the referenced Secret. The operator only reads this Secret
	// (never creates or mutates it), and the listener stays not-ready until
	// it exists and carries ca.crt.
	//
	// Currently honored only for spec.tls.gateway — the gateway verifies
	// client certificates on its client-facing listener. Setting it on
	// spec.tls.engine is rejected at admission (engine-side client-cert
	// verification is not yet wired). Requires this listener's server TLS
	// to be enabled.
	// +optional
	ClientCASecretRef *corev1.LocalObjectReference `json:"clientCASecretRef,omitempty"`

	// DNSNames lists additional Subject Alternative Names to include on
	// the provisioned certificate, beyond whatever names the operator
	// derives automatically.
	//
	// Only meaningful for spec.tls.gateway: the gateway's in-cluster
	// Service DNS names are always included automatically, but the
	// gateway has no operator-managed external entrypoint (no
	// Ingress/LoadBalancer hostname visible to the operator — see
	// TLSSpec's doc comment), so any name a client outside the cluster
	// will actually present at the TLS handshake — a custom domain, an
	// external load balancer's hostname — must be listed here
	// explicitly, mirroring the sibling firebolt-instance-helm chart's
	// tls.gateway.certManager.dnsNames.
	//
	// Ignored for spec.tls.engine: its SANs are fully derived from the
	// namespace (see engineTLSWildcardDNSName) and cannot be extended,
	// since every engine's routing Service already matches the
	// namespace-wide wildcard.
	// +optional
	DNSNames []string `json:"dnsNames,omitempty"`
}

// TLSSpec configures TLS termination for the operator-managed network
// hops between a client and an engine: the gateway's client-facing
// listener, and each engine's own listeners (reached directly by
// in-cluster clients, and by the gateway when it re-encrypts upstream).
// Engine-to-metadata gRPC and inter-node broadcast TLS are out of scope
// for this field and are not currently exposed on the CRD.
//
// The gateway's Service is ClusterIP with no operator-managed external
// entrypoint (no Ingress/LoadBalancer the operator creates or observes);
// fronting it with one, and pointing that entrypoint's DNS name at
// TLSListenerSpec.DNSNames, is an operator decision outside this CRD.
type TLSSpec struct {
	// Gateway configures TLS termination on the Envoy gateway's
	// client-facing listener.
	// +optional
	Gateway *TLSListenerSpec `json:"gateway,omitempty"`

	// Engine configures TLS termination on each engine's HTTP and
	// Postgres-wire listeners.
	// +optional
	Engine *TLSListenerSpec `json:"engine,omitempty"`
}

// MetricScrapeMode selects how the operator reaches engine pods to scrape
// the Prometheus /metrics endpoint that backs the drain probe and the
// autoStop activity poll.
// +kubebuilder:validation:Enum=PodIP;ApiserverProxy
type MetricScrapeMode string

// MetricScrapeModePodIP and MetricScrapeModeApiserverProxy enumerate
// the supported scrape transports. See FireboltInstanceSpec.MetricScrapeMode.
const (
	// MetricScrapeModePodIP dials engine pod IPs directly on
	// MetricsPort from the controller pod. Default; matches every
	// standard in-cluster scraper (Prometheus, metrics-server,
	// OpenTelemetry, KSM) and doesn't depend on apiserver->node:9090
	// SG rules that EKS / kubeadm don't open by default.
	MetricScrapeModePodIP MetricScrapeMode = "PodIP"

	// MetricScrapeModeApiserverProxy routes the scrape through the
	// apiserver pods/proxy subresource. Opt-in for out-of-cluster
	// operator runs (`make run`) or networks that forbid node-to-node
	// on MetricsPort but allow apiserver-proxy; requires the cluster
	// SG to allow apiserver->node on MetricsPort, which is NOT the
	// default on EKS.
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

	// Auth configures authentication for every engine in this Instance.
	// See AuthSpec for why this is Instance-wide rather than per-Engine.
	// +optional
	Auth *AuthSpec `json:"auth,omitempty"`

	// TLS configures TLS termination on the gateway's client-facing
	// listener and on each engine's own listeners.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// MetricScrapeMode selects the transport the operator uses to scrape
	// engine pod /metrics for the drain probe and autoStop activity
	// poll. Read fresh on every scrape so it can be flipped without a
	// controller restart. Defaults to PodIP; flip to ApiserverProxy
	// only when in-cluster pod IPs aren't reachable from the controller
	// (out-of-cluster `make run`, or networks that block node-to-node
	// on MetricsPort but allow apiserver-proxy).
	// +kubebuilder:default=PodIP
	// +optional
	MetricScrapeMode MetricScrapeMode `json:"metricScrapeMode,omitempty"`
}

// SigningKeyPhase is a signing key's current role in Instance-wide JWT
// signing and validation. Every "which key is active" or "which keys are
// still valid" decision reads this field, not a key's position within
// AuthStatus.SigningKeys — position carries no meaning, precisely so a
// multi-step, requeue-tolerant rotation pipeline never depends on the
// operator writing the slice back in a particular order.
// +kubebuilder:validation:Enum=Active;ValidationOnly;Removing
type SigningKeyPhase string

const (
	// SigningKeyActive is the key packdb's embedded server currently
	// signs new tokens with. Exactly one key is Active at a time.
	SigningKeyActive SigningKeyPhase = "Active"
	// SigningKeyValidationOnly is a retained key still rendered into
	// signing_keys[] so packdb continues to validate tokens signed with
	// it, but not used to sign new ones. A key passes through this phase
	// twice in one rotation: briefly right after creation (before every
	// engine has it and it is safe to promote), and again after being
	// demoted from Active (until RetainDuration elapses).
	SigningKeyValidationOnly SigningKeyPhase = "ValidationOnly"
	// SigningKeyRemoving marks a key that must no longer be rendered or
	// mounted on any engine, but whose Certificate/Secret the operator
	// has not yet deleted — it is waiting for every engine to confirm
	// (via ObservedAuthHash) it has rolled onto a signing_keys[] that no
	// longer includes this key, so a slow engine can never be left
	// referencing a private_key_path that has vanished out from under it.
	SigningKeyRemoving SigningKeyPhase = "Removing"
)

// SigningKeyStatus records one JWT signing key the operator has
// provisioned for this Instance.
type SigningKeyStatus struct {
	// ID is the key identifier rendered as the JWT "kid" and as
	// instance.auth.local.signing_keys[].id on every engine.
	ID string `json:"id"`

	// SecretName is the cert-manager-managed Secret holding this key's
	// PEM private key (data key "tls.key").
	SecretName string `json:"secretName"`

	// CreatedAt is when this key was provisioned.
	CreatedAt metav1.Time `json:"createdAt"`

	// Algorithm and Size record the cert-manager key algorithm and size this
	// key was issued with — the resolved
	// spec.auth.local.signingKeys.certManager values in effect when it was
	// minted. Because the signing Certificate uses rotationPolicy:Never,
	// cert-manager will not regenerate a key whose algorithm/size later
	// changes; it awaits user intervention instead. The rotation state machine
	// compares these against the current policy and, on a mismatch, mints a
	// fresh NAMED key (new kid → new Secret) so new material is issued cleanly
	// rather than leaving the engine fleet wedged on a key the issuer refuses
	// to update. Empty/zero on a key minted before these fields existed; the
	// controller adopts the current resolved policy as the baseline on the
	// next reconcile.
	// +optional
	Algorithm string `json:"algorithm,omitempty"`
	// +optional
	Size int32 `json:"size,omitempty"`

	// Phase is this key's current role — see SigningKeyPhase. Unset
	// (empty string) is treated as Active for compatibility with
	// Instances that provisioned their one signing key before this field
	// existed; the controller normalizes it to Active explicitly on the
	// next reconcile.
	// +optional
	Phase SigningKeyPhase `json:"phase,omitempty"`

	// DemotedAt is when this key was demoted from Active, unset for a key
	// that either still is Active or has never been Active (newly minted,
	// not yet promoted) — used only to tell those two ValidationOnly
	// sub-states apart. Deliberately NOT what RetainDuration counts from:
	// engines keep signing with this key until they actually roll onto
	// the promoted config, which happens after this moment, so anchoring
	// the retain window here would let it elapse before every engine has
	// even stopped using this key. See RetireEligibleAt.
	// +optional
	DemotedAt *metav1.Time `json:"demotedAt,omitempty"`

	// RetireEligibleAt is when every engine's ObservedAuthHash first
	// confirmed it had rolled onto the config produced by this key's
	// demotion — i.e. the earliest instant at which no engine anywhere
	// could still be signing new tokens with this key. Unset until that
	// confirmation happens. RetainDuration counts from here, not from
	// DemotedAt: a token signed in the last instant before an engine
	// rolls is only guaranteed to have expired by
	// RetireEligibleAt+RetainDuration, not by DemotedAt+RetainDuration,
	// since rolling out the promotion itself takes real time.
	// +optional
	RetireEligibleAt *metav1.Time `json:"retireEligibleAt,omitempty"`
}

// AuthStatus reports the observed state of Instance-wide auth
// provisioning — the crypto material engines need, as opposed to
// AuthSpec's desired configuration.
type AuthStatus struct {
	// SigningKeys lists every JWT signing key the operator is currently
	// provisioning or retaining for this Instance — exactly one with
	// Phase=Active, and, only while a rotation is in flight, one or more
	// additional ValidationOnly/Removing keys. See SigningKeyPolicy's
	// RotationInterval/RetainDuration for what drives a rotation, and
	// SigningKeyPhase for the states a key passes through:
	//
	//   1. A new key is created ValidationOnly (not yet used to sign)
	//      until every engine's ObservedAuthHash confirms it has rolled
	//      out signing_keys[] including the new key — only then is it
	//      safe to promote, because promoting any earlier would let a
	//      rolled engine sign tokens a not-yet-rolled engine cannot yet
	//      validate.
	//   2. Promotion flips the new key to Active and demotes the previous
	//      Active key to ValidationOnly (DemotedAt set), so tokens it
	//      already signed keep validating everywhere. Every engine still
	//      signs with the demoted key until it rolls onto this promoted
	//      config — this is not instantaneous, so the retain window
	//      cannot start counting yet.
	//   3. Once every engine's ObservedAuthHash confirms it has actually
	//      rolled onto the promoted config — meaning no engine anywhere
	//      can still be signing with the demoted key — RetireEligibleAt
	//      is stamped. This step exists specifically so RetainDuration
	//      measures from "provably stopped signing," not from "decided to
	//      stop signing": anchoring at DemotedAt instead would let the
	//      retain window elapse while a slow-rolling engine is still
	//      minting tokens with the demoted key, silently reopening the
	//      exact validation gap this whole rotation design exists to
	//      avoid.
	//   4. Once RetireEligibleAt+RetainDuration has elapsed, the demoted
	//      key moves to Removing — dropped from render immediately, but
	//      its Certificate/Secret are kept until every engine's
	//      ObservedAuthHash confirms the removal has rolled out too.
	//   5. Only then is the Removing key's Certificate deleted and its
	//      entry dropped from this list.
	//
	// A slice from the start (predating rotation) so this growth during a
	// rollover window needed no status schema change.
	// +optional
	SigningKeys []SigningKeyStatus `json:"signingKeys,omitempty"`

	// SigningKeyGeneration is a monotonically increasing counter the
	// operator uses to mint each new signing key's ID (kid) as
	// "signing-<N>", guaranteeing a fresh key never reuses an ID even
	// after an earlier key has been fully removed from SigningKeys (at
	// which point nothing in this status would otherwise remember it was
	// ever used). Never decreases.
	// +optional
	SigningKeyGeneration int `json:"signingKeyGeneration,omitempty"`
}

// EngineTLSStatus reports the observed state of engine-listener TLS
// provisioning — the crypto material engines and the gateway need, as
// opposed to TLSListenerSpec's desired configuration. Unlike AuthStatus's
// SigningKeys, this is a single Secret: engine TLS has no cross-engine
// validation constraint requiring a rotation window, so there is no
// forward-compatibility reason to model it as a slice yet.
type EngineTLSStatus struct {
	// SecretName is the cert-manager-managed Secret holding the engine
	// listener's server certificate (data keys "tls.crt", "tls.key", and,
	// when the issuer populates it, "ca.crt" — the trust anchor the
	// gateway uses to verify engines when re-encrypting upstream).
	SecretName string `json:"secretName"`

	// CreatedAt is when this certificate was provisioned.
	CreatedAt metav1.Time `json:"createdAt"`

	// Reencrypting reports whether the gateway is currently re-encrypting
	// gateway→engine traffic with TLS. It tracks the engine FLEET's observed
	// protocol, not merely whether the certificate exists: on enable it turns
	// true only once every engine has rolled onto a TLS-serving generation (so
	// the gateway does not switch to TLS while engines still serve plaintext),
	// and on disable this EngineTLSStatus is retained with Reencrypting=true
	// until every engine has drained back to plaintext (so the gateway keeps
	// the trust anchor and TLS while any engine still serves it). This narrows,
	// but does not eliminate, the mixed-protocol window during a rollout — the
	// gateway speaks one upstream protocol at a time. See engineUpstreamTLSReady.
	// +optional
	Reencrypting bool `json:"reencrypting,omitempty"`
}

// GatewayTLSStatus reports the observed state of gateway downstream
// (client-facing) TLS provisioning — the crypto material the gateway's
// listener needs, as opposed to TLSListenerSpec's desired configuration.
type GatewayTLSStatus struct {
	// SecretName is the cert-manager-managed Secret holding the gateway's
	// server certificate (data keys "tls.crt" and "tls.key"). Unlike
	// EngineTLSStatus, no "ca.crt" is required: the gateway presents this
	// certificate to clients but never uses it to authenticate a peer, so
	// no CA-backed-issuer requirement applies here.
	SecretName string `json:"secretName"`

	// CreatedAt is when this certificate was provisioned.
	CreatedAt metav1.Time `json:"createdAt"`
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

	// Auth reports the crypto material the operator has provisioned for
	// Instance-wide auth (currently: JWT signing keys). Nil when
	// spec.auth is unset or disabled.
	// +optional
	Auth *AuthStatus `json:"auth,omitempty"`

	// EngineTLS reports the crypto material the operator has provisioned
	// for engine-listener TLS. Nil when spec.tls.engine is unset or
	// disabled.
	// +optional
	EngineTLS *EngineTLSStatus `json:"engineTLS,omitempty"`

	// GatewayTLS reports the crypto material the operator has provisioned
	// for the gateway's downstream (client-facing) TLS listener. Nil when
	// spec.tls.gateway is unset or disabled.
	// +optional
	GatewayTLS *GatewayTLSStatus `json:"gatewayTLS,omitempty"`

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
