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
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/core-operator/api/v1alpha1"
)

const finalizerName = "compute.firebolt.io/engine-cleanup"

// FireboltEngineReconciler reconciles FireboltEngine objects by managing
// blue-green generational deployments of Firebolt Core StatefulSets.
type FireboltEngineReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Namespace  string
	RestConfig *rest.Config
	Clientset  *kubernetes.Clientset
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

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
		engine.Status.Phase = computev1alpha1.PhaseStable
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

	result := computeEngineReconcile(
		&engine.Spec,
		&engine.Status,
		current,
		engine.Name,
		engine.Namespace,
		engine.Generation,
	)

	if result.Status.Phase != engine.Status.Phase {
		log.Info("Phase transition", "from", engine.Status.Phase, "to", result.Status.Phase)
	}

	if err := r.applyEngineState(ctx, engine, result); err != nil {
		return ctrl.Result{}, fmt.Errorf("applyEngineState failed: %w", err)
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

	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList, client.InNamespace(ns), client.MatchingLabels{
		LabelEngine: engine.Name,
	}); err == nil {
		for i := range stsList.Items {
			log.Info("Deleting StatefulSet", "name", stsList.Items[i].Name)
			_ = r.Delete(ctx, &stsList.Items[i])
		}
	}

	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, client.InNamespace(ns), client.MatchingLabels{
		LabelEngine: engine.Name,
	}); err == nil {
		for i := range svcList.Items {
			log.Info("Deleting Service", "name", svcList.Items[i].Name)
			_ = r.Delete(ctx, &svcList.Items[i])
		}
	}

	cmList := &corev1.ConfigMapList{}
	if err := r.List(ctx, cmList, client.InNamespace(ns), client.MatchingLabels{
		LabelEngine: engine.Name,
	}); err == nil {
		for i := range cmList.Items {
			log.Info("Deleting ConfigMap", "name", cmList.Items[i].Name)
			_ = r.Delete(ctx, &cmList.Items[i])
		}
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

func genResourceName(engineName string, gen int, suffix string) string {
	return fmt.Sprintf("%s%s%d%s", engineName, SuffixGen, gen, suffix)
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
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named(name).
		Complete(r)
}
