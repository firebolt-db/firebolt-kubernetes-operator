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

package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/firebolt-analytics/firebolt-kubernetes-operator/config/images"
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

	// ConfigMountPath is where the engine config.json is mounted in the container.
	ConfigMountPath = "/firebolt-core/config.json"
)

// Default container images, sourced from config/images/defaults.env.
var (
	PostgresImage        = images.PostgresImage
	DefaultMetadataImage = images.DefaultMetadata()
	DefaultEnvoyImage    = images.DefaultEnvoy()
)

// DrainCheckSQL is the SQL query used to check if a node has finished serving queries.
// Returns "0" if drained (no running queries), "1" if still serving queries.
const DrainCheckSQL = `SELECT count(*) as num_running_queries FROM (
  SELECT is_initial_query,
    coalesce(settings_values[index_of(settings_names, 'auto_start_stop_control')], 'ignore') == 'reset' as auto_start_stop_control_reset,
    coalesce(settings_values[index_of(settings_names, 'force_auto_stop_reset')], 'false') == 'true' as force_auto_stop_reset,
    coalesce(settings_values[index_of(settings_names, 'hidden_query')],'false') == 'true' as hidden_query,
    coalesce(settings_values[index_of(settings_names, 'is_internal_query')],'false') == 'true' as is_internal_query
  FROM account_db.information_schema.internal_running_queries
  WHERE is_initial_query
    AND coalesce(settings_values[index_of(settings_names, 'auto_start_stop_control')], 'ignore') == 'reset'
    AND ((coalesce(settings_values[index_of(settings_names, 'hidden_query')],'false') != 'true'
          AND coalesce(settings_values[index_of(settings_names, 'is_internal_query')],'false') != 'true')
         OR coalesce(settings_values[index_of(settings_names, 'force_auto_stop_reset')], 'false') == 'true')
  LIMIT 1
)`

// EngineStartupScript is the script used to start the engine process
const EngineStartupScript = `
NODE_ORDINAL=${HOSTNAME##*-}
exec /firebolt-core/firebolt-core --node $NODE_ORDINAL
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
