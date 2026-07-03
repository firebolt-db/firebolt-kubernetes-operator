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
// keys carry pod-index plumbing (POD_INDEX) and AWS SDK + runtime mode
// signals that the operator must control end to end: a user override
// would either crash the engine or silently divert its identity. They
// are rejected at admission time on FireboltEngineClass spec.template and would
// be stripped if injected through another template channel.
const (
	EnginePodIndexEnvKey                    = "POD_INDEX"
	EngineAwsEC2MetadataClientEnabledEnvKey = "FB_AWS_EC2_METADATA_CLIENT_ENABLED"
	EngineCoreModeEnvKey                    = "FIREBOLT_CORE_MODE"
)

// operatorOwnedEngineEnvKeys is the set of env names the operator injects on
// the engine container and that user templates may not redefine. Maintained
// as a slice so iteration order is deterministic when reporting violations.
var operatorOwnedEngineEnvKeys = []string{
	EnginePodIndexEnvKey,
	EngineAwsEC2MetadataClientEnabledEnvKey,
	EngineCoreModeEnvKey,
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
	// enabled. One volume per key so a future rotation feature can mount
	// more than one at once without a name collision. Mounted at
	// AuthSigningMountPathBase + "/" + <key ID> on the engine container.
	EngineAuthSigningVolumeNamePrefix = "auth-signing-"
	// GatewayConfigVolumeName carries the operator-rendered Envoy
	// config (envoy.yaml). Mounted at /etc/envoy on the Envoy
	// container.
	GatewayConfigVolumeName = "config-volume"
	// GatewayTmpVolumeName is the writable /tmp emptyDir the Envoy
	// container needs alongside ReadOnlyRootFilesystem=true.
	GatewayTmpVolumeName = "tmp"
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
// The signing-key entry is a literal name, not a prefix match: Phase 1
// provisions exactly one, fixed-ID signing key
// (internal/controller.AuthSigningKeyID = "signing-1"), so
// EngineAuthSigningVolumeNamePrefix + "signing-1" is the only signing
// volume that can ever appear on a Phase-1 engine pod. A later
// operator-owned key-rotation phase will mount a dynamic, growing/
// shrinking set of signing-key volumes — at that point this exact-match
// list stops being sufficient and isReservedKey's callers (in
// particular ValidatePodTemplate) will need a prefix check against
// EngineAuthSigningVolumeNamePrefix instead of an enumerated literal.
var operatorOwnedEngineVolumeNames = []string{
	EngineConfigVolumeName,
	EngineDataVolumeName,
	EngineRuntimeVolumeName,
	EngineAuthAdminVolumeName,
	EngineAuthSigningVolumeNamePrefix + "signing-1",
}

// operatorOwnedGatewayVolumeNames are the volume names the operator
// renders on the gateway Deployment's pod template.
var operatorOwnedGatewayVolumeNames = []string{
	GatewayConfigVolumeName,
	GatewayTmpVolumeName,
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
	{Section: "", Keys: []string{"schema_version"}},
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
			// Sidecar with an allowed-sidecars ruleset: pass through.
		}
	}
	return errs
}

// validateInitContainersAgainstRules rejects init containers as a group
// when rules.AllowInitContainers is false. When permitted, an init
// container whose name collides with the primary container is still
// rejected: the operator-rendered primary container would then live
// alongside a same-named init container, and Kubernetes would never
// admit such a pod.
func validateInitContainersAgainstRules(initContainers []corev1.Container, base *field.Path, rules PodTemplateRules) field.ErrorList {
	var errs field.ErrorList
	for i := range initContainers {
		path := base.Index(i)
		if !rules.AllowInitContainers {
			errs = append(errs, field.Forbidden(path,
				fmt.Sprintf("init containers are not allowed on the %s pod template", rules.Component)))
			continue
		}
		if initContainers[i].Name == rules.PrimaryContainerName {
			errs = append(errs, field.Forbidden(path.Child("name"),
				fmt.Sprintf("init container name %q collides with the %s container; pick a different name",
					rules.PrimaryContainerName, rules.Component)))
		} else if slices.Contains(rules.ReservedContainerNames, initContainers[i].Name) {
			errs = append(errs, field.Forbidden(path.Child("name"),
				fmt.Sprintf("init container name %q is reserved by the %s operator; pick a different name",
					initContainers[i].Name, rules.Component)))
		}
	}
	return errs
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
			if !isReservedKey(c.VolumeMounts[mi].Name, rules.ReservedPrimaryVolumeMountNames) {
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
