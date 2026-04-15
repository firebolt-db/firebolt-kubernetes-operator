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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
	helmutil "github.com/firebolt-analytics/firebolt-kubernetes-operator/internal/helm"
)

const instanceFinalizerName = "compute.firebolt.io/instance-cleanup"

// FireboltInstanceReconciler reconciles FireboltInstance objects by deploying
// PostgreSQL, the metadata service, and the gateway.
type FireboltInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	MetadataChartSource string
	GatewayChartSource  string
	ChartCache          *helmutil.ChartCache
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
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// Step 2: Ensure metadata service (via Helm chart)
	if err := r.ensureMetadataService(ctx, instance); err != nil {
		log.Error(err, "Failed to ensure metadata service")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
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
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if instance.Status.AccountID != accountID {
		instance.Status.AccountID = accountID
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Step 5: Ensure Gateway (via Helm chart)
	if err := r.ensureGateway(ctx, instance); err != nil {
		log.Error(err, "Failed to ensure Gateway")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
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
	default:
		return nil
	}
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

// manifestHash returns a truncated SHA-256 hash of the given content.
func manifestHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])[:16]
}

// applyRenderedManifest decodes a multi-document YAML string and creates or
// updates each resource. Resources are labelled with the instance name and
// annotated with a content hash to skip no-op updates.
func (r *FireboltInstanceReconciler) applyRenderedManifest(ctx context.Context, instance *computev1alpha1.FireboltInstance, manifest string) error {
	log := logf.FromContext(ctx)
	hash := manifestHash(manifest)

	decoder := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decoding manifest: %w", err)
		}

		if obj.GetKind() == "" {
			continue
		}

		obj.SetNamespace(instance.Namespace)

		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[LabelInstance] = instance.Name
		obj.SetLabels(labels)

		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[AnnotationManifestHash] = hash
		obj.SetAnnotations(annotations)

		if err := controllerutil.SetControllerReference(instance, obj, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference on %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GroupVersionKind())
		err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, obj); err != nil {
				return fmt.Errorf("creating %s/%s: %w", obj.GetKind(), obj.GetName(), err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("getting %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}

		existingHash := existing.GetAnnotations()[AnnotationManifestHash]
		if existingHash == hash {
			continue
		}

		log.Info("Updating instance resource",
			"kind", obj.GetKind(), "name", obj.GetName())

		for key, value := range obj.Object {
			if key == "metadata" || key == "status" {
				continue
			}
			existing.Object[key] = value
		}

		existingAnnotations := existing.GetAnnotations()
		if existingAnnotations == nil {
			existingAnnotations = make(map[string]string)
		}
		existingAnnotations[AnnotationManifestHash] = hash
		existing.SetAnnotations(existingAnnotations)

		existingLabels := existing.GetLabels()
		if existingLabels == nil {
			existingLabels = make(map[string]string)
		}
		for k, v := range obj.GetLabels() {
			existingLabels[k] = v
		}
		existing.SetLabels(existingLabels)

		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *FireboltInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.ChartCache == nil {
		r.ChartCache = helmutil.NewChartCache()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltInstance{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named("fireboltinstance").
		Complete(r)
}
