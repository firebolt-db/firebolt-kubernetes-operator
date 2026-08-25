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
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// enginePresetFinalizerName guards FireboltEnginePreset deletion while
// at least one FireboltEngine exists in the same namespace. Preset is
// ambient — every engine in the namespace merges it — so the guard is
// the namespace engine count, not a named reference. The validating
// webhook gives synchronous apply-time rejection; this finalizer is the
// webhooks-off counterpart. Force-remove via
// `kubectl patch metadata.finalizers` is the escape hatch.
const enginePresetFinalizerName = "compute.firebolt.io/fireboltenginepresets-deletion-guard"

const enginePresetRequeueAfter = 30 * time.Second

// FireboltEnginePresetReconciler keeps FireboltEnginePreset status in
// sync and manages the deletion-guard finalizer. It writes status and
// finalizers on the Preset object itself; the operator never creates
// child resources for it.
type FireboltEnginePresetReconciler struct {
	client.Client
	// Reader reads FireboltEngines from the API server for the deletion
	// guard so a --watch-label-selector cannot hide an engine that still
	// consumes the overlay.
	Reader client.Reader
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltenginepresets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltenginepresets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltenginepresets/finalizers,verbs=update
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines,verbs=get;list;watch

// Reconcile keeps FireboltEnginePreset status and the deletion-guard
// finalizer in sync with the engines in the same namespace.
func (r *FireboltEnginePresetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("fireboltenginepreset", req.Name)

	defaults := &computev1alpha1.FireboltEnginePreset{}
	if err := r.Get(ctx, req.NamespacedName, defaults); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching FireboltEnginePreset: %w", err)
	}

	if !controllerutil.ContainsFinalizer(defaults, enginePresetFinalizerName) {
		log.Info("Adding finalizer")
		controllerutil.AddFinalizer(defaults, enginePresetFinalizerName)
		if err := r.Update(ctx, defaults); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if !defaults.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, defaults)
	}

	bound, err := r.countNamespaceEngines(ctx, r.Client, defaults.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("counting namespace engines: %w", err)
	}

	ready, reason, message := presetReadiness(defaults)

	if !enginePresetStatusEqual(defaults, bound, ready, reason, message) {
		defaults.Status.BoundEngines = bound
		defaults.Status.ObservedGeneration = defaults.Generation
		apimeta.SetStatusCondition(&defaults.Status.Conditions, metav1.Condition{
			Type:               computev1alpha1.FireboltEnginePresetConditionReady,
			Status:             ready,
			ObservedGeneration: defaults.Generation,
			Reason:             reason,
			Message:            message,
		})
		if err := r.Status().Update(ctx, defaults); err != nil {
			if errors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("updating FireboltEnginePreset status: %w", err)
		}
		log.V(1).Info("Updated FireboltEnginePreset status", "boundEngines", bound, "ready", ready, "reason", reason)
	}

	return ctrl.Result{RequeueAfter: enginePresetRequeueAfter}, nil
}

func (r *FireboltEnginePresetReconciler) reconcileDelete(ctx context.Context, defaults *computev1alpha1.FireboltEnginePreset) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(defaults, enginePresetFinalizerName) {
		return ctrl.Result{}, nil
	}

	bound, err := r.countNamespaceEngines(ctx, r.Reader, defaults.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("counting namespace engines during delete: %w", err)
	}

	if bound > 0 {
		message := fmt.Sprintf(
			"%d FireboltEngine(s) in namespace %q consume FireboltEnginePreset %q as their ambient overlay; "+
				"delete those engines before deleting the Preset object",
			bound, defaults.Namespace, defaults.Name)
		if !enginePresetStatusEqual(defaults, bound, metav1.ConditionFalse, reasonDeletionBlocked, message) {
			defaults.Status.BoundEngines = bound
			defaults.Status.ObservedGeneration = defaults.Generation
			apimeta.SetStatusCondition(&defaults.Status.Conditions, metav1.Condition{
				Type:               computev1alpha1.FireboltEnginePresetConditionReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: defaults.Generation,
				Reason:             reasonDeletionBlocked,
				Message:            message,
			})
			if err := r.Status().Update(ctx, defaults); err != nil {
				if errors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, fmt.Errorf("updating FireboltEnginePreset status: %w", err)
			}
			log.Info("Holding FireboltEnginePreset finalizer", "boundEngines", bound)
		}
		return ctrl.Result{RequeueAfter: enginePresetRequeueAfter}, nil
	}

	controllerutil.RemoveFinalizer(defaults, enginePresetFinalizerName)
	if err := r.Update(ctx, defaults); err != nil {
		if errors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	log.Info("Released FireboltEnginePreset deletion finalizer; no engines remain in the namespace")
	return ctrl.Result{}, nil
}

func (r *FireboltEnginePresetReconciler) countNamespaceEngines(ctx context.Context, reader client.Reader, namespace string) (int32, error) {
	var engines computev1alpha1.FireboltEngineList
	if err := reader.List(ctx, &engines, client.InNamespace(namespace)); err != nil {
		return 0, err
	}
	return int32(len(engines.Items)), nil //nolint:gosec // a namespace cannot hold 2^31 engines
}

func presetReadiness(defaults *computev1alpha1.FireboltEnginePreset) (status metav1.ConditionStatus, reason, message string) {
	errs := computev1alpha1.ValidateOperatorOwnedPodTemplate(&defaults.Spec.Template, field.NewPath("spec", "template"))
	if len(errs) == 0 {
		return metav1.ConditionTrue, reasonAdmissible, "spec.template contains no operator-owned paths"
	}
	return metav1.ConditionFalse, reasonOperatorOwnedFieldSet, errs.ToAggregate().Error()
}

func enginePresetStatusEqual(defaults *computev1alpha1.FireboltEnginePreset, bound int32, ready metav1.ConditionStatus, reason, message string) bool {
	if defaults.Status.BoundEngines != bound {
		return false
	}
	if defaults.Status.ObservedGeneration != defaults.Generation {
		return false
	}
	cond := apimeta.FindStatusCondition(defaults.Status.Conditions, computev1alpha1.FireboltEnginePresetConditionReady)
	if cond == nil {
		return false
	}
	return cond.Status == ready && cond.Reason == reason && cond.Message == message
}

// SetupWithManager registers the FireboltEnginePreset controller and
// a watch on FireboltEngine so create/delete events recount BoundEngines.
func (r *FireboltEnginePresetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltEnginePreset{}).
		Watches(
			&computev1alpha1.FireboltEngine{},
			handler.EnqueueRequestsFromMapFunc(r.enqueuePresetFromEngine),
		).
		Named("fireboltenginepreset").
		Complete(r)
}

// enqueuePresetFromEngine maps a FireboltEngine event to a reconcile
// of every FireboltEnginePreset in the engine's namespace. Preset is
// ambient, so any engine create/delete changes BoundEngines.
func (r *FireboltEnginePresetReconciler) enqueuePresetFromEngine(ctx context.Context, obj client.Object) []reconcile.Request {
	eng, ok := obj.(*computev1alpha1.FireboltEngine)
	if !ok {
		return nil
	}
	var list computev1alpha1.FireboltEnginePresetList
	if err := r.List(ctx, &list, client.InNamespace(eng.Namespace)); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKey{
			Name:      list.Items[i].Name,
			Namespace: list.Items[i].Namespace,
		}})
	}
	return reqs
}
