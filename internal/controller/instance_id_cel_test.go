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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// spec.id may change case only, enforced by a field-scoped CEL transition
// rule on the CRD so it holds when the validating webhook is disabled
// (the shipped Helm default). This suite runs against envtest with the
// CRD bases applied and NO webhook installed (see suite_test.go).
var _ = Describe("FireboltInstance spec.id case-only CEL (webhook-free)", func() {
	const ns = "default"
	ctx := context.Background()
	const uppercaseID = "01ARZ3NDEKTSV4RRFFQ69G5FAV"

	mutateWithRetry := func(name string, mutate func(*computev1alpha1.FireboltInstance)) error {
		var result error
		Eventually(func() bool {
			var cur computev1alpha1.FireboltInstance
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &cur); err != nil {
				result = err
				return true
			}
			mutate(&cur)
			result = k8sClient.Update(ctx, &cur)
			return !apierrors.IsConflict(result)
		}, 10*time.Second, 200*time.Millisecond).Should(BeTrue())
		return result
	}

	It("allows a case-only update of spec.id", func() {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-id-case-only", Namespace: ns},
			Spec:       computev1alpha1.FireboltInstanceSpec{ID: uppercaseID},
		}
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		Expect(mutateWithRetry(inst.Name, func(cur *computev1alpha1.FireboltInstance) {
			cur.Spec.ID = strings.ToLower(uppercaseID)
		})).To(Succeed())
	})

	It("rejects changing spec.id to a different value", func() {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-id-immutable", Namespace: ns},
			Spec:       computev1alpha1.FireboltInstanceSpec{ID: uppercaseID},
		}
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), inst) }()

		err := mutateWithRetry(inst.Name, func(cur *computev1alpha1.FireboltInstance) {
			cur.Spec.ID = computev1alpha1.MintInstanceID()
		})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("spec.id is immutable"))
	})
})
