//go:build outerharness

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

// Phase 9 of the formal-verification plan (docs-internal/formal-verification.md):
// rapid stateful property test that drives the FULL FireboltEngineReconciler
// against an envtest API server. The compute-layer harnesses (Phases 2/3/6/8)
// cannot exercise the outer Reconcile's responsibilities — instance gate,
// finalizer add/remove, owner refs on child resources, real-API
// optimistic-concurrency on status updates — and this fills that gap.
//
// Build-tagged so it does not run in default `make test`. Invoke via
// `make test-property`.
//
// Engines are scoped to Replicas=0 with DrainCheckEnabled=false. Two
// consequences:
//   - checkPodsReady is vacuously true (0 of 0 pods ready, no real Pods needed).
//   - drain probes never fire (no envtest pod IPs needed).
// This lets the rapid sequence exercise the full creating → switching →
// draining → cleaning → stopped lifecycle on top of real K8s API semantics
// without needing kubelet or HTTP probes.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"pgregory.net/rapid"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// outerHarnessEnv carries the envtest fixtures shared across rapid draws.
type outerHarnessEnv struct {
	cfg *rest.Config
	cli client.Client
}

// startOuterEnvtest brings up an envtest API server for the outer harness.
// Registered with t.Cleanup so the env stops even on test failure.
func startOuterEnvtest(t *testing.T) *outerHarnessEnv {
	t.Helper()

	if err := computev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if d := getFirstFoundEnvTestBinaryDir(); d != "" {
		env.BinaryAssetsDirectory = d
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest.Stop: %v", err)
		}
	})

	cli, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}

	return &outerHarnessEnv{cfg: cfg, cli: cli}
}

// outerDrawCounter generates a unique namespace per rapid draw so concurrent
// or replayed draws do not see each other's resources.
var outerDrawCounter atomic.Uint64

const (
	outerInstanceName = "test-instance"
	outerEngineName   = "test-engine"
)

// outerEngineSim drives the full FireboltEngineReconciler against envtest.
// Resource state lives in the API server; the sim only carries the per-draw
// fixtures (namespace, reconciler, completion bookkeeping).
type outerEngineSim struct {
	ctx        context.Context
	env        *outerHarnessEnv
	reconciler *FireboltEngineReconciler

	namespace string

	// engineGone tracks whether the engine has been reaped by the API
	// server (Get returns NotFound). Once true, mutating actions become
	// no-ops; Check then validates that no orphaned children remain.
	engineGone bool
}

func (m *outerEngineSim) engineKey() types.NamespacedName {
	return types.NamespacedName{Name: outerEngineName, Namespace: m.namespace}
}

func (m *outerEngineSim) instanceKey() types.NamespacedName {
	return types.NamespacedName{Name: outerInstanceName, Namespace: m.namespace}
}

// ---------- State-machine actions ----------

// Reconcile invokes the real reconciler. Idempotent.
func (m *outerEngineSim) Reconcile(t *rapid.T) {
	if m.engineGone {
		return
	}
	if _, err := m.reconciler.Reconcile(m.ctx, ctrl.Request{NamespacedName: m.engineKey()}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	// reconcileDelete drops the finalizer on its last step; the API server
	// then reaps the engine. Re-Get to detect this transition so subsequent
	// actions in the same rapid run skip cleanly.
	eng := &computev1alpha1.FireboltEngine{}
	if err := m.env.cli.Get(m.ctx, m.engineKey(), eng); errors.IsNotFound(err) {
		m.engineGone = true
	}
}

// ApplySpecChange bumps the engine image, triggering spec drift on the
// next Reconcile.
func (m *outerEngineSim) ApplySpecChange(t *rapid.T) {
	if m.engineGone {
		return
	}
	eng := &computev1alpha1.FireboltEngine{}
	err := m.env.cli.Get(m.ctx, m.engineKey(), eng)
	if errors.IsNotFound(err) {
		m.engineGone = true
		return
	}
	if err != nil {
		t.Fatalf("ApplySpecChange Get: %v", err)
	}
	if !eng.DeletionTimestamp.IsZero() {
		return
	}
	v := rapid.IntRange(1, 99).Draw(t, "saVersion")
	if eng.Spec.Template == nil {
		eng.Spec.Template = &corev1.PodTemplateSpec{}
	}
	eng.Spec.Template.Spec.ServiceAccountName = fmt.Sprintf("sa-v%d", v)
	if err := m.env.cli.Update(m.ctx, eng); err != nil {
		// Conflicts are benign here: another action (e.g. a Reconcile that
		// wrote status) bumped the resource version. The next Reconcile will
		// catch up; the spec change will be re-applied on the next draw of
		// this action.
		if errors.IsConflict(err) || errors.IsNotFound(err) {
			return
		}
		t.Fatalf("ApplySpecChange Update: %v", err)
	}
}

// DeleteEngine sets DeletionTimestamp on the engine. The next Reconcile runs
// reconcileDelete, which deletes children and removes the finalizer.
func (m *outerEngineSim) DeleteEngine(t *rapid.T) {
	if m.engineGone {
		return
	}
	eng := &computev1alpha1.FireboltEngine{}
	err := m.env.cli.Get(m.ctx, m.engineKey(), eng)
	if errors.IsNotFound(err) {
		m.engineGone = true
		return
	}
	if err != nil {
		t.Fatalf("DeleteEngine Get: %v", err)
	}
	if !eng.DeletionTimestamp.IsZero() {
		return
	}
	if err := m.env.cli.Delete(m.ctx, eng); err != nil && !errors.IsNotFound(err) {
		t.Fatalf("DeleteEngine: %v", err)
	}
}

// InstanceReady sets the parent FireboltInstance's MetadataEndpoint and ID,
// which is what the engine's instance gate (resolveInstanceInfo) keys on.
func (m *outerEngineSim) InstanceReady(t *rapid.T) {
	inst := &computev1alpha1.FireboltInstance{}
	if err := m.env.cli.Get(m.ctx, m.instanceKey(), inst); err != nil {
		t.Fatalf("InstanceReady Get: %v", err)
	}
	if inst.Status.MetadataEndpoint != "" {
		return
	}
	inst.Status.MetadataEndpoint = "test-metadata.svc.cluster.local:8080"
	inst.Status.MetadataReady = true
	inst.Status.Phase = computev1alpha1.InstancePhaseReady
	if err := m.env.cli.Status().Update(m.ctx, inst); err != nil {
		if errors.IsConflict(err) {
			return
		}
		t.Fatalf("InstanceReady Status().Update: %v", err)
	}
}

// InstanceDegraded clears the metadata endpoint, blocking the engine's gate
// for the phases that consume it (stable, stopped, creating).
func (m *outerEngineSim) InstanceDegraded(t *rapid.T) {
	inst := &computev1alpha1.FireboltInstance{}
	if err := m.env.cli.Get(m.ctx, m.instanceKey(), inst); err != nil {
		t.Fatalf("InstanceDegraded Get: %v", err)
	}
	if inst.Status.MetadataEndpoint == "" {
		return
	}
	inst.Status.MetadataEndpoint = ""
	inst.Status.MetadataReady = false
	inst.Status.Phase = computev1alpha1.InstancePhaseDegraded
	if err := m.env.cli.Status().Update(m.ctx, inst); err != nil {
		if errors.IsConflict(err) {
			return
		}
		t.Fatalf("InstanceDegraded Status().Update: %v", err)
	}
}

// ---------- Invariants ----------

// Check runs after every action.
func (m *outerEngineSim) Check(t *rapid.T) {
	eng := &computev1alpha1.FireboltEngine{}
	err := m.env.cli.Get(m.ctx, m.engineKey(), eng)
	if errors.IsNotFound(err) {
		m.engineGone = true
		m.checkNoOrphans(t)
		return
	}
	if err != nil {
		t.Fatalf("Check Get engine: %v", err)
	}

	// Status invariants — same shape as the compute-layer rapid harness.
	s := eng.Status
	if isTerminalPhase(s.Phase) && s.CurrentGeneration != s.ActiveGeneration {
		t.Fatalf("Inv_TerminalConsistency: phase=%s but CurrentGen=%d != ActiveGen=%d",
			s.Phase, s.CurrentGeneration, s.ActiveGeneration)
	}
	if isTerminalPhase(s.Phase) && s.DrainingGeneration != nil {
		t.Fatalf("Inv_TerminalNoDraining: phase=%s but DrainingGen=%d",
			s.Phase, *s.DrainingGeneration)
	}
	if s.DrainingGeneration != nil &&
		s.Phase != computev1alpha1.PhaseDraining &&
		s.Phase != computev1alpha1.PhaseCleaning {
		t.Fatalf("Inv_DrainingPhase: DrainingGen=%d but phase=%s",
			*s.DrainingGeneration, s.Phase)
	}

	// Owner refs: every surviving child resource must point back at the engine.
	m.checkOwnerRefs(t, eng)
}

// checkNoOrphans is the post-deletion invariant: once the engine is reaped,
// no child resources matching its labels may remain.
func (m *outerEngineSim) checkNoOrphans(t *rapid.T) {
	matchLabels := client.MatchingLabels{LabelEngine: outerEngineName}
	inNs := client.InNamespace(m.namespace)

	var stsList appsv1.StatefulSetList
	if err := m.env.cli.List(m.ctx, &stsList, inNs, matchLabels); err != nil {
		t.Fatalf("List STatefulSets after delete: %v", err)
	}
	if len(stsList.Items) > 0 {
		t.Fatalf("Inv_DeleteCleansChildren: %d StatefulSet(s) survive after engine reaped",
			len(stsList.Items))
	}

	var cmList corev1.ConfigMapList
	if err := m.env.cli.List(m.ctx, &cmList, inNs, matchLabels); err != nil {
		t.Fatalf("List ConfigMaps after delete: %v", err)
	}
	if len(cmList.Items) > 0 {
		t.Fatalf("Inv_DeleteCleansChildren: %d ConfigMap(s) survive after engine reaped",
			len(cmList.Items))
	}

	var svcList corev1.ServiceList
	if err := m.env.cli.List(m.ctx, &svcList, inNs, matchLabels); err != nil {
		t.Fatalf("List Services after delete: %v", err)
	}
	if len(svcList.Items) > 0 {
		t.Fatalf("Inv_DeleteCleansChildren: %d Service(s) survive after engine reaped",
			len(svcList.Items))
	}
}

// checkOwnerRefs enforces that every child resource carrying this engine's
// label has an ownerRef pointing back at the engine. Without this guarantee,
// API-server GC would not cascade-delete children when the engine is reaped.
func (m *outerEngineSim) checkOwnerRefs(t *rapid.T, eng *computev1alpha1.FireboltEngine) {
	matchLabels := client.MatchingLabels{LabelEngine: outerEngineName}
	inNs := client.InNamespace(m.namespace)

	var stsList appsv1.StatefulSetList
	if err := m.env.cli.List(m.ctx, &stsList, inNs, matchLabels); err != nil {
		t.Fatalf("List STatefulSets for ownerRef check: %v", err)
	}
	for i := range stsList.Items {
		assertOwnedByEngine(t, &stsList.Items[i], eng, "StatefulSet")
	}

	var cmList corev1.ConfigMapList
	if err := m.env.cli.List(m.ctx, &cmList, inNs, matchLabels); err != nil {
		t.Fatalf("List ConfigMaps for ownerRef check: %v", err)
	}
	for i := range cmList.Items {
		assertOwnedByEngine(t, &cmList.Items[i], eng, "ConfigMap")
	}

	var svcList corev1.ServiceList
	if err := m.env.cli.List(m.ctx, &svcList, inNs, matchLabels); err != nil {
		t.Fatalf("List Services for ownerRef check: %v", err)
	}
	for i := range svcList.Items {
		assertOwnedByEngine(t, &svcList.Items[i], eng, "Service")
	}
}

func assertOwnedByEngine(t *rapid.T, obj client.Object, eng *computev1alpha1.FireboltEngine, kind string) {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == eng.UID && ref.Kind == "FireboltEngine" {
			return
		}
	}
	t.Fatalf("Inv_OwnerRef: %s %q is missing ownerRef to engine %q (UID %s)",
		kind, obj.GetName(), eng.Name, eng.UID)
}

// ---------- Test entry point ----------

func TestEngineOuterStateMachine(t *testing.T) {
	env := startOuterEnvtest(t)

	rapid.Check(t, func(rt *rapid.T) {
		ns := fmt.Sprintf("outer-prop-%d", outerDrawCounter.Add(1))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Per-draw fixture: namespace + ready instance + replicas-0 engine.
		if err := env.cli.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}); err != nil {
			rt.Fatalf("Create namespace: %v", err)
		}
		// Best-effort cleanup. envtest namespace deletion is asynchronous; we
		// do not block on it because each draw uses a fresh namespace name.
		defer func() {
			_ = env.cli.Delete(context.Background(), &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: ns},
			})
		}()

		instance := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      outerInstanceName,
				Namespace: ns,
			},
			Spec: computev1alpha1.FireboltInstanceSpec{
				ID:       fmt.Sprintf("test-account-%s", ns),
				Metadata: computev1alpha1.MetadataSpec{},
				Gateway:  computev1alpha1.GatewaySpec{},
			},
		}
		if err := env.cli.Create(ctx, instance); err != nil {
			rt.Fatalf("Create instance: %v", err)
		}
		instance.Status.MetadataEndpoint = "test-metadata.svc.cluster.local:8080"
		instance.Status.MetadataReady = true
		instance.Status.Phase = computev1alpha1.InstancePhaseReady
		if err := env.cli.Status().Update(ctx, instance); err != nil {
			rt.Fatalf("Set instance Ready: %v", err)
		}

		falseVal := false
		engine := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      outerEngineName,
				Namespace: ns,
			},
			Spec: computev1alpha1.FireboltEngineSpec{
				InstanceRef:       outerInstanceName,
				Replicas:          0,
				DrainCheckEnabled: &falseVal,
			},
		}
		if err := env.cli.Create(ctx, engine); err != nil {
			rt.Fatalf("Create engine: %v", err)
		}

		reconciler := &FireboltEngineReconciler{
			Client:          env.cli,
			Scheme:          scheme.Scheme,
			MetricsRecorder: metrics.NoOpEngineRecorder{},
		}

		m := &outerEngineSim{
			ctx:        ctx,
			env:        env,
			reconciler: reconciler,
			namespace:  ns,
		}

		rt.Repeat(rapid.StateMachineActions(m))
	})
}
