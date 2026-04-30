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
	"crypto/rand"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

const instanceFinalizerName = "compute.firebolt.io/instance-cleanup"

// errPostgresSecretRefEmpty is returned at runtime when the webhook is
// bypassed and an instance still has an empty credentialsSecretRef.Name.
// Normally the validating webhook rejects this at admission time.
var errPostgresSecretRefEmpty = stderrors.New(
	"spec.metadata.postgres.credentialsSecretRef.name is empty",
)

// FireboltInstanceReconciler reconciles FireboltInstance objects by deploying
// PostgreSQL, the metadata service, and the gateway.
type FireboltInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MetricsRecorder records Prometheus metrics for instance CRs.
	// Must be non-nil; use metrics.NoOpInstanceRecorder{} in tests.
	MetricsRecorder metrics.InstanceRecorder

	// NameFilter, when non-empty, restricts this reconciler to a single
	// FireboltInstance by name. Requests for any other instance are dropped.
	// Intended for E2E tests that run multiple isolated operator instances
	// in the same namespace; in production this is left empty so the
	// reconciler processes every FireboltInstance it watches.
	NameFilter string
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures the PostgreSQL, metadata service, and gateway components
// described by a FireboltInstance are running and healthy.
func (r *FireboltInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if r.NameFilter != "" && req.Name != r.NameFilter {
		return ctrl.Result{}, nil
	}

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

	// Fallback for when the mutating webhook is disabled (local dev, E2E).
	// In production the webhook sets spec.id atomically at admission time and
	// enforces immutability; this branch never fires in that case.
	if instance.Spec.ID == "" {
		instance.Spec.ID = ulid.MustNew(ulid.Now(), rand.Reader).String()
		log.Info("Generated instance ID", "id", instance.Spec.ID)
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

	// InstancePhaseFailed is terminal. The long RequeueAfter is a safety
	// net: owned-object events will also re-enqueue, so this poll only
	// matters if the human edits the status (e.g. kubectl patch) without
	// touching any watched resource.
	if instance.Status.Phase == computev1alpha1.InstancePhaseFailed {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Step 1: Ensure PostgreSQL and metadata in the same reconcile pass.
	// Postgres and metadata are not separate phases: the metadata service
	// retries its DB connection internally for up to ~60s on startup, which
	// comfortably covers the time the postgres StatefulSet needs to become
	// ready on a fresh provisioning. Applying both resources concurrently
	// and letting the metadata-readiness check at Step 2 gate the whole
	// stack is enough. This mirrors firebolt-instance-helm, which has no
	// Helm hook ordering postgres ahead of metadata. There is no separate
	// PostgresReady condition for the same reason — a metadata pod that
	// cannot reach Postgres surfaces in the MetadataReady condition's
	// Reason/Message.
	if instance.Spec.Metadata.Postgres == nil {
		if err := r.ensurePostgreSQL(ctx, instance); err != nil {
			return r.failWithCondition(ctx, instance,
				computev1alpha1.InstanceConditionMetadataReady, "PostgresEnsureFailed", err)
		}
	} else {
		// External Postgres: make sure the user-referenced credentials
		// Secret actually exists before we roll a Deployment that mounts
		// it. Without this pre-flight the metadata pod gets scheduled,
		// kubelet fails to mount a missing Secret, and the pod sits in
		// ContainerCreating with the root cause visible only in the pod
		// events — invisible from the FireboltInstance CR.
		if err := r.checkExternalPostgresSecret(ctx, instance); err != nil {
			instance.Status.MetadataReady = false
			instance.Status.MetadataEndpoint = ""
			return r.failWithCondition(ctx, instance,
				computev1alpha1.InstanceConditionMetadataReady, "PostgresSecretPreflightFailed", err)
		}
	}

	// Step 2: Ensure metadata service (native Go resources)
	if err := r.ensureMetadataResources(ctx, instance); err != nil {
		return r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionMetadataReady, "EnsureFailed", err)
	}

	// Step 3: Check metadata readiness
	ready, err := r.isMetadataServiceReady(ctx, instance)
	if err != nil {
		return r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionMetadataReady, "ProbeFailed", err)
	}
	if !ready {
		log.Info("Metadata service not ready yet, requeueing")
		instance.Status.MetadataReady = false
		instance.Status.MetadataEndpoint = ""
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionMetadataReady, metav1.ConditionFalse,
			"Provisioning", "metadata Deployment has no ready replicas yet")
		return r.writeStatusAndPoll(ctx, instance, 5*time.Second)
	}

	instance.Status.MetadataReady = true
	instance.Status.MetadataEndpoint = metadataServiceEndpoint(instance.Name, instance.Namespace)
	setInstanceCondition(instance,
		computev1alpha1.InstanceConditionMetadataReady, metav1.ConditionTrue,
		"Ready", "metadata Deployment has at least one ready replica")

	// Step 4: Ensure gateway (native Go resources)
	if err := r.ensureGatewayResources(ctx, instance); err != nil {
		return r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionGatewayReady, "EnsureFailed", err)
	}

	gwReady, err := r.isGatewayReady(ctx, instance)
	if err != nil {
		return r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionGatewayReady, "ProbeFailed", err)
	}
	instance.Status.GatewayReady = gwReady
	if gwReady {
		instance.Status.GatewayEndpoint = fmt.Sprintf("%s%s.%s.svc.cluster.local",
			instance.Name, SuffixGateway, instance.Namespace)
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionGatewayReady, metav1.ConditionTrue,
			"Ready", "gateway Deployment has at least one ready replica")
	} else {
		instance.Status.GatewayEndpoint = ""
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionGatewayReady, metav1.ConditionFalse,
			"Provisioning", "gateway Deployment has no ready replicas yet")
	}

	setInstanceReadyRollup(instance)
	instance.Status.Phase = r.computePhase(instance)

	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	r.MetricsRecorder.Record(instance)

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
	deleteList(&corev1.ServiceAccountList{}, "ServiceAccount")
	deleteList(&rbacv1.RoleBindingList{}, "RoleBinding")
	deleteList(&rbacv1.RoleList{}, "Role")

	if len(errs) > 0 {
		return ctrl.Result{}, fmt.Errorf("cleanup failed with %d errors, first: %w", len(errs), errs[0])
	}

	controllerutil.RemoveFinalizer(instance, instanceFinalizerName)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	r.MetricsRecorder.Delete(instance.Namespace, instance.Name)

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
	case *corev1.ServiceAccountList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *rbacv1.RoleList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	case *rbacv1.RoleBindingList:
		out := make([]client.Object, len(l.Items))
		for i := range l.Items {
			out[i] = &l.Items[i]
		}
		return out
	default:
		return nil
	}
}

// checkExternalPostgresSecret verifies the Secret referenced by
// spec.metadata.postgres.credentialsSecretRef exists in the instance's
// namespace. It does NOT inspect the Secret's data (key presence,
// formatting, rotation): users who mis-key the Secret will still hit a
// crash-loop on the metadata pod itself, but the far more common
// mistakes — typoed Secret name, forgotten Secret creation, deleted
// Secret — are caught here with a message that names the missing Secret.
//
// Admission-time webhook validation already rejects empty
// credentialsSecretRef.Name; this function guards against the runtime
// case where the Name is set but the Secret does not (yet) exist.
func (r *FireboltInstanceReconciler) checkExternalPostgresSecret(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	pg := instance.Spec.Metadata.Postgres
	if pg == nil {
		return nil
	}
	name := pg.CredentialsSecretRef.Name
	if name == "" {
		return errPostgresSecretRefEmpty
	}
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: name}, &secret)
	if err == nil {
		return nil
	}
	if errors.IsNotFound(err) {
		return fmt.Errorf("external postgres credentials secret %s/%s not found", instance.Namespace, name)
	}
	return fmt.Errorf("getting external postgres credentials secret %s/%s: %w", instance.Namespace, name, err)
}

// writeStatusAndPoll persists the current in-memory status and schedules a
// fixed-interval poll. Use this for "condition is False but no error
// occurred" transient states (e.g. waiting for pods to report Ready). An
// exponential backoff would be wrong here: the polled signal becomes True
// on an event that is NOT tied to reconcile retries (pod readiness
// transition, external Secret creation), so the poll interval should stay
// short regardless of how many times we have already looped.
//
// For actual errors, use failWithCondition instead; it returns the error to
// controller-runtime so its work-queue rate-limiter applies exponential
// backoff.
func (r *FireboltInstanceReconciler) writeStatusAndPoll(
	ctx context.Context,
	instance *computev1alpha1.FireboltInstance,
	every time.Duration,
) (ctrl.Result, error) {
	// Order matters: computePhase reads InstanceConditionReady, so the
	// roll-up must be refreshed first. See computePhase godoc.
	setInstanceReadyRollup(instance)
	instance.Status.Phase = r.computePhase(instance)
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	r.MetricsRecorder.Record(instance)
	return ctrl.Result{RequeueAfter: every}, nil
}

// failWithCondition records a per-component condition as False, refreshes the
// roll-up Ready condition, persists the status best-effort, and returns the
// original error to controller-runtime so its exponential work-queue backoff
// applies to retries. This replaces the previous "log.Error + requeue-after-
// 10s with nil error" pattern, which silently capped retry frequency,
// hid failures from controller-runtime metrics, and never populated any
// user-visible condition explaining the failure.
//
// The status-write error is logged and deliberately NOT returned: we want
// the caller to see the ORIGINAL root-cause error (that is what the user
// needs to debug and what controller-runtime should back off on). A
// subsequent reconcile will retry the status write; if status writes are
// persistently failing, unrelated code paths that do `return ctrl.Result{},
// err` for status updates will surface that failure mode directly. Joining
// both errors would make the returned error message less focused and is
// not worth the trade-off given this pattern is called only on the failure
// path.
func (r *FireboltInstanceReconciler) failWithCondition(
	ctx context.Context,
	instance *computev1alpha1.FireboltInstance,
	condType, reason string,
	err error,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	setInstanceCondition(instance, condType, metav1.ConditionFalse, reason, err.Error())
	// Order matters: computePhase reads InstanceConditionReady, so the
	// roll-up must be refreshed first. See computePhase godoc.
	setInstanceReadyRollup(instance)
	instance.Status.Phase = r.computePhase(instance)
	if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
		log.Error(updateErr, "Failed to persist failure condition",
			"condition", condType, "reason", reason, "originalError", err.Error())
	}
	return ctrl.Result{}, fmt.Errorf("%s (%s): %w", condType, reason, err)
}

// setInstanceCondition writes a condition on the instance's status.
// apimeta.SetStatusCondition dedupes internally: when Type/Status/Reason/
// Message all match, LastTransitionTime is not bumped, so repeated calls
// with the same values do not generate /status churn or spam watchers.
func setInstanceCondition(
	instance *computev1alpha1.FireboltInstance,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: instance.Generation,
	})
}

// setInstanceReadyRollup recomputes InstanceConditionReady from the
// per-component conditions. Ready is True iff every required component
// condition is present AND True; otherwise False with the Reason/Message
// of the FIRST not-True component in pipeline order (Metadata →
// Gateway). Propagating the first blocker's Reason makes
// `kubectl describe fireboltinstance` surface the actual root cause on
// the headline condition, so users do not have to scan every condition
// to find the one that is False.
func setInstanceReadyRollup(instance *computev1alpha1.FireboltInstance) {
	components := []string{
		computev1alpha1.InstanceConditionMetadataReady,
		computev1alpha1.InstanceConditionGatewayReady,
	}
	for _, c := range components {
		cond := apimeta.FindStatusCondition(instance.Status.Conditions, c)
		if cond == nil {
			setInstanceCondition(instance, computev1alpha1.InstanceConditionReady,
				metav1.ConditionFalse, "Initializing",
				fmt.Sprintf("%s has not been observed yet", c))
			return
		}
		if cond.Status != metav1.ConditionTrue {
			setInstanceCondition(instance, computev1alpha1.InstanceConditionReady,
				metav1.ConditionFalse, cond.Reason,
				fmt.Sprintf("%s: %s", c, cond.Message))
			return
		}
	}
	setInstanceCondition(instance, computev1alpha1.InstanceConditionReady,
		metav1.ConditionTrue, "AllComponentsReady",
		"metadata and gateway are ready")
}

// computePhase derives the instance Phase from InstanceConditionReady,
// which is itself the roll-up of the per-component conditions
// (Postgres, Metadata, Gateway) computed by setInstanceReadyRollup.
// The invariant is:
//
//	Phase == Ready  ⇔  InstanceConditionReady.Status == True
//
// There is exactly one source of truth for "is this instance ready".
// Callers MUST refresh the roll-up (via setInstanceReadyRollup) before
// calling computePhase; otherwise a stale condition will produce a
// stale Phase. The three call sites in this file observe that order.
//
// Historical note: this function used to compute Phase from the boolean
// Status.MetadataReady && Status.GatewayReady, which diverged from
// InstanceConditionReady in two ways:
//
//  1. A per-component condition that flipped False post-rollout was
//     ignored. For example, an external-Postgres instance whose
//     credentials Secret was deleted post-rollout kept Phase=Ready
//     (mounted creds keep the metadata pod running) while
//     InstanceConditionReady correctly flipped to False on the next
//     preflight.
//  2. The mid-reconcile booleans are not reset between passes, leaving
//     stale-true values that would re-assert Phase=Ready while a
//     freshly-set component condition was False.
//
// Both cases were user-visible lies on the headline Phase field.
// Deriving Phase from the same condition "Ready" that kubectl describe
// shows eliminates them. The per-component booleans are preserved as a
// lower-level signal (and for printcolumn display) but no longer feed
// into Phase.
//
// Phase state machine:
//
//	Failed is terminal and is never overwritten by this function.
//	Provisioning → Ready    when ConditionReady flips True.
//	Ready       → Degraded  when ConditionReady flips back to False.
//	Degraded    → Ready     when ConditionReady recovers to True.
func (r *FireboltInstanceReconciler) computePhase(instance *computev1alpha1.FireboltInstance) computev1alpha1.InstancePhase {
	if instance.Status.Phase == computev1alpha1.InstancePhaseFailed {
		return computev1alpha1.InstancePhaseFailed
	}

	ready := apimeta.FindStatusCondition(
		instance.Status.Conditions,
		computev1alpha1.InstanceConditionReady,
	)
	if ready != nil && ready.Status == metav1.ConditionTrue {
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

// SetupWithManager sets up the controller with the Manager.
func (r *FireboltInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerNamed(mgr, "fireboltinstance")
}

// SetupWithManagerNamed sets up the controller with the Manager using a
// custom controller name. Useful for E2E tests that spin up multiple in-process
// reconcilers per suite and need unique metric names across them.
func (r *FireboltInstanceReconciler) SetupWithManagerNamed(mgr ctrl.Manager, name string) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltInstance{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Named(name).
		Complete(r)
}
