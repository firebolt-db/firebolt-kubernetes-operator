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
)

// RolloutStrategy defines how transitions between generations are handled
type RolloutStrategy string

const (
	// RolloutGraceful waits for pods to drain before deleting the old generation
	RolloutGraceful RolloutStrategy = "graceful"
	// RolloutRecreate immediately deletes the old generation without waiting for drain
	RolloutRecreate RolloutStrategy = "recreate"
)

// ClusterConfig represents the user-provided configuration from a ConfigMap
type ClusterConfig struct {
	Replicas           int32               `json:"replicas"`
	Image              string              `json:"image"`
	Tag                string              `json:"tag"`
	ImagePullPolicy    corev1.PullPolicy   `json:"imagePullPolicy,omitempty"`
	CPU                string              `json:"cpu"`
	Memory             string              `json:"memory"`
	DrainCheckInterval time.Duration       `json:"drainCheckInterval"`
	NodeSelector       map[string]string   `json:"nodeSelector,omitempty"`
	Tolerations        []corev1.Toleration `json:"tolerations,omitempty"`
	Rollout            RolloutStrategy     `json:"rollout,omitempty"`
}

// ClusterPhase represents the current phase of the cluster transition
type ClusterPhase string

const (
	PhaseStable    ClusterPhase = "stable"
	PhaseCreating  ClusterPhase = "creating"
	PhaseSwitching ClusterPhase = "switching"
	PhaseDraining  ClusterPhase = "draining"
	PhaseCleaning  ClusterPhase = "cleaning"
)

// ClusterStatus represents the operator-managed state stored in the status ConfigMap
type ClusterStatus struct {
	CurrentGeneration  int            `json:"currentGeneration"`
	ActiveGeneration   int            `json:"activeGeneration"`
	DrainingGeneration *int           `json:"drainingGeneration"`
	Phase              ClusterPhase   `json:"phase"`
	LastReconciled     time.Time      `json:"lastReconciled"`
	PendingMutation    *ClusterConfig `json:"pendingMutation,omitempty"`
	// LastAppliedConfig stores the config that was used to create the active generation
	LastAppliedConfig *ClusterConfig `json:"lastAppliedConfig,omitempty"`
}

// DrainCheckError represents an error in the fb command response
type DrainCheckError struct {
	Description string `json:"description"`
}

// DrainCheckResponse represents the JSON response from the fb command
type DrainCheckResponse struct {
	Data   [][]string        `json:"data,omitempty"`
	Rows   int               `json:"rows,omitempty"`
	Errors []DrainCheckError `json:"errors,omitempty"`
}
