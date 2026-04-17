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
	stderrors "errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

const finalizerName = "compute.firebolt.io/engine-cleanup"

// FireboltEngineReconciler reconciles FireboltEngine objects by managing
// blue-green generational deployments of Firebolt engine StatefulSets.
type FireboltEngineReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Namespace string
	// Clientset is used for the drain-check pod-proxy scrape
	// (Pods/proxy subresource). Populated in SetupWithManager if nil.
	Clientset *kubernetes.Clientset

	// InstanceFilter, when non-empty, restricts this reconciler to engines
	// referencing a single FireboltInstance (by spec.instanceRef). Requests
	// for engines bound to any other instance are dropped, and instance
	// watches are short-circuited so unrelated instance events do not fan
	// out. Intended for E2E tests that run multiple isolated operator
	// instances in the same namespace; in production this is left empty so
	// the reconciler processes every FireboltEngine it watches.
	InstanceFilter string

	// DisableGC disables the orphaned-generation garbage collector. When
	// true, the reconciler will not sweep resources from abandoned
	// generations. E2E tests set this to verify that the happy path never
	// produces orphans; only tests that explicitly exercise mid-flight
	// spec changes should enable GC.
	DisableGC bool
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/proxy,verbs=get
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// Reconcile reads the current engine state from the cluster, computes the
// reconcile actions needed, and applies them. Deletion is handled separately
// via a finalizer.
func (r *FireboltEngineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	engine := &computev1alpha1.FireboltEngine{}
	if err := r.Get(ctx, req.NamespacedName, engine); err != nil {
		if errors.IsNotFound(err) {
			log.Info("FireboltEngine deleted, skipping reconciliation")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if r.InstanceFilter != "" && engine.Spec.InstanceRef != r.InstanceFilter {
		return ctrl.Result{}, nil
	}

	log = log.WithValues("engine", engine.Name)

	if !controllerutil.ContainsFinalizer(engine, finalizerName) {
		log.Info("Adding finalizer")
		controllerutil.AddFinalizer(engine, finalizerName)
		if err := r.Update(ctx, engine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if !engine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, engine)
	}

	if engine.Status.Phase == "" {
		log.Info("Initializing engine status", "activeGeneration", -1)
		engine.Status.Phase = computev1alpha1.PhaseCreating
		engine.Status.ActiveGeneration = -1
		apimeta.SetStatusCondition(&engine.Status.Conditions, metav1.Condition{
			Type:               computev1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: engine.Generation,
			Reason:             "Initializing",
			Message:            "Engine status has not yet been populated",
		})
		if err := r.updateStatus(ctx, engine); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Reconciling engine",
		"phase", engine.Status.Phase,
		"currentGen", engine.Status.CurrentGeneration,
		"activeGen", engine.Status.ActiveGeneration,
	)

	current, err := r.getEngineState(ctx, engine)
	if err != nil {
		// Surface a drain-probe failure as a user-facing condition
		// BEFORE returning the error. Without this the reconciler
		// would back off silently and the only signal a cluster
		// admin would see is a PhaseDraining that never completes.
		// Non-drain errors (API unavailability, RBAC, cache misses)
		// are left alone: they are either transient and self-heal
		// on the next retry, or they affect every getter and a
		// bespoke condition would not help triage them.
		if setDrainCheckFailingCondition(&engine.Status, err, engine.Generation) {
			if updErr := r.updateStatus(ctx, engine); updErr != nil {
				log.Info("Failed to persist DrainCheckFailing condition; controller-runtime will retry the reconcile",
					"error", updErr)
			}
		}
		return ctrl.Result{}, fmt.Errorf("getEngineState failed: %w", err)
	}

	// Only PhaseStable and PhaseCreating actually consume InstanceInfo
	// (to render ConfigMaps with multi_engine_endpoint / account_id).
	// Switching / Draining / Cleaning operate on already-rendered
	// resources and are functionally independent of the FireboltInstance:
	// draining an old-gen pod does not need a metadata endpoint, cleaning
	// up our own resources does not need an account ID.
	//
	// We deliberately do NOT touch the FireboltInstance from those
	// phases - not even to refresh ConditionInstanceReady. Reasons:
	//
	//   1. Determinism. The engine's ability to finish its own
	//      lifecycle phases should not depend on a resource it does
	//      not need. A transient cache miss, a mid-deletion race, or
	//      a malformed spec.InstanceRef should not flip the engine's
	//      Ready reason to InstanceNotReady while a drain is
	//      progressing correctly - that is a triage red herring.
	//
	//   2. Freshness without polling. SetupWithManager registers a
	//      Watch on FireboltInstance (see instanceToEngines); every
	//      change to the referenced instance re-enqueues this engine.
	//      So the condition is refreshed reactively the next time we
	//      land in a needsInstance phase. The worst-case staleness
	//      is bounded by the duration of a blue-green rollout, and
	//      the condition only lies about a state the engine is not
	//      currently consuming.
	//
	//   3. Consumers already have a truer signal. The FireboltInstance
	//      itself carries Phase/Conditions; anyone asking "is the
	//      instance healthy?" should read that, not a
	//      mirrored-and-stale copy on the engine.
	//
	// Phase == "" is handled by the early-return above which initializes
	// status and requeues, so it cannot reach this point.
	needsInstance := engine.Status.Phase == computev1alpha1.PhaseStable ||
		engine.Status.Phase == computev1alpha1.PhaseCreating

	var instanceInfo InstanceInfo
	if needsInstance {
		var instanceErr error
		instanceInfo, instanceErr = r.resolveInstanceInfo(ctx, engine)
		if instanceErr != nil {
			apimeta.SetStatusCondition(&engine.Status.Conditions, metav1.Condition{
				Type:               computev1alpha1.ConditionInstanceReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: engine.Generation,
				Reason:             "InstanceNotReady",
				Message:            instanceErr.Error(),
			})
			setReadyCondition(&engine.Status, current, engine.Generation)
			if updateErr := r.updateStatus(ctx, engine); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		apimeta.SetStatusCondition(&engine.Status.Conditions, metav1.Condition{
			Type:               computev1alpha1.ConditionInstanceReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: engine.Generation,
			Reason:             "InstanceReady",
			Message:            "Referenced FireboltInstance is ready",
		})
	}

	result := computeEngineReconcile(
		&engine.Spec,
		&engine.Status,
		current,
		engine.Name,
		engine.Namespace,
		engine.Generation,
		instanceInfo,
	)

	if result.Status.Phase != engine.Status.Phase {
		log.Info("Phase transition", "from", engine.Status.Phase, "to", result.Status.Phase)
	}

	setReadyCondition(&result.Status, current, engine.Generation)

	if err := r.applyEngineState(ctx, engine, &result); err != nil {
		return ctrl.Result{}, fmt.Errorf("applyEngineState failed: %w", err)
	}

	if !r.DisableGC && engine.Status.Phase == computev1alpha1.PhaseStable {
		r.gcOrphanedResources(ctx, engine)
	}

	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: result.RequeueAfter,
	}, nil
}

// reconcileDelete removes all generation-scoped resources owned by this engine
// and then removes the finalizer to allow garbage collection.
func (r *FireboltEngineReconciler) reconcileDelete(ctx context.Context, engine *computev1alpha1.FireboltEngine) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)
	log.Info("Handling engine deletion")

	ns := engine.Namespace
	var errs []error

	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList, client.InNamespace(ns), client.MatchingLabels{
		LabelEngine: engine.Name,
	}); err != nil {
		log.Error(err, "Failed to list StatefulSets for cleanup")
		errs = append(errs, err)
	} else {
		for i := range stsList.Items {
			log.Info("Deleting StatefulSet", "name", stsList.Items[i].Name)
			if err := r.Delete(ctx, &stsList.Items[i]); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete StatefulSet", "name", stsList.Items[i].Name)
				errs = append(errs, err)
			}
		}
	}

	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, client.InNamespace(ns), client.MatchingLabels{
		LabelEngine: engine.Name,
	}); err != nil {
		log.Error(err, "Failed to list Services for cleanup")
		errs = append(errs, err)
	} else {
		for i := range svcList.Items {
			log.Info("Deleting Service", "name", svcList.Items[i].Name)
			if err := r.Delete(ctx, &svcList.Items[i]); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete Service", "name", svcList.Items[i].Name)
				errs = append(errs, err)
			}
		}
	}

	cmList := &corev1.ConfigMapList{}
	if err := r.List(ctx, cmList, client.InNamespace(ns), client.MatchingLabels{
		LabelEngine: engine.Name,
	}); err != nil {
		log.Error(err, "Failed to list ConfigMaps for cleanup")
		errs = append(errs, err)
	} else {
		for i := range cmList.Items {
			log.Info("Deleting ConfigMap", "name", cmList.Items[i].Name)
			if err := r.Delete(ctx, &cmList.Items[i]); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete ConfigMap", "name", cmList.Items[i].Name)
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return ctrl.Result{}, fmt.Errorf("cleanup failed with %d errors, first: %w", len(errs), errs[0])
	}

	controllerutil.RemoveFinalizer(engine, finalizerName)
	if err := r.Update(ctx, engine); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Finalizer removed, deletion complete")
	return ctrl.Result{}, nil
}

// updateStatus writes engine.Status back to the cluster with a one-shot
// conflict recovery: on 409 Conflict, re-GET the latest object and force
// the in-memory Status onto it.
//
// IMPORTANT invariant: the FireboltEngine /status subresource has a single
// writer, namely this reconciler. Controller-runtime's leader election
// ensures only one instance is active at a time, and the per-object
// work queue serializes all reconciles for the same engine, so the
// only legitimate source of 409 here is stale ResourceVersion caused
// by the cache lagging behind a previous status write we ourselves
// just made. In that case the in-memory Status is by definition the
// most recent intended state, and stomping it over the fresh object
// is safe.
//
// Adding a second writer to /status (a sidecar, a human running
// kubectl edit engine/... --subresource=status, a different
// controller) would break this assumption: we'd silently clobber
// their fields on every conflict. If that ever becomes a real use
// case, switch to a strategic-merge patch against the specific
// fields this reconciler owns rather than a whole-status Update.
//
// Scope: this invariant covers the /status subresource only. The
// reconciler also writes the main object (e.g. adding the finalizer
// in Reconcile and the delete path removing it), and those writes
// race with humans and other controllers that legitimately mutate
// spec/metadata. Conflicts on the main object are handled by
// returning the error and letting controller-runtime requeue - do
// NOT copy the force-overwrite pattern below to non-status writes.
func (r *FireboltEngineReconciler) updateStatus(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	now := metav1.Now()
	engine.Status.LastReconciled = &now
	err := r.Status().Update(ctx, engine)
	if !errors.IsConflict(err) {
		return err
	}
	fresh := &computev1alpha1.FireboltEngine{}
	if err := r.Get(ctx, types.NamespacedName{Name: engine.Name, Namespace: engine.Namespace}, fresh); err != nil {
		return err
	}
	fresh.Status = engine.Status
	return r.Status().Update(ctx, fresh)
}

// setReadyCondition derives the top-level ConditionReady from the
// engine's post-reconcile status and the observed cluster state, and
// writes it onto status.Conditions (idempotent via SetStatusCondition).
//
// Precondition: status.Phase is non-empty. The very first reconcile of
// a fresh FireboltEngine hits an early-return in Reconcile that seeds
// Phase=Creating and writes an "Initializing" ConditionReady inline
// (see the Phase == "" branch near the top of Reconcile), so by the
// time setReadyCondition runs the phase is always one of the declared
// EnginePhase values. Keeping the Initializing write out of here means
// there is exactly one place that emits it, which avoids silent drift
// between two copies of the same message.
//
// The precedence below is intentional: a higher-priority Reason masks
// every lower one, so the single condition users read gives them the
// most actionable signal. In particular:
//
//  1. InstanceNotReady first: nothing downstream will work until the
//     backing FireboltInstance is healthy, regardless of our phase.
//  2. Rolling: any non-terminal, non-stable phase (Creating / Switching
//     / Draining / Cleaning).
//  3. PodsNotReady: phase is Stable but the active-generation pods
//     haven't all reported Ready yet (e.g. image pull in progress on
//     a freshly scheduled replica). This is what distinguishes
//     ConditionReady from Phase==Stable: the latter can be true while
//     pods are still coming up.
//  4. Otherwise True: serving traffic, all replicas ready.
func setReadyCondition(
	status *computev1alpha1.FireboltEngineStatus,
	current EngineState,
	generation int64,
) {
	cond := metav1.Condition{
		Type:               computev1alpha1.ConditionReady,
		ObservedGeneration: generation,
	}
	switch {
	case !isInstanceConditionTrue(status.Conditions):
		cond.Status = metav1.ConditionFalse
		cond.Reason = "InstanceNotReady"
		cond.Message = "Referenced FireboltInstance is not ready"
	case status.Phase != computev1alpha1.PhaseStable:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Rolling"
		cond.Message = fmt.Sprintf("Engine is in %s phase", status.Phase)
	case !current.CurrentPodsReady:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PodsNotReady"
		cond.Message = fmt.Sprintf(
			"generation %d has %d ready pod(s); not all replicas are ready yet",
			status.ActiveGeneration, current.CurrentPodCount,
		)
	default:
		cond.Status = metav1.ConditionTrue
		cond.Reason = "EngineReady"
		cond.Message = fmt.Sprintf(
			"Engine is serving traffic on generation %d",
			status.ActiveGeneration,
		)
	}
	apimeta.SetStatusCondition(&status.Conditions, cond)
}

// setDrainCheckFailingCondition flips ConditionReady to False with
// Reason=DrainCheckFailing when err is (or wraps) a *DrainProbeError.
// Returns true if the condition was updated so the caller knows to
// persist status; returns false on any other error (or on nil), leaving
// conditions untouched.
//
// Why this is a separate branch from setReadyCondition: setReadyCondition
// only runs on the happy path, after getEngineState succeeded. A broken
// drain probe by definition makes getEngineState fail, so without this
// helper the most recent Ready reason (typically "Rolling") would stay
// pinned and a user would have no signal that the probe is stuck. Once
// the probe recovers, the next successful reconcile calls
// setReadyCondition and overwrites DrainCheckFailing with the
// appropriate Rolling/EngineReady reason - no explicit clear needed.
func setDrainCheckFailingCondition(status *computev1alpha1.FireboltEngineStatus, err error, generation int64) bool {
	var drainErr *DrainProbeError
	if !stderrors.As(err, &drainErr) {
		return false
	}
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               computev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "DrainCheckFailing",
		Message:            drainErr.Error(),
		ObservedGeneration: generation,
	})
	return true
}

// isInstanceConditionTrue reports whether ConditionInstanceReady is
// present AND True. A missing condition (no lookup yet this reconcile)
// is treated as "not True" so Ready does not briefly flip to True in
// the window between init and the first instance-resolve.
func isInstanceConditionTrue(conds []metav1.Condition) bool {
	c := apimeta.FindStatusCondition(conds, computev1alpha1.ConditionInstanceReady)
	return c != nil && c.Status == metav1.ConditionTrue
}

// resolveInstanceInfo looks up the FireboltInstance referenced by the engine's
// spec.instanceRef and returns its metadata endpoint and account ID.
// Reconciliation is blocked until the instance exists and has both fields populated.
func (r *FireboltEngineReconciler) resolveInstanceInfo(ctx context.Context, engine *computev1alpha1.FireboltEngine) (InstanceInfo, error) {
	inst := &computev1alpha1.FireboltInstance{}
	key := types.NamespacedName{Name: engine.Spec.InstanceRef, Namespace: engine.Namespace}
	if err := r.Get(ctx, key, inst); err != nil {
		if errors.IsNotFound(err) {
			return InstanceInfo{}, fmt.Errorf("FireboltInstance %q not found in namespace %s", engine.Spec.InstanceRef, engine.Namespace)
		}
		return InstanceInfo{}, fmt.Errorf("getting FireboltInstance %q: %w", engine.Spec.InstanceRef, err)
	}

	if inst.Status.MetadataEndpoint == "" {
		return InstanceInfo{}, fmt.Errorf("FireboltInstance %q has no metadata endpoint yet", inst.Name)
	}
	if inst.Spec.ID == "" {
		return InstanceInfo{}, fmt.Errorf("FireboltInstance %q has no instance ID yet", inst.Name)
	}

	return InstanceInfo{
		MetadataEndpoint: inst.Status.MetadataEndpoint,
		AccountID:        inst.Spec.ID,
	}, nil
}

func genResourceName(engineName string, gen int, suffix string) string {
	return fmt.Sprintf("%s%s%d%s", engineName, SuffixGen, gen, suffix)
}

// SetupWithManager sets up the controller with the Manager.
func (r *FireboltEngineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerNamed(mgr, "fireboltengine")
}

// SetupWithManagerNamed sets up the controller with the Manager using a custom controller name.
func (r *FireboltEngineReconciler) SetupWithManagerNamed(mgr ctrl.Manager, name string) error {
	if r.Clientset == nil {
		clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			return fmt.Errorf("failed to create clientset: %w", err)
		}
		r.Clientset = clientset
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltEngine{}).
		Watches(&computev1alpha1.FireboltInstance{},
			handler.EnqueueRequestsFromMapFunc(r.instanceToEngines)).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named(name).
		Complete(r)
}

// instanceToEngines maps a FireboltInstance event to reconcile requests for
// all engines in the same namespace that reference it via spec.instanceRef.
func (r *FireboltEngineReconciler) instanceToEngines(ctx context.Context, obj client.Object) []reconcile.Request {
	if r.InstanceFilter != "" && obj.GetName() != r.InstanceFilter {
		return nil
	}

	engineList := &computev1alpha1.FireboltEngineList{}
	if err := r.List(ctx, engineList, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list engines for instance watch")
		return nil
	}

	var requests []reconcile.Request
	for i := range engineList.Items {
		if engineList.Items[i].Spec.InstanceRef == obj.GetName() {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      engineList.Items[i].Name,
					Namespace: engineList.Items[i].Namespace,
				},
			})
		}
	}
	return requests
}
