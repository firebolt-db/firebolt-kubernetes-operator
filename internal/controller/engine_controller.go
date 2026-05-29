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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

const finalizerName = "compute.firebolt.io/engine-cleanup"

// reasonStatefulSetWarning is the fallback Reason stamped on the Ready
// condition when a propagated StatefulSet event carries a Reason string
// that is not a valid metav1.Condition Reason (must match
// ^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$). Standard kube-emitted
// reasons (FailedCreate, BackOff, ...) pass through unchanged; this is
// defense against custom controllers emitting malformed reasons that
// would otherwise cause the status update to be rejected by the apiserver.
const reasonStatefulSetWarning = "StatefulSetWarning"

// reasonExternalFinalizer is the Ready=False reason surfaced when
// reconcileDelete observes a non-operator finalizer on an
// owned StatefulSet / Service / ConfigMap. The operator does not stamp
// finalizers on engine-owned children itself, so any finalizer found
// on a labeled child is by definition installed by an external
// controller (backup tools, service mesh injectors, custom admission
// hooks). The condition is informational, not blocking: the engine's
// own finalizer is still removed at the end of reconcileDelete so the
// engine CR can be garbage-collected; the message tells the operator
// which external finalizer to chase when child resources linger past
// engine deletion. See the eventReason of the same name for the
// matching Event.
const reasonExternalFinalizer = "ExternalFinalizer"

// eventReasonExternalFinalizer is the Reason on the Warning Event
// emitted alongside the ExternalFinalizer condition. Same semantic as
// reasonExternalFinalizer; the Event carries the per-resource detail
// (kind/name/finalizer) so the report survives engine CR garbage
// collection, since the condition disappears with the CR.
const eventReasonExternalFinalizer = "ExternalFinalizerOnOwnedResource"

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

	// MetricsRecorder records Prometheus metrics for engine CRs.
	// Must be non-nil; use metrics.NoOpEngineRecorder{} in tests.
	MetricsRecorder metrics.EngineRecorder

	// EventRecorder emits Kubernetes Events on the engine CR. Populated
	// in SetupWithManager when nil; unit tests that exercise event-emitting
	// paths should inject a record.FakeRecorder. Nil is tolerated: the
	// emit-event helpers no-op so tests that do not care about events can
	// leave the field unset.
	EventRecorder record.EventRecorder

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
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=engineclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=engineclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/proxy,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch

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
		return ctrl.Result{}, r.reconcileDelete(ctx, engine)
	}

	// Record metrics on every return path beyond this point so error
	// branches (getEngineState failure, instance-gate block, applyEngineState
	// failure, status-update failure) still publish current in-memory state.
	// The closure reads `current` at function exit; before getEngineState
	// runs it stays zero-valued, which truthfully reflects "phase initialized
	// but pod state not yet observed". Deletion is intentionally above this
	// line because reconcileDelete owns the matching Delete call.
	var current EngineState
	defer func() {
		r.MetricsRecorder.Record(engine, current.CurrentPodReady, current.CurrentPodTotal)
	}()

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
			r.MetricsRecorder.RecordDrainCheckError(engine.Namespace, engine.Name, engine.Spec.InstanceRef)
			if updErr := r.updateStatus(ctx, engine); updErr != nil {
				log.Info("Failed to persist DrainCheckFailing condition; controller-runtime will retry the reconcile",
					"error", updErr)
			}
		}
		return ctrl.Result{}, fmt.Errorf("getEngineState failed: %w", err)
	}

	// Only PhaseStable, PhaseStopped, and PhaseCreating actually consume
	// InstanceInfo (to render ConfigMaps with instance.multi_engine.
	// metadata_endpoint / instance.id). Stopped is included because
	// computeStable can re-materialize a missing ConfigMap in place at
	// the current generation, which uses instanceInfo even when
	// spec.Replicas is 0. Switching / Draining / Cleaning operate on
	// already-rendered resources and are functionally independent of the
	// FireboltInstance: draining an old-gen pod does not need a metadata
	// endpoint, cleaning up our own resources does not need an instance ID.
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
		engine.Status.Phase == computev1alpha1.PhaseStopped ||
		engine.Status.Phase == computev1alpha1.PhaseCreating

	var instanceInfo InstanceInfo
	if needsInstance {
		var instanceErr error
		instanceInfo, instanceErr = r.resolveInstanceInfo(ctx, engine)
		if instanceErr != nil {
			log.Info("Instance gate blocking reconcile", "reason", instanceErr.Error())
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

	// EngineClass resolution is bubbled up to controller-runtime on
	// every failure. NotFound (errEngineClassNotFound) is normally
	// caught at admission by the FireboltEngine validating webhook,
	// but the helm chart ships webhooks disabled by default, so a
	// missing class can reach this point in dev/test deployments. The
	// engine stays stuck at the Initializing condition and the
	// reconciler-loop log carries the missing-class name (the
	// resolver wraps the upstream error with the name + namespace).
	// We intentionally do NOT mint a dedicated EngineClassReady
	// condition for the missing-class case: the state space is binary
	// (exists / doesn't), not a lifecycle worth narrating like
	// InstanceReady.
	//
	// errEngineClassUnready is treated differently: the class exists
	// but the EngineClassReconciler stamped Ready=False/
	// OperatorOwnedFieldSet. Refuse to render a StatefulSet off it and
	// surface ConditionReady=False/EngineClassUnready on the engine so
	// kubectl describe shows the pointer to the offending class. No
	// backoff error: the engine is correctly reconciled given the
	// unready class, and the next reconcile fires when the class
	// reconciler updates the class status (the FireboltEngine watch on
	// EngineClass already covers this through enqueueClassFromEngine's
	// symmetric counterpart).
	classInfo, classErr := r.resolveEngineClassInfo(ctx, engine)
	if classErr != nil {
		return r.handleEngineClassError(ctx, engine, classErr)
	}

	result := computeEngineReconcile(
		&engine.Spec,
		&engine.Status,
		current,
		engine.Name,
		engine.Namespace,
		engine.Generation,
		instanceInfo,
		classInfo,
	)

	if result.Status.Phase != engine.Status.Phase {
		log.Info("Phase transition", "from", engine.Status.Phase, "to", result.Status.Phase)
	}

	setReadyCondition(&result.Status, current, engine.Generation)

	// When the engine is stuck because the StatefulSet controller cannot
	// create the desired pod count (missing ServiceAccount, quota, admission
	// rejection, PVC unbindable, ...), the only place the actionable error
	// appears is on the StatefulSet's events. Surface the latest Warning
	// onto the Ready condition so users do not have to know to run
	// `kubectl describe sts`. The lookup is best-effort; failures here
	// only forfeit the diagnostic, never the reconcile.
	if shouldSurfaceStatefulSetEvent(&result.Status, current, engine.Spec.Replicas) {
		if ev := r.latestStatefulSetWarning(ctx, current.CurrentSTS); ev != nil {
			applyStatefulSetEventToReadyCondition(&result.Status, ev, engine.Generation)
		}
	}

	if err := r.applyEngineState(ctx, engine, &result); err != nil {
		return ctrl.Result{}, fmt.Errorf("applyEngineState failed: %w", err)
	}

	if !r.DisableGC &&
		(engine.Status.Phase == computev1alpha1.PhaseStable ||
			engine.Status.Phase == computev1alpha1.PhaseStopped) {
		r.gcOrphanedResources(ctx, engine)
	}

	requeueAfter := result.RequeueAfter

	// Autoscaler runs only after a clean main reconcile and only in terminal
	// phases. It may patch spec.replicas (level-driven: the patch flows
	// through the FireboltEngine watch and the next reconcile picks it up
	// via the existing blue-green path). Errors here do not poison the main
	// reconcile result, since the cluster state we just wrote is consistent.
	asResult, asErr := r.runAutoscaler(ctx, engine)
	if asErr != nil {
		log.Error(asErr, "Autoscaler step failed; will retry on next reconcile")
	}
	if asResult.RequeueAfter > 0 && (requeueAfter == 0 || asResult.RequeueAfter < requeueAfter) {
		requeueAfter = asResult.RequeueAfter
	}

	return ctrl.Result{
		Requeue:      result.Requeue,
		RequeueAfter: requeueAfter,
	}, nil
}

// externalFinalizerEntry records one labeled child resource that
// carries a finalizer the operator did not install. Used to build the
// ExternalFinalizer Ready condition message and the matching Warning
// Event so users can see what external controller (backup tool,
// service mesh injector, custom admission hook) is holding up
// cleanup.
type externalFinalizerEntry struct {
	Kind       string
	Name       string
	Finalizers []string
}

// reconcileDelete deletes generation-scoped resources owned by this
// engine, surfaces any external finalizers it observed on those
// children, and then removes the engine's own finalizer so the CR can
// be garbage-collected. External finalizers are reported but do not
// block the engine's finalizer removal: backup tools and similar
// integrations are legitimate users of finalizers, and pinning the
// engine CR indefinitely would force operators to manually edit
// finalizers out for every routine deletion. The Warning Event
// emitted alongside the condition survives the CR's deletion long
// enough for an operator to find it via `kubectl get events`.
func (r *FireboltEngineReconciler) reconcileDelete(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)
	log.Info("Handling engine deletion")

	ns := engine.Namespace
	var errs []error
	var externals []externalFinalizerEntry

	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList, client.InNamespace(ns), client.MatchingLabels{
		LabelEngine: engine.Name,
	}); err != nil {
		log.Error(err, "Failed to list StatefulSets for cleanup")
		errs = append(errs, err)
	} else {
		for i := range stsList.Items {
			externals = appendExternalFinalizer(externals, "StatefulSet", &stsList.Items[i])
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
			externals = appendExternalFinalizer(externals, "Service", &svcList.Items[i])
			log.Info("Deleting Service", "name", svcList.Items[i].Name)
			if err := r.deleteIfExists(ctx, &svcList.Items[i]); err != nil {
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
			externals = appendExternalFinalizer(externals, "ConfigMap", &cmList.Items[i])
			log.Info("Deleting ConfigMap", "name", cmList.Items[i].Name)
			if err := r.deleteIfExists(ctx, &cmList.Items[i]); err != nil {
				log.Error(err, "Failed to delete ConfigMap", "name", cmList.Items[i].Name)
				errs = append(errs, err)
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup failed with %d errors, first: %w", len(errs), errs[0])
	}

	r.surfaceExternalFinalizers(ctx, engine, externals)

	controllerutil.RemoveFinalizer(engine, finalizerName)
	if err := r.Update(ctx, engine); err != nil {
		return err
	}

	r.MetricsRecorder.Delete(engine.Namespace, engine.Name)

	log.Info("Finalizer removed, deletion complete")
	return nil
}

// appendExternalFinalizer adds obj to the report iff it carries one or
// more finalizers. The operator does not stamp finalizers on engine
// children, so any non-empty finalizer slice on a labeled child is
// "external" by construction.
func appendExternalFinalizer(externals []externalFinalizerEntry, kind string, obj client.Object) []externalFinalizerEntry {
	fins := obj.GetFinalizers()
	if len(fins) == 0 {
		return externals
	}
	// Defensive copy: GetFinalizers returns the backing slice and the
	// caller may mutate the object later (e.g. set DeletionTimestamp on
	// re-list); we want a stable snapshot for the Event message.
	copied := make([]string, len(fins))
	copy(copied, fins)
	return append(externals, externalFinalizerEntry{
		Kind:       kind,
		Name:       obj.GetName(),
		Finalizers: copied,
	})
}

// surfaceExternalFinalizers persists the ExternalFinalizer condition
// on the engine and emits a matching Warning Event so the report
// outlives the CR. No-op when the report is empty. Best-effort: a
// status write or event-emit failure logs but does not propagate, so
// the calling reconcileDelete can still remove the engine's finalizer
// — losing the diagnostic is preferable to leaving the engine CR
// stuck in DeletionTimestamp because of an unrelated transient
// API failure.
func (r *FireboltEngineReconciler) surfaceExternalFinalizers(
	ctx context.Context,
	engine *computev1alpha1.FireboltEngine,
	externals []externalFinalizerEntry,
) {
	if len(externals) == 0 {
		return
	}
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	msg := formatExternalFinalizerMessage(externals)
	log.Info("External finalizers detected on owned resources", "detail", msg)

	if r.EventRecorder != nil {
		r.EventRecorder.Event(engine, corev1.EventTypeWarning, eventReasonExternalFinalizer, msg)
	}

	apimeta.SetStatusCondition(&engine.Status.Conditions, metav1.Condition{
		Type:               computev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: engine.Generation,
		Reason:             reasonExternalFinalizer,
		Message:            msg,
	})
	if err := r.updateStatus(ctx, engine); err != nil {
		log.Info("Failed to persist ExternalFinalizer condition; engine CR will still be garbage-collected", "error", err)
	}
}

// formatExternalFinalizerMessage renders the per-resource detail as a
// single multi-line string suitable for both an Event message and a
// status condition Message. Keeps the kind/name first so a human
// scanning `kubectl describe engine` can identify the offending
// resource at a glance.
func formatExternalFinalizerMessage(externals []externalFinalizerEntry) string {
	var b strings.Builder
	// strings.Builder.WriteString and fmt.Fprintf into a Builder both
	// return errors that documentation guarantees to be nil; the linter
	// flags them anyway, so we discard explicitly.
	_, _ = b.WriteString("External finalizers on owned resources (engine CR will be garbage-collected; the listed resources will linger until their finalizers are removed by their owners):")
	for _, e := range externals {
		_, _ = fmt.Fprintf(&b, "\n  - %s/%s: %s", e.Kind, e.Name, strings.Join(e.Finalizers, ", "))
	}
	return b.String()
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
//
// ResourceVersion sync: a /status subresource Update bumps the
// object's resourceVersion just like a main-object Update does.
// On the conflict-recovery path we Get + Update a `fresh` copy,
// which leaves the caller's `engine` pointing at a stale RV. The
// next main-object write the caller issues (typically
// reconcileDelete removing the finalizer) would then hit a
// guaranteed 409, adding an extra requeue cycle for no reason.
// Sync the new RV back onto the caller's engine before returning.
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
	if err := r.Status().Update(ctx, fresh); err != nil {
		return err
	}
	engine.ResourceVersion = fresh.ResourceVersion
	return nil
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
//  2. Stopped: phase is Stopped (spec.replicas is 0). The engine is
//     intentionally parked; distinguishing this from Rolling avoids
//     GitOps tools treating a stopped engine as mid-transition.
//  3. Rolling: any non-terminal phase (Creating / Switching / Draining /
//     Cleaning).
//  4. PodsNotReady: phase is Stable but the active-generation pods
//     haven't all reported Ready yet (e.g. image pull in progress on
//     a freshly scheduled replica). This is what distinguishes
//     ConditionReady from Phase==Stable: the latter can be true while
//     pods are still coming up.
//  5. Otherwise True: serving traffic, all replicas ready.
//
// EngineClass resolution failures are not in this list. A missing class
// is caught by the FireboltEngine validating webhook at admission time
// in normal deployments; when webhooks are disabled (helm chart
// default) the runtime error is logged and bubbled to controller-
// runtime for backoff retry, but does not get its own condition type.
// The class state space is binary (exists / doesn't), unlike the
// instance which has a real lifecycle worth narrating in status.
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
	case status.Phase == computev1alpha1.PhaseStopped:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Stopped"
		cond.Message = "Engine is stopped (spec.replicas is 0)"
	case status.Phase != computev1alpha1.PhaseStable:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "Rolling"
		cond.Message = fmt.Sprintf("Engine is in %s phase", status.Phase)
	case !current.CurrentPodsReady:
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PodsNotReady"
		cond.Message = fmt.Sprintf(
			"generation %d has %d of %d pod(s) ready",
			status.ActiveGeneration, current.CurrentPodReady, current.CurrentPodTotal,
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

// handleEngineClassError translates a non-nil error from
// resolveEngineClassInfo into the appropriate Reconcile result. The
// errEngineClassUnready branch surfaces ConditionReady=False/
// EngineClassUnready on the engine and requeues without returning an
// error to controller-runtime (the engine is correctly reconciled
// given the unready class; backoff would only delay the next
// reactive watch event). Every other error — a missing class, an
// apiserver outage — is bubbled up so controller-runtime applies
// exponential backoff per the existing missing-class contract.
func (r *FireboltEngineReconciler) handleEngineClassError(ctx context.Context, engine *computev1alpha1.FireboltEngine, classErr error) (ctrl.Result, error) {
	if !stderrors.Is(classErr, errEngineClassUnready) {
		return ctrl.Result{}, classErr
	}
	apimeta.SetStatusCondition(&engine.Status.Conditions, metav1.Condition{
		Type:               computev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: engine.Generation,
		Reason:             reasonEngineClassUnready,
		Message:            classErr.Error(),
	})
	if updateErr := r.updateStatus(ctx, engine); updateErr != nil {
		return ctrl.Result{}, updateErr
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// shouldSurfaceStatefulSetEvent reports whether the Ready condition would
// benefit from being decorated with the latest Warning event recorded on
// the current-generation StatefulSet. The lookup is gated to keep the
// per-reconcile cost (one Events API call) tied to a concrete symptom:
//
//  1. CurrentSTS must exist — without an STS there is nothing to look up
//     events for, and computeStable would already be creating a new
//     generation rather than waiting on the old one.
//  2. CurrentPodTotal must be below the desired replica count. This
//     specifically targets the "STS controller cannot create pods" case
//     (forbidden SA, quota, admission rejection, ...). Pods that exist
//     but are CrashLoopBackOff'ing emit pod-level events, not STS events,
//     so we do not surface those here.
//  3. The existing Ready reason must be one of the two generic "stuck"
//     reasons (Rolling / PodsNotReady). More specific reasons
//     (InstanceNotReady, DrainCheckFailing, Stopped, EngineReady) are
//     either higher-precedence diagnostics we must not mask, or
//     statements about a healthy engine.
func shouldSurfaceStatefulSetEvent(
	status *computev1alpha1.FireboltEngineStatus,
	current EngineState,
	expectedReplicas int32,
) bool {
	if current.CurrentSTS == nil {
		return false
	}
	if current.CurrentPodTotal >= int(expectedReplicas) {
		return false
	}
	cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		return false
	}
	switch cond.Reason {
	case "Rolling", "PodsNotReady":
		return true
	default:
		return false
	}
}

// applyStatefulSetEventToReadyCondition rewrites the Ready=False condition
// to carry the actionable Reason/Message from a recent StatefulSet Warning
// event (typically FailedCreate). Decorating the existing Ready condition
// — rather than emitting a sibling condition — keeps a single top-level
// "what is wrong?" signal: consumers already keying off Ready inherit the
// improved diagnostic for free, and the operator does not split the
// diagnostic surface across two fields.
//
// The decoration is idempotent: each reconcile recomputes Ready from
// scratch (setReadyCondition) and then re-applies the latest event. Once
// pods come up the trigger gate stops firing and the next reconcile
// restores the natural EngineReady reason.
func applyStatefulSetEventToReadyCondition(
	status *computev1alpha1.FireboltEngineStatus,
	ev *corev1.Event,
	generation int64,
) {
	if ev == nil {
		return
	}
	reason := sanitizeConditionReason(ev.Reason)
	message := fmt.Sprintf("StatefulSet %s: %s",
		ev.InvolvedObject.Name, strings.TrimSpace(ev.Message))
	if ev.Count > 1 {
		message = fmt.Sprintf("%s (x%d)", message, ev.Count)
	}
	apimeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               computev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// sanitizeConditionReason returns s when it is a valid metav1.Condition
// reason (must match ^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$ per the
// apimachinery validation rules), or a generic fallback otherwise. Standard
// Kubernetes event reasons (FailedCreate, FailedScheduling, BackOff, ...)
// satisfy the regex; the fallback is defense-in-depth against a custom
// controller emitting a reason with reserved characters that would
// otherwise cause the status update to be rejected by the apiserver.
func sanitizeConditionReason(s string) string {
	if s == "" {
		return reasonStatefulSetWarning
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case (r >= '0' && r <= '9') || r == '_' || r == ',' || r == ':':
			if i == 0 {
				return reasonStatefulSetWarning
			}
		default:
			return reasonStatefulSetWarning
		}
	}
	last := s[len(s)-1]
	if last == ',' || last == ':' {
		return reasonStatefulSetWarning
	}
	return s
}

// isInstanceConditionTrue reports whether ConditionInstanceReady is
// present AND True. A missing condition (no lookup yet this reconcile)
// is treated as "not True" so Ready does not briefly flip to True in
// the window between init and the first instance-resolve.
func isInstanceConditionTrue(conds []metav1.Condition) bool {
	c := apimeta.FindStatusCondition(conds, computev1alpha1.ConditionInstanceReady)
	return c != nil && c.Status == metav1.ConditionTrue
}

// errEngineClassNotFound is returned by resolveEngineClassInfo when the
// engine's spec.engineClassRef names an EngineClass that does not exist
// in the engine's namespace. Wrapping via stderrors.Is lets future
// callers distinguish this user-actionable case (typo, class in wrong
// namespace) from transient API errors (apiserver down, RBAC race).
// Today's single caller — Reconcile — bubbles both kinds up to
// controller-runtime for backoff retry; admission catches the
// missing-class case in normal deployments. The sentinel earns its
// keep against future evolution: an Event emitter on the engine, or a
// different log severity on the actionable path, would key off it.
var errEngineClassNotFound = stderrors.New("EngineClass referenced by spec.engineClassRef not found")

// errEngineClassUnready is returned by resolveEngineClassInfo when the
// referenced class exists but its EngineClassReconciler has stamped
// Ready=False with reason OperatorOwnedFieldSet — the class carries
// user input on a path the operator owns end-to-end and is therefore
// not safe to render into a StatefulSet. The validating webhook
// normally rejects such classes at admission; this case fires when
// admission was bypassed and the EngineClassReconciler's defense-in-
// depth check is the first observer of the violation. Surfaced on
// the engine's ConditionReady with reason EngineClassUnready so the
// user gets a pointer to the offending class without having to inspect
// every dependent engine's events.
var errEngineClassUnready = stderrors.New("EngineClass referenced by spec.engineClassRef is not Ready")

// reasonEngineClassUnready is the engine ConditionReady reason set
// when resolveEngineClassInfo refuses to consume an
// OperatorOwnedFieldSet class. Set directly in Reconcile (rather than
// derived inside setReadyCondition) because the gate short-circuits
// reconciliation before the renderer runs, so the regular
// setReadyCondition path never executes for the offending engine.
const reasonEngineClassUnready = "EngineClassUnready"

// resolveEngineClassInfo fetches the EngineClass referenced by
// engine.spec.engineClassRef and returns the resolved template plus a
// content hash. Returns nil info (and no error) when no class is
// referenced — that engine falls back to operator defaults.
//
// EngineClass is namespaced; the lookup is scoped to the engine's
// namespace, matching the FireboltEngine validating webhook and how
// Kubernetes resolves the volume / SA / secret refs the template
// carries.
//
// Admission normally rejects a missing engineClassRef. A NotFound at
// reconcile time means either the class was removed out of band (force
// deletion bypassing the deletion-blocking webhook) or admission webhooks
// were disabled at the time of apply — the helm chart ships webhooks
// off by default, so the latter is the realistic case. The returned
// error wraps errEngineClassNotFound so future callers can distinguish
// it from a transient API failure via stderrors.Is; today's caller
// (Reconcile) just bubbles both kinds up to controller-runtime's
// backoff retry.
//
// A second admission-bypass gate runs here: if the EngineClassReconciler
// has stamped Ready=False/OperatorOwnedFieldSet (its defense-in-depth
// check caught a path the operator owns end-to-end), the resolver
// returns errEngineClassUnready and Reconcile surfaces
// ConditionReady=False/EngineClassUnready on the engine without
// rendering a StatefulSet off the offending class. A missing Ready
// condition (class freshly created, EngineClassReconciler hasn't run
// yet) is treated as "not yet evaluated" and allowed through — the
// engine's next reconcile will pick up the status once the class
// controller catches up. DeletionBlocked is not a gate: engines that
// stay bound to a Terminating class are the exact reason the class
// can't finish deleting, so blocking renders would deadlock
// (engine stops reconciling → can't be deleted → class can't be
// released).
func (r *FireboltEngineReconciler) resolveEngineClassInfo(ctx context.Context, engine *computev1alpha1.FireboltEngine) (*EngineClassInfo, error) {
	if engine.Spec.EngineClassRef == nil || *engine.Spec.EngineClassRef == "" {
		return nil, nil
	}
	class := &computev1alpha1.EngineClass{}
	key := types.NamespacedName{Name: *engine.Spec.EngineClassRef, Namespace: engine.Namespace}
	if err := r.Get(ctx, key, class); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q in namespace %q", errEngineClassNotFound, *engine.Spec.EngineClassRef, engine.Namespace)
		}
		return nil, fmt.Errorf("getting EngineClass %q in namespace %q: %w", *engine.Spec.EngineClassRef, engine.Namespace, err)
	}
	if cond := apimeta.FindStatusCondition(class.Status.Conditions, computev1alpha1.EngineClassConditionReady); cond != nil &&
		cond.Status == metav1.ConditionFalse && cond.Reason == reasonOperatorOwnedFieldSet {
		return nil, fmt.Errorf("%w: %q in namespace %q: %s",
			errEngineClassUnready, class.Name, class.Namespace, cond.Message)
	}
	return newEngineClassInfo(class), nil
}

// resolveInstanceInfo looks up the FireboltInstance referenced by the engine's
// spec.instanceRef and returns its metadata endpoint and instance ID.
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
		InstanceID:       inst.Spec.ID,
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
	if r.EventRecorder == nil {
		r.EventRecorder = mgr.GetEventRecorderFor(name)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltEngine{}).
		Watches(&computev1alpha1.FireboltInstance{},
			handler.EnqueueRequestsFromMapFunc(r.instanceToEngines)).
		Watches(&computev1alpha1.EngineClass{},
			handler.EnqueueRequestsFromMapFunc(r.engineClassToEngines)).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Named(name).
		Complete(r)
}

// engineClassToEngines maps an EngineClass event to reconcile requests
// for every FireboltEngine in the same namespace that references the
// class via spec.engineClassRef. EngineClass is namespaced, so a class
// can only be referenced by engines in its own namespace — the watch
// handler scopes the list to obj.GetNamespace() instead of fanning out
// across the cluster.
func (r *FireboltEngineReconciler) engineClassToEngines(ctx context.Context, obj client.Object) []reconcile.Request {
	className := obj.GetName()
	classNamespace := obj.GetNamespace()
	if r.Namespace != "" && classNamespace != r.Namespace {
		return nil
	}
	engineList := &computev1alpha1.FireboltEngineList{}
	if err := r.List(ctx, engineList, client.InNamespace(classNamespace)); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to list engines for EngineClass watch", "engineclass", className, "namespace", classNamespace)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(engineList.Items))
	for i := range engineList.Items {
		ref := engineList.Items[i].Spec.EngineClassRef
		if ref == nil || *ref != className {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      engineList.Items[i].Name,
				Namespace: engineList.Items[i].Namespace,
			},
		})
	}
	return requests
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
