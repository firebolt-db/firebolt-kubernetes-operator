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

	// AnnotationConfigHash carries a content-hash of the rendered config
	// for the gateway and metadata Deployments. It is set on the pod
	// template and serves two purposes: (1) any config change propagates
	// here and triggers a rollout via the resulting template-spec diff,
	// (2) deploymentSpecEqual uses it as the single pod-template equality
	// signal, avoiding a DeepEqual on server-defaulted PodSpec fields.
	AnnotationConfigHash = "firebolt.io/config-hash"

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
	// PostgresUser is the database user for the internal PostgreSQL instance.
	PostgresUser = "firebolt"
	// PostgresPVCSize is the default PVC size for the internal PostgreSQL instance.
	PostgresPVCSize = "10Gi"

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

	// ConfigMountPath is where the engine config.json is mounted in the container.
	ConfigMountPath = "/firebolt-core/config.json"

	// DefaultTerminationGracePeriodSeconds is the default value applied to the
	// engine pod's terminationGracePeriodSeconds when FireboltEngine.spec leaves
	// it unset. 60s gives in-flight queries up to 55s (TGPS − EngineShutdownMarginSeconds)
	// to complete after SIGTERM before SIGKILL is sent.
	DefaultTerminationGracePeriodSeconds = 60
	// EngineShutdownMarginSeconds is subtracted from terminationGracePeriodSeconds
	// to compute shutdown_wait_unfinished. The remaining margin covers container
	// runtime teardown and pod API deletion after the engine process exits.
	EngineShutdownMarginSeconds = 5
)

// Default container images, sourced from config/images/defaults.env.
var (
	PostgresImage        = images.PostgresImage
	DefaultMetadataImage = images.DefaultMetadata()
	DefaultEnvoyImage    = images.DefaultEnvoy()
	DefaultEngineImage   = images.DefaultEngine()
)

// EngineStartupScript is the script used to start the engine process.
// POD_INDEX is injected via the downward API in buildStatefulSet, sourced
// from the apps.kubernetes.io/pod-index label that the StatefulSet
// controller sets on each pod (GA in Kubernetes 1.28). On older clusters
// the label is absent and POD_INDEX will be empty; in that case we fall
// back to extracting the ordinal from HOSTNAME (<sts-name>-<ordinal>).
const EngineStartupScript = `
set -euo pipefail
if [ -z "${POD_INDEX:-}" ]; then
  POD_INDEX="${HOSTNAME##*-}"
fi
exec /firebolt-core/firebolt-core --node "$POD_INDEX"
`

// GetServicePorts returns the standard service ports for a Firebolt engine
func GetServicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{Name: "http-query", Port: 3473, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(3473)},
		{Name: "health", Port: 8122, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(8122)},
		{Name: "execp", Port: 5678, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(5678)},
		{Name: "datacp", Port: 16000, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(16000)},
		{Name: "storage-manager", Port: 1717, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(1717)},
		{Name: "storage-agent", Port: 3434, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(3434)},
		{Name: "metadata", Port: 6500, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(6500)},
		{Name: "metrics", Port: 9090, Protocol: corev1.ProtocolTCP, TargetPort: intstr.FromInt(9090)},
	}
}

// GetContainerPorts returns the container ports for the engine container
func GetContainerPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{Name: "http-query", ContainerPort: 3473, Protocol: corev1.ProtocolTCP},
		{Name: "health", ContainerPort: 8122, Protocol: corev1.ProtocolTCP},
		{Name: "execp", ContainerPort: 5678, Protocol: corev1.ProtocolTCP},
		{Name: "datacp", ContainerPort: 16000, Protocol: corev1.ProtocolTCP},
		{Name: "storage-manager", ContainerPort: 1717, Protocol: corev1.ProtocolTCP},
		{Name: "storage-agent", ContainerPort: 3434, Protocol: corev1.ProtocolTCP},
		{Name: "metadata", ContainerPort: 6500, Protocol: corev1.ProtocolTCP},
		{Name: "metrics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
	}
}
