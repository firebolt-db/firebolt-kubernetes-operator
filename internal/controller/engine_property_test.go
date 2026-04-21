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

// Phase 2 of the formal verification plan (docs/formal-verification.md):
// stateful property tests that drive computeEngineReconcile with random
// operation sequences and check the same invariants modeled in
// formal/FireboltEngine.tla after every step.
//
// The test runs entirely in-memory against the pure computeEngineReconcile
// function — no envtest, no Kubernetes API server required.  This makes it
// fast enough to run under `make test` with a large number of draws.
//
// CrashReconcile simulates a crash between the last resource write and the
// status update in applyEngineState: resources are applied but the status
// write is omitted.  The next Reconcile call then exercises crash recovery.

import (
	"fmt"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"pgregory.net/rapid"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	propEngineName = "prop-engine"
	propNamespace  = "prop-ns"
)

// engineSim is the state machine.  It holds the simulated cluster state and
// drives computeEngineReconcile to verify invariants from the TLA+ spec.
type engineSim struct {
	spec   computev1alpha1.FireboltEngineSpec
	status computev1alpha1.FireboltEngineStatus

	// stses tracks which generation StatefulSets exist.
	stses map[int]*appsv1.StatefulSet

	// clusterSvc is the single cluster-facing Service; nil means absent.
	clusterSvc *corev1.Service

	// podsReady reflects whether all pods in currentGen are Running+Ready.
	// Reset to false whenever currentGen is bumped.
	podsReady bool

	// podsDrained reflects whether the draining gen has zero active queries.
	// Reset to false whenever a new drainingGen is established.
	podsDrained bool
}

// buildState constructs the EngineState to pass to computeEngineReconcile
// from the current simulated cluster view.
func (m *engineSim) buildState() EngineState {
	state := EngineState{ClusterServiceTargetGen: -1}

	gen := m.status.CurrentGeneration
	if sts := m.stses[gen]; sts != nil {
		state.CurrentSTS = sts
		state.CurrentPodsReady = m.podsReady
	}

	if m.status.DrainingGeneration != nil {
		dg := *m.status.DrainingGeneration
		if sts := m.stses[dg]; sts != nil {
			state.DrainingSTS = sts
			state.DrainingPodsDrained = m.podsDrained
		} else {
			// STS already deleted — treat drain as complete.
			state.DrainingPodsDrained = true
		}
	}

	if m.clusterSvc != nil {
		state.ClusterService = m.clusterSvc
		if s, ok := m.clusterSvc.Spec.Selector[LabelGeneration]; ok {
			if g, err := strconv.Atoi(s); err == nil {
				state.ClusterServiceTargetGen = g
			}
		}
	}

	return state
}

// applyResult applies an EngineReconcileResult to the simulated cluster state.
// Pass applyStatus=false to simulate a crash before the status write.
func (m *engineSim) applyResult(result *EngineReconcileResult, applyStatus bool) {
	if result.EnsureStatefulSet != nil {
		gen := stsLabelGen(result.EnsureStatefulSet.Labels)
		m.stses[gen] = result.EnsureStatefulSet
	}
	if result.EnsureClusterSvc != nil {
		m.clusterSvc = result.EnsureClusterSvc
	}
	for _, obj := range result.DeleteResources {
		if sts, ok := obj.(*appsv1.StatefulSet); ok {
			gen := stsLabelGen(sts.Labels)
			if gen >= 0 {
				delete(m.stses, gen)
			}
		}
	}

	if !applyStatus {
		return
	}

	prevCurrentGen := m.status.CurrentGeneration
	prevDrainingGen := m.status.DrainingGeneration
	m.status = result.Status

	if m.status.CurrentGeneration != prevCurrentGen {
		m.podsReady = false
	}

	newDG := m.status.DrainingGeneration
	if newDG != nil && (prevDrainingGen == nil || *prevDrainingGen != *newDG) {
		m.podsDrained = false
	}
}

// stsLabelGen extracts the generation number from an object's labels.
func stsLabelGen(labels map[string]string) int {
	if s, ok := labels[LabelGeneration]; ok {
		if g, err := strconv.Atoi(s); err == nil {
			return g
		}
	}
	return -1
}

// ---------- State machine actions ----------

// gcStaleSTSes mirrors gcOrphanedResources, which runs after applyEngineState
// in the real controller when phase=Stable.
func (m *engineSim) gcStaleSTSes() {
	keepGens := map[int]bool{m.status.CurrentGeneration: true}
	if m.status.DrainingGeneration != nil {
		keepGens[*m.status.DrainingGeneration] = true
	}
	for gen := range m.stses {
		if !keepGens[gen] {
			delete(m.stses, gen)
		}
	}
}

// checkRequeue enforces Inv_AlwaysRequeues: every computeEngineReconcile call
// must schedule a follow-up reconcile. A result with neither Requeue nor
// RequeueAfter would silently strand the engine in a non-terminal phase.
func checkRequeue(t *rapid.T, result *EngineReconcileResult) {
	if !result.Requeue && result.RequeueAfter == 0 {
		t.Fatalf("Inv_AlwaysRequeues: result has neither Requeue nor RequeueAfter (phase=%s)",
			result.Status.Phase)
	}
}

// Reconcile runs a full reconcile cycle and applies all results including status.
// When the resulting phase is Stable it also runs GC, mirroring the real controller.
func (m *engineSim) Reconcile(t *rapid.T) {
	result := computeEngineReconcile(
		&m.spec, &m.status, m.buildState(),
		propEngineName, propNamespace, 0, testInstanceInfo(),
	)
	checkRequeue(t, &result)
	m.applyResult(&result, true)
	if m.status.Phase == computev1alpha1.PhaseStable {
		m.gcStaleSTSes()
	}
}

// CrashReconcile applies only the resource writes — not the status update.
// Simulates a crash in applyEngineState between the last resource write and
// the updateStatus call.  The following Reconcile exercises crash recovery.
func (m *engineSim) CrashReconcile(t *rapid.T) {
	result := computeEngineReconcile(
		&m.spec, &m.status, m.buildState(),
		propEngineName, propNamespace, 0, testInstanceInfo(),
	)
	checkRequeue(t, &result)
	m.applyResult(&result, false)
}

// ApplySpecChange bumps the engine image tag, triggering spec drift detection.
func (m *engineSim) ApplySpecChange(t *rapid.T) {
	v := rapid.IntRange(1, 99).Draw(t, "imageVersion")
	m.spec.Image = &computev1alpha1.ImageSpec{
		Repository: "firebolt/core",
		Tag:        fmt.Sprintf("v%d.0", v),
	}
}

// ScaleReplicas changes the replica count, also triggering spec drift.
func (m *engineSim) ScaleReplicas(t *rapid.T) {
	m.spec.Replicas = int32(rapid.IntRange(1, 5).Draw(t, "replicas"))
}

// PodsBecomesReady marks the current generation's pods as all Running+Ready.
func (m *engineSim) PodsBecomesReady(_ *rapid.T) {
	m.podsReady = true
}

// DrainCompletes marks the draining generation's pods as fully drained.
func (m *engineSim) DrainCompletes(_ *rapid.T) {
	m.podsDrained = true
}

// DeleteEngine simulates the CR being deleted mid-flight: wipes all tracked
// resources and resets status to initial, mirroring reconcileDelete removing
// all generation-scoped objects before stripping the finalizer.
func (m *engineSim) DeleteEngine(_ *rapid.T) {
	m.stses = make(map[int]*appsv1.StatefulSet)
	m.clusterSvc = nil
	m.podsReady = false
	m.podsDrained = true
	m.status = computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 0,
		ActiveGeneration:  -1,
	}
}

// ---------- Invariant checks (mirrors formal/FireboltEngine.tla Safety) ----------

// Check is called by rapid after every action.
func (m *engineSim) Check(t *rapid.T) {
	s := &m.status

	// Inv_StableConsistency: Phase=Stable => CurrentGeneration == ActiveGeneration
	if s.Phase == computev1alpha1.PhaseStable &&
		s.CurrentGeneration != s.ActiveGeneration {
		t.Fatalf("Inv_StableConsistency: phase=Stable but CurrentGen=%d != ActiveGen=%d",
			s.CurrentGeneration, s.ActiveGeneration)
	}

	// Inv_NoDrainingInStable: Phase=Stable => DrainingGeneration == nil
	if s.Phase == computev1alpha1.PhaseStable && s.DrainingGeneration != nil {
		t.Fatalf("Inv_NoDrainingInStable: phase=Stable but DrainingGen=%d",
			*s.DrainingGeneration)
	}

	// Inv_ActiveHasSTS: ActiveGeneration >= 0 => STS for that gen exists
	if s.ActiveGeneration >= 0 && m.stses[s.ActiveGeneration] == nil {
		t.Fatalf("Inv_ActiveHasSTS: ActiveGen=%d has no STS in cluster",
			s.ActiveGeneration)
	}

	// Inv_ServiceKnownGen + Inv_ServiceValid: once traffic is active, the
	// cluster service selector points to a gen in {activeGen, currentGen}
	// and that gen's STS exists.
	if m.clusterSvc != nil && s.ActiveGeneration >= 0 {
		genStr, ok := m.clusterSvc.Spec.Selector[LabelGeneration]
		if !ok {
			t.Fatalf("cluster service missing %s label", LabelGeneration)
		}
		targetGen, err := strconv.Atoi(genStr)
		if err != nil {
			t.Fatalf("invalid %s label on cluster service: %v", LabelGeneration, err)
		}
		if targetGen != s.ActiveGeneration && targetGen != s.CurrentGeneration {
			t.Fatalf("Inv_ServiceKnownGen: svcTargetGen=%d ∉ {activeGen=%d, currentGen=%d}",
				targetGen, s.ActiveGeneration, s.CurrentGeneration)
		}
		if m.stses[targetGen] == nil {
			t.Fatalf("Inv_ServiceValid: svcTargetGen=%d has no STS in cluster", targetGen)
		}
	}

	// Inv_NoOrphanedSTSes: Phase=Stable => only currentGen STS survives.
	// GC runs as part of Reconcile when phase=Stable, so any stale gens still
	// present after a Reconcile call indicate a GC gap.
	if s.Phase == computev1alpha1.PhaseStable {
		for gen := range m.stses {
			if gen != s.CurrentGeneration {
				t.Fatalf("Inv_NoOrphanedSTSes: phase=Stable but STS gen=%d survives (currentGen=%d)",
					gen, s.CurrentGeneration)
			}
		}
	}
}

func TestEngineStateMachine(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		m := &engineSim{
			spec: *testSpec(),
			status: computev1alpha1.FireboltEngineStatus{
				Phase:             computev1alpha1.PhaseCreating,
				CurrentGeneration: 0,
				ActiveGeneration:  -1,
			},
			stses:       make(map[int]*appsv1.StatefulSet),
			podsDrained: true,
		}
		t.Repeat(rapid.StateMachineActions(m))
	})
}
