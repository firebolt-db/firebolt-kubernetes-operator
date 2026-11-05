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
	"encoding/json"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	DescribeTable("validates explicit IDs at creation without a webhook",
		func(id string, valid bool) {
			// Use an unstructured object so the empty-string case reaches the
			// API server explicitly instead of being omitted by the Go JSON tag.
			inst := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": "compute.firebolt.io/v1alpha1",
				"kind":       "FireboltInstance",
				"metadata": map[string]interface{}{
					"generateName": "id-validation-",
					"namespace":    ns,
				},
				"spec": map[string]interface{}{
					"id":       id,
					"metadata": map[string]interface{}{},
					"gateway":  map[string]interface{}{},
				},
			}}
			err := k8sClient.Create(ctx, inst)
			if err == nil {
				DeferCleanup(func() {
					Expect(k8sClient.Delete(context.Background(), inst)).To(Succeed())
				})
			}
			if valid {
				Expect(err).NotTo(HaveOccurred())
			} else {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected invalid spec.id, got %v", err)
				Expect(err.Error()).To(ContainSubstring("spec.id"))
			}
		},
		Entry("empty, for controller defaulting", "", true),
		Entry("lowercase", strings.ToLower(uppercaseID), true),
		Entry("uppercase, for the image-gated migration", uppercaseID, true),
		Entry("mixed case", "01arz3NDEKTSV4RRFFQ69G5FAV", true),
		Entry("minimum ULID", strings.Repeat("0", 26), true),
		Entry("maximum ULID", "7"+strings.Repeat("z", 25), true),
		Entry("arbitrary string", "my-custom-id", false),
		Entry("short", strings.Repeat("0", 25), false),
		Entry("long", strings.Repeat("0", 27), false),
		Entry("128-bit overflow", "8"+strings.Repeat("0", 25), false),
		Entry("non-digit first character", "a"+strings.Repeat("0", 25), false),
		Entry("forbidden i", strings.Repeat("0", 25)+"i", false),
		Entry("forbidden L", strings.Repeat("0", 25)+"L", false),
		Entry("forbidden o", strings.Repeat("0", 25)+"o", false),
		Entry("forbidden U", strings.Repeat("0", 25)+"U", false),
		Entry("punctuation", strings.Repeat("0", 25)+"-", false),
		Entry("trailing newline", strings.ToLower(uppercaseID)+"\n", false),
	)

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

	It("allows an omitted ID and the controller's empty-to-generated update", func() {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "cel-id-default", Namespace: ns},
		}
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
		DeferCleanup(func() {
			Expect(k8sClient.Delete(context.Background(), inst)).To(Succeed())
		})
		Expect(inst.Spec.ID).To(BeEmpty())

		Expect(mutateWithRetry(inst.Name, func(cur *computev1alpha1.FireboltInstance) {
			cur.Spec.ID = "invalid-id"
		})).To(MatchError(ContainSubstring("spec.id")))
		reconciler := &FireboltInstanceReconciler{Client: k8sClient}
		requeue, err := reconciler.ensureInstanceID(ctx, inst)
		Expect(err).NotTo(HaveOccurred())
		Expect(requeue).To(BeTrue())
		got := &computev1alpha1.FireboltInstance{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(inst), got)).To(Succeed())
		Expect(got.Spec.ID).To(Equal(inst.Spec.ID))
		Expect(got.Spec.ID).To(HaveLen(26))
		Expect(got.Spec.ID).To(Equal(strings.ToLower(got.Spec.ID)))
	})

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

// Exercise Kubernetes' schema validator against the generated CRD: tightening
// the pattern must not prevent unrelated updates to an unchanged legacy ID on
// clusters with validation ratcheting enabled. CEL identity immutability is
// covered separately by the API-server tests above.
func TestInstanceIDSchemaRatcheting(t *testing.T) {
	crd := loadCRDMap(t, "../../config/crd/bases/compute.firebolt.io_fireboltinstances.yaml")
	versions := crd["spec"].(map[string]any)["versions"].([]any)
	schema := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"]
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var props apiextensions.JSONSchemaProps
	if err := json.Unmarshal(raw, &props); err != nil {
		t.Fatal(err)
	}
	validator, _, err := validation.NewSchemaValidator(&props)
	if err != nil {
		t.Fatal(err)
	}
	oldSpec := map[string]any{
		"id": "legacy-invalid-id", "metadata": map[string]any{}, "gateway": map[string]any{},
	}
	old := map[string]any{"spec": oldSpec}
	updated := map[string]any{
		"spec":     oldSpec,
		"metadata": map[string]any{"annotations": map[string]any{"note": "updated"}},
	}
	if result := validator.Validate(updated); result.IsValid() {
		t.Fatal("creating an instance with a malformed ID must fail")
	}
	if result := validator.ValidateUpdate(updated, old, validation.WithRatcheting(nil)); !result.IsValid() {
		t.Fatalf("unchanged malformed ID must allow unrelated updates with ratcheting: %v", result.Errors)
	}
	updated["spec"] = map[string]any{
		"id": "another-invalid-id", "metadata": map[string]any{}, "gateway": map[string]any{},
	}
	if result := validator.ValidateUpdate(updated, old, validation.WithRatcheting(nil)); result.IsValid() {
		t.Fatal("ratcheting must not admit a different malformed ID")
	}
}
