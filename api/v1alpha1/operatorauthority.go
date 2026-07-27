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
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// This file is the single source of truth for fields the operator owns
// across user-supplied templating surfaces (spec.customEngineConfig,
// FireboltEngineClass.spec.template, and reserved label/annotation key prefixes
// on every CR that lets users set metadata). Consumers reference these
// declarations directly instead of restating the path lists, so a future
// addition lands in one place and propagates to every strip / reject
// site.

// ReservedFireboltKeyPrefix is the label and annotation key prefix owned by
// the operator. Users MUST NOT set any key with this prefix on a CR that
// supports user-supplied labels/annotations — the controller unconditionally
// overwrites several keys to drive behavior (firebolt.io/config-hash,
// firebolt.io/generation, etc.), and letting users seed them silently freezes
// rollouts or corrupts routing.
const ReservedFireboltKeyPrefix = "firebolt.io/"

// EngineContainerName is the fixed name of the firebolt engine container
// inside each generation's StatefulSet pod template. The drain check, the
// stsMatchesSpec drift detection, and the FireboltEngineClass validating webhook
// all rely on it being stable and operator-owned. The name matches the
// public CRD ("engine"); the engine binary inside the image lives at
// /opt/firebolt/firebolt, but that's an image-internal path and doesn't
// surface on the pod template.
const EngineContainerName = "engine"

// EngineWebContainerName is the fixed name of the operator-injected Engine Web UI
// sidecar, deployed into engine pods when spec.uiSidecar resolves to true.
// Like EngineContainerName it is operator-owned: the FireboltEngineClass
// validating webhook rejects a user-supplied container or init container
// with this name (on both the engine and class templates) so the operator's
// injected sidecar can never collide with one the user wrote. The operator
// renders the container end-to-end, so there is no user-extension surface
// on it the way there is on the engine container.
const EngineWebContainerName = "engine-web"

// GatewayContainerName is the fixed name of the Envoy container inside
// the FireboltInstance gateway Deployment's pod template. The
// FireboltInstance validating webhook uses it to locate the primary
// container on a user-supplied gateway template; the controller's
// build function emits a single container with the same name.
const GatewayContainerName = "envoy"

// MetadataContainerName is the fixed name of the dedicated-pensieve
// container inside the FireboltInstance metadata Deployment's pod
// template. Used by the validating webhook and the builder, same
// pattern as GatewayContainerName.
const MetadataContainerName = "metadata"

// Operator-injected environment variables on the engine container. These
// keys carry pod-index plumbing (POD_INDEX) and AWS SDK behavior that
// the operator must control end to end: a user override would either crash
// the engine or silently divert its identity. They are rejected at admission
// time on FireboltEngineClass spec.template and would be stripped if injected
// through another template channel.
const (
	EnginePodIndexEnvKey                    = "POD_INDEX"
	EngineAwsEC2MetadataClientEnabledEnvKey = "FB_AWS_EC2_METADATA_CLIENT_ENABLED"
)

// operatorOwnedEngineEnvKeys is the set of env names the operator injects on
// the engine container and that user templates may not redefine. Maintained
// as a slice so iteration order is deterministic when reporting violations.
var operatorOwnedEngineEnvKeys = []string{
	EnginePodIndexEnvKey,
	EngineAwsEC2MetadataClientEnabledEnvKey,
}

// MetadataPostgresUsernameEnvKey and MetadataPostgresPasswordEnvKey are
// the env vars the operator injects on the metadata container to point
// dedicated-pensieve at its Postgres credentials Secret. User-supplied
// templates may not redefine these names; the validator rejects them.
const (
	MetadataPostgresUsernameEnvKey = "POSTGRES_USERNAME_FILE"
	MetadataPostgresPasswordEnvKey = "POSTGRES_PASSWORD_FILE" //nolint:gosec // legit:ignore-secrets — env-var name, not a credential
)

// operatorOwnedMetadataEnvKeys is the set of env names the operator
// injects on the metadata container.
var operatorOwnedMetadataEnvKeys = []string{
	MetadataPostgresUsernameEnvKey,
	MetadataPostgresPasswordEnvKey,
}

// Operator-rendered volume names on each component's primary
// container. User templates may not declare volumeMounts with these
// names; the validator rejects them so a renamed mount can't shadow
// the operator's config / credentials / data volumes.
const (
	// EngineConfigVolumeName is the projected-volume name carrying the
	// engine config.yaml (operator-rendered ConfigMap). It is mounted
	// at ConfigMountPath on the engine container.
	EngineConfigVolumeName = "engine-config"
	// EngineDataVolumeName is the data volume backing the engine's
	// per-pod state — either a PVC synthesized from the StatefulSet's
	// VolumeClaimTemplate, an emptyDir, or a hostPath, depending on
	// FireboltEngineSpec.Storage. Mounted at DataMountPath.
	EngineDataVolumeName = "data"
	// EngineRuntimeVolumeName is the emptyDir volume mounted at
	// /run/firebolt for the engine's unix domain socket.
	EngineRuntimeVolumeName = "runtime"
	// EngineAuthAdminVolumeName is the projected Secret volume carrying
	// the Instance admin password, present only when spec.auth is
	// enabled. Mounted at AuthAdminMountPath on the engine container.
	EngineAuthAdminVolumeName = "auth-admin"
	// EngineAuthSigningVolumeNamePrefix names each provisioned signing
	// key's Secret volume: EngineAuthSigningVolumeNamePrefix + key ID
	// (e.g. "auth-signing-signing-1"), present only when spec.auth is
	// enabled. One volume per key so a rotation in flight can mount the
	// Active key and the one other key it is promoting or retiring at
	// once, without a name collision. Mounted at
	// AuthSigningMountPathBase + "/" + <key ID> on the engine container.
	EngineAuthSigningVolumeNamePrefix = "auth-signing-"
	// EngineTLSVolumeName is the projected Secret volume carrying the
	// engine listener's TLS server certificate, present only when
	// spec.tls.engine is enabled. Mounted at EngineTLSMountPath on the
	// engine container.
	EngineTLSVolumeName = "tls-engine"
	// GatewayConfigVolumeName carries the operator-rendered Envoy
	// config (envoy.yaml). Mounted at /etc/envoy on the Envoy
	// container.
	GatewayConfigVolumeName = "config-volume"
	// GatewayTmpVolumeName is the writable /tmp emptyDir the Envoy
	// container needs alongside ReadOnlyRootFilesystem=true.
	GatewayTmpVolumeName = "tmp"
	// GatewayEngineCAVolumeName carries the "ca.crt" entry from the
	// engine-listener TLS Secret, present only when spec.tls.engine is
	// enabled. Mounted read-only on the Envoy container so the gateway
	// can validate engine server certificates when re-encrypting
	// gateway->engine traffic (see buildEnvoyConfigYAML's
	// dynamic_forward_proxy transport_socket).
	GatewayEngineCAVolumeName = "engine-ca"
	// GatewayTLSVolumeName carries the gateway's downstream (client-facing)
	// TLS server certificate, present only when spec.tls.gateway is
	// enabled. Mounted read-only on the Envoy container; referenced by the
	// listener's DownstreamTlsContext in buildEnvoyConfigYAML.
	GatewayTLSVolumeName = "tls-gateway"
	// GatewayClientCAVolumeName carries the "ca.crt" the gateway verifies
	// client certificates against when mutual TLS is enabled on the
	// client-facing listener (spec.tls.gateway.clientCASecretRef). Present
	// only once gateway TLS is ready and a client CA is configured; mounted
	// read-only on the Envoy container.
	GatewayClientCAVolumeName = "client-ca"
	// GatewayEngineCRLVolumeName carries the "crl.pem" revocation list the
	// gateway checks engine certificates against when re-encrypting upstream
	// (spec.tls.engine.crlSecretRef). Present only when one is configured;
	// mounted read-only on the Envoy container.
	GatewayEngineCRLVolumeName = "engine-crl"
	// GatewayClientCRLVolumeName carries the "crl.pem" revocation list the
	// gateway checks client certificates against on its client-facing listener
	// (spec.tls.gateway.crlSecretRef). Present only when mutual TLS is on and
	// one is configured; mounted read-only on the Envoy container.
	GatewayClientCRLVolumeName = "client-crl"
	// MetadataConfigVolumeName carries the operator-rendered Pensieve
	// XML config. Mounted at /configs on the metadata container.
	MetadataConfigVolumeName = "config"
	// MetadataPostgresCredsVolumeName is the projected Secret with the
	// dedicated-pensieve Postgres username/password. Mounted at
	// /secrets/postgres on the metadata container.
	MetadataPostgresCredsVolumeName = "postgres-creds" //nolint:gosec // volume name, not a credential
	// MetadataTmpVolumeName is the writable /tmp emptyDir the metadata
	// container needs alongside ReadOnlyRootFilesystem=true.
	MetadataTmpVolumeName = "tmp"
)

// operatorOwnedEngineVolumeNames are the volume names the operator
// renders on the engine StatefulSet's pod template. User templates may
// not declare volumes or volumeMounts with these names.
//
// Signing-key volumes are deliberately absent from this list: rotation
// mounts a dynamic, growing/shrinking set of "auth-signing-<kid>" volumes
// (one per currently-tracked key), so no static enumeration of every
// possible kid could ever be complete. isReservedVolumeMountName covers
// them instead, via a prefix check against
// EngineAuthSigningVolumeNamePrefix — see its doc comment.
var operatorOwnedEngineVolumeNames = []string{
	EngineConfigVolumeName,
	EngineDataVolumeName,
	EngineRuntimeVolumeName,
	EngineAuthAdminVolumeName,
	EngineTLSVolumeName,
}

// operatorOwnedGatewayVolumeNames are the volume names the operator
// renders on the gateway Deployment's pod template.
var operatorOwnedGatewayVolumeNames = []string{
	GatewayConfigVolumeName,
	GatewayTmpVolumeName,
	GatewayEngineCAVolumeName,
	GatewayTLSVolumeName,
	GatewayClientCAVolumeName,
	GatewayEngineCRLVolumeName,
	GatewayClientCRLVolumeName,
}

// operatorOwnedMetadataVolumeNames are the volume names the operator
// renders on the metadata Deployment's pod template.
var operatorOwnedMetadataVolumeNames = []string{
	MetadataConfigVolumeName,
	MetadataPostgresCredsVolumeName,
	MetadataTmpVolumeName,
}

// EngineConfigOwnedSection enumerates one operator-owned section of the
// rendered engine config.yaml. Section is the top-level key (empty string
// for the document root). Keys lists the immediate child keys under Section
// that the operator manages exclusively.
//
// When Section is non-empty and the user-supplied value at that section is
// not a JSON object, the entire section is dropped from user input: a deep
// merge would otherwise replace the operator-built section wholesale with
// the user's scalar, losing every authoritative key.
type EngineConfigOwnedSection struct {
	// Section is the top-level key in the rendered config document, or "" for
	// the document root.
	Section string

	// Keys are the immediate children of Section managed exclusively by the
	// operator. User input at any of these paths is silently stripped.
	Keys []string
}

// OperatorOwnedEngineConfigPaths declares every path in the rendered engine
// config.yaml that the operator manages exclusively. It is consumed by
// stripProtectedEngineConfigPaths (internal/controller/engine_reconcile.go),
// which removes these paths from spec.customEngineConfig before the deep
// merge into the canonical document.
//
// Stripping is silent so that the same FireboltEngine spec stays portable
// across operator releases even when this list grows: users do not need to
// chase the protected set in their CRs to keep them applying cleanly.
var OperatorOwnedEngineConfigPaths = []EngineConfigOwnedSection{
	{Section: "", Keys: []string{"schema_version", "endpoints"}},
	{Section: "instance", Keys: []string{"id", "type", "multi_engine", "auth"}},
	{Section: "engine", Keys: []string{"id", "nodes", "termination_grace_period"}},
}

// ValidateReservedKeyPrefix rejects any key in m whose name starts with
// ReservedFireboltKeyPrefix. Used by every webhook that accepts user-set
// label or annotation maps on a CR. Returns one *field.Error per offending
// key, sorted alphabetically so test fixtures stay stable.
func ValidateReservedKeyPrefix(path *field.Path, m map[string]string) field.ErrorList {
	reserved := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, ReservedFireboltKeyPrefix) {
			reserved = append(reserved, k)
		}
	}
	if len(reserved) == 0 {
		return nil
	}
	sort.Strings(reserved)
	errs := make(field.ErrorList, 0, len(reserved))
	for _, k := range reserved {
		errs = append(errs, field.Forbidden(path.Key(k),
			fmt.Sprintf("keys with the %q prefix are reserved for the operator", ReservedFireboltKeyPrefix),
		))
	}
	return errs
}

// PodTemplateRules declares the per-component validation contract for a
// FireboltInstance subcomponent's pod template (engine, gateway,
// metadata). One ruleset per component, consumed by ValidatePodTemplate.
// The walker rejects any user-supplied input on fields the operator
// owns end-to-end while passing through fields the user is allowed to
// set; "allowed" is an explicit allowlist on the primary container so a
// future container field added by Kubernetes lands as rejected by
// default (fail-safe direction).
//
// The pod-level rejected fields (TerminationGracePeriodSeconds,
// Subdomain, Hostname, RestartPolicy, ActiveDeadlineSeconds) are
// universally operator-owned across engine, gateway, and metadata —
// every component stamps them from operator constants or relies on a
// StatefulSet / Deployment contract — so they are rejected
// unconditionally and don't appear on PodTemplateRules.
type PodTemplateRules struct {
	// Component is the short component name used in error messages
	// ("engine", "gateway", "metadata").
	Component string

	// PrimaryContainerName is the name of the operator-rendered
	// container for this component. A second container with the same
	// name is rejected as a duplicate.
	PrimaryContainerName string

	// AllowedPrimaryFields enumerates the container-level fields the
	// user may set on the primary container. Any field that the user
	// sets and is not allowed here is rejected.
	AllowedPrimaryFields PrimaryContainerFields

	// ReservedPrimaryEnvKeys are env var names the operator injects on
	// the primary container; user entries with these names are
	// rejected. Only consulted when AllowedPrimaryFields.Env is true.
	ReservedPrimaryEnvKeys []string

	// ReservedPrimaryVolumeMountNames are mount names the operator
	// renders on the primary container; user entries with these names
	// in the primary container's volumeMounts are rejected. Only
	// consulted when AllowedPrimaryFields.VolumeMounts is true.
	ReservedPrimaryVolumeMountNames []string

	// AllowSidecars permits additional containers (any container whose
	// name is not PrimaryContainerName). When false, any sidecar is
	// rejected as a whole.
	AllowSidecars bool

	// AllowInitContainers permits user-supplied initContainers. When
	// false, any init container is rejected as a whole. When true,
	// an init container named PrimaryContainerName is still rejected
	// (it would collide with the operator-rendered primary container).
	AllowInitContainers bool

	// ReservedContainerNames are additional operator-owned container names
	// (beyond PrimaryContainerName) that the operator may inject into the
	// rendered pod, e.g. the optional engine web UI sidecar. A user container
	// or init container with one of these names is rejected even when
	// AllowSidecars / AllowInitContainers is true: the operator-rendered
	// container would otherwise collide with it, and Kubernetes requires
	// container names to be unique across the regular and init lists.
	ReservedContainerNames []string
}

// PrimaryContainerFields declares which container-level fields a user
// is allowed to set on the operator-rendered primary container. Every
// field defaults to false (rejected) so silently adding a Container
// field to the Kubernetes API surface keeps the operator's owned-by-
// default posture without a code change here.
type PrimaryContainerFields struct {
	Image                    bool // image and imagePullPolicy
	Resources                bool
	Env                      bool // entries with reserved keys still rejected
	EnvFrom                  bool
	VolumeMounts             bool // entries with reserved names still rejected
	SecurityContext          bool
	Lifecycle                bool
	WorkingDir               bool
	TerminationMessagePath   bool
	TerminationMessagePolicy bool
	VolumeDevices            bool
	ResizePolicy             bool
}

// FireboltEngineClassPodTemplateRules is the ruleset for FireboltEngineClass.spec.template.
// The engine container is the user-extension point most heavily used —
// users routinely set image, env, volumeMounts, securityContext — so the
// allowlist is wide. Sidecars and additional init containers pass
// through; the FireboltEngineClass merge layer in engine_reconcile.go appends
// them onto the operator-rendered pod spec.
var FireboltEngineClassPodTemplateRules = PodTemplateRules{
	Component:            "engine",
	PrimaryContainerName: EngineContainerName,
	AllowedPrimaryFields: PrimaryContainerFields{
		Image:                    true,
		Resources:                true,
		Env:                      true,
		EnvFrom:                  true,
		VolumeMounts:             true,
		SecurityContext:          true,
		Lifecycle:                true,
		WorkingDir:               true,
		TerminationMessagePath:   true,
		TerminationMessagePolicy: true,
		VolumeDevices:            true,
		ResizePolicy:             true,
	},
	ReservedPrimaryEnvKeys:          operatorOwnedEngineEnvKeys,
	ReservedPrimaryVolumeMountNames: operatorOwnedEngineVolumeNames,
	AllowSidecars:                   true,
	AllowInitContainers:             true,
	ReservedContainerNames:          []string{EngineWebContainerName},
}

// GatewayPodTemplateRules is the ruleset for FireboltInstance.spec.gateway.template.
// Envoy is operator-rendered end-to-end (config, command via args,
// ports, probes, preStop drain hook, securityContext, the config and
// tmp volume mounts), so the user-allowed surface on the primary
// container is intentionally narrow: only image (so users can roll
// Envoy versions) and resources (so users can size the pod). The user
// may add sidecars (e.g. a stats exporter, a network filter) and
// init containers (e.g. a config validator); the gateway builder
// appends them after the operator-rendered Envoy container.
var GatewayPodTemplateRules = PodTemplateRules{
	Component:            "gateway",
	PrimaryContainerName: GatewayContainerName,
	AllowedPrimaryFields: PrimaryContainerFields{
		Image:     true,
		Resources: true,
	},
	ReservedPrimaryVolumeMountNames: operatorOwnedGatewayVolumeNames,
	AllowSidecars:                   true,
	AllowInitContainers:             true,
}

// MetadataPodTemplateRules is the ruleset for FireboltInstance.spec.metadata.template.
// The Pensieve container is operator-rendered (command, ports, probes,
// the POSTGRES_USERNAME_FILE/POSTGRES_PASSWORD_FILE env vars, the
// config / postgres-creds / tmp volume mounts, securityContext), so
// only image and resources are user-settable on the primary container.
// Sidecars and additional init containers pass through, same shape as
// the gateway.
var MetadataPodTemplateRules = PodTemplateRules{
	Component:            "metadata",
	PrimaryContainerName: MetadataContainerName,
	AllowedPrimaryFields: PrimaryContainerFields{
		Image:     true,
		Resources: true,
	},
	ReservedPrimaryEnvKeys:          operatorOwnedMetadataEnvKeys,
	ReservedPrimaryVolumeMountNames: operatorOwnedMetadataVolumeNames,
	AllowSidecars:                   true,
	AllowInitContainers:             true,
}

// ValidateOperatorOwnedPodTemplate is the FireboltEngineClass entry point for
// pod-template validation. Kept as a stable named function because the
// FireboltEngineClass webhook references it directly; the implementation
// delegates to the generic ValidatePodTemplate walker driven by
// FireboltEngineClassPodTemplateRules.
func ValidateOperatorOwnedPodTemplate(template *corev1.PodTemplateSpec, base *field.Path) field.ErrorList {
	return ValidatePodTemplate(template, base, FireboltEngineClassPodTemplateRules)
}

// ValidatePodTemplate walks a user-supplied PodTemplateSpec and rejects
// any input that conflicts with the supplied component rules. It is the
// single enforcement entry point for every component pod template the
// operator templates over (engine, gateway, metadata).
//
// Rejection covers four layers:
//
//   - pod-template metadata.labels / metadata.annotations under the
//     ReservedFireboltKeyPrefix.
//   - pod-level fields the operator owns universally:
//     terminationGracePeriodSeconds, subdomain, hostname,
//     restartPolicy, activeDeadlineSeconds.
//   - the primary container (matched by rules.PrimaryContainerName):
//     allowlist-driven — only fields enabled in rules.AllowedPrimaryFields
//     pass; everything else is rejected. Within env and volumeMounts,
//     entries with reserved names are rejected even when those fields
//     are allowed in general.
//   - init containers and additional containers (anything not
//     PrimaryContainerName): rejected entirely when their respective
//     AllowSidecars / AllowInitContainers flag is false. When permitted,
//     they pass through with the single exception that no init
//     container may take the primary container's name.
//
// base is the field.Path the caller used to reach this PodTemplateSpec
// in its own object (e.g. field.NewPath("spec","template") for
// FireboltEngineClass; field.NewPath("spec","gateway","template") for the
// FireboltInstance gateway). Returned errors carry the full nested
// path so kubectl apply surfaces every violation at the offending
// coordinate.
func ValidatePodTemplate(template *corev1.PodTemplateSpec, base *field.Path, rules PodTemplateRules) field.ErrorList {
	if template == nil {
		return nil
	}
	var errs field.ErrorList

	metaPath := base.Child("metadata")
	errs = append(errs, validatePodTemplateMetadata(&template.ObjectMeta, metaPath)...)
	errs = append(errs, ValidateReservedKeyPrefix(metaPath.Child("labels"), template.Labels)...)
	errs = append(errs, ValidateReservedKeyPrefix(metaPath.Child("annotations"), template.Annotations)...)

	specPath := base.Child("spec")
	errs = append(errs, validateUniversalPodFields(&template.Spec, specPath)...)
	errs = append(errs, validateContainersAgainstRules(template.Spec.Containers, specPath.Child("containers"), rules)...)
	errs = append(errs, validateInitContainersAgainstRules(template.Spec.InitContainers, specPath.Child("initContainers"), rules)...)

	return errs
}

// validatePodTemplateMetadata closes the silent-drop path on
// spec.template.metadata: the embedded corev1.ObjectMeta lets users
// submit name / namespace / ownerReferences / finalizers / etc., and
// the operator silently strips them at render time (the StatefulSet
// controller assigns identity to per-pod ObjectMetas). Reject those
// fields at admission so users discover the no-op immediately rather
// than wondering why their finalizer never ran.
//
// Only labels and annotations are passed through (and further
// constrained by ValidateReservedKeyPrefix). Everything else on
// ObjectMeta has no meaning at the pod-template level for any
// workload controller.
func validatePodTemplateMetadata(meta *metav1.ObjectMeta, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	if meta.Name != "" {
		errs = append(errs, field.Forbidden(base.Child("name"),
			"pod template metadata.name is assigned by the StatefulSet controller; remove it"))
	}
	if meta.GenerateName != "" {
		errs = append(errs, field.Forbidden(base.Child("generateName"),
			"pod template metadata.generateName has no effect under a StatefulSet; remove it"))
	}
	if meta.Namespace != "" {
		errs = append(errs, field.Forbidden(base.Child("namespace"),
			"pod template metadata.namespace is inherited from the owning resource; remove it"))
	}
	if meta.UID != "" {
		errs = append(errs, field.Forbidden(base.Child("uid"),
			"pod template metadata.uid is assigned by the API server; remove it"))
	}
	if meta.ResourceVersion != "" {
		errs = append(errs, field.Forbidden(base.Child("resourceVersion"),
			"pod template metadata.resourceVersion is assigned by the API server; remove it"))
	}
	if meta.Generation != 0 {
		errs = append(errs, field.Forbidden(base.Child("generation"),
			"pod template metadata.generation is assigned by the API server; remove it"))
	}
	if meta.CreationTimestamp != (metav1.Time{}) {
		errs = append(errs, field.Forbidden(base.Child("creationTimestamp"),
			"pod template metadata.creationTimestamp is assigned by the API server; remove it"))
	}
	if meta.DeletionTimestamp != nil {
		errs = append(errs, field.Forbidden(base.Child("deletionTimestamp"),
			"pod template metadata.deletionTimestamp has no meaning here; remove it"))
	}
	if meta.DeletionGracePeriodSeconds != nil {
		errs = append(errs, field.Forbidden(base.Child("deletionGracePeriodSeconds"),
			"pod template metadata.deletionGracePeriodSeconds has no meaning here; remove it"))
	}
	if len(meta.OwnerReferences) > 0 {
		errs = append(errs, field.Forbidden(base.Child("ownerReferences"),
			"pod template metadata.ownerReferences are operator-managed; remove them"))
	}
	if len(meta.Finalizers) > 0 {
		errs = append(errs, field.Forbidden(base.Child("finalizers"),
			"pod template metadata.finalizers are silently dropped at render time; remove them"))
	}
	if len(meta.ManagedFields) > 0 {
		errs = append(errs, field.Forbidden(base.Child("managedFields"),
			"pod template metadata.managedFields are assigned by the API server; remove them"))
	}
	return errs
}

// validateUniversalPodFields enforces the pod-level (non-container)
// ownership rules that apply to every component. Three categories:
//
//   - Operator-stamped: terminationGracePeriodSeconds (component default
//     or hardcoded engine 60s).
//   - Workload-contract: subdomain / hostname (headless DNS),
//     restartPolicy (StatefulSet / Deployment), activeDeadlineSeconds
//     (long-lived pods).
//   - Security / footgun: hostNetwork / hostPID / hostIPC /
//     shareProcessNamespace / hostUsers. Sharing the node network or
//     PID namespace with the engine pod defeats the isolation a
//     long-lived data-plane workload depends on; we close those at
//     admission rather than silently accept and let a user accidentally
//     expose engine memory to anything else running on the node.
func validateUniversalPodFields(spec *corev1.PodSpec, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	if spec.TerminationGracePeriodSeconds != nil {
		errs = append(errs, field.Forbidden(base.Child("terminationGracePeriodSeconds"),
			"terminationGracePeriodSeconds is operator-owned"))
	}
	if spec.Subdomain != "" {
		errs = append(errs, field.Forbidden(base.Child("subdomain"),
			"subdomain is owned by the operator"))
	}
	if spec.Hostname != "" {
		errs = append(errs, field.Forbidden(base.Child("hostname"),
			"hostname is owned by the operator"))
	}
	if spec.RestartPolicy != "" {
		errs = append(errs, field.Forbidden(base.Child("restartPolicy"),
			"restartPolicy is fixed by the workload controller"))
	}
	if spec.ActiveDeadlineSeconds != nil {
		errs = append(errs, field.Forbidden(base.Child("activeDeadlineSeconds"),
			"activeDeadlineSeconds is incompatible with long-lived component pods"))
	}
	if spec.HostNetwork {
		errs = append(errs, field.Forbidden(base.Child("hostNetwork"),
			"hostNetwork sharing is not permitted for component pods"))
	}
	if spec.HostPID {
		errs = append(errs, field.Forbidden(base.Child("hostPID"),
			"hostPID sharing is not permitted for component pods"))
	}
	if spec.HostIPC {
		errs = append(errs, field.Forbidden(base.Child("hostIPC"),
			"hostIPC sharing is not permitted for component pods"))
	}
	if spec.ShareProcessNamespace != nil {
		errs = append(errs, field.Forbidden(base.Child("shareProcessNamespace"),
			"shareProcessNamespace is not permitted for component pods"))
	}
	if spec.HostUsers != nil {
		errs = append(errs, field.Forbidden(base.Child("hostUsers"),
			"hostUsers is not permitted for component pods"))
	}
	return errs
}

// validateContainersAgainstRules walks template.spec.containers. The
// container whose name matches rules.PrimaryContainerName is validated
// against the allowlist; a second container with the same name is
// rejected as a duplicate (the operator's container-merge would emit
// two containers with the same name and the pod would never be
// created). Containers with any other name are sidecars: rejected as a
// group when rules.AllowSidecars is false, passed through unchanged
// otherwise.
func validateContainersAgainstRules(containers []corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	primarySeen := false
	for i := range containers {
		c := &containers[i]
		path := base.Index(i)
		switch {
		case c.Name == rules.PrimaryContainerName:
			if primarySeen {
				errs = append(errs, field.Duplicate(path.Child("name"), rules.PrimaryContainerName))
				continue
			}
			primarySeen = true
			errs = append(errs, validatePrimaryContainerFields(c, path, rules)...)
		case slices.Contains(rules.ReservedContainerNames, c.Name):
			errs = append(errs, field.Forbidden(path.Child("name"),
				fmt.Sprintf("container name %q is reserved by the %s operator and cannot be set on the pod template",
					c.Name, rules.Component)))
		case !rules.AllowSidecars:
			errs = append(errs, field.Forbidden(path,
				fmt.Sprintf("additional containers are not allowed on the %s pod template; only the %q container may be defined here",
					rules.Component, rules.PrimaryContainerName)))
		default:
			// Sidecar with an allowed-sidecars ruleset: pass through, but
			// still reject any volumeMount that names an operator-owned
			// volume so a sidecar cannot mount the auth/TLS Secret volumes
			// the operator renders at pod level (see
			// validateReservedVolumeMounts).
			errs = append(errs, validateReservedVolumeMounts(c, path, rules)...)
		}
	}
	return errs
}

// validateInitContainersAgainstRules rejects init containers as a group
// when rules.AllowInitContainers is false. When permitted, an init
// container whose name collides with the primary container is still
// rejected: the operator-rendered primary container would then live
// alongside a same-named init container, and Kubernetes would never
// admit such a pod. A permitted init container's volumeMounts are also
// screened for operator-owned volume names (see validateReservedVolumeMounts).
func validateInitContainersAgainstRules(initContainers []corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	for i := range initContainers {
		c := &initContainers[i]
		path := base.Index(i)
		if !rules.AllowInitContainers {
			errs = append(errs, field.Forbidden(path,
				fmt.Sprintf("init containers are not allowed on the %s pod template", rules.Component)))
			continue
		}
		if c.Name == rules.PrimaryContainerName {
			errs = append(errs, field.Forbidden(path.Child("name"),
				fmt.Sprintf("init container name %q collides with the %s container; pick a different name",
					rules.PrimaryContainerName, rules.Component)))
		} else if slices.Contains(rules.ReservedContainerNames, c.Name) {
			errs = append(errs, field.Forbidden(path.Child("name"),
				fmt.Sprintf("init container name %q is reserved by the %s operator; pick a different name",
					c.Name, rules.Component)))
		}
		errs = append(errs, validateReservedVolumeMounts(c, path, rules)...)
	}
	return errs
}

// validateReservedVolumeMounts rejects any volumeMount on a user-supplied
// sidecar or init container that names an operator-owned volume. The
// operator renders those volumes (config / data / runtime and, when auth or
// TLS is enabled, the auth-admin / auth-signing-<kid> / tls-engine Secret
// volumes) at the pod level, so a sidecar or init container that mounts one
// by name would gain access the pod-template model never grants — most
// critically reading the admin credentials or JWT signing keys straight off
// disk, a privilege-escalation path for a template author who cannot
// otherwise read those Secrets. Reserved names are matched via
// isReservedVolumeMountName so the dynamically-numbered auth-signing-<kid>
// set is covered by prefix, exactly as the primary container's check is.
func validateReservedVolumeMounts(c *corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	for mi := range c.VolumeMounts {
		if !isReservedVolumeMountName(c.VolumeMounts[mi].Name, rules.ReservedPrimaryVolumeMountNames) {
			continue
		}
		errs = append(errs, field.Forbidden(base.Child("volumeMounts").Index(mi).Child("name"),
			fmt.Sprintf("volumeMount name %q is operator-owned and cannot be mounted by an additional container on the %s pod template",
				c.VolumeMounts[mi].Name, rules.Component)))
	}
	return errs
}

// VolumeSecretRefs returns every Secret name v reaches. The first two sources
// place the Secret's contents in the pod filesystem; the rest hand it to a
// storage driver, which is not a read primitive for the pod but is still a way
// to route the material somewhere the template author chose.
//
// A PVC — and therefore an ephemeral volume's claim template — cannot name a
// Secret as its content, so neither appears here.
func VolumeSecretRefs(v *corev1.Volume) []string {
	var out []string
	add := func(name string) {
		if name != "" {
			out = append(out, name)
		}
	}
	if v.Secret != nil {
		add(v.Secret.SecretName)
	}
	if v.Projected != nil {
		for i := range v.Projected.Sources {
			if s := v.Projected.Sources[i].Secret; s != nil {
				add(s.Name)
			}
		}
	}
	if v.CSI != nil && v.CSI.NodePublishSecretRef != nil {
		add(v.CSI.NodePublishSecretRef.Name)
	}
	if v.AzureFile != nil {
		add(v.AzureFile.SecretName)
	}
	if v.CephFS != nil && v.CephFS.SecretRef != nil {
		add(v.CephFS.SecretRef.Name)
	}
	if v.Cinder != nil && v.Cinder.SecretRef != nil {
		add(v.Cinder.SecretRef.Name)
	}
	if v.FlexVolume != nil && v.FlexVolume.SecretRef != nil {
		add(v.FlexVolume.SecretRef.Name)
	}
	if v.ISCSI != nil && v.ISCSI.SecretRef != nil {
		add(v.ISCSI.SecretRef.Name)
	}
	if v.RBD != nil && v.RBD.SecretRef != nil {
		add(v.RBD.SecretRef.Name)
	}
	if v.ScaleIO != nil && v.ScaleIO.SecretRef != nil {
		add(v.ScaleIO.SecretRef.Name)
	}
	if v.StorageOS != nil && v.StorageOS.SecretRef != nil {
		add(v.StorageOS.SecretRef.Name)
	}
	return out
}

// ValidateNoSecretAliasVolumes rejects a user-supplied pod volume that reaches
// one of the protected Secrets under a name of the author's own choosing.
//
// The reserved-volume-name guard cannot see this. The volume name is arbitrary,
// only the source gives it away, and every component ruleset permits sidecars
// and init containers whose image and command the author controls — so mounting
// such an alias reads the Instance admin password or a JWT signing private key
// off disk without any Secret-read RBAC.
//
// isProtected decides which Secret names are off limits, so each caller supplies
// exactly what it knows: the controller knows the names it resolved and mounted
// plus the generation-numbered engine-TLS family, while the webhook knows only
// what it can derive from the CR under review. A nil predicate protects nothing.
func ValidateNoSecretAliasVolumes(
	volumes []corev1.Volume, base *field.Path, isProtected func(string) bool, component string,
) field.ErrorList {
	if isProtected == nil {
		return nil
	}
	const detail = "volume %q references Secret %q, which the %s operator mounts itself; " +
		"an additional container could read the Instance admin password or a JWT signing key through it"
	var errs field.ErrorList
	for i := range volumes {
		for _, name := range VolumeSecretRefs(&volumes[i]) {
			if !isProtected(name) {
				continue
			}
			errs = append(errs, field.Forbidden(base.Index(i),
				fmt.Sprintf(detail, volumes[i].Name, name, component)))
		}
	}
	return errs
}

// ContainerSecretRefs returns every Secret name c pulls into its environment,
// through either a single-key env reference or a whole-Secret envFrom.
func ContainerSecretRefs(c *corev1.Container) []string {
	var out []string
	for i := range c.Env {
		if vf := c.Env[i].ValueFrom; vf != nil && vf.SecretKeyRef != nil && vf.SecretKeyRef.Name != "" {
			out = append(out, vf.SecretKeyRef.Name)
		}
	}
	for i := range c.EnvFrom {
		if ref := c.EnvFrom[i].SecretRef; ref != nil && ref.Name != "" {
			out = append(out, ref.Name)
		}
	}
	return out
}

// ValidateNoSecretRefEnv rejects a container that pulls a protected Secret into
// its environment. This is the same escalation as a volume alias by a different
// route: the reserved-volume-name rules screen mounts, and the alias rules screen
// volume sources, but neither sees env. A permitted sidecar whose image and
// command the author controls can simply read the value out of its own
// environment.
//
// Unlike a volume, an env reference is resolved once when the pod starts and is
// never re-synced, so refusing to render is a complete remedy — there is no
// already-running pod that can acquire the material later.
func ValidateNoSecretRefEnv(
	containers []corev1.Container, base *field.Path, isProtected func(string) bool, component string,
) field.ErrorList {
	if isProtected == nil {
		return nil
	}
	const detail = "container %q reads Secret %q into its environment, and the %s operator mounts that Secret " +
		"itself; this would expose the Instance admin password or a JWT signing key to a container the " +
		"pod-template model does not grant it to"
	var errs field.ErrorList
	for i := range containers {
		for _, name := range ContainerSecretRefs(&containers[i]) {
			if !isProtected(name) {
				continue
			}
			errs = append(errs, field.Forbidden(base.Index(i),
				fmt.Sprintf(detail, containers[i].Name, name, component)))
		}
	}
	return errs
}

// InstanceOperatorSecretNames returns every operator-managed Secret belonging to
// this Instance that is derivable from the CR itself, REGARDLESS of which
// component's pod happens to mount it: the admin password, each JWT signing key,
// the engine-listener TLS anchor, the gateway's serving certificate, the gateway
// client CA, and an externally-supplied metadata Postgres credential. Empty
// entries are omitted, so an Instance whose auth or TLS is still provisioning
// contributes nothing.
//
// The set is deliberately Instance-wide rather than per-component. It used to be
// three disjoint per-component lists, which meant each pod template was screened
// only against the Secrets ITS OWN pod mounts — so a gateway or metadata template
// could alias the admin password or a JWT signing key, and one engine's template
// could alias another engine's serving key. All of those are the same escalation
// the guard exists to stop: a template author who cannot read Secrets mounts one
// into a sidecar whose image and command they control. Screening every template
// against the union closes the cross-component routes.
//
// Two names cannot be derived here because they are formed from suffixes private
// to the controller package: the engine CA bundle, and the operator-generated
// (as opposed to user-supplied) Postgres credential. The controller adds those,
// along with the shape match for per-generation engine-TLS Secrets — see
// instanceProtectedSecret in the controller package, which is the authoritative,
// complete predicate. This function is what the admission webhooks can compute
// on their own.
func InstanceOperatorSecretNames(inst *FireboltInstance) []string {
	var names []string
	add := func(n string) {
		if n != "" {
			names = append(names, n)
		}
	}
	if inst.Spec.Auth != nil && inst.Spec.Auth.Enabled && inst.Spec.Auth.Local != nil {
		add(inst.Spec.Auth.Local.Admin.Password.Name)
	}
	if inst.Status.Auth != nil {
		for _, k := range inst.Status.Auth.SigningKeys {
			add(k.SecretName)
		}
	}
	if inst.Status.EngineTLS != nil {
		add(inst.Status.EngineTLS.SecretName)
	}
	if inst.Status.GatewayTLS != nil {
		add(inst.Status.GatewayTLS.SecretName)
	}
	if inst.Spec.TLS != nil && inst.Spec.TLS.Gateway != nil {
		if ref := inst.Spec.TLS.Gateway.ClientCASecretRef; ref != nil {
			add(ref.Name)
		}
	}
	if inst.Spec.Metadata.Postgres != nil {
		add(inst.Spec.Metadata.Postgres.CredentialsSecretRef.Name)
	}
	return names
}

// InstanceProvisionedSecretNames returns only the Secrets the operator CREATES
// itself for this Instance — never one the user pointed it at.
//
// This is deliberately narrower than InstanceOperatorSecretNames, and the
// difference matters. The broad set is right for the pod-template guards: a
// template has no business mounting the admin password or the client CA even
// though the user created those. But it is wrong for screening the user's own
// spec-level references, because a bring-your-own listener's Secret ends up
// recorded in Status.*TLS.SecretName — so screening spec.tls.gateway.secretRef
// against the broad set would reject the very Secret the user legitimately
// supplied, the moment the operator adopted it.
//
// What remains here is the material whose private key the operator mints: signing
// keypairs, and each TLS listener's certificate ONLY on the cert-manager path.
// Two names formed from controller-private suffixes (the engine CA bundle and the
// operator-generated Postgres credential) are added by the caller in the
// controller package.
func InstanceProvisionedSecretNames(inst *FireboltInstance) []string {
	var names []string
	add := func(n string) {
		if n != "" {
			names = append(names, n)
		}
	}
	if inst.Status.Auth != nil {
		for _, k := range inst.Status.Auth.SigningKeys {
			add(k.SecretName)
		}
	}
	if tls := inst.Spec.TLS; tls != nil {
		if tls.Engine != nil && tls.Engine.SecretRef == nil && inst.Status.EngineTLS != nil {
			add(inst.Status.EngineTLS.SecretName)
		}
		if tls.Gateway != nil && tls.Gateway.SecretRef == nil && inst.Status.GatewayTLS != nil {
			add(inst.Status.GatewayTLS.SecretName)
		}
	}
	return names
}

// instanceProtectedSecretPredicate adapts InstanceProvisionedSecretNames into the
// predicate the TLS reference checks take. Non-nil even on a first apply, when
// the Instance has provisioned nothing to name yet: the per-generation engine
// serving Secrets are matched by shape and exist independently of anything this
// Instance's status records.
func instanceProtectedSecretPredicate(inst *FireboltInstance) func(string) bool {
	set := make(map[string]struct{})
	for _, n := range InstanceProvisionedSecretNames(inst) {
		set[n] = struct{}{}
	}
	return func(name string) bool {
		if name == "" {
			return false
		}
		if _, hit := set[name]; hit {
			return true
		}
		return IsGeneratedEngineTLSSecretName(name) || IsSigningKeySecretName(name)
	}
}

// Suffixes of operator-generated Secret names that admission has to recognize
// without being able to enumerate the live set. Kept in step with the
// controller's own constants by the shape-match binding tests.
const (
	suffixEngineTLS   = "-engine-tls"
	suffixGen         = "-g"
	suffixAuthSigning = "-auth-signing"
)

// IsSigningKeySecretName reports whether name is a JWT signing keypair Secret
// belonging to ANY Instance in the namespace: a bootstrap key
// ("<instance>-auth-signing") or a rotation generation
// ("<instance>-auth-signing-<kid>").
//
// Matched by shape rather than against the names in status.auth.signingKeys,
// because status trails the Secret. On a first apply status.auth is nil, and
// the Instance reconciler validates pod templates before it provisions the
// signing key, so a name-only screen admits a template that aliases the key the
// operator is about to mint. Future rotation generations have the same problem:
// their names do not exist anywhere until the moment they are minted.
//
// Any-Instance, not this-Instance, for the same reason IsGeneratedEngineTLSSecretName
// is any-engine: protection is a property of the Secret, not of which resource is
// under review. Scoping the match to the Instance being validated let a template on
// one Instance mount a SIBLING Instance's signing key out of the same namespace and
// mint tokens the sibling's engines honor — the cross-component escalation this
// guard exists to stop, one level up.
//
// Reserving the shape also claims names the operator does not use, such as
// "app-auth-signing-mine". That is the intended trade, and the same one already
// accepted for engine serving certificates.
func IsSigningKeySecretName(name string) bool {
	base, _, found := strings.Cut(name, suffixAuthSigning)
	if !found || base == "" {
		return false
	}
	// Only an exact suffix or a "-<kid>" continuation; "app-auth-signingkey" is
	// someone else's Secret.
	rest := name[len(base)+len(suffixAuthSigning):]
	return rest == "" || strings.HasPrefix(rest, "-")
}

// IsGeneratedEngineTLSSecretName reports whether name has the shape of a
// per-generation engine serving-certificate Secret ("<engine>-g<N>-engine-tls")
// for ANY engine in the namespace.
//
// Matched by shape rather than by name because the generation number means no
// admission call can know which ones currently exist, and by any-engine because
// a sibling engine's serving key is worth as much to an attacker as one's own:
// the gateway authenticates engines against the engine CA and a namespace
// wildcard SAN, so holding any engine's key is enough to impersonate an engine
// to it.
func IsGeneratedEngineTLSSecretName(name string) bool {
	if !strings.HasSuffix(name, suffixEngineTLS) {
		return false
	}
	// Require the generation infix so the instance-wide anchor
	// ("<instance>-engine-tls") and an unrelated user Secret merely ending in
	// "-engine-tls" are not swept in.
	return strings.Contains(strings.TrimSuffix(name, suffixEngineTLS), suffixGen)
}

// validatePrimaryContainerFields walks every user-set container field on
// the primary container and rejects any that the allowlist does not
// permit. The check splits into three groups: hardcoded operator-owned
// fields (Name, Command, Args, Ports, Probes), interactive-orchestration
// fields rejected for every component (RestartPolicy, Stdin/Once, TTY),
// and allowlist-toggled fields. Env and VolumeMounts, even when
// allowed, have their reserved-key / reserved-name filter applied per
// entry.
func validatePrimaryContainerFields(c *corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	errs = append(errs, validatePrimaryHardcodedRejects(c, base, rules)...)
	errs = append(errs, validatePrimaryInteractiveRejects(c, base, rules)...)
	errs = append(errs, validatePrimaryAllowlistedScalars(c, base, rules)...)
	errs = append(errs, validatePrimaryAllowlistedSlices(c, base, rules)...)
	errs = append(errs, validatePrimaryAllowlistedExtras(c, base, rules)...)
	return errs
}

// validatePrimaryHardcodedRejects covers fields the operator owns
// unconditionally: name (via container-walk), command, args, ports,
// all three probes. These have no allowlist toggle.
func validatePrimaryHardcodedRejects(c *corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	if len(c.Command) > 0 {
		errs = append(errs, field.Forbidden(base.Child("command"),
			fmt.Sprintf("%s container command is operator-owned", rules.Component)))
	}
	if len(c.Args) > 0 {
		errs = append(errs, field.Forbidden(base.Child("args"),
			fmt.Sprintf("%s container args are operator-owned", rules.Component)))
	}
	if len(c.Ports) > 0 {
		errs = append(errs, field.Forbidden(base.Child("ports"),
			fmt.Sprintf("%s container ports are operator-owned", rules.Component)))
	}
	if c.ReadinessProbe != nil {
		errs = append(errs, field.Forbidden(base.Child("readinessProbe"),
			fmt.Sprintf("%s container readinessProbe is operator-owned", rules.Component)))
	}
	if c.LivenessProbe != nil {
		errs = append(errs, field.Forbidden(base.Child("livenessProbe"),
			fmt.Sprintf("%s container livenessProbe is operator-owned", rules.Component)))
	}
	if c.StartupProbe != nil {
		errs = append(errs, field.Forbidden(base.Child("startupProbe"),
			fmt.Sprintf("%s container startupProbe is operator-owned", rules.Component)))
	}
	return errs
}

// validatePrimaryInteractiveRejects rejects fields that make no sense
// on a long-lived data-plane container. RestartPolicy on a non-init
// container is silently dropped by the kubelet; Stdin/StdinOnce/TTY
// are kubectl-exec ergonomics with no meaning on a server process.
// Closing these here gives users immediate feedback instead of
// "set it, nothing happened".
func validatePrimaryInteractiveRejects(c *corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	if c.RestartPolicy != nil {
		errs = append(errs, field.Forbidden(base.Child("restartPolicy"),
			fmt.Sprintf("%s container restartPolicy has no effect on a long-lived workload container", rules.Component)))
	}
	if c.Stdin {
		errs = append(errs, field.Forbidden(base.Child("stdin"),
			fmt.Sprintf("%s container stdin is for interactive use only; the %s runs non-interactively", rules.Component, rules.Component)))
	}
	if c.StdinOnce {
		errs = append(errs, field.Forbidden(base.Child("stdinOnce"),
			fmt.Sprintf("%s container stdinOnce is for interactive use only; the %s runs non-interactively", rules.Component, rules.Component)))
	}
	if c.TTY {
		errs = append(errs, field.Forbidden(base.Child("tty"),
			fmt.Sprintf("%s container tty is for interactive use only; the %s runs non-interactively", rules.Component, rules.Component)))
	}
	return errs
}

// validatePrimaryAllowlistedScalars covers the scalar / pointer
// container fields whose allowlist toggle is per-ruleset:
// image+imagePullPolicy, resources, securityContext, lifecycle.
func validatePrimaryAllowlistedScalars(c *corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	allowed := rules.AllowedPrimaryFields
	if !allowed.Image && (c.Image != "" || c.ImagePullPolicy != "") {
		errs = append(errs, field.Forbidden(base.Child("image"),
			fmt.Sprintf("%s container image is operator-owned", rules.Component)))
	}
	if !allowed.Resources && HasContainerResources(c.Resources) {
		errs = append(errs, field.Forbidden(base.Child("resources"),
			fmt.Sprintf("%s container resources are operator-owned", rules.Component)))
	}
	if !allowed.SecurityContext && c.SecurityContext != nil {
		errs = append(errs, field.Forbidden(base.Child("securityContext"),
			fmt.Sprintf("%s container securityContext is operator-owned", rules.Component)))
	}
	if !allowed.Lifecycle && c.Lifecycle != nil {
		errs = append(errs, field.Forbidden(base.Child("lifecycle"),
			fmt.Sprintf("%s container lifecycle is operator-owned", rules.Component)))
	}
	return errs
}

// validatePrimaryAllowlistedSlices covers the slice-typed container
// fields whose allowlist toggle is per-ruleset (Env, EnvFrom,
// VolumeMounts) and applies the reserved-key / reserved-name per-entry
// filter when allowed.
func validatePrimaryAllowlistedSlices(c *corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	allowed := rules.AllowedPrimaryFields
	if allowed.Env {
		for ei := range c.Env {
			if !isReservedKey(c.Env[ei].Name, rules.ReservedPrimaryEnvKeys) {
				continue
			}
			errs = append(errs, field.Forbidden(base.Child("env").Index(ei).Child("name"),
				fmt.Sprintf("env key %q is injected by the operator; pick a different name", c.Env[ei].Name)))
		}
	} else if len(c.Env) > 0 {
		errs = append(errs, field.Forbidden(base.Child("env"),
			fmt.Sprintf("%s container env is operator-owned", rules.Component)))
	}
	if !allowed.EnvFrom && len(c.EnvFrom) > 0 {
		errs = append(errs, field.Forbidden(base.Child("envFrom"),
			fmt.Sprintf("%s container envFrom is operator-owned", rules.Component)))
	}
	if allowed.VolumeMounts {
		for mi := range c.VolumeMounts {
			if !isReservedVolumeMountName(c.VolumeMounts[mi].Name, rules.ReservedPrimaryVolumeMountNames) {
				continue
			}
			errs = append(errs, field.Forbidden(base.Child("volumeMounts").Index(mi).Child("name"),
				fmt.Sprintf("volumeMount name %q is operator-owned; pick a different name", c.VolumeMounts[mi].Name)))
		}
	} else if len(c.VolumeMounts) > 0 {
		errs = append(errs, field.Forbidden(base.Child("volumeMounts"),
			fmt.Sprintf("%s container volumeMounts are operator-owned", rules.Component)))
	}
	return errs
}

// validatePrimaryAllowlistedExtras covers the optional primary-container
// allowlist toggles: WorkingDir, TerminationMessagePath/Policy,
// VolumeDevices, ResizePolicy. Each is rejected when the ruleset
// does not opt the component in.
func validatePrimaryAllowlistedExtras(c *corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	allowed := rules.AllowedPrimaryFields
	if !allowed.WorkingDir && c.WorkingDir != "" {
		errs = append(errs, field.Forbidden(base.Child("workingDir"),
			fmt.Sprintf("%s container workingDir is operator-owned", rules.Component)))
	}
	if !allowed.TerminationMessagePath && c.TerminationMessagePath != "" {
		errs = append(errs, field.Forbidden(base.Child("terminationMessagePath"),
			fmt.Sprintf("%s container terminationMessagePath is operator-owned", rules.Component)))
	}
	if !allowed.TerminationMessagePolicy && c.TerminationMessagePolicy != "" {
		errs = append(errs, field.Forbidden(base.Child("terminationMessagePolicy"),
			fmt.Sprintf("%s container terminationMessagePolicy is operator-owned", rules.Component)))
	}
	if !allowed.VolumeDevices && len(c.VolumeDevices) > 0 {
		errs = append(errs, field.Forbidden(base.Child("volumeDevices"),
			fmt.Sprintf("%s container volumeDevices are operator-owned", rules.Component)))
	}
	if !allowed.ResizePolicy && len(c.ResizePolicy) > 0 {
		errs = append(errs, field.Forbidden(base.Child("resizePolicy"),
			fmt.Sprintf("%s container resizePolicy is operator-owned", rules.Component)))
	}
	return errs
}

// HasContainerResources reports whether a ResourceRequirements struct
// carries any user input (requests, limits, or claims). Exported so
// controller code that consumes the API package can reuse it instead
// of restating the predicate (callers: builders that decide whether
// to copy a user-supplied Resources field through to the rendered
// container, drift comparators that need to distinguish "user said
// nothing" from "user said empty").
func HasContainerResources(r corev1.ResourceRequirements) bool {
	return len(r.Requests) > 0 || len(r.Limits) > 0 || len(r.Claims) > 0
}

// isReservedKey reports whether name appears in the reserved slice.
// O(n) suits the small reserved sets the operator carries (at most a
// few entries per component).
func isReservedKey(name string, reserved []string) bool {
	for _, k := range reserved {
		if name == k {
			return true
		}
	}
	return false
}

// isReservedVolumeMountName reports whether name is a container
// volumeMount name the operator owns: either an exact match against
// reserved, or a name starting with EngineAuthSigningVolumeNamePrefix.
// The prefix check exists only for that one case: signing-key rotation
// can mount any number of dynamically-numbered "auth-signing-<kid>"
// volumes (one per currently-tracked key), so no static enumeration of
// every possible kid — the way every other reserved name is a single
// fixed string — could ever be complete.
func isReservedVolumeMountName(name string, reserved []string) bool {
	if strings.HasPrefix(name, EngineAuthSigningVolumeNamePrefix) {
		return true
	}
	return isReservedKey(name, reserved)
}
