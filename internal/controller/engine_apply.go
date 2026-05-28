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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// The three ensure* functions below write through Server-Side Apply
// (client.Apply) with FieldManager OperatorFieldManager and
// ForceOwnership — same idiom as the instance-side resources in
// instance_gateway.go / instance_metadata.go / instance_postgres.go,
// with the design rationale documented in the file-level note above
// ensureGatewayConfigMap.
//
// The pre-SSA code carried a client-side equality check
// (stsSpecEqual, the ClusterIP+selector+PublishNotReadyAddresses
// trio, the ConfigMap content-hash comparison) that short-circuited
// the write when nothing changed; the apiserver's SSA short-circuit
// (no managedFields change → no resourceVersion bump → no rollout)
// covers that case server-side, so the client-side checks are now
// redundant and have been removed. stsMatchesSpec in
// engine_reconcile.go remains in place — it serves the orthogonal
// "do I need a new blue-green generation?" decision, which is taken
// before the apply call and is independent of the apply mechanism.
//
// SSA Apply is also upsert, so the previous IsAlreadyExists fallback
// (cache-staleness retry on Create) is no longer needed: a concurrent
// reconcile that already created the object simply means our Apply
// patches into the existing object.
func (r *FireboltEngineReconciler) ensureConfigMap(ctx context.Context, engine *computev1alpha1.FireboltEngine, want *corev1.ConfigMap) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	want.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	if err := controllerutil.SetControllerReference(engine, want, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}
	log.V(1).Info("Applying ConfigMap", "name", want.Name)
	return r.Patch(ctx, want, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
}

func (r *FireboltEngineReconciler) ensureService(ctx context.Context, engine *computev1alpha1.FireboltEngine, want *corev1.Service) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	want.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	if err := controllerutil.SetControllerReference(engine, want, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}
	log.V(1).Info("Applying Service", "name", want.Name,
		"selectorGeneration", want.Spec.Selector[LabelGeneration])
	return r.Patch(ctx, want, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
}

func (r *FireboltEngineReconciler) ensureStatefulSetResource(ctx context.Context, engine *computev1alpha1.FireboltEngine, want *appsv1.StatefulSet) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	want.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}
	if err := controllerutil.SetControllerReference(engine, want, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}
	log.V(1).Info("Applying StatefulSet", "name", want.Name,
		"replicas", *want.Spec.Replicas,
		"image", want.Spec.Template.Spec.Containers[0].Image)
	return r.Patch(ctx, want, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
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
