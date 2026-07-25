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
	"strconv"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// gcOrphanedResources deletes StatefulSets, Services, ConfigMaps, and
// per-generation TLS Certificates/Secrets that belong to this engine (by
// LabelEngine) but whose LabelGeneration does not match either
// CurrentGeneration or DrainingGeneration.
//
// Why this exists: Kubernetes does not support multi-resource transactions.
// When computeCreating abandons a generation mid-flight (spec changed while
// pods are still starting), it deletes the current generation's resources and
// bumps CurrentGeneration. If the operator crashes between a resource delete
// and the subsequent status write, or if a delete fails transiently, the
// abandoned generation's resources become orphans — no future reconcile will
// reference them because getEngineState only reads CurrentGeneration and
// DrainingGeneration.
//
// This sweep runs in any terminal phase (PhaseStable or PhaseStopped), where
// exactly which generations should exist is unambiguous, making it a safe,
// eventually-consistent safety net rather than the primary lifecycle mechanism.
func (r *FireboltEngineReconciler) gcOrphanedResources(ctx context.Context, engine *computev1alpha1.FireboltEngine) {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name)

	keepGens := map[string]bool{
		strconv.Itoa(engine.Status.CurrentGeneration): true,
	}
	if engine.Status.DrainingGeneration != nil {
		keepGens[strconv.Itoa(*engine.Status.DrainingGeneration)] = true
	}

	engineLabels := client.MatchingLabels{LabelEngine: engine.Name}
	ns := client.InNamespace(engine.Namespace)

	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list StatefulSets")
		return
	}
	// GC scope invariant: we only sweep resources that explicitly claim a
	// generation via LabelGeneration. Engine-tagged resources without a
	// generation label (the cluster Service today, potentially future
	// per-engine shared resources, or anything a human/other controller
	// labeled by mistake) are out of scope. Treating a missing label as
	// "some non-matching generation" would make an empty gen key fail the
	// keepGens lookup and delete the object, which is a strictly larger
	// blast radius than this safety-net is meant to have.
	for i := range stsList.Items {
		gen := stsList.Items[i].Labels[LabelGeneration]
		if gen == "" {
			continue
		}
		if !keepGens[gen] {
			log.Info("GC: deleting orphaned StatefulSet", "name", stsList.Items[i].Name, "generation", gen)
			if err := r.deleteIfExists(ctx, &stsList.Items[i]); err != nil {
				log.Error(err, "GC: failed to delete StatefulSet", "name", stsList.Items[i].Name)
			}
		}
	}

	svcList := &corev1.ServiceList{}
	if err := r.List(ctx, svcList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list Services")
		return
	}
	for i := range svcList.Items {
		gen := svcList.Items[i].Labels[LabelGeneration]
		if gen == "" {
			continue
		}
		if !keepGens[gen] {
			log.Info("GC: deleting orphaned Service", "name", svcList.Items[i].Name, "generation", gen)
			if err := r.deleteIfExists(ctx, &svcList.Items[i]); err != nil {
				log.Error(err, "GC: failed to delete Service", "name", svcList.Items[i].Name)
			}
		}
	}

	cmList := &corev1.ConfigMapList{}
	if err := r.List(ctx, cmList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list ConfigMaps")
		return
	}
	for i := range cmList.Items {
		gen := cmList.Items[i].Labels[LabelGeneration]
		if gen == "" {
			continue
		}
		if !keepGens[gen] {
			log.Info("GC: deleting orphaned ConfigMap", "name", cmList.Items[i].Name, "generation", gen)
			if err := r.deleteIfExists(ctx, &cmList.Items[i]); err != nil {
				log.Error(err, "GC: failed to delete ConfigMap", "name", cmList.Items[i].Name)
			}
		}
	}

	// Per-generation engine TLS Certificates and their cert-manager-derived
	// Secrets (FB-896 #1) carry LabelEngine + LabelGeneration too (the Secret
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
			return
		}
	} else {
		for i := range certList.Items {
			gen := certList.Items[i].Labels[LabelGeneration]
			if gen == "" {
				continue
			}
			if !keepGens[gen] {
				log.Info("GC: deleting orphaned Certificate", "name", certList.Items[i].Name, "generation", gen)
				if err := r.deleteIfExists(ctx, &certList.Items[i]); err != nil {
					log.Error(err, "GC: failed to delete Certificate", "name", certList.Items[i].Name)
				}
			}
		}
	}

	secretList := &corev1.SecretList{}
	if err := r.List(ctx, secretList, ns, engineLabels); err != nil {
		log.Error(err, "GC: failed to list Secrets")
		return
	}
	for i := range secretList.Items {
		gen := secretList.Items[i].Labels[LabelGeneration]
		if gen == "" {
			continue
		}
		if !keepGens[gen] {
			log.Info("GC: deleting orphaned Secret", "name", secretList.Items[i].Name, "generation", gen)
			if err := r.deleteIfExists(ctx, &secretList.Items[i]); err != nil {
				log.Error(err, "GC: failed to delete Secret", "name", secretList.Items[i].Name)
			}
		}
	}
}
