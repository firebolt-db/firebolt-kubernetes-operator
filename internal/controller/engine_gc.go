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
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// gcOrphanedResources deletes StatefulSets, Services, ConfigMaps, and
// per-generation TLS Certificates/Secrets that belong to this engine (by
// LabelEngine) but whose LabelGeneration matches none of CurrentGeneration,
// ActiveGeneration, or DrainingGeneration. It returns true when the pass left
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

	keepGens := map[string]bool{
		strconv.Itoa(engine.Status.CurrentGeneration): true,
	}
	// Negative means "none yet" for ActiveGeneration (pre-first-switch) and is
	// never a label value, but keeping the guard explicit documents that the
	// sweep is protecting a generation that exists.
	if engine.Status.ActiveGeneration >= 0 {
		keepGens[strconv.Itoa(engine.Status.ActiveGeneration)] = true
	}
	if engine.Status.DrainingGeneration != nil {
		keepGens[strconv.Itoa(*engine.Status.DrainingGeneration)] = true
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
	for i := range stsList.Items {
		if !r.sweepOrphan(ctx, log, &stsList.Items[i], keepGens, sweep) {
			return sweep.retry
		}
	}

	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list Services")
		return true
	}
	for i := range svcList.Items {
		if !r.sweepOrphan(ctx, log, &svcList.Items[i], keepGens, sweep) {
			return sweep.retry
		}
	}

	cmList := &corev1.ConfigMapList{}
	if err := r.List(ctx, cmList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list ConfigMaps")
		return true
	}
	for i := range cmList.Items {
		if !r.sweepOrphan(ctx, log, &cmList.Items[i], keepGens, sweep) {
			return sweep.retry
		}
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
		for i := range certList.Items {
			if !r.sweepOrphan(ctx, log, &certList.Items[i], keepGens, sweep) {
				return sweep.retry
			}
		}
	}

	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list Secrets")
		return true
	}
	for i := range secretList.Items {
		if !r.sweepOrphan(ctx, log, &secretList.Items[i], keepGens, sweep) {
			return sweep.retry
		}
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
	obj client.Object,
	keepGens map[string]bool,
	sweep *orphanSweep,
) bool {
	gen := obj.GetLabels()[LabelGeneration]
	if gen == "" || keepGens[gen] || obj.GetDeletionTimestamp() != nil {
		return true
	}
	if sweep.budget <= 0 {
		sweep.retry = true
		return false
	}
	sweep.budget--

	kind := fmt.Sprintf("%T", obj)
	log.Info("GC: deleting orphaned resource", "kind", kind, "name", obj.GetName(), "generation", gen)
	if err := r.deleteIfExists(ctx, obj); err != nil {
		log.Error(err, "GC: failed to delete orphaned resource", "kind", kind, "name", obj.GetName())
		sweep.retry = true
	}
	return true
}
