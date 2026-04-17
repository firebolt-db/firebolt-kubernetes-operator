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
		return ctrl.Result{}, fmt.Errorf("getEngineState failed: %w", err)
	}

	// Resolve instance info. Only stable and creating phases need it (they
	// build ConfigMaps with multi_engine_endpoint / account_id). Switching,
	// draining, and cleaning operate on existing resources and must not be
	// blocked by a transient instance issue.
	var instanceInfo InstanceInfo
	needsInstance := engine.Status.Phase == "" ||
		engine.Status.Phase == computev1alpha1.PhaseStable ||
		engine.Status.Phase == computev1alpha1.PhaseCreating

	if needsInstance {
		instanceInfo, err = r.resolveInstanceInfo(ctx, engine)
		if err != nil {
			apimeta.SetStatusCondition(&engine.Status.Conditions, metav1.Condition{
				Type:               computev1alpha1.ConditionInstanceReady,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: engine.Generation,
				Reason:             "InstanceNotReady",
				Message:            err.Error(),
			})
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
