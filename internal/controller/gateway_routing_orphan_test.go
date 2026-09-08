// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func orphanRoutingFixture(t *testing.T) (*FireboltInstanceReconciler, *corev1.ConfigMap, *computev1alpha1.FireboltEngine) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Namespace: "orphan", Name: "instance", UID: "original-instance"}}
	state := routing.NewState(string(instance.UID))
	if _, err := routing.CloseInstance(&state); err != nil {
		t.Fatal(err)
	}
	data, err := routing.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace, Name: routing.ConfigMapName(instance.Name), UID: "proof", Finalizers: []string{gatewayRoutingStateFinalizer}}, Data: map[string]string{routing.DataKey: string(data)}}
	if err := controllerutil.SetControllerReference(instance, cm, scheme); err != nil {
		t.Fatal(err)
	}
	engine := &computev1alpha1.FireboltEngine{ObjectMeta: metav1.ObjectMeta{Namespace: instance.Namespace, Name: "engine", UID: "engine", Finalizers: []string{finalizerName}}, Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: instance.Name}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, engine).Build()
	return &FireboltInstanceReconciler{Client: c, APIReader: c, Scheme: scheme}, cm, engine
}

func TestUninitializedEngineDeletionWithoutRoutingState(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, computev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		status  computev1alpha1.FireboltEngineStatus
		child   client.Object
		allowed bool
	}{
		{name: "no status", allowed: true},
		{name: "initialized awaiting missing Instance", status: computev1alpha1.FireboltEngineStatus{Phase: computev1alpha1.PhaseCreating, ActiveGeneration: -1}, allowed: true},
		{name: "previously active", status: computev1alpha1.FireboltEngineStatus{Phase: computev1alpha1.PhaseStable}},
		{name: "later generation", status: computev1alpha1.FireboltEngineStatus{Phase: computev1alpha1.PhaseCreating, ActiveGeneration: -1, CurrentGeneration: 1}},
		{name: "StatefulSet exists", child: &appsv1.StatefulSet{}},
		{name: "Pod exists", child: &corev1.Pod{}},
		{name: "Service exists", child: &corev1.Service{}},
		{name: "ConfigMap exists", child: &corev1.ConfigMap{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &computev1alpha1.FireboltEngine{ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "engine"}, Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: "missing"}, Status: tc.status}
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.child != nil {
				tc.child.SetNamespace(engine.Namespace)
				tc.child.SetName("generation")
				tc.child.SetLabels(map[string]string{LabelEngine: engine.Name})
				builder.WithObjects(tc.child)
			}
			c := builder.Build()
			r := &FireboltEngineReconciler{Client: c, APIReader: c}
			allowed, err := r.allowUninitializedEngineDeletion(context.Background(), engine)
			if err != nil || allowed != tc.allowed {
				t.Fatalf("allowed=%t, err=%v; wanted allowed=%t", allowed, err, tc.allowed)
			}
		})
	}
}

func TestOrphanRoutingProofSurvivesUntilEngineDeletionCompletes(t *testing.T) {
	r, cm, engine := orphanRoutingFixture(t)
	ctx := context.Background()
	key := types.NamespacedName{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef}
	if err := r.Delete(ctx, engine); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanupOrphanGatewayRouting(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("terminating Engine lost shutdown proof: %v", err)
	}
	if !controllerutil.ContainsFinalizer(cm, gatewayRoutingStateFinalizer) {
		t.Fatal("terminating Engine lost proof finalizer")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
		t.Fatal(err)
	}
	controllerutil.RemoveFinalizer(engine, finalizerName)
	if err := r.Update(ctx, engine); err != nil {
		t.Fatal(err)
	}
	// A new reconciler models recovery after the Engine deletion completed but
	// before its event was processed; the retained ConfigMap is the retry source.
	restarted := &FireboltInstanceReconciler{Client: r.Client, APIReader: r.APIReader}
	if _, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); !apierrors.IsNotFound(err) {
		t.Fatalf("unused routing proof was not reclaimed: %v", err)
	}
}

func TestOrphanRoutingProofAllowsEngineRemovalDuringInstanceRecreation(t *testing.T) {
	r, cm, engine := orphanRoutingFixture(t)
	ctx := context.Background()
	er := &FireboltEngineReconciler{Client: r.Client, APIReader: r.APIReader}
	if err := er.authorizeGenerationRemoval(ctx, engine, 0); err != nil {
		t.Fatalf("deleted Instance prevents Engine generation cleanup: %v", err)
	}
	replacement := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef, UID: "replacement-instance"}}
	if err := r.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayRouting(ctx, replacement); err == nil {
		t.Fatal("replacement erased routing proof while old Engine still exists")
	}
	if err := er.authorizeGenerationRemoval(ctx, engine, 0); err != nil {
		t.Fatalf("replacement Instance prevents predecessor Engine cleanup: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
		t.Fatal(err)
	}
	controllerutil.RemoveFinalizer(engine, finalizerName)
	if err := r.Update(ctx, engine); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, engine); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureGatewayRouting(ctx, replacement); err != nil {
		t.Fatalf("replacement cannot initialize after predecessor Engine deletion: %v", err)
	}
	_, state, err := readRoutingState(ctx, r.APIReader, cm.Namespace, replacement.Name)
	if err != nil || state.InstanceUID != string(replacement.UID) || state.Closed {
		t.Fatalf("replacement did not receive a fresh admission record: state=%+v err=%v", state, err)
	}
}

func TestRoutingInitializationRejectsStaleInstance(t *testing.T) {
	for _, replacementExists := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacementExists), func(t *testing.T) {
			r, cm, engine := orphanRoutingFixture(t)
			ctx := context.Background()
			stale := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef, UID: "original-instance"}}
			if replacementExists {
				replacement := stale.DeepCopy()
				replacement.UID = "replacement-instance"
				if err := r.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.ensureGatewayRouting(ctx, stale); err == nil {
				t.Fatal("stale Instance initialized routing")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
				t.Fatalf("stale reconcile changed predecessor proof: %v", err)
			}
			state, err := routing.Decode([]byte(cm.Data[routing.DataKey]))
			if err != nil || !state.Closed || state.InstanceUID != string(stale.UID) {
				t.Fatalf("stale reconcile changed predecessor proof: state=%+v err=%v", state, err)
			}
		})
	}
}

func TestOrphanRoutingProofAllowsDeletingSameNameNewEngine(t *testing.T) {
	r, cm, engine := orphanRoutingFixture(t)
	ctx := context.Background()
	if err := appsv1.AddToScheme(r.Scheme); err != nil {
		t.Fatal(err)
	}
	state := routing.NewState("original-instance")
	if _, err := routing.SetRoute(&state, engine.Name, routing.Route{EngineUID: "deleted-predecessor", Generation: 0, Authority: "old:3473", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.CloseInstance(&state); err != nil {
		t.Fatal(err)
	}
	data, err := routing.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[routing.DataKey] = string(data)
	if err := r.Update(ctx, cm); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef}
	if err := r.cleanupOrphanGatewayRouting(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("new bound Engine did not retain predecessor proof: %v", err)
	}
	if err := r.Delete(ctx, engine); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
		t.Fatal(err)
	}
	er := &FireboltEngineReconciler{Client: r.Client, APIReader: r.APIReader, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	if err := er.reconcileDelete(ctx, engine); err != nil {
		t.Fatalf("new Engine cannot be deleted against predecessor's closed routing proof: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(engine), engine); !apierrors.IsNotFound(err) {
		t.Fatalf("new Engine remains stuck deleting: %v", err)
	}
	if err := r.cleanupOrphanGatewayRouting(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); !apierrors.IsNotFound(err) {
		t.Fatalf("predecessor proof remains stuck after new Engine deletion: %v", err)
	}
}

func TestNewEngineCannotWithdrawPredecessorWithoutClosedProof(t *testing.T) {
	for _, closed := range []bool{false, true} {
		t.Run(fmt.Sprintf("closed=%t", closed), func(t *testing.T) {
			r, cm, engine := orphanRoutingFixture(t)
			ctx := context.Background()
			state := routing.NewState("original-instance")
			if _, err := routing.RegisterSession(&state, "gateway", "boot"); err != nil {
				t.Fatal(err)
			}
			if _, err := routing.SetRoute(&state, engine.Name, routing.Route{EngineUID: "predecessor", Generation: 0, Authority: "old:3473", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			if closed {
				if _, err := routing.CloseInstance(&state); err != nil {
					t.Fatal(err)
				}
			}
			data, err := routing.Encode(state)
			if err != nil {
				t.Fatal(err)
			}
			cm.Data[routing.DataKey] = string(data)
			if err := r.Update(ctx, cm); err != nil {
				t.Fatal(err)
			}
			er := &FireboltEngineReconciler{Client: r.Client, APIReader: r.APIReader}
			if err := er.withdrawEngineRoute(ctx, engine); err == nil {
				t.Fatal("new Engine withdrew predecessor's route without complete shutdown proof")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
				t.Fatal(err)
			}
			if cm.Data[routing.DataKey] != string(data) {
				t.Fatal("rejected withdrawal changed predecessor's routing state")
			}
		})
	}
}

func TestClosedGatewayRoutingProofRejectsMissingSessionsAndDetachedHolders(t *testing.T) {
	state := routing.NewState("instance")
	if _, err := routing.RegisterSession(&state, "lost-pod", "lost-boot"); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.SetRoute(&state, "engine", routing.Route{EngineUID: "engine", Generation: 1, Authority: "engine:3473", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.CloseInstance(&state); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(closedGatewayRoutingProof(state), errGatewayWithdrawalPending) {
		t.Fatal("unobserved gateway termination was accepted")
	}
	delete(state.Sessions, "lost-pod")
	if !errors.Is(closedGatewayRoutingProof(state), errGatewayWithdrawalPending) {
		t.Fatal("detached retirement holder was accepted")
	}
}

func TestOrphanRoutingCleanupUsesUncachedEngineList(t *testing.T) {
	r, cm, engine := orphanRoutingFixture(t)
	// The cache has already dropped all Engines, but the authoritative reader
	// still sees one. Reclaiming from the cache would erase its shutdown proof.
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(cm).Build()
	if err := r.cleanupOrphanGatewayRouting(context.Background(), types.NamespacedName{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("uncached bound Engine did not preserve proof: %v", err)
	}
}

func TestOrphanRoutingCleanupFailsClosedOnReadErrors(t *testing.T) {
	r, cm, engine := orphanRoutingFixture(t)
	failure := errors.New("API unavailable")
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(cm, engine).WithInterceptorFuncs(interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error { return failure }}).Build()
	if err := r.cleanupOrphanGatewayRouting(context.Background(), types.NamespacedName{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef}); !errors.Is(err, failure) {
		t.Fatalf("wanted authoritative read failure, got %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatalf("API read failure lost proof: %v", err)
	}
}

func TestOrphanRoutingCleanupRecoversInterruptedFinalizerRemoval(t *testing.T) {
	r, cm, engine := orphanRoutingFixture(t)
	ctx := context.Background()
	interrupted := errors.New("operator interrupted before finalizer update")
	failUpdate := true
	c := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(cm).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if failUpdate {
				return interrupted
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()
	r.Client, r.APIReader = c, c
	key := types.NamespacedName{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef}
	if err := r.cleanupOrphanGatewayRouting(ctx, key); !errors.Is(err, interrupted) {
		t.Fatalf("wanted interruption, got %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatal(err)
	}
	if cm.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(cm, gatewayRoutingStateFinalizer) {
		t.Fatal("interrupted cleanup did not preserve a discoverable, protected routing proof")
	}
	failUpdate = false
	restarted := &FireboltInstanceReconciler{Client: c, APIReader: c}
	if _, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cm), cm); !apierrors.IsNotFound(err) {
		t.Fatalf("restarted cleanup did not finish: %v", err)
	}
}

func TestOrphanRoutingCleanupDistinguishesInstanceIncarnations(t *testing.T) {
	for _, uid := range []types.UID{"original-instance", "replacement-instance"} {
		t.Run(string(uid), func(t *testing.T) {
			r, cm, engine := orphanRoutingFixture(t)
			ctx := context.Background()
			engine.Finalizers = nil
			if err := r.Update(ctx, engine); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete(ctx, engine); err != nil {
				t.Fatal(err)
			}
			instance := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Namespace: cm.Namespace, Name: engine.Spec.InstanceRef, UID: uid}}
			if err := r.Create(ctx, instance); err != nil {
				t.Fatal(err)
			}
			if err := r.cleanupOrphanGatewayRouting(ctx, client.ObjectKeyFromObject(instance)); err != nil {
				t.Fatal(err)
			}
			err := r.Get(ctx, client.ObjectKeyFromObject(cm), cm)
			if uid == "original-instance" && err != nil {
				t.Fatalf("live original Instance lost its routing state: %v", err)
			}
			if uid == "replacement-instance" && !apierrors.IsNotFound(err) {
				t.Fatalf("replacement Instance remains blocked by unused predecessor proof: %v", err)
			}
		})
	}
}
