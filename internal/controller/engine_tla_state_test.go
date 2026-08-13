/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Phase 3 of the formal-verification plan:
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
// (specVer, specWantsStop). specVer is encoded into ServiceAccountName so
// stsMatchesSpec correctly tracks per-generation drift — the same convention
// used by ApplySpecChange in the rapid property test. (The image tag carried
// this role until FireboltEngineSpec.Image moved into FireboltEngineClass.)
func tlaSpecForState(s tlaState) computev1alpha1.FireboltEngineSpec {
	replicas := int32(3)
	if s.SpecWantsStop {
		replicas = 0
	}
	sa := fmt.Sprintf("sa-v%d", s.SpecVer)
	return computev1alpha1.FireboltEngineSpec{
		InstanceRef: "test-instance",
		Replicas:    replicas,
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				ServiceAccountName: sa,
				Containers: []corev1.Container{{
					Name: computev1alpha1.EngineContainerName,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("8Gi"),
						},
					},
				}},
			},
		},
		Rollout: computev1alpha1.RolloutGraceful,
	}
}

// tlaMakeSTS builds a StatefulSet stamped with the given stsSpecVer. The base
// is constructed with the same buildStatefulSet the real reconciler uses, so
// every field stsMatchesSpec inspects (ServiceAccountName, security contexts,
// annotations, VolumeClaimTemplates, …) is consistent with the spec. The
// pod-template ServiceAccountName is then overridden so the TLA+ relation
// `StsMatchesSpec(g) ⟺ stsSpecVer[g] = specVer` matches Go's stsMatchesSpec.
// (Previously this used the container image; the image moved out of
// FireboltEngineSpec into FireboltEngineClass, so SA is the carrier now.)
func tlaMakeSTS(spec *computev1alpha1.FireboltEngineSpec, gen, stsSpecVer int) *appsv1.StatefulSet {
	sts := buildStatefulSet(spec, propEngineName, propNamespace, gen, InstanceInfo{}, nil)
	sts.Spec.Template.Spec.ServiceAccountName = fmt.Sprintf("sa-v%d", stsSpecVer)
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
// are skipped at test time (see tlaShouldGateOut). Both api and cache views
// are initialized identically: state cover only runs one Reconcile per state,
// so cache lag is not modeled here (the rapid harness in
// engine_property_test.go is where lag is exercised via CacheCatchesUp).
func materializeTLAState(s tlaState) *engineSim {
	spec := tlaSpecForState(s)
	m := &engineSim{
		spec: spec,
		status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.EnginePhase(s.Phase),
			CurrentGeneration: s.CurrentGen,
			ActiveGeneration:  s.ActiveGen,
		},
		api:         newClusterView(),
		cache:       newClusterView(),
		podsReady:   s.PodsReady,
		podsDrained: s.PodsDrained,
	}
	if s.DrainingGen >= 0 {
		dg := s.DrainingGen
		m.status.DrainingGeneration = &dg
	}
	for g, sv := range s.StsSpecVer {
		if sv < 0 {
			continue
		}
		sts := tlaMakeSTS(&spec, g, sv)
		// ConfigMap and headless Service are co-resources of the STS — populate
		// stub objects so assembleEngineState sees a consistent per-gen picture.
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      genResourceName(propEngineName, g, SuffixConfig),
				Namespace: propNamespace,
				Labels: map[string]string{
					LabelEngine:     propEngineName,
					LabelGeneration: strconv.Itoa(g),
				},
			},
		}
		hl := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      genResourceName(propEngineName, g, SuffixHL),
				Namespace: propNamespace,
				Labels: map[string]string{
					LabelEngine:     propEngineName,
					LabelGeneration: strconv.Itoa(g),
				},
			},
		}
		m.api.stses[g] = sts
		m.api.configMaps[g] = cm
		m.api.headlessSvcs[g] = hl
		m.cache.stses[g] = sts
		m.cache.configMaps[g] = cm
		m.cache.headlessSvcs[g] = hl
	}
	if s.SvcTargetGen >= 0 {
		svc := tlaMakeClusterSvc(s.SvcTargetGen)
		m.api.clusterSvc = svc
		m.cache.clusterSvc = svc
	}
	return m
}

// projectEngineSim extracts the TLA+ observable variables from the simulated
// cluster state. instanceReady and classReady are preserved from the input state
// because the compute layer cannot change either — both gates are enforced by
// the outer Reconcile.
//
// Every field of tlaState must be populated here. The round-trip guard in
// TestTLAEngineStateCover (materialize → project == start) is what enforces
// that: a field left at its zero value silently narrows what
// tlaClosureContains compares, which weakens every assertion in the suite.
func projectEngineSim(m *engineSim, instanceReady, classReady bool) tlaState {
	st := tlaState{
		Phase:         string(m.status.Phase),
		CurrentGen:    m.status.CurrentGeneration,
		ActiveGen:     m.status.ActiveGeneration,
		DrainingGen:   -1,
		SpecVer:       parseSpecTemplateSAVer(m.spec),
		SpecWantsStop: m.spec.Replicas == 0,
		SvcTargetGen:  -1,
		PodsReady:     m.podsReady,
		PodsDrained:   m.podsDrained,
		InstanceReady: instanceReady,
		ClassReady:    classReady,
	}
	for g := range st.StsSpecVer {
		st.StsSpecVer[g] = -1
	}
	if m.status.DrainingGeneration != nil {
		st.DrainingGen = *m.status.DrainingGeneration
	}
	for g, sts := range m.api.stses {
		if g < 0 || g >= len(st.StsSpecVer) {
			continue
		}
		if len(sts.Spec.Template.Spec.Containers) == 0 {
			continue
		}
		st.StsSpecVer[g] = parseSANameVer(sts.Spec.Template.Spec.ServiceAccountName)
	}
	if m.api.clusterSvc != nil {
		if v, ok := m.api.clusterSvc.Spec.Selector[LabelGeneration]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				st.SvcTargetGen = n
			}
		}
	}
	return st
}

// parseSpecTemplateSAVer extracts the integer N from a "sa-v<N>"
// ServiceAccountName found on spec.template.spec, used by the TLA+
// harness to encode specVer. Returns -1 if the template is nil or the
// SA does not parse — every test state uses the canonical form so this
// is a defensive guard, not a behavior.
func parseSpecTemplateSAVer(spec computev1alpha1.FireboltEngineSpec) int {
	if spec.Template == nil {
		return -1
	}
	return parseSANameVer(spec.Template.Spec.ServiceAccountName)
}

// parseSANameVer is the StatefulSet-side counterpart of parseSAVer: the
// pod template's ServiceAccountName is a plain string, not a pointer.
func parseSANameVer(sa string) int {
	if sa == "" {
		return -1
	}
	return parseSAToken(sa)
}

// parseSAToken parses "sa-v<N>" and returns N (-1 on malformed input).
func parseSAToken(s string) int {
	const prefix = "sa-v"
	if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
		return -1
	}
	n, err := strconv.Atoi(s[len(prefix):])
	if err != nil {
		return -1
	}
	return n
}

// tlaShouldGateOut returns true when one of the outer Reconcile method's
// gates (instance-Ready or class-Ready) would prevent
// computeEngineReconcile from running at all. Both gates engage when
// the corresponding flag is false and phase is in {stable, stopped,
// creating}; the other phases (switching, draining, cleaning) bypass
// the gates deliberately because they do not re-resolve the instance
// or the class. State cover for the compute layer skips these states
// because the compute layer runs only when both gates are open.
func tlaShouldGateOut(s tlaState) bool {
	if s.InstanceReady && s.ClassReady {
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

// tlaInvariants runs the shared invariant registry (engine_invariants_test.go),
// the same set the rapid harness checks after every action. Keyed by the spec's
// Safety conjunct names, so a conjunct added to formal/FireboltEngine.tla fails
// TestEngineInvariantsMatchSpec until it is implemented here too.
func tlaInvariants(t *testing.T, m *engineSim) {
	t.Helper()
	checkEngineInvariants(t, m)
}

// Coverage pins. These are hand-maintained on purpose: they cannot live in the
// generated fixture, because the fixture is regenerated from the spec and
// therefore always agrees with itself. A spec change that collapses the state
// space, or a widened skip predicate that swallows states the compute layer
// used to be checked against, leaves `make formal-verify` green — the fixture
// still matches the generator's output, there are simply far fewer cases in it,
// and the test only logged the counts.
//
// Pinning them in hand-written code forces any movement in coverage to appear
// as an edit in the diff. If one of these assertions fails, the question to
// answer in the commit message is *why* coverage moved — then update the number.
const (
	tlaExpectedCases           = 6404
	tlaExpectedRan             = 3512
	tlaExpectedSkippedGate     = 2856
	tlaExpectedSkippedBoundary = 36
)

func TestTLAEngineStateCover(t *testing.T) {
	if len(tlaEngineStateCases) != tlaExpectedCases {
		t.Fatalf("fixture has %d cases, expected %d: the state space moved. Regenerate with `make formal-gen`, then update tlaExpectedCases and say why in the commit",
			len(tlaEngineStateCases), tlaExpectedCases)
	}
	skippedGate := 0
	skippedBoundary := 0
	for i := range tlaEngineStateCases {
		tc := tlaEngineStateCases[i]
		start := tlaStatePool[tc.Start]
		if tlaShouldGateOut(start) {
			skippedGate++
			continue
		}
		if tlaModelBoundary(start) {
			skippedBoundary++
			continue
		}
		name := fmt.Sprintf("case-%04d/%s/g%d/a%d/d%d/s%d",
			i, start.Phase, start.CurrentGen, start.ActiveGen,
			start.DrainingGen, start.SpecVer)
		t.Run(name, func(t *testing.T) {
			m := materializeTLAState(start)

			// Guard the fixture itself: if materialization does not reproduce the
			// starting state, every closure assertion below is meaningless. It
			// also forces projectEngineSim to populate every tlaState field — a
			// dropped field fails the round-trip for any state whose value
			// differs from that field's zero value. Every state-cover harness
			// carries this guard, through the same shared comparison.
			if got := projectEngineSim(m, start.InstanceReady, start.ClassReady); !tlaProjectionEqual(got, start) {
				t.Fatalf("materialization does not round-trip\n  want: %+v\n  got:  %+v", start, got)
			}

			result := computeEngineReconcile(
				&m.spec, &m.status, m.buildState(),
				propEngineName, propNamespace, 0, testInstanceInfo(), nil,
			)
			if !result.Requeue && result.RequeueAfter == 0 {
				t.Fatalf("Inv_AlwaysRequeues: result has neither Requeue nor RequeueAfter (phase=%s)",
					result.Status.Phase)
			}
			m.applyResult(&result, true)
			m.gcStaleResources()
			tlaInvariants(t, m)

			actual := projectEngineSim(m, start.InstanceReady, start.ClassReady)
			if !tlaClosureContains(tlaStatePool, tc.Closure, actual) {
				t.Fatalf("result not in TLA+ reconciler closure of starting state\n  start:    %+v\n  actual:   %+v\n  closure (%d states):\n%s",
					start, actual, len(tc.Closure), tlaFormatClosure(tlaStatePool, tc.Closure))
			}
		})
	}
	ran := len(tlaEngineStateCases) - skippedGate - skippedBoundary
	t.Logf("state cover: ran %d / %d, skipped %d gated (instanceReady=false OR classReady=false in {stable,stopped,creating}), %d at MaxGen boundary",
		ran, len(tlaEngineStateCases), skippedGate, skippedBoundary)

	// The skip predicates are the other way coverage can quietly vanish: widen
	// tlaShouldGateOut or tlaModelBoundary and states stop being exercised with
	// no fixture change at all, so `make formal-verify` cannot see it.
	if skippedGate != tlaExpectedSkippedGate {
		t.Errorf("tlaShouldGateOut skipped %d cases, expected %d: the gate predicate changed. Justify the new coverage and update tlaExpectedSkippedGate",
			skippedGate, tlaExpectedSkippedGate)
	}
	if skippedBoundary != tlaExpectedSkippedBoundary {
		t.Errorf("tlaModelBoundary skipped %d cases, expected %d: the boundary predicate changed. Justify the new coverage and update tlaExpectedSkippedBoundary",
			skippedBoundary, tlaExpectedSkippedBoundary)
	}
	if ran != tlaExpectedRan {
		t.Errorf("ran %d cases against computeEngineReconcile, expected %d", ran, tlaExpectedRan)
	}
}
