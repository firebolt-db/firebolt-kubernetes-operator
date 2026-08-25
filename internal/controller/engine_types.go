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

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// EngineState captures the observed cluster state for a FireboltEngine,
// including all generation-scoped resources, pod readiness, and drain status.
type EngineState struct {
	CurrentSTS         *appsv1.StatefulSet
	CurrentConfigMap   *corev1.ConfigMap
	CurrentHeadlessSvc *corev1.Service
	CurrentPodsReady   bool
	// CurrentPodTotal is the number of pods that currently exist for the
	// current generation (including non-running and non-ready ones). It can
	// be less than spec.replicas if the StatefulSet has not finished
	// creating pods yet.
	CurrentPodTotal int
	// CurrentPodReady is the subset of CurrentPodTotal that is in
	// PodRunning phase with PodReady=True. Always <= CurrentPodTotal.
	CurrentPodReady int

	// ActivePodReady is the ready-pod count of status.ActiveGeneration — the
	// generation the cluster Service selects, hence the one serving traffic.
	// It equals CurrentPodReady outside a rollout, when the current and active
	// generations are the same. While a new generation comes up the two differ,
	// and this field stays with the outgoing generation until the cutover, so it
	// never reports capacity that is not reachable yet. Zero when no generation
	// is active (first creation) or when the active generation's StatefulSet is
	// absent.
	ActivePodReady int

	DrainingSTS         *appsv1.StatefulSet
	DrainingConfigMap   *corev1.ConfigMap
	DrainingHeadlessSvc *corev1.Service
	DrainingPodsDrained bool

	ClusterService          *corev1.Service
	ClusterServiceTargetGen int
}

// EngineReconcileResult describes the resources to create, update, or delete
// and the new status to write after reconciling a FireboltEngine.
type EngineReconcileResult struct {
	Status computev1alpha1.FireboltEngineStatus

	EnsureConfigMap   *corev1.ConfigMap
	EnsureHeadlessSvc *corev1.Service
	EnsureStatefulSet *appsv1.StatefulSet
	EnsureClusterSvc  *corev1.Service

	// EnsureEngineTLSCert is the per-generation engine TLS server Certificate,
	// set only when engine TLS is enabled. cert-manager issues its
	// Secret, which the generation's StatefulSet mounts and serves. nil when
	// engine TLS is disabled.
	EnsureEngineTLSCert *certmanagerv1.Certificate

	DeleteResources []client.Object

	RequeueAfter time.Duration
	Requeue      bool
}
