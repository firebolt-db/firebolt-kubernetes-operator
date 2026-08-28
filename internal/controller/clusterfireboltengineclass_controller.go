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

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// clusterEngineClassFinalizerName guards ClusterFireboltEngineClass
// deletion while any FireboltEngine in any namespace has resolved to
// it (engineClassRef matches and no namespaced FireboltEngineClass of
// the same name exists in that engine's namespace).
const clusterEngineClassFinalizerName = "compute.firebolt.io/clusterfireboltengineclass-deletion-guard"

// ClusterFireboltEngineClassReconciler keeps ClusterFireboltEngineClass
// status in sync and manages the deletion-guard finalizer. It writes
// status and finalizers on the catalog object; the operator never
// creates child resources for it.
type ClusterFireboltEngineClassReconciler struct {
	client.Client
	// Reader reads engines and namespaced classes from the API server
	// for the deletion guard. Releasing a finalizer on a cache count
	// would let a catalog vanish while an engine outside
	// --watch-label-selector still resolves to it.
	Reader client.Reader
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=clusterfireboltengineclasses,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=clusterfireboltengineclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=clusterfireboltengineclasses/finalizers,verbs=update
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines,verbs=get;list;watch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengineclasses,verbs=get;list;watch

// Reconcile recomputes status for one ClusterFireboltEngineClass and
// manages the deletion-guard finalizer.
func (r *ClusterFireboltEngineClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("clusterfireboltengineclass", req.Name)

	class := &computev1alpha1.ClusterFireboltEngineClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching ClusterFireboltEngineClass: %w", err)
	}

	if !controllerutil.ContainsFinalizer(class, clusterEngineClassFinalizerName) {
		log.Info("Adding finalizer")
		controllerutil.AddFinalizer(class, clusterEngineClassFinalizerName)
		if err := r.Update(ctx, class); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if !class.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, class)
	}

	bound, err := countClusterBoundEngines(ctx, r.Client, class.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("counting bound engines: %w", err)
	}

	ready, reason, message := clusterClassReadiness(class)

	if !clusterEngineClassStatusEqual(class, bound, ready, reason, message) {
		class.Status.BoundEngines = bound
		class.Status.ObservedGeneration = class.Generation
		apimeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
			Type:               computev1alpha1.ClusterFireboltEngineClassConditionReady,
			Status:             ready,
			ObservedGeneration: class.Generation,
			Reason:             reason,
			Message:            message,
		})
		if err := r.Status().Update(ctx, class); err != nil {
			if errors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("updating ClusterFireboltEngineClass status: %w", err)
		}
		log.V(1).Info("Updated ClusterFireboltEngineClass status", "boundEngines", bound, "ready", ready, "reason", reason)
	}

	return ctrl.Result{RequeueAfter: engineClassRequeueAfter}, nil
}

func (r *ClusterFireboltEngineClassReconciler) reconcileDelete(ctx context.Context, class *computev1alpha1.ClusterFireboltEngineClass) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(class, clusterEngineClassFinalizerName) {
		return ctrl.Result{}, nil
	}

	bound, err := countClusterBoundEngines(ctx, r.Reader, class.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("counting bound engines during delete: %w", err)
	}

	if bound > 0 {
		message := fmt.Sprintf(
			"ClusterFireboltEngineClass %q is resolved by %d FireboltEngine(s); "+
				"clear spec.engineClassRef on those engines before deleting the catalog object",
			class.Name, bound)
		if !clusterEngineClassStatusEqual(class, bound, metav1.ConditionFalse, reasonDeletionBlocked, message) {
			class.Status.BoundEngines = bound
			class.Status.ObservedGeneration = class.Generation
			apimeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
				Type:               computev1alpha1.ClusterFireboltEngineClassConditionReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: class.Generation,
				Reason:             reasonDeletionBlocked,
				Message:            message,
			})
			if err := r.Status().Update(ctx, class); err != nil {
				if errors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, fmt.Errorf("updating ClusterFireboltEngineClass status: %w", err)
			}
			log.Info("Holding ClusterFireboltEngineClass finalizer", "boundEngines", bound)
		}
		return ctrl.Result{RequeueAfter: engineClassRequeueAfter}, nil
	}

	controllerutil.RemoveFinalizer(class, clusterEngineClassFinalizerName)
	if err := r.Update(ctx, class); err != nil {
		if errors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	log.Info("Released ClusterFireboltEngineClass deletion finalizer; no bound engines remain")
	return ctrl.Result{}, nil
}

// countClusterBoundEngines counts engines that have resolved to
// clusterName: spec.engineClassRef matches and no namespaced
// FireboltEngineClass of that name exists in the engine's namespace.
func countClusterBoundEngines(ctx context.Context, reader client.Reader, clusterName string) (int32, error) {
	var engines computev1alpha1.FireboltEngineList
	if err := reader.List(ctx, &engines); err != nil {
		return 0, err
	}
	var classes computev1alpha1.FireboltEngineClassList
	if err := reader.List(ctx, &classes); err != nil {
		return 0, err
	}
	overrideNS := map[string]struct{}{}
	for i := range classes.Items {
		if classes.Items[i].Name == clusterName {
			overrideNS[classes.Items[i].Namespace] = struct{}{}
		}
	}
	var count int32
	for i := range engines.Items {
		ref := engines.Items[i].Spec.EngineClassRef
		if ref == nil || *ref != clusterName {
			continue
		}
		if _, ok := overrideNS[engines.Items[i].Namespace]; ok {
			continue
		}
		count++
	}
	return count, nil
}

// clusterClassReadiness runs operator-owned-path and SKU-only checks.
func clusterClassReadiness(class *computev1alpha1.ClusterFireboltEngineClass) (status metav1.ConditionStatus, reason, message string) {
	base := field.NewPath("spec", "template")
	owned := computev1alpha1.ValidateOperatorOwnedPodTemplate(&class.Spec.Template, base)
	sku := computev1alpha1.ValidateClusterEngineClassSKUOnly(&class.Spec.Template, base)
	if len(owned) == 0 && len(sku) == 0 {
		return metav1.ConditionTrue, reasonAdmissible, "spec.template is SKU-only and contains no operator-owned paths"
	}
	if len(owned) == 0 {
		return metav1.ConditionFalse, reasonNamespaceResolvedFieldSet, sku.ToAggregate().Error()
	}
	owned = append(owned, sku...)
	return metav1.ConditionFalse, reasonOperatorOwnedFieldSet, owned.ToAggregate().Error()
}

func clusterEngineClassStatusEqual(class *computev1alpha1.ClusterFireboltEngineClass, bound int32, ready metav1.ConditionStatus, reason, message string) bool {
	if class.Status.BoundEngines != bound {
		return false
	}
	if class.Status.ObservedGeneration != class.Generation {
		return false
	}
	cond := apimeta.FindStatusCondition(class.Status.Conditions, computev1alpha1.ClusterFireboltEngineClassConditionReady)
	if cond == nil {
		return false
	}
	return cond.Status == ready && cond.Reason == reason && cond.Message == message
}

// SetupWithManager registers the ClusterFireboltEngineClass controller.
func (r *ClusterFireboltEngineClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.ClusterFireboltEngineClass{}).
		Watches(
			&computev1alpha1.FireboltEngine{},
			handler.EnqueueRequestsFromMapFunc(enqueueClusterClassFromEngine),
		).
		Watches(
			&computev1alpha1.FireboltEngineClass{},
			handler.EnqueueRequestsFromMapFunc(enqueueClusterClassFromNamespacedClass),
		).
		Named("clusterfireboltengineclass").
		Complete(r)
}

func enqueueClusterClassFromEngine(_ context.Context, obj client.Object) []reconcile.Request {
	eng, ok := obj.(*computev1alpha1.FireboltEngine)
	if !ok {
		return nil
	}
	if eng.Spec.EngineClassRef == nil || *eng.Spec.EngineClassRef == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: *eng.Spec.EngineClassRef},
	}}
}

func enqueueClusterClassFromNamespacedClass(_ context.Context, obj client.Object) []reconcile.Request {
	class, ok := obj.(*computev1alpha1.FireboltEngineClass)
	if !ok {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: class.Name},
	}}
}
