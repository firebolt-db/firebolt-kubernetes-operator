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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

// EngineState captures the observed cluster state for a FireboltEngine,
// including all generation-scoped resources, pod readiness, and drain status.
type EngineState struct {
	CurrentSTS         *appsv1.StatefulSet
	CurrentConfigMap   *corev1.ConfigMap
	CurrentHeadlessSvc *corev1.Service
	CurrentPodsReady   bool
	CurrentPodCount    int

	DrainingSTS         *appsv1.StatefulSet
	DrainingConfigMap   *corev1.ConfigMap
	DrainingHeadlessSvc *corev1.Service
	DrainingPodsDrained bool

	ClusterService          *corev1.Service
	ClusterServiceTargetGen int

	// ClusterServiceEndpointsReady is true when the cluster service's Endpoints
	// object has at least one ready address. This lags behind a selector update
	// by one endpoints-controller cycle; computeSwitching gates on it to avoid
	// a brief window where the service has no backends.
	ClusterServiceEndpointsReady bool
}

// EngineReconcileResult describes the resources to create, update, or delete
// and the new status to write after reconciling a FireboltEngine.
type EngineReconcileResult struct {
	Status computev1alpha1.FireboltEngineStatus

	EnsureConfigMap   *corev1.ConfigMap
	EnsureHeadlessSvc *corev1.Service
	EnsureStatefulSet *appsv1.StatefulSet
	EnsureClusterSvc  *corev1.Service

	DeleteResources []client.Object

	RequeueAfter time.Duration
	Requeue      bool
}
