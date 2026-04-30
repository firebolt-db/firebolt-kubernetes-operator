/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Phase 3 of the formal-verification plan (docs/formal-verification.md):
// deterministic exhaustive state-cover testing for computeEngineReconcile.
//
// For every reachable state in the TLC state graph (regenerated via
// `make formal-gen`), this test materializes an engineSim matching the
// state, calls computeEngineReconcile, and verifies that the resulting
// state lies in the model's reconciler closure of the starting state —
// i.e. is a state TLC says is reachable from the start by zero or more
// consecutive reconciler-only transitions.
//
// Phase 2 (engine_property_test.go) drives the same compute layer with
// random sequences. Phase 3 is its deterministic, exhaustive complement:
// random walks miss states they didn't happen to visit; state cover hits
// every reachable input by construction.

import (
	"fmt"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// tlaSpecForState builds a FireboltEngineSpec consistent with the TLA+ state's
// (specVer, specWantsStop). specVer is encoded into the image tag so
// stsMatchesSpec correctly tracks per-generation drift — the same convention
// used by ApplySpecChange in the rapid property test.
func tlaSpecForState(s tlaState) computev1alpha1.FireboltEngineSpec {
	replicas := int32(3)
	if s.SpecWantsStop {
		replicas = 0
	}
	return computev1alpha1.FireboltEngineSpec{
		InstanceRef: "test-instance",
		Replicas:    replicas,
		Image: &computev1alpha1.ImageSpec{
			Repository: "firebolt/engine",
			Tag:        fmt.Sprintf("v%d.0", s.SpecVer),
		},
		Resources: computev1alpha1.ResourceRequirements{
			CPU:    resource.MustParse("2"),
			Memory: resource.MustParse("8Gi"),
		},
		Rollout: computev1alpha1.RolloutGraceful,
	}
}

// tlaMakeSTS builds a StatefulSet stamped with the given stsSpecVer. The base
// is constructed with the same buildStatefulSet the real reconciler uses, so
// every field stsMatchesSpec inspects (ServiceAccountName, security contexts,
// annotations, VolumeClaimTemplates, …) is consistent with the spec. The
// container image is then overridden so the TLA+ relation
// `StsMatchesSpec(g) ⟺ stsSpecVer[g] = specVer` matches Go's stsMatchesSpec.
func tlaMakeSTS(spec *computev1alpha1.FireboltEngineSpec, gen, stsSpecVer int) *appsv1.StatefulSet {
	sts := buildStatefulSet(spec, propEngineName, propNamespace, gen)
	overrideImage := fmt.Sprintf("%s:v%d.0", spec.Image.Repository, stsSpecVer)
	for i := range sts.Spec.Template.Spec.Containers {
		sts.Spec.Template.Spec.Containers[i].Image = overrideImage
	}
	return sts
}

func tlaMakeClusterSvc(gen int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      propEngineName + SuffixService,
			Namespace: propNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				LabelEngine:     propEngineName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
	}
}

// materializeTLAState constructs an engineSim whose simulated cluster state
// corresponds to the given TLA+ state. instanceReady is intentionally not
// plumbed — the real instance gate lives in the outer Reconcile method, not
// in the compute layer this test exercises; states gated by instanceReady=FALSE
// are skipped at test time (see tlaShouldGateOut).
func materializeTLAState(s tlaState) *engineSim {
	spec := tlaSpecForState(s)
	m := &engineSim{
		spec: spec,
		status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.EnginePhase(s.Phase),
			CurrentGeneration: s.CurrentGen,
			ActiveGeneration:  s.ActiveGen,
		},
		stses:        make(map[int]*appsv1.StatefulSet),
		configMaps:   make(map[int]*corev1.ConfigMap),
		headlessSvcs: make(map[int]*corev1.Service),
		podsReady:    s.PodsReady,
		podsDrained:  s.PodsDrained,
	}
	if s.DrainingGen >= 0 {
		dg := s.DrainingGen
		m.status.DrainingGeneration = &dg
	}
	for g, sv := range s.StsSpecVer {
		if sv < 0 {
			continue
		}
		m.stses[g] = tlaMakeSTS(&spec, g, sv)
		// ConfigMap and headless Service are co-resources of the STS — populate
		// stub objects so assembleEngineState sees a consistent per-gen picture.
		m.configMaps[g] = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      genResourceName(propEngineName, g, SuffixConfig),
				Namespace: propNamespace,
				Labels: map[string]string{
					LabelEngine:     propEngineName,
					LabelGeneration: strconv.Itoa(g),
				},
			},
		}
		m.headlessSvcs[g] = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      genResourceName(propEngineName, g, SuffixHL),
				Namespace: propNamespace,
				Labels: map[string]string{
					LabelEngine:     propEngineName,
					LabelGeneration: strconv.Itoa(g),
				},
			},
		}
	}
	if s.SvcTargetGen >= 0 {
		m.clusterSvc = tlaMakeClusterSvc(s.SvcTargetGen)
	}
	return m
}

// projectEngineSim extracts the TLA+ observable variables from the simulated
// cluster state. instanceReady is preserved from the input state because the
// compute layer cannot change it — the gate is enforced by the outer Reconcile.
func projectEngineSim(m *engineSim, instanceReady bool) tlaState {
	st := tlaState{
		Phase:         string(m.status.Phase),
		CurrentGen:    m.status.CurrentGeneration,
		ActiveGen:     m.status.ActiveGeneration,
		DrainingGen:   -1,
		SpecVer:       parseImageTagVer(m.spec.Image),
		SpecWantsStop: m.spec.Replicas == 0,
		SvcTargetGen:  -1,
		PodsReady:     m.podsReady,
		PodsDrained:   m.podsDrained,
		InstanceReady: instanceReady,
	}
	for g := range st.StsSpecVer {
		st.StsSpecVer[g] = -1
	}
	if m.status.DrainingGeneration != nil {
		st.DrainingGen = *m.status.DrainingGeneration
	}
	for g, sts := range m.stses {
		if g < 0 || g >= len(st.StsSpecVer) {
			continue
		}
		if len(sts.Spec.Template.Spec.Containers) == 0 {
			continue
		}
		st.StsSpecVer[g] = parseImageVer(sts.Spec.Template.Spec.Containers[0].Image)
	}
	if m.clusterSvc != nil {
		if v, ok := m.clusterSvc.Spec.Selector[LabelGeneration]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				st.SvcTargetGen = n
			}
		}
	}
	return st
}

// parseImageTagVer extracts the integer N from an ImageSpec with tag "vN.0".
// Returns -1 if the tag does not parse — in practice every test state uses
// the canonical "v<N>.0" form so this is a defensive guard, not a behavior.
func parseImageTagVer(img *computev1alpha1.ImageSpec) int {
	if img == nil {
		return -1
	}
	return parseVTag(img.Tag)
}

// parseImageVer extracts N from a container image string "<repo>:vN.0".
func parseImageVer(image string) int {
	for i := len(image) - 1; i >= 0; i-- {
		if image[i] == ':' {
			return parseVTag(image[i+1:])
		}
	}
	return parseVTag(image)
}

func parseVTag(tag string) int {
	if len(tag) < 3 || tag[0] != 'v' {
		return -1
	}
	for i := 1; i < len(tag); i++ {
		if tag[i] == '.' {
			n, err := strconv.Atoi(tag[1:i])
			if err != nil {
				return -1
			}
			return n
		}
	}
	return -1
}

// tlaShouldGateOut returns true when the outer Reconcile method's instance gate
// would prevent computeEngineReconcile from running at all. The gate engages
// when instanceReady is false and phase is in {stable, stopped, creating}; the
// other phases (switching, draining, cleaning) bypass it deliberately. State
// cover for the compute layer skips these states because the compute layer
// runs only when the gate is open.
func tlaShouldGateOut(s tlaState) bool {
	if s.InstanceReady {
		return false
	}
	switch s.Phase {
	case "stable", "stopped", "creating":
		return true
	default:
		return false
	}
}

// tlaModelBoundary skips states where the TLA+ MaxGen ceiling forces the
// model to handle drift differently than the implementation would.
//
// At the boundary the model has two devices:
//   - In terminal phases (`stable`, `stopped`), `ReconcileTerminal_Drift`
//     requires `currentGen < MaxGen`, so it does not fire — the model stutters.
//   - In `creating`, `ReconcileCreating_SpecDrift_AtMax` deletes the STS in
//     place and keeps `currentGen=MaxGen`.
//
// In both cases the real Go code instead bumps `currentGen` to MaxGen+1,
// landing in a state the model never represents. These states are model
// bounding artifacts, not real divergence; skipping them keeps state cover
// honest within the model's scope. Spec drift at currentGen<MaxGen still
// exercises the bump-and-delete path against the model.
func tlaModelBoundary(s tlaState) bool {
	if s.CurrentGen < tlaMaxGen {
		return false
	}
	stsVer := -1
	if s.CurrentGen >= 0 && s.CurrentGen < len(s.StsSpecVer) {
		stsVer = s.StsSpecVer[s.CurrentGen]
	}
	// Boundary divergence only happens when an STS at currentGen exists AND its
	// spec version differs from the current spec — i.e. spec drift at the
	// ceiling. STS absent (EnsureSTS creates one) and STS matches (no drift)
	// both behave identically in model and implementation.
	if stsVer == -1 || stsVer == s.SpecVer {
		return false
	}
	switch s.Phase {
	case "stable", "stopped", "creating":
		return true
	default:
		return false
	}
}

// tlaInvariants verifies the same Safety predicates checked in the rapid
// property test (engine_property_test.go's engineSim.Check) plus the TLA+
// safety invariants that depend only on observable simulated state.
func tlaInvariants(t *testing.T, m *engineSim) {
	t.Helper()
	s := &m.status

	if isTerminalPhase(s.Phase) && s.CurrentGeneration != s.ActiveGeneration {
		t.Fatalf("Inv_TerminalConsistency: phase=%s but CurrentGen=%d != ActiveGen=%d",
			s.Phase, s.CurrentGeneration, s.ActiveGeneration)
	}
	if isTerminalPhase(s.Phase) && s.DrainingGeneration != nil {
		t.Fatalf("Inv_TerminalNoDraining: phase=%s but DrainingGen=%d",
			s.Phase, *s.DrainingGeneration)
	}
	if s.ActiveGeneration >= 0 && m.stses[s.ActiveGeneration] == nil {
		t.Fatalf("Inv_ActiveHasSTS: ActiveGen=%d has no STS", s.ActiveGeneration)
	}
	if s.DrainingGeneration != nil && s.Phase != computev1alpha1.PhaseDraining && s.Phase != computev1alpha1.PhaseCleaning {
		t.Fatalf("Inv_DrainingPhase: DrainingGen=%d but phase=%s",
			*s.DrainingGeneration, s.Phase)
	}
	if s.DrainingGeneration != nil && *s.DrainingGeneration >= s.CurrentGeneration {
		t.Fatalf("Inv_DrainingOlderThanCurrent: DrainingGen=%d, CurrentGen=%d",
			*s.DrainingGeneration, s.CurrentGeneration)
	}
	if s.ActiveGeneration > s.CurrentGeneration {
		t.Fatalf("Inv_GenOrder: ActiveGen=%d > CurrentGen=%d",
			s.ActiveGeneration, s.CurrentGeneration)
	}
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
			t.Fatalf("Inv_ServiceKnownGen: svcTargetGen=%d not in {activeGen=%d, currentGen=%d}",
				targetGen, s.ActiveGeneration, s.CurrentGeneration)
		}
		if m.stses[targetGen] == nil {
			t.Fatalf("Inv_ServiceValid: svcTargetGen=%d has no STS", targetGen)
		}
	}
}

// closureContains reports whether `actual` is one of the TLA+ states the model
// considers reachable from the test's starting state via 0+ consecutive
// reconciler-only transitions. A real Reconcile call may perform several model
// sub-steps in one shot (the spec models reconciles atomically per sub-action;
// the implementation batches), so the resulting state is checked for closure
// membership rather than equality with any single specific successor.
func closureContains(closure []tlaState, actual tlaState) bool {
	for i := range closure {
		if tlaStateEqual(closure[i], actual) {
			return true
		}
	}
	return false
}

func tlaStateEqual(a, b tlaState) bool {
	if a.Phase != b.Phase ||
		a.CurrentGen != b.CurrentGen ||
		a.ActiveGen != b.ActiveGen ||
		a.DrainingGen != b.DrainingGen ||
		a.SpecVer != b.SpecVer ||
		a.SpecWantsStop != b.SpecWantsStop ||
		a.SvcTargetGen != b.SvcTargetGen ||
		a.PodsReady != b.PodsReady ||
		a.PodsDrained != b.PodsDrained ||
		a.InstanceReady != b.InstanceReady {
		return false
	}
	for i := range a.StsSpecVer {
		if a.StsSpecVer[i] != b.StsSpecVer[i] {
			return false
		}
	}
	return true
}

func TestTLAEngineStateCover(t *testing.T) {
	skippedGate := 0
	skippedBoundary := 0
	for i := range tlaEngineStateCases {
		tc := tlaEngineStateCases[i]
		if tlaShouldGateOut(tc.Start) {
			skippedGate++
			continue
		}
		if tlaModelBoundary(tc.Start) {
			skippedBoundary++
			continue
		}
		name := fmt.Sprintf("case-%04d/%s/g%d/a%d/d%d/s%d",
			i, tc.Start.Phase, tc.Start.CurrentGen, tc.Start.ActiveGen,
			tc.Start.DrainingGen, tc.Start.SpecVer)
		t.Run(name, func(t *testing.T) {
			m := materializeTLAState(tc.Start)
			result := computeEngineReconcile(
				&m.spec, &m.status, m.buildState(),
				propEngineName, propNamespace, 0, testInstanceInfo(),
			)
			if !result.Requeue && result.RequeueAfter == 0 {
				t.Fatalf("Inv_AlwaysRequeues: result has neither Requeue nor RequeueAfter (phase=%s)",
					result.Status.Phase)
			}
			m.applyResult(&result, true)
			if isTerminalPhase(m.status.Phase) {
				m.gcStaleResources()
			}
			tlaInvariants(t, m)

			actual := projectEngineSim(m, tc.Start.InstanceReady)
			if !closureContains(tc.Closure, actual) {
				t.Fatalf("result not in TLA+ reconciler closure of starting state\n  start:    %+v\n  actual:   %+v\n  closure (%d states):\n%s",
					tc.Start, actual, len(tc.Closure), formatClosure(tc.Closure))
			}
		})
	}
	t.Logf("state cover: ran %d / %d, skipped %d gated (instanceReady=false in {stable,stopped,creating}), %d at MaxGen boundary",
		len(tlaEngineStateCases)-skippedGate-skippedBoundary, len(tlaEngineStateCases),
		skippedGate, skippedBoundary)
}

func formatClosure(closure []tlaState) string {
	const limit = 8
	out := ""
	for i, s := range closure {
		if i >= limit {
			out += fmt.Sprintf("    ... (%d more)\n", len(closure)-limit)
			break
		}
		out += fmt.Sprintf("    [%d] %+v\n", i, s)
	}
	return out
}
