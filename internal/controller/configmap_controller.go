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
	"k8s.io/apimachinery/pkg/api/resource"
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
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"sigs.k8s.io/yaml"
)

// ConfigMapReconciler reconciles ConfigMap objects that match the configured prefix
type ConfigMapReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	ConfigPrefix string
	Namespace    string
	RestConfig   *rest.Config
	Clientset    *kubernetes.Clientset
}

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

// Reconcile handles changes to ConfigMaps that match the configured prefix
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Skip if not in our namespace
	if req.Namespace != r.Namespace {
		return ctrl.Result{}, nil
	}

	// Skip if doesn't match our prefix
	if !strings.HasPrefix(req.Name, r.ConfigPrefix) {
		return ctrl.Result{}, nil
	}

	// Skip status ConfigMaps (we manage those)
	if strings.HasSuffix(req.Name, SuffixStatus) {
		return ctrl.Result{}, nil
	}

	// Skip per-generation ConfigMaps (nodes config)
	// Pattern: {cluster}-g{N} or {cluster}-g{N}-{suffix}
	if isGenerationResource(req.Name) {
		return ctrl.Result{}, nil
	}

	clusterName := req.Name
	log = log.WithValues("cluster", clusterName)
	log.Info("Reconciling cluster")

	// Get the config ConfigMap
	configCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, req.NamespacedName, configCM); err != nil {
		if errors.IsNotFound(err) {
			log.Info("Config ConfigMap deleted, skipping reconciliation")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get config ConfigMap")
		return ctrl.Result{}, err
	}

	// Parse the configuration
	config, err := r.parseConfig(configCM)
	if err != nil {
		log.Error(err, "Failed to parse config ConfigMap")
		return ctrl.Result{}, err
	}

	// Get or create the status ConfigMap
	status, statusCM, err := r.getOrCreateStatus(ctx, clusterName, configCM)
	if err != nil {
		log.Error(err, "Failed to get or create status ConfigMap")
		return ctrl.Result{}, err
	}

	// Check if we need to reconcile
	needsReconcile := r.needsReconcile(config, status)

	if !needsReconcile && status.Phase == PhaseStable {
		log.V(1).Info("No changes detected, cluster is stable")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Handle reconciliation based on current phase
	result, err := r.reconcilePhase(ctx, clusterName, configCM, config, status, statusCM)
	if err != nil {
		log.Error(err, "Failed to reconcile phase", "phase", status.Phase)
		return ctrl.Result{}, err
	}

	return result, nil
}

// parseConfig parses a ConfigMap into a ClusterConfig
func (r *ConfigMapReconciler) parseConfig(cm *corev1.ConfigMap) (*ClusterConfig, error) {
	config := &ClusterConfig{
		DrainCheckInterval: DefaultDrainCheckInterval,
	}

	// Parse replicas (required, must be >= 1)
	if replicasStr, ok := cm.Data["replicas"]; ok {
		replicas, err := strconv.ParseInt(replicasStr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid replicas value: %w", err)
		}
		if replicas < 1 {
			return nil, fmt.Errorf("replicas must be at least 1, got %d", replicas)
		}
		config.Replicas = int32(replicas)
	} else {
		return nil, fmt.Errorf("replicas is required")
	}

	// Parse image (required, must not be empty)
	if image, ok := cm.Data["image"]; ok && image != "" {
		config.Image = image
	} else {
		return nil, fmt.Errorf("image is required and must not be empty")
	}

	// Parse tag (required, must not be empty)
	if tag, ok := cm.Data["tag"]; ok && tag != "" {
		config.Tag = tag
	} else {
		return nil, fmt.Errorf("tag is required and must not be empty")
	}

	// Parse imagePullPolicy (optional, defaults to IfNotPresent)
	if policy, ok := cm.Data["imagePullPolicy"]; ok {
		switch policy {
		case "Always":
			config.ImagePullPolicy = corev1.PullAlways
		case "Never":
			config.ImagePullPolicy = corev1.PullNever
		case "IfNotPresent":
			config.ImagePullPolicy = corev1.PullIfNotPresent
		default:
			return nil, fmt.Errorf("invalid imagePullPolicy: %s (must be Always, Never, or IfNotPresent)", policy)
		}
	} else {
		config.ImagePullPolicy = corev1.PullIfNotPresent // Default
	}

	// Parse cpu (required, validate format)
	if cpu, ok := cm.Data["cpu"]; ok && cpu != "" {
		if _, err := resource.ParseQuantity(cpu); err != nil {
			return nil, fmt.Errorf("invalid cpu value: %w", err)
		}
		config.CPU = cpu
	} else {
		return nil, fmt.Errorf("cpu is required and must not be empty")
	}

	// Parse memory (required, validate format)
	if memory, ok := cm.Data["memory"]; ok && memory != "" {
		if _, err := resource.ParseQuantity(memory); err != nil {
			return nil, fmt.Errorf("invalid memory value: %w", err)
		}
		config.Memory = memory
	} else {
		return nil, fmt.Errorf("memory is required and must not be empty")
	}

	// Parse drainCheckInterval (optional, must be positive)
	if intervalStr, ok := cm.Data["drainCheckInterval"]; ok {
		interval, err := time.ParseDuration(intervalStr)
		if err != nil {
			return nil, fmt.Errorf("invalid drainCheckInterval: %w", err)
		}
		if interval <= 0 {
			return nil, fmt.Errorf("drainCheckInterval must be positive, got %s", interval)
		}
		config.DrainCheckInterval = interval
	}

	// Parse nodeSelector (optional, YAML format)
	if nodeSelectorStr, ok := cm.Data["nodeSelector"]; ok && nodeSelectorStr != "" {
		nodeSelector := make(map[string]string)
		if err := yaml.Unmarshal([]byte(nodeSelectorStr), &nodeSelector); err != nil {
			return nil, fmt.Errorf("invalid nodeSelector YAML: %w", err)
		}
		config.NodeSelector = nodeSelector
	}

	// Parse tolerations (optional, YAML format)
	if tolerationsStr, ok := cm.Data["tolerations"]; ok && tolerationsStr != "" {
		var tolerations []corev1.Toleration
		if err := yaml.Unmarshal([]byte(tolerationsStr), &tolerations); err != nil {
			return nil, fmt.Errorf("invalid tolerations YAML: %w", err)
		}
		config.Tolerations = tolerations
	}

	// Parse rollout strategy (optional, defaults to graceful)
	if rollout, ok := cm.Data["rollout"]; ok {
		switch rollout {
		case string(RolloutGraceful):
			config.Rollout = RolloutGraceful
		case string(RolloutRecreate):
			config.Rollout = RolloutRecreate
		default:
			return nil, fmt.Errorf("invalid rollout: %s (must be graceful or recreate)", rollout)
		}
	} else {
		config.Rollout = RolloutGraceful
	}

	return config, nil
}

// getOrCreateStatus retrieves or creates the status ConfigMap
func (r *ConfigMapReconciler) getOrCreateStatus(ctx context.Context, clusterName string, configCM *corev1.ConfigMap) (*ClusterStatus, *corev1.ConfigMap, error) {
	log := logf.FromContext(ctx)
	statusName := clusterName + SuffixStatus

	statusCM := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: statusName, Namespace: r.Namespace}, statusCM)

	if errors.IsNotFound(err) {
		// Create initial status
		log.Info("Creating initial status ConfigMap", "name", statusName)

		status := &ClusterStatus{
			CurrentGeneration:  0,
			ActiveGeneration:   -1, // No active generation yet
			DrainingGeneration: nil,
			Phase:              PhaseCreating,
			LastReconciled:     time.Now(),
		}

		statusData, err := json.Marshal(status)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal status: %w", err)
		}

		statusCM = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      statusName,
				Namespace: r.Namespace,
			},
			Data: map[string]string{
				"state": string(statusData),
			},
		}

		// Set owner reference to the config ConfigMap
		if err := controllerutil.SetControllerReference(configCM, statusCM, r.Scheme); err != nil {
			return nil, nil, fmt.Errorf("failed to set owner reference: %w", err)
		}

		if err := r.Create(ctx, statusCM); err != nil {
			return nil, nil, fmt.Errorf("failed to create status ConfigMap: %w", err)
		}

		return status, statusCM, nil
	}

	if err != nil {
		return nil, nil, fmt.Errorf("failed to get status ConfigMap: %w", err)
	}

	// Parse existing status
	status := &ClusterStatus{}
	if stateStr, ok := statusCM.Data["state"]; ok {
		if err := json.Unmarshal([]byte(stateStr), status); err != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal status: %w", err)
		}
	}

	return status, statusCM, nil
}

// configsEqual checks if two configs are equal (all configurable fields)
func configsEqual(a, b *ClusterConfig) bool {
	if a == nil || b == nil {
		return a == b
	}

	// Compare basic fields
	if a.Replicas != b.Replicas ||
		a.Image != b.Image ||
		a.Tag != b.Tag ||
		a.ImagePullPolicy != b.ImagePullPolicy ||
		a.CPU != b.CPU ||
		a.Memory != b.Memory ||
		a.DrainCheckInterval != b.DrainCheckInterval ||
		a.Rollout != b.Rollout {
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

	return true
}

// tolerationEqual compares two Tolerations
func tolerationEqual(a, b corev1.Toleration) bool {
	if a.Key != b.Key ||
		a.Operator != b.Operator ||
		a.Value != b.Value ||
		a.Effect != b.Effect {
		return false
	}
	// Compare TolerationSeconds pointers
	if a.TolerationSeconds == nil && b.TolerationSeconds == nil {
		return true
	}
	if a.TolerationSeconds == nil || b.TolerationSeconds == nil {
		return false
	}
	return *a.TolerationSeconds == *b.TolerationSeconds
}

// needsReconcile checks if the config has changed compared to the last applied config
func (r *ConfigMapReconciler) needsReconcile(config *ClusterConfig, status *ClusterStatus) bool {
	if status.Phase != PhaseStable {
		return true // Continue in-progress transition
	}

	if status.LastAppliedConfig == nil {
		return true // No config applied yet
	}

	// Check if there's a pending mutation to apply
	if status.PendingMutation != nil {
		return true
	}

	// Compare live config to last applied config
	return !configsEqual(config, status.LastAppliedConfig)
}

// updateStatus updates the status ConfigMap
func (r *ConfigMapReconciler) updateStatus(ctx context.Context, statusCM *corev1.ConfigMap, status *ClusterStatus) error {
	status.LastReconciled = time.Now()
	statusData, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("failed to marshal status: %w", err)
	}

	statusCM.Data["state"] = string(statusData)
	return r.Update(ctx, statusCM)
}

// reconcilePhase handles reconciliation based on the current phase
func (r *ConfigMapReconciler) reconcilePhase(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, config *ClusterConfig, status *ClusterStatus, statusCM *corev1.ConfigMap) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	switch status.Phase {
	case PhaseStable:
		// Determine which config to use: PendingMutation takes priority
		targetConfig := config
		if status.PendingMutation != nil {
			log.Info("Applying pending mutation")
			targetConfig = status.PendingMutation
			status.PendingMutation = nil
		}

		// A change was detected, start a new transition
		if status.ActiveGeneration == -1 {
			// First time setup
			log.Info("Initial cluster setup, creating generation 0")
		} else {
			// Queue mutation if transition is needed
			log.Info("Configuration change detected, starting new generation",
				"currentGeneration", status.CurrentGeneration,
				"newGeneration", status.CurrentGeneration+1)
			status.CurrentGeneration++
		}

		// Snapshot the target config - this is the config we'll use for this transition
		status.LastAppliedConfig = targetConfig
		status.Phase = PhaseCreating

		if err := r.updateStatus(ctx, statusCM, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil

	case PhaseCreating, PhaseSwitching, PhaseDraining, PhaseCleaning:
		// During transitions, use the snapshotted LastAppliedConfig, not the live config
		// If live config differs from LastAppliedConfig, save it as PendingMutation
		if status.LastAppliedConfig != nil && !configsEqual(config, status.LastAppliedConfig) {
			if status.PendingMutation == nil || !configsEqual(config, status.PendingMutation) {
				log.Info("New config change detected during transition, saving as pending mutation",
					"phase", status.Phase)
				status.PendingMutation = config
				if err := r.updateStatus(ctx, statusCM, status); err != nil {
					return ctrl.Result{}, err
				}
			}
		}

		// Use the snapshotted config for the current transition
		activeConfig := status.LastAppliedConfig
		if activeConfig == nil {
			// Fallback to live config if no snapshot (shouldn't happen normally)
			activeConfig = config
		}

		switch status.Phase {
		case PhaseCreating:
			return r.reconcileCreating(ctx, clusterName, configCM, activeConfig, status, statusCM)
		case PhaseSwitching:
			return r.reconcileSwitching(ctx, clusterName, configCM, activeConfig, status, statusCM)
		case PhaseDraining:
			return r.reconcileDraining(ctx, clusterName, configCM, activeConfig, status, statusCM)
		case PhaseCleaning:
			return r.reconcileCleaning(ctx, clusterName, configCM, activeConfig, status, statusCM)
		}
	}

	log.Error(nil, "Unknown phase", "phase", status.Phase)
	return ctrl.Result{}, fmt.Errorf("unknown phase: %s", status.Phase)
}

// reconcileCreating handles the creating phase
func (r *ConfigMapReconciler) reconcileCreating(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, config *ClusterConfig, status *ClusterStatus, statusCM *corev1.ConfigMap) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	gen := status.CurrentGeneration

	log.Info("Creating resources for generation", "generation", gen)

	// Create nodes ConfigMap
	if err := r.ensureCoreConfigMap(ctx, clusterName, configCM, config, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create nodes ConfigMap: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterCoreConfigMapCreated)

	// Create headless service
	if err := r.ensureHeadlessService(ctx, clusterName, configCM, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create headless service: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterHeadlessServiceCreated)

	// Create StatefulSet
	if err := r.ensureStatefulSet(ctx, clusterName, configCM, config, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create StatefulSet: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterStatefulSetCreated)

	// Ensure cluster service exists (created once, selector updated during switching)
	if err := r.ensureClusterService(ctx, clusterName, configCM, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create cluster service: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterClusterServiceEnsured)

	// Check if all pods are ready
	ready, err := r.arePodsReady(ctx, clusterName, gen, int(config.Replicas))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check pod readiness: %w", err)
	}

	if !ready {
		log.Info("Waiting for pods to become ready", "generation", gen)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	log.Info("All pods ready, moving to switching phase", "generation", gen)
	status.Phase = PhaseSwitching
	MaybeCrash(clusterName, CrashBeforeCreatingToSwitching)
	if err := r.updateStatus(ctx, statusCM, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// reconcileSwitching handles the switching phase
func (r *ConfigMapReconciler) reconcileSwitching(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, config *ClusterConfig, status *ClusterStatus, statusCM *corev1.ConfigMap) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	gen := status.CurrentGeneration

	log.Info("Switching traffic to new generation", "generation", gen)

	// Update cluster service selector to point to new generation
	if err := r.updateClusterServiceSelector(ctx, clusterName, gen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update cluster service: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterServiceSelectorUpdate)

	oldGen := status.ActiveGeneration
	status.ActiveGeneration = gen

	if oldGen >= 0 {
		// There's an old generation to drain
		status.DrainingGeneration = &oldGen
		status.Phase = PhaseDraining
		log.Info("Traffic switched, starting drain of old generation",
			"newGeneration", gen, "drainingGeneration", oldGen)
	} else {
		// First deployment, no old generation
		status.Phase = PhaseStable
		// LastAppliedConfig is already set at the start of transition
		log.Info("Initial deployment complete", "generation", gen)
	}

	MaybeCrash(clusterName, CrashBeforeSwitchingStatusUpdate)
	if err := r.updateStatus(ctx, statusCM, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// reconcileDraining handles the draining phase
func (r *ConfigMapReconciler) reconcileDraining(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, config *ClusterConfig, status *ClusterStatus, statusCM *corev1.ConfigMap) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if status.DrainingGeneration == nil {
		log.Error(nil, "Draining phase but no draining generation set")
		status.Phase = PhaseStable
		if err := r.updateStatus(ctx, statusCM, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	drainingGen := *status.DrainingGeneration

	// If rollout is "recreate", skip drain check and immediately clean up
	if config.Rollout == RolloutRecreate {
		log.Info("Rollout strategy is 'recreate', skipping drain check", "drainingGeneration", drainingGen)
		status.Phase = PhaseCleaning
		if err := r.updateStatus(ctx, statusCM, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Checking drain status", "drainingGeneration", drainingGen)

	// Get pods for the draining generation
	drained, err := r.checkDrainComplete(ctx, clusterName, drainingGen)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to check drain status: %w", err)
	}

	if !drained {
		log.Info("Pods still draining, will retry", "drainingGeneration", drainingGen)
		return ctrl.Result{RequeueAfter: config.DrainCheckInterval}, nil
	}

	log.Info("All pods drained, moving to cleaning phase", "drainingGeneration", drainingGen)
	status.Phase = PhaseCleaning
	MaybeCrash(clusterName, CrashBeforeDrainingToCleaning)
	if err := r.updateStatus(ctx, statusCM, status); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// reconcileCleaning handles the cleaning phase
func (r *ConfigMapReconciler) reconcileCleaning(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, config *ClusterConfig, status *ClusterStatus, statusCM *corev1.ConfigMap) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if status.DrainingGeneration == nil {
		log.Error(nil, "Cleaning phase but no draining generation set")
		status.Phase = PhaseStable
		if err := r.updateStatus(ctx, statusCM, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	drainingGen := *status.DrainingGeneration
	log.Info("Cleaning up old generation", "generation", drainingGen)

	// Delete StatefulSet
	if err := r.deleteStatefulSet(ctx, clusterName, drainingGen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete StatefulSet: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterStatefulSetDeleted)

	// Delete headless service
	if err := r.deleteHeadlessService(ctx, clusterName, drainingGen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete headless service: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterHeadlessServiceDeleted)

	// Delete nodes ConfigMap
	if err := r.deleteCoreConfigMap(ctx, clusterName, drainingGen); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to delete nodes ConfigMap: %w", err)
	}
	MaybeCrash(clusterName, CrashAfterCoreConfigMapDeleted)

	log.Info("Cleanup complete, transition finished", "oldGeneration", drainingGen, "activeGeneration", status.ActiveGeneration)

	status.DrainingGeneration = nil
	status.Phase = PhaseStable
	// LastAppliedConfig is already set at the start of transition

	MaybeCrash(clusterName, CrashBeforeCleaningToStable)
	if err := r.updateStatus(ctx, statusCM, status); err != nil {
		return ctrl.Result{}, err
	}

	// If there's a pending mutation, requeue immediately to apply it
	if status.PendingMutation != nil {
		log.Info("Pending mutation detected, will apply immediately")
		return ctrl.Result{Requeue: true}, nil
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// genResourceName returns the resource name for a given cluster and generation
func genResourceName(clusterName string, gen int, suffix string) string {
	return fmt.Sprintf("%s%s%d%s", clusterName, SuffixGen, gen, suffix)
}

// isGenerationResource checks if a resource name matches the pattern for per-generation resources
// Pattern: {anything}-g{digit(s)} or {anything}-g{digit(s)}-{suffix}
func isGenerationResource(name string) bool {
	// Find the last occurrence of "-g" followed by at least one digit
	idx := strings.LastIndex(name, SuffixGen)
	if idx == -1 {
		return false
	}
	// Check if there's at least one character after "-g"
	afterGen := name[idx+len(SuffixGen):]
	if len(afterGen) == 0 {
		return false
	}
	// The first character after "-g" must be a digit
	if afterGen[0] < '0' || afterGen[0] > '9' {
		return false
	}
	return true
}

// ensureCoreConfigMap creates or updates the core config ConfigMap for a generation
func (r *ConfigMapReconciler) ensureCoreConfigMap(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, config *ClusterConfig, gen int) error {
	name := genResourceName(clusterName, gen, SuffixConfig)
	headlessSvcName := genResourceName(clusterName, gen, SuffixHL)

	// Generate core config
	coreConfig := r.generateCoreConfig(clusterName, gen, headlessSvcName, int(config.Replicas))
	configJSON, err := json.MarshalIndent(coreConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal nodes config: %w", err)
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
			Labels: map[string]string{
				LabelCluster:    clusterName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Data: map[string]string{
			"config.json": string(configJSON),
		},
	}

	if err := controllerutil.SetControllerReference(configCM, cm, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}

	// Only update if data has changed (avoid triggering unnecessary watch events)
	if existing.Data["config.json"] == cm.Data["config.json"] {
		return nil
	}
	existing.Data = cm.Data
	return r.Update(ctx, existing)
}

// generateCoreConfig generates the core configuration for config.json
func (r *ConfigMapReconciler) generateCoreConfig(clusterName string, gen int, headlessSvcName string, replicas int) map[string]interface{} {
	stsName := genResourceName(clusterName, gen, "")

	nodes := make([]map[string]string, replicas)
	for i := 0; i < replicas; i++ {
		// Pod FQDN: <pod-name>.<headless-svc>.<namespace>.svc
		podName := fmt.Sprintf("%s-%d", stsName, i)
		host := fmt.Sprintf("%s.%s.%s.svc", podName, headlessSvcName, r.Namespace)
		nodes[i] = map[string]string{"host": host}
	}

	return map[string]interface{}{
		"nodes": nodes,
	}
}

// ensureHeadlessService creates or updates the headless service for a generation
func (r *ConfigMapReconciler) ensureHeadlessService(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, gen int) error {
	name := genResourceName(clusterName, gen, SuffixHL)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
			Labels: map[string]string{
				LabelCluster:    clusterName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			// Required for cluster formation - nodes must reach each other before ready
			PublishNotReadyAddresses: true,
			Selector: map[string]string{
				LabelCluster:    clusterName,
				LabelGeneration: strconv.Itoa(gen),
			},
			Ports: GetServicePorts(),
		},
	}

	if err := controllerutil.SetControllerReference(configCM, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}

	// Update if PublishNotReadyAddresses changed
	if existing.Spec.PublishNotReadyAddresses != svc.Spec.PublishNotReadyAddresses {
		existing.Spec.PublishNotReadyAddresses = svc.Spec.PublishNotReadyAddresses
		return r.Update(ctx, existing)
	}
	return nil
}

// ensureStatefulSet creates or updates the StatefulSet for a generation
func (r *ConfigMapReconciler) ensureStatefulSet(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, config *ClusterConfig, gen int) error {
	name := genResourceName(clusterName, gen, "")
	headlessSvcName := genResourceName(clusterName, gen, SuffixHL)
	coreConfigName := genResourceName(clusterName, gen, SuffixConfig)

	labels := map[string]string{
		LabelCluster:    clusterName,
		LabelGeneration: strconv.Itoa(gen),
	}

	// Parse resource quantities
	cpuQty, err := resource.ParseQuantity(config.CPU)
	if err != nil {
		return fmt.Errorf("invalid CPU value: %w", err)
	}
	memQty, err := resource.ParseQuantity(config.Memory)
	if err != nil {
		return fmt.Errorf("invalid memory value: %w", err)
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName:         headlessSvcName,
			Replicas:            &config.Replicas,
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					NodeSelector: config.NodeSelector,
					Tolerations:  config.Tolerations,
					Containers: []corev1.Container{
						{
							Name:            ContainerNameCore,
							Image:           fmt.Sprintf("%s:%s", config.Image, config.Tag),
							ImagePullPolicy: config.ImagePullPolicy,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQty,
									corev1.ResourceMemory: memQty,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQty,
									corev1.ResourceMemory: memQty,
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

	if err := controllerutil.SetControllerReference(configCM, sts, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, sts)
	}
	return err // If exists, no update needed (we create new generations instead)
}

// ensureClusterService creates the main cluster service if it doesn't exist
func (r *ConfigMapReconciler) ensureClusterService(ctx context.Context, clusterName string, configCM *corev1.ConfigMap, gen int) error {
	name := clusterName + SuffixService

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
			Labels: map[string]string{
				LabelCluster: clusterName,
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				LabelCluster:    clusterName,
				LabelGeneration: strconv.Itoa(gen),
			},
			Ports: GetServicePorts(),
		},
	}

	if err := controllerutil.SetControllerReference(configCM, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	return err
}

// updateClusterServiceSelector updates the cluster service selector to point to a specific generation
func (r *ConfigMapReconciler) updateClusterServiceSelector(ctx context.Context, clusterName string, gen int) error {
	name := clusterName + SuffixService

	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: r.Namespace}, svc); err != nil {
		return fmt.Errorf("failed to get cluster service: %w", err)
	}

	// Ensure selector map exists (shouldn't be nil if we created the service, but be defensive)
	if svc.Spec.Selector == nil {
		svc.Spec.Selector = make(map[string]string)
	}
	svc.Spec.Selector[LabelGeneration] = strconv.Itoa(gen)
	return r.Update(ctx, svc)
}

// arePodsReady checks if all pods for a generation are ready
func (r *ConfigMapReconciler) arePodsReady(ctx context.Context, clusterName string, gen int, expectedReplicas int) (bool, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(r.Namespace), client.MatchingLabels{
		LabelCluster:    clusterName,
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

// checkDrainComplete checks if all pods in a generation have finished draining
func (r *ConfigMapReconciler) checkDrainComplete(ctx context.Context, clusterName string, gen int) (bool, error) {
	log := logf.FromContext(ctx)

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(r.Namespace), client.MatchingLabels{
		LabelCluster:    clusterName,
		LabelGeneration: strconv.Itoa(gen),
	}); err != nil {
		return false, fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range podList.Items {
		// Skip pods that aren't running (they have no queries)
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}

		drained, err := r.isPodDrained(ctx, &pod)
		if err != nil {
			log.Error(err, "Failed to check drain status for pod", "pod", pod.Name)
			return false, nil // Retry on next reconcile
		}

		if !drained {
			log.V(1).Info("Pod still has active queries", "pod", pod.Name)
			return false, nil
		}
	}

	return true, nil
}

// isPodDrained executes the drain check command on a pod
func (r *ConfigMapReconciler) isPodDrained(ctx context.Context, pod *corev1.Pod) (bool, error) {
	if r.Clientset == nil || r.RestConfig == nil {
		return false, fmt.Errorf("clientset or rest config not initialized")
	}

	// Build the drain check command
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

	// Parse the JSON response
	output := stdout.String()
	var response DrainCheckResponse
	if err := json.Unmarshal([]byte(output), &response); err != nil {
		return false, fmt.Errorf("failed to parse drain check response: %w, output: %s", err, output)
	}

	// If there are errors in the response, return them - don't silently treat as drained
	if len(response.Errors) > 0 {
		return false, fmt.Errorf("drain check query returned error: %s", response.Errors[0].Description)
	}

	// Check the count value - must be exactly "0" or "1"
	if len(response.Data) == 0 || len(response.Data[0]) == 0 {
		return false, fmt.Errorf("drain check response missing data field: %s", output)
	}

	count := response.Data[0][0]
	switch count {
	case "0":
		return true, nil // Drained - no running queries
	case "1":
		return false, nil // Not drained - queries still running
	default:
		return false, fmt.Errorf("unexpected drain check count value: %s (expected '0' or '1')", count)
	}
}

// deleteStatefulSet deletes the StatefulSet for a generation
func (r *ConfigMapReconciler) deleteStatefulSet(ctx context.Context, clusterName string, gen int) error {
	name := genResourceName(clusterName, gen, "")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
		},
	}
	err := r.Delete(ctx, sts)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// deleteHeadlessService deletes the headless service for a generation
func (r *ConfigMapReconciler) deleteHeadlessService(ctx context.Context, clusterName string, gen int) error {
	name := genResourceName(clusterName, gen, SuffixHL)
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
		},
	}
	err := r.Delete(ctx, svc)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// deleteCoreConfigMap deletes the core config ConfigMap for a generation
func (r *ConfigMapReconciler) deleteCoreConfigMap(ctx context.Context, clusterName string, gen int) error {
	name := genResourceName(clusterName, gen, SuffixConfig)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
		},
	}
	err := r.Delete(ctx, cm)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConfigMapReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerNamed(mgr, "core-ctrl")
}

// SetupWithManagerNamed sets up the controller with the Manager using a custom controller name.
// This is useful when running multiple controller instances (e.g., in tests).
func (r *ConfigMapReconciler) SetupWithManagerNamed(mgr ctrl.Manager, name string) error {
	// Initialize the kubernetes clientset for exec operations if not already set
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
		For(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			// Only watch objects in our namespace
			if object.GetNamespace() != r.Namespace {
				return false
			}
			// Only watch ConfigMaps with our prefix (or owned by them)
			if cm, ok := object.(*corev1.ConfigMap); ok {
				return strings.HasPrefix(cm.Name, r.ConfigPrefix)
			}
			// For other resources, check if they're owned by our ConfigMaps
			for _, ref := range object.GetOwnerReferences() {
				if ref.Kind == "ConfigMap" && strings.HasPrefix(ref.Name, r.ConfigPrefix) {
					return true
				}
			}
			return false
		})).
		Named(name).
		Complete(r)
}
