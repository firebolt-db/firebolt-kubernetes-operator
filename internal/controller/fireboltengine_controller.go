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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/core-operator/api/v1alpha1"
)

// FireboltEngineReconciler reconciles FireboltEngine objects
type FireboltEngineReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config
	Clientset  *kubernetes.Clientset
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create

// Reconcile handles changes to FireboltEngine resources
func (r *FireboltEngineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Get the FireboltEngine resource
	engine := &computev1alpha1.FireboltEngine{}
	if err := r.Get(ctx, req.NamespacedName, engine); err != nil {
		if errors.IsNotFound(err) {
			log.Info("FireboltEngine deleted, skipping reconciliation")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get FireboltEngine")
		return ctrl.Result{}, err
	}

	engineName := engine.Name
	log = log.WithValues("engine", engineName)
	log.Info("Reconciling engine")

	spec := &engine.Spec
	status := &engine.Status

	// Ensure per-engine metadata service resources exist (PostgreSQL + dedicated pensieve)
	if err := r.ensureMetadataService(ctx, engine, spec); err != nil {
		log.Error(err, "Failed to ensure metadata service")
		return ctrl.Result{}, err
	}

	// Wait until the metadata service Deployment has at least one ready replica
	// before attempting gRPC calls against it.
	if status.AccountID == "" {
		ready, err := r.isMetadataServiceReady(ctx, engine)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			log.Info("Metadata service not ready yet, requeueing")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		accountID, err := r.ensureAccountInitialized(ctx, engine)
		if err != nil {
			log.Error(err, "Failed to ensure metadata service account")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		status.AccountID = accountID
		if err := r.updateStatus(ctx, engine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize status on first reconcile
	if status.Phase == "" {
		status.Phase = computev1alpha1.PhaseCreating
		status.ActiveGeneration = -1
		status.LastAppliedConfig = spec.DeepCopy()
		if err := r.updateStatus(ctx, engine); err != nil {
			log.Error(err, "Failed to initialize status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if we need to reconcile
	needsReconcile := r.needsReconcile(spec, status)

	if !needsReconcile && status.Phase == computev1alpha1.PhaseStable {
		log.V(1).Info("No changes detected, engine is stable")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Handle reconciliation based on current phase
	result, err := r.reconcilePhase(ctx, engine)
	if err != nil {
		log.Error(err, "Failed to reconcile phase", "phase", status.Phase)
		return ctrl.Result{}, err
	}

	return result, nil
}

// specsEqual checks if two specs are equal (all configurable fields)
func specsEqual(a, b *computev1alpha1.FireboltEngineSpec) bool {
	if a == nil || b == nil {
		return a == b
	}

	if a.Replicas != b.Replicas ||
		a.Image.Repository != b.Image.Repository ||
		a.Image.Tag != b.Image.Tag ||
		a.Image.PullPolicy != b.Image.PullPolicy ||
		a.Resources.CPU.Cmp(b.Resources.CPU) != 0 ||
		a.Resources.Memory.Cmp(b.Resources.Memory) != 0 ||
		a.Rollout != b.Rollout {
		return false
	}

	// Compare drain check intervals
	aDrain := getDrainCheckInterval(a)
	bDrain := getDrainCheckInterval(b)
	if aDrain != bDrain {
		return false
	}

	// Compare NodeSelector maps
	if len(a.NodeSelector) != len(b.NodeSelector) {
		return false
	}
	for k, v := range a.NodeSelector {
		if bv, ok := b.NodeSelector[k]; !ok || v != bv {
			return false
		}
	}

	// Compare Tolerations slices
	if len(a.Tolerations) != len(b.Tolerations) {
		return false
	}
	for i := range a.Tolerations {
		if !tolerationEqual(a.Tolerations[i], b.Tolerations[i]) {
			return false
		}
	}

	if !metadataServiceEqual(a.MetadataService, b.MetadataService) {
		return false
	}

	return true
}

func getDrainCheckInterval(spec *computev1alpha1.FireboltEngineSpec) time.Duration {
	if spec.DrainCheckInterval != nil {
		return spec.DrainCheckInterval.Duration
	}
	return DefaultDrainCheckInterval
}

func servicePortsNeedUpdate(existing, desired []corev1.ServicePort) bool {
	if len(existing) != len(desired) {
		return true
	}
	for i := range desired {
		if existing[i].Name != desired[i].Name ||
			existing[i].Port != desired[i].Port ||
			existing[i].Protocol != desired[i].Protocol ||
			existing[i].TargetPort.IntValue() != desired[i].TargetPort.IntValue() {
			return true
		}
	}
	return false
}

func metadataServiceEqual(a, b *computev1alpha1.MetadataServiceSpec) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return imageSpecEqual(a.Image, b.Image)
}

func imageSpecEqual(a, b *computev1alpha1.ImageSpec) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Repository == b.Repository && a.Tag == b.Tag && a.PullPolicy == b.PullPolicy
}

func tolerationEqual(a, b corev1.Toleration) bool {
	if a.Key != b.Key ||
		a.Operator != b.Operator ||
		a.Value != b.Value ||
		a.Effect != b.Effect {
		return false
	}
	if a.TolerationSeconds == nil && b.TolerationSeconds == nil {
		return true
	}
	if a.TolerationSeconds == nil || b.TolerationSeconds == nil {
		return false
	}
	return *a.TolerationSeconds == *b.TolerationSeconds
}

// needsReconcile checks if the spec has changed compared to the last applied config
func (r *FireboltEngineReconciler) needsReconcile(spec *computev1alpha1.FireboltEngineSpec, status *computev1alpha1.FireboltEngineStatus) bool {
	if status.Phase != computev1alpha1.PhaseStable {
		return true
	}
	if status.LastAppliedConfig == nil {
		return true
	}
	if status.PendingMutation != nil {
		return true
	}
	return !specsEqual(spec, status.LastAppliedConfig)
}

// updateStatus writes the status subresource
func (r *FireboltEngineReconciler) updateStatus(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	now := metav1.Now()
	engine.Status.LastReconciled = &now
	return r.Status().Update(ctx, engine)
}

// reconcilePhase handles reconciliation based on the current phase
func (r *FireboltEngineReconciler) reconcilePhase(ctx context.Context, engine *computev1alpha1.FireboltEngine) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	spec := &engine.Spec
	status := &engine.Status

	switch status.Phase {
	case computev1alpha1.PhaseStable:
		targetSpec := spec.DeepCopy()
		if status.PendingMutation != nil {
			log.Info("Applying pending mutation")
			targetSpec = status.PendingMutation.DeepCopy()
			status.PendingMutation = nil
		}

		if status.ActiveGeneration == -1 {
			log.Info("Initial engine setup, creating generation 0")
		} else {
			log.Info("Configuration change detected, starting new generation",
				"currentGeneration", status.CurrentGeneration,
				"newGeneration", status.CurrentGeneration+1)
			status.CurrentGeneration++
		}

		status.LastAppliedConfig = targetSpec
		status.Phase = computev1alpha1.PhaseCreating

		if err := r.updateStatus(ctx, engine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case computev1alpha1.PhaseCreating, computev1alpha1.PhaseSwitching, computev1alpha1.PhaseDraining, computev1alpha1.PhaseCleaning:
		// During transitions, queue config changes as pending mutations
		if status.LastAppliedConfig != nil && !specsEqual(spec, status.LastAppliedConfig) {
			if status.PendingMutation == nil || !specsEqual(spec, status.PendingMutation) {
				log.Info("New config change detected during transition, saving as pending mutation",
					"phase", status.Phase)
				status.PendingMutation = spec.DeepCopy()
				if err := r.updateStatus(ctx, engine); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		activeSpec := status.LastAppliedConfig
		if activeSpec == nil {
			activeSpec = spec.DeepCopy()
		}

		switch status.Phase {
		case computev1alpha1.PhaseCreating:
			return r.reconcileCreating(ctx, engine, activeSpec)
		case computev1alpha1.PhaseSwitching:
			return r.reconcileSwitching(ctx, engine, activeSpec)
		case computev1alpha1.PhaseDraining:
			return r.reconcileDraining(ctx, engine, activeSpec)
		case computev1alpha1.PhaseCleaning:
			return r.reconcileCleaning(ctx, engine, activeSpec)
		}
	}

	log.Error(nil, "Unknown phase", "phase", status.Phase)
	return ctrl.Result{}, fmt.Errorf("unknown phase: %s", status.Phase)
}

func (r *FireboltEngineReconciler) reconcileCreating(ctx context.Context, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	engineName := engine.Name
	status := &engine.Status
	gen := status.CurrentGeneration

	log.Info("Creating resources for generation", "generation", gen)

	if err := r.ensureCoreConfigMap(ctx, engineName, engine, spec, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create nodes ConfigMap: %w", err)
	}
	MaybeCrash(engineName, CrashAfterCoreConfigMapCreated)

	if err := r.ensureHeadlessService(ctx, engineName, engine, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create headless service: %w", err)
	}
	MaybeCrash(engineName, CrashAfterHeadlessServiceCreated)

	if err := r.ensureStatefulSet(ctx, engineName, engine, spec, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create StatefulSet: %w", err)
	}
	MaybeCrash(engineName, CrashAfterStatefulSetCreated)

	if err := r.ensureClusterService(ctx, engineName, engine, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create cluster service: %w", err)
	}
	MaybeCrash(engineName, CrashAfterClusterServiceEnsured)

	ready, err := r.arePodsReady(ctx, engine, gen, int(spec.Replicas))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check pod readiness: %w", err)
	}

	if !ready {
		log.Info("Waiting for pods to become ready", "generation", gen)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	log.Info("All pods ready, moving to switching phase", "generation", gen)
	status.Phase = computev1alpha1.PhaseSwitching
	MaybeCrash(engineName, CrashBeforeCreatingToSwitching)
	if err := r.updateStatus(ctx, engine); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

func (r *FireboltEngineReconciler) reconcileSwitching(ctx context.Context, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	engineName := engine.Name
	status := &engine.Status
	gen := status.CurrentGeneration

	log.Info("Switching traffic to new generation", "generation", gen)

	if err := r.updateClusterServiceSelector(ctx, engine, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update cluster service: %w", err)
	}
	MaybeCrash(engineName, CrashAfterServiceSelectorUpdate)

	oldGen := status.ActiveGeneration
	status.ActiveGeneration = gen

	if oldGen >= 0 {
		status.DrainingGeneration = &oldGen
		status.Phase = computev1alpha1.PhaseDraining
		log.Info("Traffic switched, starting drain of old generation",
			"newGeneration", gen, "drainingGeneration", oldGen)
	} else {
		status.LastAppliedConfig = spec
		status.Phase = computev1alpha1.PhaseStable
		log.Info("Initial deployment complete", "generation", gen)
	}

	MaybeCrash(engineName, CrashBeforeSwitchingStatusUpdate)
	if err := r.updateStatus(ctx, engine); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

func (r *FireboltEngineReconciler) reconcileDraining(ctx context.Context, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	engineName := engine.Name
	status := &engine.Status

	if status.DrainingGeneration == nil {
		log.Error(nil, "Draining phase but no draining generation set")
		status.Phase = computev1alpha1.PhaseStable
		if err := r.updateStatus(ctx, engine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	drainingGen := *status.DrainingGeneration

	// Scale the draining StatefulSet to 0 so Kubernetes won't restart crashed pods.
	// Running pods receive SIGTERM and stay in Terminating state until they exit;
	// the drain check below continues to monitor them until queries complete.
	if err := r.scaleStatefulSet(ctx, engineName, engine.Namespace, drainingGen, 0); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to scale down draining StatefulSet: %w", err)
	}

	if engine.Spec.Rollout == computev1alpha1.RolloutRecreate {
		log.Info("Rollout strategy is 'recreate', skipping drain check", "drainingGeneration", drainingGen)
		status.Phase = computev1alpha1.PhaseCleaning
		if err := r.updateStatus(ctx, engine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Checking drain status", "drainingGeneration", drainingGen)

	drained, err := r.checkDrainComplete(ctx, engine, drainingGen)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check drain status: %w", err)
	}

	if !drained {
		log.Info("Pods still draining, will retry", "drainingGeneration", drainingGen)
		return ctrl.Result{RequeueAfter: getDrainCheckInterval(spec)}, nil
	}

	log.Info("All pods drained, moving to cleaning phase", "drainingGeneration", drainingGen)
	status.Phase = computev1alpha1.PhaseCleaning
	MaybeCrash(engineName, CrashBeforeDrainingToCleaning)
	if err := r.updateStatus(ctx, engine); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

func (r *FireboltEngineReconciler) reconcileCleaning(ctx context.Context, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	engineName := engine.Name
	status := &engine.Status
	ns := engine.Namespace

	if status.DrainingGeneration == nil {
		log.Error(nil, "Cleaning phase but no draining generation set")
		status.Phase = computev1alpha1.PhaseStable
		if err := r.updateStatus(ctx, engine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	drainingGen := *status.DrainingGeneration
	log.Info("Cleaning up old generation", "generation", drainingGen)

	if err := r.deleteStatefulSet(ctx, engineName, ns, drainingGen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete StatefulSet: %w", err)
	}
	MaybeCrash(engineName, CrashAfterStatefulSetDeleted)

	if err := r.deleteHeadlessService(ctx, engineName, ns, drainingGen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete headless service: %w", err)
	}
	MaybeCrash(engineName, CrashAfterHeadlessServiceDeleted)

	if err := r.deleteCoreConfigMap(ctx, engineName, ns, drainingGen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete nodes ConfigMap: %w", err)
	}
	MaybeCrash(engineName, CrashAfterCoreConfigMapDeleted)

	log.Info("Cleanup complete, transition finished", "oldGeneration", drainingGen, "activeGeneration", status.ActiveGeneration)

	status.DrainingGeneration = nil
	status.Phase = computev1alpha1.PhaseStable

	MaybeCrash(engineName, CrashBeforeCleaningToStable)
	if err := r.updateStatus(ctx, engine); err != nil {
		return ctrl.Result{}, err
	}

	if status.PendingMutation != nil {
		log.Info("Pending mutation detected, will apply immediately")
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// --- Resource management helpers ---

func genResourceName(engineName string, gen int, suffix string) string {
	return fmt.Sprintf("%s%s%d%s", engineName, SuffixGen, gen, suffix)
}

func (r *FireboltEngineReconciler) ensureCoreConfigMap(ctx context.Context, engineName string, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec, gen int) error {
	name := genResourceName(engineName, gen, SuffixConfig)
	headlessSvcName := genResourceName(engineName, gen, SuffixHL)
	ns := engine.Namespace

	coreConfig := r.generateCoreConfig(engineName, gen, headlessSvcName, ns, engine.Status.AccountID, int(spec.Replicas))
	configJSON, err := json.MarshalIndent(coreConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal nodes config: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Data: map[string]string{
			"config.json": string(configJSON),
		},
	}

	if err := controllerutil.SetControllerReference(engine, cm, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}

	if existing.Data["config.json"] == cm.Data["config.json"] {
		return nil
	}
	existing.Data = cm.Data
	return r.Update(ctx, existing)
}

func (r *FireboltEngineReconciler) generateCoreConfig(engineName string, gen int, headlessSvcName, namespace, accountID string, replicas int) map[string]interface{} {
	stsName := genResourceName(engineName, gen, "")

	nodes := make([]map[string]string, replicas)
	for i := 0; i < replicas; i++ {
		podName := fmt.Sprintf("%s-%d", stsName, i)
		host := fmt.Sprintf("%s.%s.%s.svc", podName, headlessSvcName, namespace)
		nodes[i] = map[string]string{"host": host}
	}

	return map[string]interface{}{
		"nodes": nodes,
		"config": map[string]interface{}{
			"multi_cluster_endpoint": MetadataServiceEndpoint(engineName, namespace),
			"account_id":            accountID,
		},
	}
}

func (r *FireboltEngineReconciler) ensureHeadlessService(ctx context.Context, engineName string, engine *computev1alpha1.FireboltEngine, gen int) error {
	name := genResourceName(engineName, gen, SuffixHL)
	ns := engine.Namespace

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
			Ports: GetServicePorts(),
		},
	}

	if err := controllerutil.SetControllerReference(engine, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}

	needsUpdate := existing.Spec.PublishNotReadyAddresses != svc.Spec.PublishNotReadyAddresses ||
		servicePortsNeedUpdate(existing.Spec.Ports, svc.Spec.Ports)
	if needsUpdate {
		existing.Spec.PublishNotReadyAddresses = svc.Spec.PublishNotReadyAddresses
		existing.Spec.Ports = svc.Spec.Ports
		return r.Update(ctx, existing)
	}
	return nil
}

func (r *FireboltEngineReconciler) ensureStatefulSet(ctx context.Context, engineName string, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec, gen int) error {
	name := genResourceName(engineName, gen, "")
	headlessSvcName := genResourceName(engineName, gen, SuffixHL)
	coreConfigName := genResourceName(engineName, gen, SuffixConfig)
	ns := engine.Namespace

	labels := map[string]string{
		LabelEngine:     engineName,
		LabelGeneration: strconv.Itoa(gen),
	}

	pullPolicy := spec.Image.PullPolicy
	if pullPolicy == "" {
		pullPolicy = corev1.PullIfNotPresent
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         headlessSvcName,
			Replicas:            &spec.Replicas,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					NodeSelector: spec.NodeSelector,
					Tolerations:  spec.Tolerations,
					Containers: []corev1.Container{
						{
							Name:            ContainerNameCore,
							Image:           fmt.Sprintf("%s:%s", spec.Image.Repository, spec.Image.Tag),
							ImagePullPolicy: pullPolicy,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    spec.Resources.CPU,
									corev1.ResourceMemory: spec.Resources.Memory,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    spec.Resources.CPU,
									corev1.ResourceMemory: spec.Resources.Memory,
								},
							},
							Ports:   GetContainerPorts(),
							Command: []string{"/bin/bash", "-c"},
							Args:    []string{strings.TrimSpace(CoreStartupScript)},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "nodes-config",
									MountPath: ConfigMountPath,
									SubPath:   "config.json",
									ReadOnly:  true,
								},
							},
							ReadinessProbe: &corev1.Probe{
								InitialDelaySeconds: 5,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								FailureThreshold:    6,
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: HealthReadyPath,
										Port: intstr.FromInt(HealthPort),
									},
								},
							},
							LivenessProbe: &corev1.Probe{
								InitialDelaySeconds: 30,
								PeriodSeconds:       5,
								TimeoutSeconds:      3,
								FailureThreshold:    6,
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: HealthLivePath,
										Port: intstr.FromInt(HealthPort),
									},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "nodes-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: coreConfigName,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(engine, sts, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, sts)
	}
	return err
}

func (r *FireboltEngineReconciler) ensureClusterService(ctx context.Context, engineName string, engine *computev1alpha1.FireboltEngine, gen int) error {
	name := engineName + SuffixService
	ns := engine.Namespace

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				LabelEngine: engineName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
			Ports: GetServicePorts(),
		},
	}

	if err := controllerutil.SetControllerReference(engine, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}

	if servicePortsNeedUpdate(existing.Spec.Ports, svc.Spec.Ports) {
		existing.Spec.Ports = svc.Spec.Ports
		return r.Update(ctx, existing)
	}
	return nil
}

func (r *FireboltEngineReconciler) updateClusterServiceSelector(ctx context.Context, engine *computev1alpha1.FireboltEngine, gen int) error {
	name := engine.Name + SuffixService

	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: engine.Namespace}, svc); err != nil {
		return fmt.Errorf("failed to get cluster service: %w", err)
	}

	if svc.Spec.Selector == nil {
		svc.Spec.Selector = make(map[string]string)
	}
	svc.Spec.Selector[LabelGeneration] = strconv.Itoa(gen)
	return r.Update(ctx, svc)
}

func (r *FireboltEngineReconciler) arePodsReady(ctx context.Context, engine *computev1alpha1.FireboltEngine, gen int, expectedReplicas int) (bool, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(engine.Namespace), client.MatchingLabels{
		LabelEngine:     engine.Name,
		LabelGeneration: strconv.Itoa(gen),
	}); err != nil {
		return false, fmt.Errorf("failed to list pods: %w", err)
	}

	if len(podList.Items) != expectedReplicas {
		return false, nil
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			return false, nil
		}
		ready := false
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if !ready {
			return false, nil
		}
	}

	return true, nil
}

func (r *FireboltEngineReconciler) checkDrainComplete(ctx context.Context, engine *computev1alpha1.FireboltEngine, gen int) (bool, error) {
	log := logf.FromContext(ctx)

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(engine.Namespace), client.MatchingLabels{
		LabelEngine:     engine.Name,
		LabelGeneration: strconv.Itoa(gen),
	}); err != nil {
		return false, fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		drained, err := r.isPodDrained(ctx, &pod)
		if err != nil {
			log.Error(err, "Failed to check drain status for pod", "pod", pod.Name)
			return false, nil
		}

		if !drained {
			log.V(1).Info("Pod still has active queries", "pod", pod.Name)
			return false, nil
		}
	}

	return true, nil
}

func (r *FireboltEngineReconciler) isPodDrained(ctx context.Context, pod *corev1.Pod) (bool, error) {
	if r.Clientset == nil || r.RestConfig == nil {
		return false, fmt.Errorf("clientset or rest config not initialized")
	}

	cmd := []string{
		"fb",
		"--core",
		"--no-spinner",
		"--concise",
		"--label", "core-operator-drain-check",
		"--extra", "access_internal_system_tables=1",
		"--format", "JSON_Compact",
		"--command", DrainCheckSQL,
	}

	req := r.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		Param("container", ContainerNameCore).
		Param("command", cmd[0])

	for _, c := range cmd[1:] {
		req = req.Param("command", c)
	}
	req = req.Param("stdout", "true").
		Param("stderr", "true")

	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, "POST", req.URL())
	if err != nil {
		return false, fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return false, fmt.Errorf("exec failed: %w, stderr: %s", err, stderr.String())
	}

	output := stdout.String()
	var response DrainCheckResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return false, fmt.Errorf("failed to parse drain check response: %w, output: %s", err, output)
	}

	if len(response.Errors) > 0 {
		return false, fmt.Errorf("drain check query returned error: %s", response.Errors[0].Description)
	}

	if len(response.Data) == 0 || len(response.Data[0]) == 0 {
		return false, fmt.Errorf("drain check response missing data field: %s", output)
	}

	count := response.Data[0][0]
	switch count {
	case "0":
		return true, nil
	case "1":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected drain check count value: %s (expected '0' or '1')", count)
	}
}

func (r *FireboltEngineReconciler) scaleStatefulSet(ctx context.Context, engineName, namespace string, gen int, replicas int32) error {
	name := genResourceName(engineName, gen, "")
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sts); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == replicas {
		return nil
	}
	sts.Spec.Replicas = &replicas
	return r.Update(ctx, sts)
}

func (r *FireboltEngineReconciler) deleteStatefulSet(ctx context.Context, engineName, namespace string, gen int) error {
	name := genResourceName(engineName, gen, "")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	err := r.Delete(ctx, sts)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *FireboltEngineReconciler) deleteHeadlessService(ctx context.Context, engineName, namespace string, gen int) error {
	name := genResourceName(engineName, gen, SuffixHL)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	err := r.Delete(ctx, svc)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *FireboltEngineReconciler) deleteCoreConfigMap(ctx context.Context, engineName, namespace string, gen int) error {
	name := genResourceName(engineName, gen, SuffixConfig)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	err := r.Delete(ctx, cm)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *FireboltEngineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerNamed(mgr, "fireboltengine")
}

// SetupWithManagerNamed sets up the controller with the Manager using a custom controller name.
func (r *FireboltEngineReconciler) SetupWithManagerNamed(mgr ctrl.Manager, name string) error {
	if r.RestConfig == nil {
		r.RestConfig = mgr.GetConfig()
	}
	if r.Clientset == nil {
		clientset, err := kubernetes.NewForConfig(r.RestConfig)
		if err != nil {
			return fmt.Errorf("failed to create clientset: %w", err)
		}
		r.Clientset = clientset
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltEngine{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named(name).
		Complete(r)
}
