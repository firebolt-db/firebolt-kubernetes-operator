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

package controller

import (
	"fmt"
	"time"

	dockerref "github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
)

const (
	// OperatorFieldManager is the Server-Side Apply field-manager name
	// stamped on every operator-emitted apply (applySSA). All
	// `ensure*` functions across the engine, instance, gateway, metadata,
	// postgres, and RBAC paths converge on this single identifier so
	// `kubectl get <resource> -o yaml` shows one consistent owner under
	// `metadata.managedFields[].manager` for everything the operator
	// produces. Changing the value is a breaking change: existing
	// resources would be left half-owned by the old manager name until
	// the operator next applies them with ForceOwnership.
	OperatorFieldManager = "firebolt-operator"

	// LabelEngine identifies the engine a resource belongs to.
	LabelEngine = "firebolt.io/engine"
	// LabelGeneration identifies the generation of a resource.
	LabelGeneration = "firebolt.io/generation"
	// LabelInstance identifies the instance a resource belongs to.
	LabelInstance = "firebolt.io/instance"
	// LabelComponent identifies the component type (metadata, gateway, etc.).
	LabelComponent = "firebolt.io/component"

	// AnnotationMetadataOverride records the MetadataEndpointOverride used
	// to build the engine ConfigMap. stsMatchesSpec compares against this to
	// detect changes that require a new generation.
	AnnotationMetadataOverride = "firebolt.io/metadata-override"

	// AnnotationEngineClassHash records a content-hash of the resolved
	// FireboltEngineClass.spec.template merged into the engine pod spec.
	// Absent when spec.engineClassRef is nil. stsMatchesSpec compares the
	// annotation to the freshly resolved class hash and rolls a new
	// blue-green generation on any mismatch — covers both
	// engineClassRef changes and in-place edits to the referenced class.
	AnnotationEngineClassHash = "firebolt.io/engine-class-hash"

	// AnnotationCustomEngineConfigHash records a content-hash of the
	// spec.customEngineConfig payload baked into the engine ConfigMap.
	// stsMatchesSpec compares against this to detect changes that require
	// a new generation, since ConfigMap content drift is not checked
	// independently.
	AnnotationCustomEngineConfigHash = "firebolt.io/custom-engine-config-hash"

	// AnnotationAuthHash records a content-hash of the auth-relevant
	// fields from InstanceInfo.Auth (enabled/disabled, admin Secret
	// name+key, each signing key's ID+SecretName). stsMatchesSpec
	// compares against this to detect auth drift — e.g. auth just got
	// enabled, or the admin/signing Secret a volume points at changed
	// name — because that drift lives in corev1.Volume.VolumeSource
	// (SecretName), a field VolumeMounts equality does not observe: two
	// Volumes named identically but pointing at different Secrets
	// produce byte-identical VolumeMounts.
	AnnotationAuthHash = "firebolt.io/auth-hash"

	// AnnotationEngineTLSHash records a content-hash of InstanceInfo.TLS
	// (enabled/disabled plus the TLS Secret name). Same rationale as
	// AnnotationAuthHash: the Secret-name drift it detects lives in
	// corev1.Volume.VolumeSource, which VolumeMounts equality cannot see.
	AnnotationEngineTLSHash = "firebolt.io/engine-tls-hash"

	// AnnotationConfigHash carries a content-hash of the rendered config
	// for the gateway and metadata Deployments. It is set on the pod
	// template and serves two purposes: (1) any config change propagates
	// here and triggers a rollout via the resulting template-spec diff,
	// (2) deploymentSpecEqual uses it as the single pod-template equality
	// signal, avoiding a DeepEqual on server-defaulted PodSpec fields.
	AnnotationConfigHash = "firebolt.io/config-hash"

	// AnnotationWakeRequested is the contract between the gateway and the
	// engine autoStop: the gateway patches this annotation with an
	// RFC 3339 timestamp when it observes a request for an engine that is
	// currently scaled to zero. The engine autoStop treats a fresh value
	// (within DefaultAutoStopWakeTTL of now) as a request to immediately
	// scale up to spec.autoStop.activeReplicas, bypassing the idle-timeout
	// check. Stale values are ignored, so the gateway must keep stamping
	// the annotation while it has buffered queries waiting for the engine.
	//
	// The annotation is honored only when spec.autoStop.enabled=true:
	// without an autoStop policy the operator has no ActiveReplicas to
	// scale to, and respecting the wake from a non-policy actor would
	// silently override the user's spec.replicas==0 intent.
	AnnotationWakeRequested = "firebolt.io/wake-requested"

	// SuffixService is appended to form the cluster Service name.
	SuffixService = "-service"
	// SuffixGen is appended to form generation-scoped resource names.
	SuffixGen = "-g"
	// SuffixHL is appended to form headless Service names.
	SuffixHL = "-hl"
	// SuffixConfig is appended to form ConfigMap names.
	SuffixConfig = "-config"
	// SuffixMetadataService is appended to form metadata Deployment/Service names.
	SuffixMetadataService = "-metadata"
	// SuffixMetadataPG is appended to form the internal PostgreSQL resource names.
	SuffixMetadataPG = "-metadata-pg"
	// SuffixMetadataPostgresCreds is appended to form the PG credentials Secret name.
	SuffixMetadataPostgresCreds = "-metadata-postgres-creds" //nolint:gosec // resource name suffix, not a credential
	// SuffixGateway is appended to form gateway Deployment/Service names.
	SuffixGateway = "-gateway"
	// SuffixAuthSigning is appended to form the name of the cert-manager
	// Certificate (and its target Secret) holding the JWT signing
	// keypair every engine in the Instance shares.
	SuffixAuthSigning = "-auth-signing"
	// SuffixEngineTLS is appended to form the name of the cert-manager
	// Certificate (and its target Secret) holding the TLS server
	// certificate every engine in the Instance shares.
	SuffixEngineTLS = "-engine-tls"
	// SuffixGatewayTLS is appended to form the name of the cert-manager
	// Certificate (and its target Secret) holding the gateway's
	// downstream (client-facing) TLS server certificate.
	SuffixGatewayTLS = "-gateway-tls"
	// SuffixEngineCABundle is appended to form the name of the
	// operator-owned Secret holding the concatenated engine trust bundle the
	// gateway mounts as trusted_ca: the union of every CA currently signing a
	// live engine generation's certificate (plus the anchor). Unlike the other
	// suffixed resources this is NOT a cert-manager Certificate — the operator
	// assembles and writes it directly (see ensureEngineCABundle, FB-896 #4).
	SuffixEngineCABundle = "-engine-ca-bundle"

	// MetadataServicePort is the gRPC port the metadata service listens on.
	MetadataServicePort = 7000
	// PostgresPort is the default PostgreSQL port.
	PostgresPort = 5432
	// PostgresDBName is the database name for the internal PostgreSQL instance.
	PostgresDBName = "firebolt_metadata"
	// PostgresDefaultSchema is the schema used by the metadata service when
	// the user does not specify spec.metadata.postgres.schema (and always for
	// the internal Postgres deployment, which is created with this schema).
	PostgresDefaultSchema = "public"
	// PostgresUser is the database user for the internal PostgreSQL instance.
	PostgresUser = "firebolt"
	// PostgresPVCSize is the default PVC size for the internal PostgreSQL instance.
	PostgresPVCSize = "10Gi"
	// PostgresUID is the numeric UID/GID of the built-in `postgres` user in
	// the `postgres:16-alpine` image. We pin RunAsUser/RunAsGroup/FSGroup to
	// this value so the pod-level SecurityContext can enforce RunAsNonRoot=true
	// and the kubelet chowns the per-pod data PVC to a UID the postgres
	// process actually runs as. The Debian-flavored `postgres` images use
	// UID 999 instead; if PostgresImage is ever switched off Alpine this
	// constant must be revisited.
	PostgresUID int64 = 70

	// MetadataUID is the numeric UID/GID of the built-in `dedicated-pensieve`
	// user in the metadata image. The Dockerfile creates the user with this
	// fixed UID and sets `USER dedicated-pensieve`, so pinning
	// RunAsUser/RunAsGroup here just locks in the image's own default and
	// lets the pod-level SecurityContext assert RunAsNonRoot=true. Revisit
	// if the metadata image's user is ever renumbered.
	MetadataUID int64 = 1111

	// DefaultDrainCheckInterval is how often the operator polls draining pods.
	DefaultDrainCheckInterval = 5 * time.Second

	// HealthReadyPath is the HTTP path for readiness probes on engine pods.
	HealthReadyPath = "/health/ready"
	// HealthLivePath is the HTTP path for liveness probes on engine pods.
	HealthLivePath = "/health/live"
	// HealthPort is the port exposing health endpoints on engine pods.
	HealthPort = 8122
	// MetricsPort is the port exposing Prometheus metrics on engine pods.
	// The operator scrapes firebolt_running_queries and firebolt_suspended_queries
	// here via the Kubernetes pod-proxy subresource to drive the drain check.
	MetricsPort = 9090
	// MetricsPath is the HTTP path exposing Prometheus metrics on engine pods.
	MetricsPath = "/metrics"
	// MetricRunningQueries is the Prometheus metric name for in-flight queries.
	MetricRunningQueries = "firebolt_running_queries"
	// MetricSuspendedQueries is the Prometheus metric name for suspended queries.
	MetricSuspendedQueries = "firebolt_suspended_queries"

	// ConfigMountPath is where the engine config.yaml is mounted in the container.
	// With --server-config unset the engine falls back to <data-dir>/config.yaml,
	// so the file must land at DataMountPath/config.yaml inside the container.
	ConfigMountPath = "/var/lib/firebolt/config.yaml"

	// DataMountPath is where the engine's per-pod data volume is mounted. Matches
	// the --data-dir passed to the engine binary: the writable state root that
	// holds persistent_data, diagnostic_data, and scratch.
	DataMountPath = "/var/lib/firebolt"

	// AuthSigningMountPathBase is the directory under which each
	// provisioned JWT signing key is mounted, one subdirectory per key ID
	// (AuthSigningMountPathBase + "/" + kid + "/tls.key"), so
	// instance.auth.local.signing_keys[].private_key_path can address
	// any one of them individually — needed once a future rotation
	// feature mounts more than one key at a time.
	AuthSigningMountPathBase = "/secrets/auth/signing"
	// AuthAdminMountPath is the directory the admin password Secret is
	// mounted at. The configured spec.auth.local.admin.password.key
	// names the file within it.
	AuthAdminMountPath = "/secrets/auth/admin"
	// EngineTLSMountPath is the directory the engine-listener TLS
	// Secret (tls.crt/tls.key/ca.crt) is mounted at (read-only). Must agree
	// with engineTLSKeyPath's mount-path scheme and with EngineStartupScript's
	// bundle-assembly paths.
	EngineTLSMountPath = "/secrets/tls/engine"
	// EngineTLSBundlePath is the writable path (under the runtime emptyDir
	// mounted at /run/firebolt) where EngineStartupScript assembles the
	// leaf+CA bundle packdb reads as its listener certificate_file — see
	// engineTLSCertPath and FB-896 #2. It must NOT live under
	// EngineTLSMountPath, which is a read-only Secret mount.
	EngineTLSBundlePath = "/run/firebolt/engine-tls-bundle.crt"
	// DataVolumeName is the name of the data volume inside the StatefulSet's
	// VolumeClaimTemplates and the corresponding container VolumeMount.
	DataVolumeName = "data"
	// DefaultEngineStorageSize is the PVC size applied when
	// FireboltEngineSpec.Storage.Size is unset. Mirrors the kubebuilder default
	// so unit tests building specs as Go literals see the same value as
	// CRD-loaded specs.
	DefaultEngineStorageSize = "1Gi"

	// DefaultTerminationGracePeriodSeconds is the default value applied to the
	// engine pod's terminationGracePeriodSeconds when FireboltEngine.spec leaves
	// it unset. 60s gives in-flight queries up to 55s (TGPS − EngineShutdownMarginSeconds)
	// to complete after SIGTERM before SIGKILL is sent.
	DefaultTerminationGracePeriodSeconds = 60
	// DefaultEngineFSGroup is the pod-level fsGroup applied to engine pods
	// when spec.podSecurityContext.fsGroup is unset. Matches the engine
	// container's listening port (3473) as a memorable shared GID; the
	// kernel uses it to chown the per-pod data PVC so the (non-root) engine
	// process can read and write its own volume.
	DefaultEngineFSGroup int64 = 3473
	// DefaultEngineWebD is the non-root UID the engine container runs
	// under when FireboltEngine.spec.securityContext does not override
	// it. 3473 matches DefaultEngineFSGroup so the process's primary
	// GID, the volume-mount fsGroup, and the port mnemonic all line up
	// — and matches the sibling firebolt-instance-helm chart's engine
	// StatefulSet, so engines moved between deployment paths keep their
	// on-disk ownership.
	DefaultEngineWebD int64 = 3473
	// DefaultEngineGID is the non-root GID the engine container runs
	// under. Pinned to the same value as DefaultEngineWebD and
	// DefaultEngineFSGroup; see DefaultEngineWebD.
	DefaultEngineGID int64 = 3473
	// EngineShutdownMarginSeconds is subtracted from terminationGracePeriodSeconds
	// to compute shutdown_wait_unfinished. The remaining margin covers container
	// runtime teardown and pod API deletion after the engine process exits.
	EngineShutdownMarginSeconds = 5
)

// Default container images, sourced from the variant-specific
// config/images/defaults.<variant>.env file embedded by the images package.
//
// The Default*Image strings are the convenience "repository:tag" form used
// when comparing or stamping a fully-resolved reference; the matching
// Default*Repository / Default*Tag pairs are exposed separately so the
// resolveImageRef helper can fall back to either half independently when a
// user supplies a partial ImageSpec override.
var (
	PostgresImage             = images.PostgresImage
	DefaultMetadataRepository = images.MetadataImage
	DefaultMetadataTag        = images.MetadataTag
	DefaultEnvoyRepository    = images.EnvoyImage
	DefaultEnvoyTag           = images.EnvoyTag
	DefaultEngineImage        = images.DefaultEngine()
	DefaultEngineRepository   = images.EngineImage
	DefaultEngineTag          = images.EngineTag
	// DefaultEngineWebImage is the "repository:tag" reference for the optional
	// engine web UI sidecar injected when an engine/class sets uiSidecar: true.
	DefaultEngineWebImage = images.DefaultEngineWeb()
)

// resolveImageRef returns "repository:tag" for a component, using fields from
// the user-supplied ImageSpec when present and falling back to the supplied
// component defaults otherwise. A nil spec, an empty repository, or an empty
// tag each independently fall back to the corresponding default, so users may
// override only the repository (to pull from a mirror) or only the tag (to
// pin a version) without restating the other half.
func resolveImageRef(spec *computev1alpha1.ImageSpec, defaultRepo, defaultTag string) string {
	repo := defaultRepo
	tag := defaultTag
	if spec != nil {
		if spec.Repository != "" {
			repo = spec.Repository
		}
		if spec.Tag != "" {
			tag = spec.Tag
		}
	}
	return repo + ":" + tag
}

// resolveWorkloadImagePullPolicy returns the default pull policy for the
// engine and metadata images: the Kubernetes tag-based rule (see
// resolveContainerImagePullPolicy), with the "dev" tag additionally treated
// like ":latest". The engine and metadata GHCR packages publish "dev" as a
// mutable alias tracking the development branch, so caching it under
// IfNotPresent would pin every node to whatever build it pulled first.
func resolveWorkloadImagePullPolicy(image string) corev1.PullPolicy {
	if containerImageDefaultTag(image) == "dev" {
		return corev1.PullAlways
	}
	return resolveContainerImagePullPolicy(image, "")
}

// containerImageDefaultTag returns the effective tag used for pull-policy
// defaulting, mirroring k8s.io/kubernetes/pkg/util/parsers.ParseImageName.
func containerImageDefaultTag(image string) string {
	named, err := dockerref.ParseNormalizedNamed(image)
	if err != nil {
		return ""
	}
	var tag string
	if tagged, ok := named.(dockerref.Tagged); ok {
		tag = tagged.Tag()
	}
	var digest string
	if digested, ok := named.(dockerref.Digested); ok {
		digest = digested.Digest().String()
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return tag
}

// resolveContainerImagePullPolicy returns the effective pull policy for a
// container image, mirroring the API server's defaulting rules when policy
// is unset.
func resolveContainerImagePullPolicy(image string, policy corev1.PullPolicy) corev1.PullPolicy {
	if policy != "" {
		return policy
	}
	// SetDefaults_Container ignores ParseImageName errors and assumes validation
	// happened elsewhere; an empty tag therefore defaults to IfNotPresent.
	if containerImageDefaultTag(image) == "latest" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

// EngineStartupScript is the script used to start the engine process.
// POD_INDEX is injected via the downward API in buildStatefulSet, sourced
// from the apps.kubernetes.io/pod-index label that the StatefulSet
// controller sets on each pod (GA in Kubernetes 1.28). On older clusters
// the label is absent and POD_INDEX will be empty; in that case we fall
// back to extracting the ordinal from HOSTNAME (<sts-name>-<ordinal>).
//
// `firebolt server` is the engine image's unified entrypoint; the binary lives
// at /opt/firebolt/firebolt (read-only payload) and the writable state root is
// passed as --data-dir /var/lib/firebolt.
// FIREBOLT_CORE_MODE=1 (set as a container env var) selects the firebolt-core
// code path so the operator-rendered config (config.yaml at the data-dir root)
// is treated as authoritative and not rewritten at startup.
const EngineStartupScript = `
set -euo pipefail
if [ -z "${POD_INDEX:-}" ]; then
  POD_INDEX="${HOSTNAME##*-}"
fi
# Engine TLS (FB-896 #2): packdb uses the listener certificate_file as BOTH the
# served chain and its own client-side CA bundle for HTTPS startup checks, so it
# must carry the leaf AND the issuing CA. A CA-backed cert-manager issuer splits
# these across tls.crt (leaf) and ca.crt (issuer), so assemble a combined bundle
# at a writable path (paths must match EngineTLSMountPath / EngineTLSBundlePath).
# Guarded on file existence so a plaintext engine, which mounts neither, is
# unaffected.
if [ -f /secrets/tls/engine/tls.crt ] && [ -f /secrets/tls/engine/ca.crt ]; then
  cat /secrets/tls/engine/tls.crt /secrets/tls/engine/ca.crt > /run/firebolt/engine-tls-bundle.crt
fi
exec /opt/firebolt/firebolt server --node "$POD_INDEX" --data-dir /var/lib/firebolt
`

// EngineHTTPQueryPort is packdb's http-query listener port (confirmed
// against packdb's own default, programs/firebolt-core/FireboltCoreConfig.h
// kHttpPort). Unauthenticated/plaintext today; once spec.tls.engine is
// enabled this is the same port renumbered to TLS-only (packdb's
// ApplyToLegacyConfig replaces the plaintext http_port default entirely
// the instant any endpoints.http.listeners entry is rendered — see
// buildConfigMap's renderEngineTLSListener). Extracted as a named
// constant (rather than the three separate inline "3473" literals this
// replaces) because a fourth call site — the TLS listener render — would
// otherwise need to restate it; GetServicePorts's doc comment already
// flagged the lack of a compile-time link to instance_gateway.go's Lua
// :authority rewrite, which still hardcodes "3473" as its own literal.
const EngineHTTPQueryPort int32 = 3473

// GetServicePorts returns the externally-meaningful service ports for a
// Firebolt engine. The aragog / shufflepuff / storage-manager / storage-
// agent ports are intentionally omitted: they are intra-engine peer
// traffic carried over the headless service's pod-IP DNS records, so they
// don't need a Service port declaration to be reachable, and they are
// never directly consumed by users or the gateway.
func GetServicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "http-query", Port: EngineHTTPQueryPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt32(EngineHTTPQueryPort)},
		{Name: "health", Port: HealthPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(HealthPort)},
		{Name: "metrics", Port: MetricsPort, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(MetricsPort)},
	}
}

// GetContainerPorts returns the container ports for the engine container.
// Mirrors GetServicePorts (see its docstring): intra-engine peer ports are
// omitted because ContainerPort declarations are informational only and
// the engine binary opens those listeners regardless.
func GetContainerPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{Name: "http-query", ContainerPort: EngineHTTPQueryPort, Protocol: corev1.ProtocolTCP},
		{Name: "health", ContainerPort: HealthPort, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP},
	}
}

// Engine Web UI sidecar constants. The sidecar is injected only when an engine or
// its class opts in via uiSidecar: true; see buildEngineWebSidecar. Its
// container name is the operator-owned EngineWebContainerName (api/v1alpha1),
// which the validating webhook reserves so a user container cannot collide.
const (
	// EngineWebPortName / EngineWebPort are the name and number of the container
	// port the engine web UI listens on.
	EngineWebPortName       = "web-ui"
	EngineWebPort     int32 = 9100
	// EngineWebWritableVolumeName is the emptyDir mounted into the UI container
	// at nginx's writable paths, since it runs with a read-only root FS. It is
	// listed in operatorOwnedPodVolumeNames so a user volume of this name is
	// dropped at render and cannot collide with the operator's.
	EngineWebWritableVolumeName = "nginx-writable-dir"
)

// engineWebBackendURL returns the local engine endpoint the web UI talks
// to: the engine's http-query port (EngineHTTPQueryPort) on loopback
// within the same pod. Scheme depends on tlsEnabled — see
// buildEngineWebSidecar's doc comment for why this must track
// renderEndpointsConfig's condition exactly.
func engineWebBackendURL(tlsEnabled bool) string {
	scheme := "http"
	if tlsEnabled {
		scheme = "https"
	}
	return fmt.Sprintf("%s://localhost:%d", scheme, EngineHTTPQueryPort)
}
