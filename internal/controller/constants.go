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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
)

const (
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

	// AnnotationCustomEngineConfigHash records a content-hash of the
	// spec.customEngineConfig payload baked into the engine ConfigMap.
	// stsMatchesSpec compares against this to detect changes that require
	// a new generation, since ConfigMap content drift is not checked
	// independently.
	AnnotationCustomEngineConfigHash = "firebolt.io/custom-engine-config-hash"

	// AnnotationConfigHash carries a content-hash of the rendered config
	// for the gateway and metadata Deployments. It is set on the pod
	// template and serves two purposes: (1) any config change propagates
	// here and triggers a rollout via the resulting template-spec diff,
	// (2) deploymentSpecEqual uses it as the single pod-template equality
	// signal, avoiding a DeepEqual on server-defaulted PodSpec fields.
	AnnotationConfigHash = "firebolt.io/config-hash"

	// AnnotationWakeRequested is the contract between the gateway and the
	// engine autoscaler: the gateway patches this annotation with an
	// RFC 3339 timestamp when it observes a request for an engine that is
	// currently scaled to zero. The engine autoscaler treats a fresh value
	// (within DefaultAutoscalerWakeTTL of now) as a request to immediately
	// scale up to spec.autoscaling.maxReplicas, bypassing the idle-timeout
	// check. Stale values are ignored, so the gateway must keep stamping
	// the annotation while it has buffered queries waiting for the engine.
	//
	// The annotation is honored only when spec.autoscaling.enabled=true:
	// without an autoscaling policy the operator has no MaxReplicas to
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

	// ContainerNameEngine is the container name inside engine StatefulSet pods.
	ContainerNameEngine = "core"

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
	// FireboltCoreServer reads <root>/config.yaml from its --data-dir when present,
	// so the file must land at this path inside the container.
	ConfigMountPath = "/firebolt-core/config.yaml"

	// DataMountPath is where the engine's per-pod PVC is mounted. Matches the
	// path the engine binary uses for its working data.
	DataMountPath = "/firebolt-core/volume"
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

// resolveImagePullPolicy returns the pull policy from the user-supplied
// ImageSpec when set, or IfNotPresent otherwise. The API server defaults
// pullPolicy to IfNotPresent on admission, so this fallback only matters for
// unit tests that build specs directly without going through the API server.
func resolveImagePullPolicy(spec *computev1alpha1.ImageSpec) corev1.PullPolicy {
	if spec != nil && spec.PullPolicy != "" {
		return spec.PullPolicy
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
// `firebolt server` is the unified entrypoint introduced in packdb FB-914;
// FIREBOLT_CORE_MODE=1 (set as a container env var) selects the firebolt-core
// code path so the operator-rendered config (config.yaml at the data-dir root)
// is treated as authoritative and not rewritten at startup.
const EngineStartupScript = `
set -euo pipefail
if [ -z "${POD_INDEX:-}" ]; then
  POD_INDEX="${HOSTNAME##*-}"
fi
exec /firebolt-core/firebolt server --node "$POD_INDEX" --data-dir /firebolt-core
`

// GetServicePorts returns the externally-meaningful service ports for a
// Firebolt engine. The aragog / shufflepuff / storage-manager / storage-
// agent ports are intentionally omitted: they are intra-engine peer
// traffic carried over the headless service's pod-IP DNS records, so they
// don't need a Service port declaration to be reachable, and they are
// never directly consumed by users or the gateway.
func GetServicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "http-query", Port: 3473, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(3473)},
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
		{Name: "http-query", ContainerPort: 3473, Protocol: corev1.ProtocolTCP},
		{Name: "health", ContainerPort: HealthPort, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: MetricsPort, Protocol: corev1.ProtocolTCP},
	}
}
