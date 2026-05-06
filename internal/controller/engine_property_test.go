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
//
// Phase 8 (informer cache staleness): the simulated cluster carries two
// views — `api` is the source of truth (what the K8s API server stores)
// and `cache` is what the controller's informer cache sees via
// getEngineState. Reconcile reads from cache and writes to api; the cache
// is propagated via the explicit CacheCatchesUp action. Until that action
// fires, the cache lags behind api, which is the production failure mode
// where a controller decides based on stale child-resource state.
// All safety invariants are checked against the api view (truth);
// transient cache/api divergence is the input space, not the bug surface.

import (
	"fmt"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"pgregory.net/rapid"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	propEngineName = "prop-engine"
	propNamespace  = "prop-ns"
)

// clusterView is one consistent snapshot of the child resources owned by an
// engine. The api view is the source of truth (what the K8s API server has);
// the cache view is what the controller's informer cache sees. Production
// cache lag is per-resource; this sim treats it as all-or-nothing (a single
// CacheCatchesUp action copies the full api snapshot to the cache) to keep
// the model simple and the false-positive surface contained.
type clusterView struct {
	stses        map[int]*appsv1.StatefulSet
	configMaps   map[int]*corev1.ConfigMap
	headlessSvcs map[int]*corev1.Service
	clusterSvc   *corev1.Service
}

func newClusterView() clusterView {
	return clusterView{
		stses:        make(map[int]*appsv1.StatefulSet),
		configMaps:   make(map[int]*corev1.ConfigMap),
		headlessSvcs: make(map[int]*corev1.Service),
	}
}

// snapshot returns a shallow copy of v: the maps are duplicated so subsequent
// writes to one view do not bleed into the other, but the value pointers
// (*StatefulSet, *ConfigMap, …) are shared. This is sound because
// applyResultUpTo always replaces values via assignment rather than mutating
// the underlying struct in place.
func (v clusterView) snapshot() clusterView {
	out := clusterView{
		stses:        make(map[int]*appsv1.StatefulSet, len(v.stses)),
		configMaps:   make(map[int]*corev1.ConfigMap, len(v.configMaps)),
		headlessSvcs: make(map[int]*corev1.Service, len(v.headlessSvcs)),
		clusterSvc:   v.clusterSvc,
	}
	for k, val := range v.stses {
		out.stses[k] = val
	}
	for k, val := range v.configMaps {
		out.configMaps[k] = val
	}
	for k, val := range v.headlessSvcs {
		out.headlessSvcs[k] = val
	}
	return out
}

// engineSim is the state machine.  It holds the simulated cluster state and
// drives computeEngineReconcile to verify invariants from the TLA+ spec.
type engineSim struct {
	spec   computev1alpha1.FireboltEngineSpec
	status computev1alpha1.FireboltEngineStatus

	// api is the source of truth: what the K8s API server stores. Reconciler
	// writes go here; safety invariants are checked against this view.
	api clusterView

	// cache is the controller's informer view. Reconcile reads from it via
	// buildState. Lags behind api until CacheCatchesUp fires.
	cache clusterView

	// podsReady reflects whether all pods in currentGen are Running+Ready.
	// Reset to false whenever currentGen is bumped.
	podsReady bool

	// podsDrained reflects whether the draining gen has zero active queries.
	// Reset to false whenever a new drainingGen is established.
	podsDrained bool
}

// buildState constructs the EngineState to pass to computeEngineReconcile
// from the cache view (what getEngineState would observe in production).
// All guard logic lives in assembleEngineState so there is no risk of sim
// drift from the real getEngineState.
func (m *engineSim) buildState() EngineState {
	gen := m.status.CurrentGeneration
	raw := rawEngineResources{
		CurrentSTS:         m.cache.stses[gen],
		CurrentConfigMap:   m.cache.configMaps[gen],
		CurrentHeadlessSvc: m.cache.headlessSvcs[gen],
		CurrentPodsReady:   m.podsReady,
		ClusterService:     m.cache.clusterSvc,
	}

	if m.status.DrainingGeneration != nil {
		dg := *m.status.DrainingGeneration
		raw.DrainingSTS = m.cache.stses[dg]
		raw.DrainingConfigMap = m.cache.configMaps[dg]
		raw.DrainingHeadlessSvc = m.cache.headlessSvcs[dg]
		raw.DrainingPodsDrained = m.podsDrained
		// assembleEngineState handles DrainingSTS==nil → DrainingPodsDrained=true
		// and the drainingGen != currentGen guard.
	}

	state, err := assembleEngineState(&m.status, raw)
	if err != nil {
		panic(fmt.Sprintf("assembleEngineState: %v", err))
	}
	return state
}

// applyEngineState executes side effects in the order:
//
//	1: EnsureConfigMap
//	2: EnsureHeadlessSvc
//	3: EnsureStatefulSet
//	4: EnsureClusterSvc
//	5: DeleteResources (loop)
//	6: status update
//
// applyResultUpTo applies the first k of those steps to the api view (the
// cache is only updated by CacheCatchesUp). k=6 is a successful reconcile;
// k<6 simulates a crash after step k. The 9 MaybeCrash points in
// engine_apply.go are all prefixes of this sequence:
//
//	CrashAfterEngineConfigMapCreated   -> k=1
//	CrashAfterHeadlessServiceCreated   -> k=2
//	CrashAfterStatefulSetCreated       -> k=3
//	CrashAfterClusterServiceEnsured    -> k=4
//	CrashBeforeCreatingToSwitching     -> k=5 (creating reconcile)
//	CrashAfterServiceSelectorUpdate    -> k=4 (switching reconcile)
//	CrashBeforeSwitchingStatusUpdate   -> k=5 (switching reconcile)
//	CrashAfterStatefulSetDeleted       -> k=5 (cleaning reconcile)
//	CrashBeforeCleaningToTerminal      -> k=5 (cleaning reconcile)
//
// The TLA+ spec models reconciles atomically; every partial-write state below
// is already a reachable model state, so no spec extension is required for
// safety. CrashAtPrefix exercises recovery from each prefix; all the existing
// invariants (terminal consistency, service-known-gen, no-orphans, etc.) must
// hold after the next Reconcile.
func (m *engineSim) applyResultUpTo(result *EngineReconcileResult, k int) {
	if k >= 1 && result.EnsureConfigMap != nil {
		gen := labelGen(result.EnsureConfigMap.Labels)
		if gen >= 0 {
			m.api.configMaps[gen] = result.EnsureConfigMap
		}
	}
	if k >= 2 && result.EnsureHeadlessSvc != nil {
		gen := labelGen(result.EnsureHeadlessSvc.Labels)
		if gen >= 0 {
			m.api.headlessSvcs[gen] = result.EnsureHeadlessSvc
		}
	}
	if k >= 3 && result.EnsureStatefulSet != nil {
		gen := labelGen(result.EnsureStatefulSet.Labels)
		m.api.stses[gen] = result.EnsureStatefulSet
	}
	if k >= 4 && result.EnsureClusterSvc != nil {
		m.api.clusterSvc = result.EnsureClusterSvc
	}
	if k >= 5 {
		for _, obj := range result.DeleteResources {
			gen := labelGen(obj.GetLabels())
			if gen < 0 {
				continue
			}
			switch obj.(type) {
			case *appsv1.StatefulSet:
				delete(m.api.stses, gen)
			case *corev1.ConfigMap:
				delete(m.api.configMaps, gen)
			case *corev1.Service:
				delete(m.api.headlessSvcs, gen)
			}
		}
	}
	if k >= 6 {
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
}

// applyResult is the legacy two-mode entry point: the historical
// applyStatus=true is k=6 (full success); applyStatus=false is k=5 (status
// dropped; the original CrashReconcile model).
func (m *engineSim) applyResult(result *EngineReconcileResult, applyStatus bool) {
	k := 5
	if applyStatus {
		k = 6
	}
	m.applyResultUpTo(result, k)
}

// labelGen extracts the generation number from an object's labels.
func labelGen(labels map[string]string) int {
	if s, ok := labels[LabelGeneration]; ok {
		if g, err := strconv.Atoi(s); err == nil {
			return g
		}
	}
	return -1
}

// isTerminalPhase mirrors terminalPhase() in engine_reconcile.go: both
// PhaseStable and PhaseStopped are terminal.
func isTerminalPhase(phase computev1alpha1.EnginePhase) bool {
	return phase == computev1alpha1.PhaseStable || phase == computev1alpha1.PhaseStopped
}

// ---------- State machine actions ----------

// gcStaleResources mirrors gcOrphanedResources, which runs after applyEngineState
// in the real controller when phase is any terminal phase (Stable or Stopped).
// GC is an api-side delete; cache observation comes through the next
// CacheCatchesUp.
func (m *engineSim) gcStaleResources() {
	keepGens := map[int]bool{m.status.CurrentGeneration: true}
	if m.status.DrainingGeneration != nil {
		keepGens[*m.status.DrainingGeneration] = true
	}
	for gen := range m.api.stses {
		if !keepGens[gen] {
			delete(m.api.stses, gen)
		}
	}
	for gen := range m.api.configMaps {
		if !keepGens[gen] {
			delete(m.api.configMaps, gen)
		}
	}
	for gen := range m.api.headlessSvcs {
		if !keepGens[gen] {
			delete(m.api.headlessSvcs, gen)
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
// When the resulting phase is terminal it also runs GC, mirroring the real controller.
func (m *engineSim) Reconcile(t *rapid.T) {
	result := computeEngineReconcile(
		&m.spec, &m.status, m.buildState(),
		propEngineName, propNamespace, 0, testInstanceInfo(),
	)
	checkRequeue(t, &result)
	m.applyResult(&result, true)
	if isTerminalPhase(m.status.Phase) {
		m.gcStaleResources()
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

// CrashAtPrefix simulates a crash after the k-th side effect of
// applyEngineState, drawn uniformly from [1, 4]. CrashReconcile (k=5) is the
// final-prefix special case kept as a separate action for shrinking clarity;
// CrashAtPrefix exercises the four earlier prefixes (k=1..4) that correspond
// to the CrashAfter*Created / CrashAfter*Ensured points in crash_points.go.
// Recovery is exercised on the next Reconcile / CrashReconcile / CrashAtPrefix
// the rapid sequence draws; all invariants must hold after every recovery.
func (m *engineSim) CrashAtPrefix(t *rapid.T) {
	result := computeEngineReconcile(
		&m.spec, &m.status, m.buildState(),
		propEngineName, propNamespace, 0, testInstanceInfo(),
	)
	checkRequeue(t, &result)
	k := rapid.IntRange(1, 4).Draw(t, "crashPrefix")
	m.applyResultUpTo(&result, k)
}

// CacheCatchesUp models the informer cache observing the latest api state.
// In production the cache is updated continuously via watch events; this
// action is the explicit, atomic counterpart used by the harness. Until it
// fires, the cache view stays at whatever snapshot it carried after the
// previous CacheCatchesUp (or the test's initial state). No fairness
// guarantee is needed because rapid's uniform action distribution means the
// cache catches up within a bounded number of steps.
func (m *engineSim) CacheCatchesUp(_ *rapid.T) {
	m.cache = m.api.snapshot()
}

// ApplySpecChange bumps the engine image tag, triggering spec drift detection.
func (m *engineSim) ApplySpecChange(t *rapid.T) {
	v := rapid.IntRange(1, 99).Draw(t, "imageVersion")
	m.spec.Image = &computev1alpha1.ImageSpec{
		Repository: "firebolt/engine",
		Tag:        fmt.Sprintf("v%d.0", v),
	}
}

// ScaleReplicas changes the replica count, also triggering spec drift.
// Range includes 0 so that PhaseStopped is reachable.
func (m *engineSim) ScaleReplicas(t *rapid.T) {
	m.spec.Replicas = int32(rapid.IntRange(0, 5).Draw(t, "replicas"))
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
// resources from both api and cache views and resets status to initial,
// mirroring reconcileDelete removing all generation-scoped objects before
// stripping the finalizer.
func (m *engineSim) DeleteEngine(_ *rapid.T) {
	m.api = newClusterView()
	m.cache = newClusterView()
	m.podsReady = false
	m.podsDrained = true
	m.status = computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 0,
		ActiveGeneration:  -1,
	}
}

// ---------- Invariant checks (mirrors formal/FireboltEngine.tla Safety) ----------

// Check is called by rapid after every action. All resource invariants run
// against m.api (the api truth). The cache is only the controller's input;
// transient cache/api divergence is the input space, not the bug surface.
func (m *engineSim) Check(t *rapid.T) {
	s := &m.status

	// Inv_TerminalConsistency: terminal phase => CurrentGeneration == ActiveGeneration
	if isTerminalPhase(s.Phase) && s.CurrentGeneration != s.ActiveGeneration {
		t.Fatalf("Inv_TerminalConsistency: phase=%s but CurrentGen=%d != ActiveGen=%d",
			s.Phase, s.CurrentGeneration, s.ActiveGeneration)
	}

	// Inv_TerminalNoDraining: terminal phase => DrainingGeneration == nil
	if isTerminalPhase(s.Phase) && s.DrainingGeneration != nil {
		t.Fatalf("Inv_TerminalNoDraining: phase=%s but DrainingGen=%d",
			s.Phase, *s.DrainingGeneration)
	}

	// Inv_ActiveHasSTS: ActiveGeneration >= 0 => STS for that gen exists
	if s.ActiveGeneration >= 0 && m.api.stses[s.ActiveGeneration] == nil {
		t.Fatalf("Inv_ActiveHasSTS: ActiveGen=%d has no STS in cluster",
			s.ActiveGeneration)
	}

	// Inv_ServiceKnownGen + Inv_ServiceValid: once traffic is active, the
	// cluster service selector points to a gen in {activeGen, currentGen}
	// and that gen's STS exists.
	if m.api.clusterSvc != nil && s.ActiveGeneration >= 0 {
		genStr, ok := m.api.clusterSvc.Spec.Selector[LabelGeneration]
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
		if m.api.stses[targetGen] == nil {
			t.Fatalf("Inv_ServiceValid: svcTargetGen=%d has no STS in cluster", targetGen)
		}
	}

	// Inv_NoOrphanedResources: terminal phase => only currentGen resources survive.
	// GC runs as part of Reconcile when phase is terminal, so any stale gens still
	// present after a Reconcile call indicate a GC gap.
	if isTerminalPhase(s.Phase) {
		for gen := range m.api.stses {
			if gen != s.CurrentGeneration {
				t.Fatalf("Inv_NoOrphanedResources: phase=%s but STS gen=%d survives (currentGen=%d)",
					s.Phase, gen, s.CurrentGeneration)
			}
		}
		for gen := range m.api.configMaps {
			if gen != s.CurrentGeneration {
				t.Fatalf("Inv_NoOrphanedResources: phase=%s but ConfigMap gen=%d survives (currentGen=%d)",
					s.Phase, gen, s.CurrentGeneration)
			}
		}
		for gen := range m.api.headlessSvcs {
			if gen != s.CurrentGeneration {
				t.Fatalf("Inv_NoOrphanedResources: phase=%s but HeadlessSvc gen=%d survives (currentGen=%d)",
					s.Phase, gen, s.CurrentGeneration)
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
			api:         newClusterView(),
			cache:       newClusterView(),
			podsDrained: true,
		}
		t.Repeat(rapid.StateMachineActions(m))
	})
}
