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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// This spec runs against the REAL envtest apiserver (via the shared
// k8sClient/scheme.Scheme from suite_test.go), which — like any cluster
// that hasn't had cert-manager installed — has cert-manager's Go type
// registered (suite_test.go's BeforeSuite calls certmanagerv1.AddToScheme)
// but no cert-manager CRD manifest applied. That is exactly the
// environment reconcileDelete's Certificate cleanup step must tolerate:
// Listing an uninstalled CRD's kind must not turn an ordinary instance
// deletion (with auth never enabled) into a stuck finalizer.
//
// See the "Certificate MUST be deleted before Secret" comment in
// reconcileDelete for the production bug this closes: without the
// apimeta.IsNoMatchError tolerance in deleteList, this List call errors,
// reconcileDelete never removes the finalizer, and the FireboltInstance
// hangs in Terminating forever on any cluster without cert-manager CRDs
// installed — which, before auth existed, was every cluster.
var _ = Describe("FireboltInstance deletion tolerates a missing cert-manager CRD", func() {
	It("removes the finalizer without error", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		instance := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "gc-no-certmanager-crd",
				Namespace:  "default",
				Finalizers: []string{instanceFinalizerName},
			},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Metadata: computev1alpha1.MetadataSpec{
					Postgres: nil,
				},
			},
		}
		Expect(k8sClient.Create(ctx, instance)).To(Succeed())

		// The finalizer keeps the object present after Delete: the
		// server stamps deletionTimestamp but does not remove the object
		// from etcd until every finalizer is cleared. Re-Get to observe
		// that stamped timestamp, matching what Reconcile sees on a real
		// deletion (DeletionTimestamp.IsZero() == false).
		Expect(k8sClient.Delete(ctx, instance)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, instance)).To(Succeed())
		Expect(instance.DeletionTimestamp.IsZero()).To(BeFalse())

		r := &FireboltInstanceReconciler{
			Client:          k8sClient,
			Scheme:          scheme.Scheme,
			MetricsRecorder: metrics.NoOpInstanceRecorder{},
		}

		// reconcileDelete lists-and-deletes every operator-owned kind by
		// label, including certmanagerv1.CertificateList — the assertion
		// here is that doing so against an apiserver with no
		// cert-manager CRD installed does not surface as an error, and
		// that the finalizer removal it performs actually lets the
		// server complete the deletion.
		Expect(r.reconcileDelete(ctx, instance)).To(Succeed())

		err := k8sClient.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, &computev1alpha1.FireboltInstance{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "instance should be fully deleted once reconcileDelete clears the finalizer, got err=%v", err)
	})
})
