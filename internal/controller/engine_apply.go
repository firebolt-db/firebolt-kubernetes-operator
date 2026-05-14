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
	"context"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// applyEngineState writes the EngineReconcileResult to the cluster: ensures
// resources exist, deletes stale ones, and updates the engine status.
// All operations are idempotent.
func (r *FireboltEngineReconciler) applyEngineState(ctx context.Context, engine *computev1alpha1.FireboltEngine, result *EngineReconcileResult) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	if result.EnsureConfigMap != nil {
		log.Info("Ensuring ConfigMap", "name", result.EnsureConfigMap.Name)
		if err := r.ensureConfigMap(ctx, engine, result.EnsureConfigMap); err != nil {
			return fmt.Errorf("failed to ensure ConfigMap: %w", err)
		}
		MaybeCrash(engine.Name, CrashAfterEngineConfigMapCreated)
	}

	if result.EnsureHeadlessSvc != nil {
		log.Info("Ensuring headless Service", "name", result.EnsureHeadlessSvc.Name)
		if err := r.ensureService(ctx, engine, result.EnsureHeadlessSvc); err != nil {
			return fmt.Errorf("failed to ensure headless service: %w", err)
		}
		MaybeCrash(engine.Name, CrashAfterHeadlessServiceCreated)
	}

	if result.EnsureStatefulSet != nil {
		log.Info("Ensuring StatefulSet", "name", result.EnsureStatefulSet.Name,
			"replicas", *result.EnsureStatefulSet.Spec.Replicas)
		if err := r.ensureStatefulSetResource(ctx, engine, result.EnsureStatefulSet); err != nil {
			return fmt.Errorf("failed to ensure StatefulSet: %w", err)
		}
		MaybeCrash(engine.Name, CrashAfterStatefulSetCreated)
	}

	if result.EnsureClusterSvc != nil {
		targetGen := result.EnsureClusterSvc.Spec.Selector[LabelGeneration]
		log.Info("Ensuring cluster Service", "name", result.EnsureClusterSvc.Name,
			"targetGeneration", targetGen)
		if err := r.ensureService(ctx, engine, result.EnsureClusterSvc); err != nil {
			return fmt.Errorf("failed to ensure cluster service: %w", err)
		}
		MaybeCrash(engine.Name, CrashAfterClusterServiceEnsured)
	}

	for _, obj := range result.DeleteResources {
		log.Info("Deleting resource", "kind", fmt.Sprintf("%T", obj), "name", obj.GetName())
		if err := r.deleteIfExists(ctx, obj); err != nil {
			log.Error(err, "Failed to delete resource", "resource", client.ObjectKeyFromObject(obj))
			return err
		}
	}

	oldPhase := engine.Status.Phase
	newPhase := result.Status.Phase

	if oldPhase == computev1alpha1.PhaseSwitching {
		log.Info("Switching phase apply",
			"ensureClusterSvcNil", result.EnsureClusterSvc == nil,
			"oldPhase", oldPhase,
			"newPhase", newPhase,
		)
	}

	switch oldPhase {
	case computev1alpha1.PhaseCreating:
		if newPhase == computev1alpha1.PhaseSwitching {
			MaybeCrash(engine.Name, CrashBeforeCreatingToSwitching)
		}
	case computev1alpha1.PhaseSwitching:
		if result.EnsureClusterSvc != nil || newPhase != computev1alpha1.PhaseSwitching {
			// When EnsureClusterSvc is nil and we're still in PhaseSwitching,
			// the selector already pointed to the new generation (cache race
			// or previous partial reconcile). Fire the crash point anyway so
			// tests can simulate a crash after the traffic switch.
			MaybeCrash(engine.Name, CrashAfterServiceSelectorUpdate)
		}
		MaybeCrash(engine.Name, CrashBeforeSwitchingStatusUpdate)
	case computev1alpha1.PhaseCleaning:
		if len(result.DeleteResources) > 0 {
			MaybeCrash(engine.Name, CrashAfterStatefulSetDeleted)
		}
		if newPhase == computev1alpha1.PhaseStable ||
			newPhase == computev1alpha1.PhaseStopped {
			MaybeCrash(engine.Name, CrashBeforeCleaningToTerminal)
		}
	default:
	}

	engine.Status = result.Status

	// Invariant: a terminal phase (Stable or Stopped) implies
	// CurrentGeneration == ActiveGeneration. Modeled as
	// Inv_TerminalConsistency in formal/FireboltEngine.tla. Stable and
	// Stopped are structurally identical terminals differing only in
	// the surfaced name; the guard must catch a divergence in either.
	// The analogous negative-ActiveGeneration guard lives in engine_reconcile.go.
	if (engine.Status.Phase == computev1alpha1.PhaseStable ||
		engine.Status.Phase == computev1alpha1.PhaseStopped) &&
		engine.Status.CurrentGeneration != engine.Status.ActiveGeneration {
		panic(fmt.Sprintf(
			"BUG: Phase=%s with CurrentGeneration=%d != ActiveGeneration=%d for engine %s",
			engine.Status.Phase, engine.Status.CurrentGeneration, engine.Status.ActiveGeneration, engine.Name,
		))
	}

	if err := r.updateStatus(ctx, engine); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	return nil
}

func (r *FireboltEngineReconciler) ensureConfigMap(ctx context.Context, engine *computev1alpha1.FireboltEngine, want *corev1.ConfigMap) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	if err := controllerutil.SetControllerReference(engine, want, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating ConfigMap", "name", want.Name)
		return r.Create(ctx, want)
	}
	if err != nil {
		return err
	}

	if existing.Data[ConfigFileName] == want.Data[ConfigFileName] {
		return nil
	}
	log.Info("Updating ConfigMap", "name", want.Name)
	existing.Data = want.Data
	return r.Update(ctx, existing)
}

func (r *FireboltEngineReconciler) ensureService(ctx context.Context, engine *computev1alpha1.FireboltEngine, want *corev1.Service) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	if err := controllerutil.SetControllerReference(engine, want, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating Service", "name", want.Name)
		createErr := r.Create(ctx, want)
		if errors.IsAlreadyExists(createErr) {
			// The informer cache was stale — another reconcile already created
			// the resource. We skip the update path here because re-reading
			// from the same cache would likely still return NotFound. Any spec
			// drift will be corrected on the next reconcile once the cache
			// catches up (level-triggered convergence).
			return nil
		}
		return createErr
	}
	if err != nil {
		return err
	}

	existingGenLabel := existing.Spec.Selector[LabelGeneration]
	wantGenLabel := want.Spec.Selector[LabelGeneration]

	needsUpdate := (existing.Spec.ClusterIP != want.Spec.ClusterIP && want.Spec.ClusterIP != "") ||
		existing.Spec.PublishNotReadyAddresses != want.Spec.PublishNotReadyAddresses ||
		existingGenLabel != wantGenLabel

	if !needsUpdate {
		return nil
	}

	log.Info("Updating Service", "name", want.Name,
		"selectorGeneration", existingGenLabel+"→"+wantGenLabel)
	existing.Spec.Selector = want.Spec.Selector
	existing.Spec.PublishNotReadyAddresses = want.Spec.PublishNotReadyAddresses
	return r.Update(ctx, existing)
}

func (r *FireboltEngineReconciler) ensureStatefulSetResource(ctx context.Context, engine *computev1alpha1.FireboltEngine, want *appsv1.StatefulSet) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	if err := controllerutil.SetControllerReference(engine, want, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, existing)
	if errors.IsNotFound(err) {
		log.Info("Creating StatefulSet", "name", want.Name,
			"replicas", *want.Spec.Replicas,
			"image", want.Spec.Template.Spec.Containers[0].Image)
		createErr := r.Create(ctx, want)
		if errors.IsAlreadyExists(createErr) {
			// The informer cache was stale — another reconcile already created
			// the resource. We skip the update path here because re-reading
			// from the same cache would likely still return NotFound. Any spec
			// drift will be corrected on the next reconcile once the cache
			// catches up (level-triggered convergence).
			return nil
		}
		return createErr
	}
	if err != nil {
		return err
	}

	if stsSpecEqual(existing, want) {
		return nil
	}

	log.Info("Updating StatefulSet", "name", want.Name,
		"replicas", *want.Spec.Replicas,
		"image", want.Spec.Template.Spec.Containers[0].Image)
	existing.Spec.Replicas = want.Spec.Replicas
	existing.Spec.Template = want.Spec.Template
	return r.Update(ctx, existing)
}

func (r *FireboltEngineReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	var opts []client.DeleteOption
	if _, ok := obj.(*appsv1.StatefulSet); ok {
		// Foreground propagation: K8s GC deletes pods before removing the STS.
		// Without this, background deletion leaves orphaned pods Running+Ready,
		// which inflates pod counts seen by the test helper and the drain check.
		prop := metav1.DeletePropagationForeground
		opts = append(opts, &client.DeleteOptions{PropagationPolicy: &prop})
	}
	err := r.Delete(ctx, obj, opts...)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// stsSpecEqual compares only the fields we explicitly manage in buildStatefulSet,
// ignoring API-server-defaulted fields that would cause false mismatches with DeepEqual.
func stsSpecEqual(a, b *appsv1.StatefulSet) bool {
	if a.Spec.Replicas == nil || b.Spec.Replicas == nil || *a.Spec.Replicas != *b.Spec.Replicas {
		return false
	}

	aContainers := a.Spec.Template.Spec.Containers
	bContainers := b.Spec.Template.Spec.Containers
	if len(aContainers) == 0 || len(bContainers) == 0 {
		return false
	}
	ac, bc := aContainers[0], bContainers[0]

	if ac.Image != bc.Image {
		return false
	}
	if ac.ImagePullPolicy != bc.ImagePullPolicy {
		return false
	}
	if !reflect.DeepEqual(ac.Resources, bc.Resources) {
		return false
	}
	if !reflect.DeepEqual(ac.Env, bc.Env) {
		return false
	}
	if !reflect.DeepEqual(a.Spec.Template.Spec.NodeSelector, b.Spec.Template.Spec.NodeSelector) {
		return false
	}
	if !reflect.DeepEqual(a.Spec.Template.Spec.Tolerations, b.Spec.Template.Spec.Tolerations) {
		return false
	}
	if !reflect.DeepEqual(a.Spec.Template.Spec.Affinity, b.Spec.Template.Spec.Affinity) {
		return false
	}

	if a.Spec.Template.Spec.ServiceAccountName != b.Spec.Template.Spec.ServiceAccountName {
		return false
	}

	aTGPS := a.Spec.Template.Spec.TerminationGracePeriodSeconds
	bTGPS := b.Spec.Template.Spec.TerminationGracePeriodSeconds
	if (aTGPS == nil) != (bTGPS == nil) || (aTGPS != nil && *aTGPS != *bTGPS) {
		return false
	}

	if !reflect.DeepEqual(a.Spec.Template.Spec.SecurityContext, b.Spec.Template.Spec.SecurityContext) {
		return false
	}
	if !reflect.DeepEqual(ac.SecurityContext, bc.SecurityContext) {
		return false
	}

	if a.Annotations[AnnotationMetadataOverride] != b.Annotations[AnnotationMetadataOverride] {
		return false
	}

	return true
}
