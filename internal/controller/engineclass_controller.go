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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// engineClassRequeueAfter is the steady-state safety-net requeue for
// the EngineClass reconciler. Engine create / update / delete events
// already enqueue the class reactively (via the FireboltEngine watch
// in SetupWithManager), so this requeue only kicks in if a watch
// event is missed; tighter than the engine reconciler's drift loop
// because status.boundEngines is the printcolumn / kubectl-describe
// surface users look at, and a stale count is mildly more confusing
// than a stale generation.
const engineClassRequeueAfter = 30 * time.Second

// EngineClassReconciler keeps EngineClass status in sync with cluster
// state. It writes status only; the operator never creates child
// resources for an EngineClass.
//
// Two status fields are maintained:
//
//   - BoundEngines: the count of FireboltEngines in the class's
//     namespace whose spec.engineClassRef names this class. EngineClass
//     is namespaced, so engines outside the class's namespace cannot
//     bind to it and are not counted. The deletion-blocking webhook
//     does its own live list (status can be stale across reconciles);
//     this value is purely a visibility surface. The count is
//     recomputed from scratch each reconcile by listing FireboltEngines
//     in the class's namespace.
//   - Conditions[Ready]: True when the class's spec.template passes
//     ValidateOperatorOwnedPodTemplate. Admission normally catches
//     offending specs, so Ready=False is reserved for classes admitted
//     under an older operator with a narrower rejection set — a defense
//     in depth, not an everyday signal.
type EngineClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Reconcile recomputes status for one EngineClass.
func (r *EngineClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("engineclass", req.Name)

	class := &computev1alpha1.EngineClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching EngineClass: %w", err)
	}

	bound, err := r.countBoundEngines(ctx, class.Namespace, class.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("counting bound engines: %w", err)
	}

	ready, reason, message := classReadiness(class)

	if !engineClassStatusEqual(class, bound, ready, reason, message) {
		class.Status.BoundEngines = bound
		class.Status.ObservedGeneration = class.Generation
		apimeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
			Type:               computev1alpha1.EngineClassConditionReady,
			Status:             ready,
			ObservedGeneration: class.Generation,
			Reason:             reason,
			Message:            message,
		})
		if err := r.Status().Update(ctx, class); err != nil {
			if errors.IsConflict(err) {
				// Another writer beat us; the next reconcile recomputes.
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("updating EngineClass status: %w", err)
		}
		log.V(1).Info("Updated EngineClass status", "boundEngines", bound, "ready", ready, "reason", reason)
	}

	return ctrl.Result{RequeueAfter: engineClassRequeueAfter}, nil
}

// countBoundEngines lists FireboltEngines in the class's namespace and
// counts those whose spec.engineClassRef matches className. EngineClass
// is namespaced, so engines outside this namespace cannot bind to it.
// The list path is O(N) in engines per namespace; that's acceptable at
// our scale (tens of engines per instance) and avoids a per-class index.
func (r *EngineClassReconciler) countBoundEngines(ctx context.Context, namespace, className string) (int32, error) {
	var engines computev1alpha1.FireboltEngineList
	if err := r.List(ctx, &engines, client.InNamespace(namespace)); err != nil {
		return 0, err
	}
	var count int32
	for i := range engines.Items {
		ref := engines.Items[i].Spec.EngineClassRef
		if ref != nil && *ref == className {
			count++
		}
	}
	return count, nil
}

// classReadiness derives the Ready condition from a defense-in-depth pass
// over spec.template using ValidateOperatorOwnedPodTemplate. Admission
// normally rejects offending specs at apply time, so the only realistic
// path to Ready=False is an upgrade where the operator-owned rejection
// set grew and an older-admitted EngineClass now contains a path that
// would be rejected today.
func classReadiness(class *computev1alpha1.EngineClass) (status metav1.ConditionStatus, reason, message string) {
	errs := computev1alpha1.ValidateOperatorOwnedPodTemplate(&class.Spec.Template, field.NewPath("spec", "template"))
	if len(errs) == 0 {
		return metav1.ConditionTrue, "Admissible", "spec.template contains no operator-owned paths"
	}
	return metav1.ConditionFalse, "OperatorOwnedFieldSet", errs.ToAggregate().Error()
}

// engineClassStatusEqual reports whether desired status matches what's
// already persisted, so the reconciler can skip a Status.Update when
// nothing has changed.
func engineClassStatusEqual(class *computev1alpha1.EngineClass, bound int32, ready metav1.ConditionStatus, reason, message string) bool {
	if class.Status.BoundEngines != bound {
		return false
	}
	if class.Status.ObservedGeneration != class.Generation {
		return false
	}
	cond := apimeta.FindStatusCondition(class.Status.Conditions, computev1alpha1.EngineClassConditionReady)
	if cond == nil {
		return false
	}
	return cond.Status == ready && cond.Reason == reason && cond.Message == message
}

// SetupWithManager registers the EngineClass controller. The controller
// watches its own CR for Create / Update / Delete and watches
// FireboltEngines so a referencing engine appearing or disappearing
// triggers an immediate boundEngines recount on the relevant class.
func (r *EngineClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.EngineClass{}).
		Watches(
			&computev1alpha1.FireboltEngine{},
			handler.EnqueueRequestsFromMapFunc(enqueueClassFromEngine),
		).
		Named("engineclass").
		Complete(r)
}

// enqueueClassFromEngine maps a FireboltEngine event back to a reconcile
// request for the EngineClass it references. Engines without a
// spec.engineClassRef produce no events. EngineClass is namespaced, so
// the request carries the engine's namespace — that's the namespace the
// class lives in too.
func enqueueClassFromEngine(_ context.Context, obj client.Object) []reconcile.Request {
	eng, ok := obj.(*computev1alpha1.FireboltEngine)
	if !ok {
		return nil
	}
	if eng.Spec.EngineClassRef == nil || *eng.Spec.EngineClassRef == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{
			Name:      *eng.Spec.EngineClassRef,
			Namespace: eng.Namespace,
		},
	}}
}
