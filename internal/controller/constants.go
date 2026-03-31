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
)

const (
	// Label keys used by the operator
	LabelEngine     = "firebolt.io/engine"
	LabelGeneration = "firebolt.io/generation"

	// Annotation for tracking rendered manifest content
	AnnotationManifestHash = "firebolt.io/manifest-hash"

	// Suffix constants for resource naming
	SuffixService         = "-service"
	SuffixGen             = "-g"
	SuffixHL              = "-hl"
	SuffixConfig          = "-config"
	SuffixMetadata        = "-metadata"
	SuffixMetadataPG      = "-metadata-pg"
	SuffixMetadataPGCreds = "-metadata-pg-creds"

	// Metadata service (dedicated pensieve) configuration
	MetadataServicePort = 7000
	PostgresPort        = 5432
	PostgresImage       = "postgres:16-alpine"
	PostgresDBName      = "pensieve"
	PostgresUser        = "pensieve"
	PostgresPVCSize     = "1Gi"

	// Container name
	ContainerNameCore = "core"

	// Default drain check interval
	DefaultDrainCheckInterval = 5 * time.Second

	// Core binary path
	CoreBinaryPath = "/firebolt-core/firebolt-core"

	// Health check endpoints
	HealthReadyPath = "/health/ready"
	HealthLivePath  = "/health/live"
	HealthPort      = 8122

	// Config mount path
	ConfigMountPath = "/firebolt-core/config.json"
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

// CoreStartupScript is the script used to start the Core process
const CoreStartupScript = `
NODE_ORDINAL=${HOSTNAME##*-}
exec /firebolt-core/firebolt-core --node $NODE_ORDINAL
`

// GetServicePorts returns the standard service ports for Firebolt Core
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

// GetContainerPorts returns the container ports for the Core container
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
