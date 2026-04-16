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
	"time"

	"google.golang.org/grpc"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

const instanceFinalizerName = "compute.firebolt.io/instance-cleanup"

// FireboltInstanceReconciler reconciles FireboltInstance objects by deploying
// PostgreSQL, the metadata service, and the gateway.
type FireboltInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// DialMetadata, if non-nil, overrides the default gRPC dialer used by
	// account initialization. This is used in E2E tests where the operator
	// runs on the host and cannot resolve in-cluster DNS names.
	DialMetadata func(ctx context.Context, instance *computev1alpha1.FireboltInstance) (*grpc.ClientConn, func(), error)
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures the PostgreSQL, metadata service, account, and gateway
// components described by a FireboltInstance are running and healthy.
func (r *FireboltInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("instance", req.Name)

	instance := &computev1alpha1.FireboltInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(instance, instanceFinalizerName) {
		controllerutil.AddFinalizer(instance, instanceFinalizerName)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, instance)
	}

	if instance.Status.Phase == "" {
		instance.Status.Phase = computev1alpha1.InstancePhaseProvisioning
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Step 1: Ensure PostgreSQL (native, when no external PG is configured)
	if instance.Spec.Metadata.Postgres == nil {
		if err := r.ensurePostgreSQL(ctx, instance); err != nil {
			log.Error(err, "Failed to ensure PostgreSQL")
			return r.writeStatusAndRequeue(ctx, instance)
		}
		pgReady, err := r.isPostgresReady(ctx, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !pgReady {
			log.Info("PostgreSQL not ready yet, requeueing", "instance", instance.Name)
			instance.Status.Phase = r.computePhase(instance)
			if err := r.Status().Update(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	// Step 2: Ensure metadata service (native Go resources)
	if err := r.ensureMetadataResources(ctx, instance); err != nil {
		log.Error(err, "Failed to ensure metadata service")
		return r.writeStatusAndRequeue(ctx, instance)
	}

	// Step 3: Check metadata readiness
	ready, err := r.isMetadataServiceReady(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		log.Info("Metadata service not ready yet, requeueing")
		instance.Status.MetadataReady = false
		instance.Status.MetadataEndpoint = ""
		instance.Status.Phase = r.computePhase(instance)
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	instance.Status.MetadataReady = true
	instance.Status.MetadataEndpoint = metadataServiceEndpoint(instance.Name, instance.Namespace)

	// Step 4: Account initialization
	accountID, err := r.ensureAccountInitialized(ctx, instance)
	if err != nil {
		log.Error(err, "Failed to ensure account initialization")
		return r.writeStatusAndRequeue(ctx, instance)
	}
	instance.Status.AccountID = accountID

	// Step 5: Ensure gateway (native Go resources)
	if err := r.ensureGatewayResources(ctx, instance); err != nil {
		log.Error(err, "Failed to ensure gateway")
		return r.writeStatusAndRequeue(ctx, instance)
	}

	gwReady, err := r.isGatewayReady(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	instance.Status.GatewayReady = gwReady
	if gwReady {
		instance.Status.GatewayEndpoint = fmt.Sprintf("%s%s.%s.svc.cluster.local",
			instance.Name, SuffixGateway, instance.Namespace)
	} else {
		instance.Status.GatewayEndpoint = ""
	}

	instance.Status.Phase = r.computePhase(instance)

	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *FireboltInstanceReconciler) reconcileDelete(ctx context.Context, instance *computev1alpha1.FireboltInstance) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("instance", instance.Name)
	log.Info("Handling instance deletion")

	ns := instance.Namespace
	matchLabels := client.MatchingLabels{LabelInstance: instance.Name}
	var errs []error

	deleteList := func(list client.ObjectList, kind string) {
		if err := r.List(ctx, list, client.InNamespace(ns), matchLabels); err != nil {
			log.Error(err, "Failed to list resources for cleanup", "kind", kind)
			errs = append(errs, err)
			return
		}
		items := extractItems(list)
		for i := range items {
			log.Info("Deleting resource", "kind", kind, "name", items[i].GetName())
			if err := r.Delete(ctx, items[i]); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete resource", "kind", kind, "name", items[i].GetName())
				errs = append(errs, err)
			}
		}
	}

	deleteList(&appsv1.StatefulSetList{}, "StatefulSet")
	deleteList(&appsv1.DeploymentList{}, "Deployment")
	deleteList(&corev1.ServiceList{}, "Service")
	deleteList(&corev1.ConfigMapList{}, "ConfigMap")
	deleteList(&corev1.SecretList{}, "Secret")
	deleteList(&corev1.PersistentVolumeClaimList{}, "PersistentVolumeClaim")
	deleteList(&policyv1.PodDisruptionBudgetList{}, "PodDisruptionBudget")

	if len(errs) > 0 {
		return ctrl.Result{}, fmt.Errorf("cleanup failed with %d errors, first: %w", len(errs), errs[0])
	}

	controllerutil.RemoveFinalizer(instance, instanceFinalizerName)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Instance deletion complete")
	return ctrl.Result{}, nil
}

// extractItems returns the individual objects from a typed list. This avoids
// reflection and keeps the helper type-safe for the resource kinds used in
// reconcileDelete.
func extractItems(list client.ObjectList) []client.Object {
	switch l := list.(type) {
	case *appsv1.StatefulSetList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *appsv1.DeploymentList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *corev1.ServiceList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *corev1.ConfigMapList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *corev1.SecretList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *corev1.PersistentVolumeClaimList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *policyv1.PodDisruptionBudgetList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	default:
		return nil
	}
}

// writeStatusAndRequeue persists the current in-memory status (including any
// fields set earlier in the reconcile loop) and requeues. This ensures that
// partial progress (e.g., MetadataReady becoming true) is visible to dependent
// engines even when a later step fails.
const statusRequeueInterval = 10 * time.Second

func (r *FireboltInstanceReconciler) writeStatusAndRequeue(ctx context.Context, instance *computev1alpha1.FireboltInstance) (ctrl.Result, error) {
	instance.Status.Phase = r.computePhase(instance)
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: statusRequeueInterval}, nil
}

// computePhase derives the instance phase from the current readiness signals.
// Failed is terminal and is never overwritten by this function.
// Provisioning → Ready once all components are healthy.
// Ready → Degraded when any component becomes unhealthy.
// Degraded → Ready when all components recover.
func (r *FireboltInstanceReconciler) computePhase(instance *computev1alpha1.FireboltInstance) computev1alpha1.InstancePhase {
	if instance.Status.Phase == computev1alpha1.InstancePhaseFailed {
		return computev1alpha1.InstancePhaseFailed
	}

	allReady := instance.Status.MetadataReady && instance.Status.GatewayReady

	if allReady {
		return computev1alpha1.InstancePhaseReady
	}

	if instance.Status.Phase == computev1alpha1.InstancePhaseReady ||
		instance.Status.Phase == computev1alpha1.InstancePhaseDegraded {
		return computev1alpha1.InstancePhaseDegraded
	}

	return computev1alpha1.InstancePhaseProvisioning
}

func (r *FireboltInstanceReconciler) isPostgresReady(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	name := pgResourceName(instance.Name)
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return sts.Status.ReadyReplicas > 0, nil
}

func (r *FireboltInstanceReconciler) isMetadataServiceReady(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	name := instance.Name + SuffixMetadataService
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &dep); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas > 0, nil
}

func metadataServiceEndpoint(instanceName, namespace string) string {
	return fmt.Sprintf("%s%s.%s.svc.cluster.local:%d",
		instanceName, SuffixMetadataService, namespace, MetadataServicePort)
}

// instanceLabels returns the standard labels for resources owned by this instance.
func instanceLabels(instanceName, component string) map[string]string {
	return map[string]string{
		LabelInstance:  instanceName,
		LabelComponent: component,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *FireboltInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltInstance{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named("fireboltinstance").
		Complete(r)
}
