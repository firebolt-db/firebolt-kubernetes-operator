//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// EngineClass deletion guard end-to-end coverage. Pins the W1
// finalizer-based guard in a real cluster with the EngineClassReconciler
// running in-process. The validating webhook is NOT registered in the
// e2e harness (see test/e2e/helpers_test.go's StartOperator family and
// the project-wide note in AGENTS.md), so this spec exercises the
// controller-side enforcement that is the only protection a default
// chart install ships with.
//
// What this locks in:
//
//   - The EngineClassReconciler stamps the finalizer
//     compute.firebolt.io/engineclass-deletion-guard on first reconcile.
//   - DELETE on a class with at least one bound FireboltEngine leaves
//     the class Terminating with Ready=False/DeletionBlocked and the
//     bound count in the message.
//   - Once the last bound engine is removed, the class reconciler
//     drops the finalizer reactively (via the FireboltEngine watch)
//     and the apiserver completes the delete.
//
// The spec stays lightweight on purpose: no FireboltInstance is
// created and the FireboltEngineReconciler is not started. The
// FireboltEngine CR exists only as a "binding carrier" so the class
// reconciler's countBoundEngines List finds it; the engine doesn't
// need pods, condition status, or any reconcile activity.
var _ = Describe("EngineClass deletion guard", Ordered, Label("e2e", "deletion-guard"), func() {
	const (
		className   = "deletion-guard-class"
		engineName  = "deletion-guard-binder"
		instanceRef = "dummy-instance" // unresolved by design; no engine controller runs in this spec
	)

	var classOp *ClassOperator

	BeforeAll(func() {
		var err error
		classOp, err = StartClassOperator()
		Expect(err).NotTo(HaveOccurred(), "Failed to start EngineClassReconciler")
	})

	AfterAll(func() {
		// Best-effort cleanup. The engine CR has no finalizer (no
		// engine reconciler ran), so its delete is immediate. The
		// class delete proceeds once the engine is gone — the guard
		// reactively drops the finalizer via the FireboltEngine
		// watch the class reconciler already wires.
		Expect(DeleteEngine(context.Background(), engineName)).To(Succeed())
		Expect(DeleteEngineClass(context.Background(), className)).To(Succeed())
		if classOp != nil {
			classOp.Stop()
		}
	})

	It("blocks class deletion while a bound engine exists, releases once the engine is gone", func() {
		ctx := context.Background()
		key := types.NamespacedName{Name: className, Namespace: testNamespace}

		By("Creating the EngineClass")
		Expect(CreateEngineClass(ctx, className, testImage+":"+testTag)).To(Succeed())

		cl, err := getCRDClient()
		Expect(err).NotTo(HaveOccurred())

		By("Waiting for the deletion-guard finalizer to be added")
		Eventually(func(g Gomega) {
			class := &computev1alpha1.EngineClass{}
			g.Expect(cl.Get(ctx, key, class)).To(Succeed())
			g.Expect(class.Finalizers).To(ContainElement("compute.firebolt.io/engineclass-deletion-guard"),
				"finalizer must be stamped before the bound-engine count matters")
		}).WithTimeout(10 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("Creating a FireboltEngine that binds the class")
		// Engine carries the binding only; no controller is running for
		// it in this spec so it never progresses past the bare CR. That
		// is enough — countBoundEngines lists FireboltEngines and counts
		// those whose spec.engineClassRef matches.
		Expect(CreateBareEngineWithClassRef(ctx, instanceRef, engineName, className)).To(Succeed())

		By("Attempting to delete the class")
		Expect(DeleteEngineClass(ctx, className)).To(Succeed(),
			"DELETE returns success because the apiserver accepts the request; the finalizer holds the object alive")

		By("Asserting the class is Terminating with Ready=False/DeletionBlocked")
		Eventually(func(g Gomega) {
			class := &computev1alpha1.EngineClass{}
			g.Expect(cl.Get(ctx, key, class)).To(Succeed())
			g.Expect(class.DeletionTimestamp.IsZero()).To(BeFalse(),
				"DeletionTimestamp must be set after the DELETE call")
			cond := apimeta.FindStatusCondition(class.Status.Conditions, computev1alpha1.EngineClassConditionReady)
			g.Expect(cond).NotTo(BeNil(), "Ready condition must be present once reconcileDelete runs")
			g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(cond.Reason).To(Equal("DeletionBlocked"))
			g.Expect(cond.Message).To(ContainSubstring("FireboltEngine"))
			g.Expect(class.Status.BoundEngines).To(Equal(int32(1)))
		}).WithTimeout(15 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("Deleting the bound engine")
		Expect(DeleteEngine(ctx, engineName)).To(Succeed())

		By("Waiting for the engine CR to be gone")
		Eventually(func(g Gomega) {
			engine := &computev1alpha1.FireboltEngine{}
			err := cl.Get(ctx, types.NamespacedName{Name: engineName, Namespace: testNamespace}, engine)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"engine must be fully gone before the class can release its finalizer (got err=%v)", err)
		}).WithTimeout(10 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("Asserting the class deletion completes once unbound")
		// The reconciler is enqueued by the FireboltEngine watch; the
		// next reconcile pass sees boundEngines == 0 and removes the
		// finalizer. The apiserver then reaps the class.
		Eventually(func(g Gomega) {
			class := &computev1alpha1.EngineClass{}
			err := cl.Get(ctx, key, class)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
				"class should be fully reaped after the bound engine is removed (got err=%v)", err)
		}).WithTimeout(15 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
	})
})
