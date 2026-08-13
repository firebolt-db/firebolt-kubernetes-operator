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
	"strconv"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// gcOrphanedResources deletes StatefulSets, Services, ConfigMaps, and
// per-generation TLS Certificates/Secrets that this engine owns but whose
// LabelGeneration matches none of CurrentGeneration, ActiveGeneration,
// DrainingGeneration, or the generation the cluster Service selects. It returns true when the pass left
// orphans standing, either because the per-pass delete budget ran out or because
// a delete failed, so the caller can requeue promptly instead of waiting out the
// phase's own interval.
//
// Why this exists: Kubernetes does not support multi-resource transactions.
// When computeCreating abandons a generation mid-flight (spec changed while
// pods are still starting), it deletes the current generation's resources and
// bumps CurrentGeneration. Those deletes are issued once, from the reconcile
// that decided to abandon, and they only cover what that pass observed: a
// headless Service or ConfigMap the cached read missed is never queued at all,
// the generation's TLS Certificate/Secret are not in that set, and a crash
// between a delete and the status write loses the rest. Once CurrentGeneration
// has moved on, nothing in the lifecycle path refers to those objects again —
// getEngineState only reads CurrentGeneration and DrainingGeneration — so this
// sweep is what reclaims them.
//
// The keep set is what makes the sweep safe in every phase, not just the
// terminal ones: mid-rollout the generation serving traffic is ActiveGeneration
// while CurrentGeneration is the one being built, so keeping all three leaves
// exactly the abandoned generations to delete. An engine that never reaches a
// terminal phase — pods crash-looping, or a drift signal churning generations —
// is the case that leaks, so gating the sweep on Stable/Stopped would exclude
// precisely the engines that need it. Reconcile defers this call for the same
// reason: an engine parked on an unready instance or a rejected pod template
// never gets past those gates, and it needs its abandoned generations back too.
// GCOrphans in formal/FireboltEngine.tla models the sweep, and its
// GCIgnoresActiveGen counterexample pins what the ActiveGeneration entry buys.
func (r *FireboltEngineReconciler) gcOrphanedResources(ctx context.Context, engine *computev1alpha1.FireboltEngine) bool {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	status := r.sweepStatus(ctx, log, engine)
	if status == nil {
		return false
	}

	keep := &generationKeepSet{gens: map[string]bool{}}
	keep.add(status.CurrentGeneration)
	// Negative means "none yet" for ActiveGeneration (pre-first-switch) and is
	// never a label value, but keeping the guard explicit documents that the
	// sweep is protecting a generation that exists.
	if status.ActiveGeneration >= 0 {
		keep.add(status.ActiveGeneration)
	}
	if status.DrainingGeneration != nil {
		keep.add(*status.DrainingGeneration)
	}

	// Whatever the status says, the generation the cluster Service actually
	// selects is the one queries are landing on, so it is kept too. Status and
	// selector normally agree, and the phase machine is what reconciles them, but
	// they can diverge: a pass whose Service repair failed returns an error with
	// the status ahead of the selector, and this sweep runs on that path on
	// purpose. Deleting the selected generation there would turn a recoverable
	// divergence into an outage.
	//
	// A read failure other than NotFound ends the pass reporting a backlog rather
	// than sweeping blind: without the selector there is no way to tell which
	// generation is serving.
	clusterSvc := &corev1.Service{}
	clusterSvcKey := types.NamespacedName{Name: engine.Name + SuffixService, Namespace: engine.Namespace}
	switch err := r.Get(ctx, clusterSvcKey, clusterSvc); {
	case err == nil:
		if gen, ok := clusterSvc.Spec.Selector[LabelGeneration]; ok {
			keep.addLabel(gen)
		}
	case !apierrors.IsNotFound(err):
		log.Error(err, "GC: failed to read the cluster Service", "name", clusterSvcKey.Name)
		return true
	}

	sweep := &orphanSweep{budget: GCMaxDeletesPerPass}

	// A List that fails ends the pass reporting a backlog: it saw only part of
	// the engine's resources, so whether orphans are standing is unknown, and
	// coming back on the short interval is the safe reading.

	engineLabels := client.MatchingLabels{LabelEngine: engine.Name}
	ns := client.InNamespace(engine.Namespace)

	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list StatefulSets")
		return true
	}
	// StatefulSets first: deleteIfExists deletes them with foreground
	// propagation, so reclaiming the StatefulSet is also what reclaims the
	// abandoned generation's pods. Sweeping pods directly would need a delete
	// verb on pods that nothing else in the operator asks for.
	if !r.sweepKind(ctx, log, engine, len(stsList.Items),
		func(i int) client.Object { return &stsList.Items[i] }, keep, sweep) {
		return sweep.retry
	}

	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list Services")
		return true
	}
	if !r.sweepKind(ctx, log, engine, len(svcList.Items),
		func(i int) client.Object { return &svcList.Items[i] }, keep, sweep) {
		return sweep.retry
	}

	cmList := &corev1.ConfigMapList{}
	if err := r.List(ctx, cmList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list ConfigMaps")
		return true
	}
	if !r.sweepKind(ctx, log, engine, len(cmList.Items),
		func(i int) client.Object { return &cmList.Items[i] }, keep, sweep) {
		return sweep.retry
	}

	// Per-generation engine TLS Certificates and their cert-manager-derived
	// Secrets carry LabelEngine + LabelGeneration too (the Secret
	// via the Certificate's SecretTemplate), but nothing above ever referenced
	// them, so they accumulated across every TLS rollout — and because
	// cert-manager does not owner-reference the derived Secret, the Secret even
	// outlived engine deletion. Sweep them on the SAME keepGens schedule as the
	// resources above, so a generation's cert/secret are reclaimed exactly when
	// (and never before) its StatefulSet/Service/ConfigMap are.
	//
	// Certificate before Secret: while a Certificate exists, cert-manager
	// recreates its target Secret whenever that Secret goes missing, so deleting
	// the Secret first would have it immediately recreated (same ordering the
	// instance controller's reconcileDelete relies on). The Certificate List is
	// tolerant of a missing cert-manager CRD (envtest installs none), where no
	// per-generation certs/secrets can exist at all.
	certList := &certmanagerv1.CertificateList{}
	if err := r.List(ctx, certList, ns, engineLabels); err != nil {
		if !certKindUnavailable(err) {
			log.Error(err, "GC: failed to list Certificates")
			return true
		}
	} else {
		if !r.sweepKind(ctx, log, engine, len(certList.Items),
			func(i int) client.Object { return &certList.Items[i] }, keep, sweep) {
			return sweep.retry
		}
	}

	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list Secrets")
		return true
	}
	if !r.sweepKind(ctx, log, engine, len(secretList.Items),
		func(i int) client.Object { return &secretList.Items[i] }, keep, sweep) {
		return sweep.retry
	}

	return sweep.retry
}

// orphanSweep is one pass's delete allowance and what it could not finish.
type orphanSweep struct {
	// budget is how many more objects this pass may delete.
	budget int
	// retry records that orphans are still standing, so the caller should come
	// back sooner than the phase's own interval: either the budget ran out or a
	// delete failed.
	retry bool
	// kindFailures counts failed deletes within the kind being swept, reset by
	// sweepKind on entry.
	kindFailures int
	// loggedStaleView keeps the newer-than-the-keep-set note to one line per
	// pass; under churn every resource of every newer generation would hit it.
	loggedStaleView bool
}

// generationKeepSet is the set of generations a sweep must leave alone: the ones
// the status names, the one the cluster Service selects, and everything newer
// than all of those.
//
// The floor is the part that is not obvious. Abandoned generations are strictly
// older than the keep set by construction, because once CurrentGeneration moves
// on nothing recreates an older generation. A resource whose generation is newer
// than every generation the sweep knows about therefore cannot be an orphan: it
// can only mean this pass read a status that has since been superseded, which
// happens under churn because the keep set comes from a cached read. Refusing to
// delete it costs a delayed reclaim, which the next pass makes good; deleting it
// destroys the generation the engine is currently building, and the resulting
// re-ensure pushes the read further behind, which is a loop that sustains itself.
type generationKeepSet struct {
	gens map[string]bool
	// newest is the highest generation in the set, and the floor at or above
	// which nothing may be deleted.
	newest int
}

func (k *generationKeepSet) add(gen int) {
	k.gens[strconv.Itoa(gen)] = true
	if gen > k.newest {
		k.newest = gen
	}
}

// addLabel adds a generation read from a label. A value that is not a number is
// ignored: it cannot place the resource against the floor, and nothing the
// operator writes carries one.
func (k *generationKeepSet) addLabel(gen string) {
	n, err := strconv.Atoi(gen)
	if err != nil {
		return
	}
	k.add(n)
}

// protects reports whether the sweep must leave a resource carrying this
// generation label alone. Everything unrecognizable is protected: an absent
// label puts the resource out of scope entirely, and a label that is not a
// number cannot be placed against the floor.
func (k *generationKeepSet) protects(gen string) bool {
	if gen == "" || k.gens[gen] {
		return true
	}
	n, err := strconv.Atoi(gen)
	if err != nil {
		return true
	}
	return n >= k.newest
}

// newerThanNewest reports whether this generation label is above the floor,
// which is the signal that the status behind the keep set is stale.
func (k *generationKeepSet) newerThanNewest(gen string) bool {
	n, err := strconv.Atoi(gen)
	return err == nil && n > k.newest
}

// sweepStatus returns the engine status the keep set is built from, read straight
// from the API server when an APIReader is wired so the common case is a fresh
// view rather than a cached one that lags an earlier pass's write. A nil return
// means the engine is gone and there is nothing to sweep for; reconcileDelete
// owns that path.
//
// This is defense in depth, not the correctness guarantee: a read can always be
// overtaken by the next write, so the generation floor is what actually makes a
// stale keep set safe.
func (r *FireboltEngineReconciler) sweepStatus(
	ctx context.Context,
	log logr.Logger,
	engine *computev1alpha1.FireboltEngine,
) *computev1alpha1.FireboltEngineStatus {
	if r.APIReader == nil {
		return &engine.Status
	}
	live := &computev1alpha1.FireboltEngine{}
	switch err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(engine), live); {
	case err == nil:
		return &live.Status
	case apierrors.IsNotFound(err):
		return nil
	default:
		log.Error(err, "GC: failed to read the engine status uncached; using the cached copy")
		return &engine.Status
	}
}

// sweepOrphan deletes obj when its generation label is present and outside
// keepGens, charging one unit of budget. It returns false only when the budget
// is already spent and obj still needed deleting, which is the caller's signal
// to stop the pass.
//
// GC scope invariant: only resources that explicitly claim a generation via
// LabelGeneration are in scope. Engine-tagged resources without a generation
// label (the cluster Service today, potentially future per-engine shared
// resources, or anything a human/other controller labeled by mistake) are left
// alone. Treating a missing label as "some non-matching generation" would make
// an empty gen key fail the keepGens lookup and delete the object, which is a
// strictly larger blast radius than this safety net is meant to have.
//
// An object that already carries a deletionTimestamp is left alone too: the
// delete has been accepted and is waiting on finalizers (foreground propagation
// on a StatefulSet waits for its pods), so re-issuing it every pass would spend
// budget and API calls without moving anything along.
//
// A failed delete is logged, not returned: every orphan in the pass gets its
// attempt, and the sweep is level-triggered, so the next pass sees whatever is
// still standing and tries again.
func (r *FireboltEngineReconciler) sweepOrphan(
	ctx context.Context,
	log logr.Logger,
	engine *computev1alpha1.FireboltEngine,
	obj client.Object,
	keep *generationKeepSet,
	sweep *orphanSweep,
) {
	gen := obj.GetLabels()[LabelGeneration]
	if keep.protects(gen) || obj.GetDeletionTimestamp() != nil {
		if keep.newerThanNewest(gen) && !sweep.loggedStaleView {
			sweep.loggedStaleView = true
			log.Info("GC: leaving generations newer than the status this pass read",
				"newestKept", keep.newest, "generation", gen)
		}
		return
	}
	if !engineOwnsForGC(engine, obj) {
		return
	}
	sweep.budget--

	kind := fmt.Sprintf("%T", obj)
	log.Info("GC: deleting orphaned resource", "kind", kind, "name", obj.GetName(), "generation", gen)
	if err := r.deleteIfExists(ctx, obj); err != nil {
		log.Error(err, "GC: failed to delete orphaned resource", "kind", kind, "name", obj.GetName())
		sweep.retry = true
		sweep.kindFailures++
	}
}

// engineOwnsForGC reports whether the sweep may delete obj, which takes more
// than the two labels it was selected by. Labels are copyable: anything a user
// or another controller tags with this engine's name and a stale generation
// would otherwise be deleted, and for a Secret that is unrecoverable. The
// operator's own children carry a controller reference to the engine
// (SetControllerReference in every ensure*), so the reference is the proof.
//
// cert-manager's derived Secret is the documented exception: cert-manager
// owner-references it to the Certificate, not to the engine, so nothing links
// it back here. It is admitted on the same provenance its deletion path uses,
// the cert-manager certificate-name annotation naming a per-generation engine
// certificate, and on nothing else.
func engineOwnsForGC(engine *computev1alpha1.FireboltEngine, obj client.Object) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ref.UID == engine.UID {
			return true
		}
	}
	if secret, ok := obj.(*corev1.Secret); ok {
		return engineOwnedSecret(secret, engine.Name)
	}
	return false
}

// sweepKind sweeps one kind's list. It returns false when the pass is over,
// either because the delete budget is spent or because this kind's deletes keep
// failing: a kind whose deletes are rejected persistently, an RBAC gap on one
// resource for instance, would otherwise spend the whole budget on the same
// prefix every pass and the kinds after it would never be reached at all.
// Giving up on the kind after GCMaxKindFailuresPerPass leaves the rest of the
// budget for them.
func (r *FireboltEngineReconciler) sweepKind(
	ctx context.Context,
	log logr.Logger,
	engine *computev1alpha1.FireboltEngine,
	count int,
	at func(int) client.Object,
	keep *generationKeepSet,
	sweep *orphanSweep,
) bool {
	sweep.kindFailures = 0
	for i := 0; i < count; i++ {
		if sweep.budget <= 0 {
			sweep.retry = true
			return false
		}
		if sweep.kindFailures >= GCMaxKindFailuresPerPass {
			log.Info("GC: giving up on this kind for now", "failures", sweep.kindFailures)
			return true
		}
		r.sweepOrphan(ctx, log, engine, at(i), keep, sweep)
	}
	return true
}
