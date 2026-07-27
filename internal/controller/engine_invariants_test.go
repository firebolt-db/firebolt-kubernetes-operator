/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// One definition of the engine safety invariants, shared by both harnesses that
// check them: the rapid property test (engine_property_test.go, random action
// sequences) and the TLA+ state cover (engine_tla_state_test.go, every reachable
// modeled state).
//
// They used to be two hand-written copies of the spec's `Safety` conjuncts, and
// the copies had drifted: Inv_TerminalHasSTS and Inv_QuiescedPhaseMatchesSpec
// were in the spec only, Inv_NoOrphanedResources in the rapid harness only, and
// Inv_DrainingPhase / Inv_DrainingOlderThanCurrent / Inv_GenOrder in the state
// cover only. Nothing detected any of it, because nothing related the Go checks
// to the spec's list.
//
// Now the generator emits `tlaRequiredInvariants` (the spec's conjuncts, parsed
// out of `Safety ==`) into the fixture, and TestEngineInvariantsMatchSpec below
// fails when a conjunct has no entry here. Adding an invariant to the spec is
// therefore not optional on the Go side.

import (
	"sort"
	"strconv"
	"testing"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// invariantT is the slice of *testing.T and *rapid.T that the invariants need.
// It is deliberately just Fatalf: *rapid.T has no Helper(), so the failure line
// points at the invariant function rather than the caller — which is why every
// message below names the invariant it belongs to.
type invariantT interface {
	Fatalf(format string, args ...any)
}

// engineInvariants maps each name to its Go counterpart. Keys are either a
// conjunct of the spec's Safety predicate (see tlaRequiredInvariants in the
// generated fixture) or a member of goOnlyEngineInvariants below.
//
// Every invariant reads the `api` view — the source of truth, what a real API
// server would hold — never the informer `cache`, which legitimately lags.
var engineInvariants = map[string]func(t invariantT, m *engineSim){
	// TypeOK is necessarily partial in Go. The spec's version asserts membership
	// in the bounded sets Gens / SpecVers, which are a model artifact: the real
	// reconciler bumps generations without an upper bound (that mismatch is what
	// tlaModelBoundary skips in the state cover). What carries over is the
	// discriminated-union part — the phase is one the state machine knows, and
	// the generation fields are either a real generation or the -1 sentinel.
	"TypeOK": func(t invariantT, m *engineSim) {
		switch m.status.Phase {
		case "",
			computev1alpha1.PhaseStable,
			computev1alpha1.PhaseCreating,
			computev1alpha1.PhaseSwitching,
			computev1alpha1.PhaseDraining,
			computev1alpha1.PhaseCleaning,
			computev1alpha1.PhaseStopped:
		default:
			t.Fatalf("TypeOK: phase %q is not a phase the state machine defines", m.status.Phase)
		}
		if m.status.CurrentGeneration < 0 {
			t.Fatalf("TypeOK: CurrentGeneration=%d is negative", m.status.CurrentGeneration)
		}
		if m.status.ActiveGeneration < -1 {
			t.Fatalf("TypeOK: ActiveGeneration=%d is below the -1 sentinel", m.status.ActiveGeneration)
		}
		if m.status.DrainingGeneration != nil && *m.status.DrainingGeneration < 0 {
			t.Fatalf("TypeOK: DrainingGeneration=%d is negative (absence is nil, not -1)",
				*m.status.DrainingGeneration)
		}
	},

	"Inv_TerminalConsistency": func(t invariantT, m *engineSim) {
		if isTerminalPhase(m.status.Phase) && m.status.CurrentGeneration != m.status.ActiveGeneration {
			t.Fatalf("Inv_TerminalConsistency: phase=%s but CurrentGen=%d != ActiveGen=%d",
				m.status.Phase, m.status.CurrentGeneration, m.status.ActiveGeneration)
		}
	},

	"Inv_TerminalNoDraining": func(t invariantT, m *engineSim) {
		if isTerminalPhase(m.status.Phase) && m.status.DrainingGeneration != nil {
			t.Fatalf("Inv_TerminalNoDraining: phase=%s but DrainingGen=%d",
				m.status.Phase, *m.status.DrainingGeneration)
		}
	},

	// A stopped engine keeps its zero-replica STS, so this covers both terminal
	// phases.
	"Inv_TerminalHasSTS": func(t invariantT, m *engineSim) {
		if isTerminalPhase(m.status.Phase) && m.api.stses[m.status.CurrentGeneration] == nil {
			t.Fatalf("Inv_TerminalHasSTS: phase=%s but no STS for CurrentGen=%d",
				m.status.Phase, m.status.CurrentGeneration)
		}
	},

	"Inv_ActiveHasSTS": func(t invariantT, m *engineSim) {
		if m.status.ActiveGeneration >= 0 && m.api.stses[m.status.ActiveGeneration] == nil {
			t.Fatalf("Inv_ActiveHasSTS: ActiveGen=%d has no STS", m.status.ActiveGeneration)
		}
	},

	"Inv_ServiceValid": func(t invariantT, m *engineSim) {
		if m.status.ActiveGeneration < 0 {
			return
		}
		gen, ok := engineSimSvcTargetGen(t, m)
		if !ok {
			return
		}
		if m.api.stses[gen] == nil {
			t.Fatalf("Inv_ServiceValid: cluster Service targets gen=%d, which has no STS", gen)
		}
	},

	"Inv_ServiceKnownGen": func(t invariantT, m *engineSim) {
		if m.status.ActiveGeneration < 0 {
			return
		}
		gen, ok := engineSimSvcTargetGen(t, m)
		if !ok {
			return
		}
		if gen != m.status.ActiveGeneration && gen != m.status.CurrentGeneration {
			t.Fatalf("Inv_ServiceKnownGen: svcTargetGen=%d not in {activeGen=%d, currentGen=%d}",
				gen, m.status.ActiveGeneration, m.status.CurrentGeneration)
		}
	},

	"Inv_DrainingPhase": func(t invariantT, m *engineSim) {
		if m.status.DrainingGeneration == nil {
			return
		}
		if m.status.Phase != computev1alpha1.PhaseDraining && m.status.Phase != computev1alpha1.PhaseCleaning {
			t.Fatalf("Inv_DrainingPhase: DrainingGen=%d but phase=%s",
				*m.status.DrainingGeneration, m.status.Phase)
		}
	},

	"Inv_DrainingOlderThanCurrent": func(t invariantT, m *engineSim) {
		if m.status.DrainingGeneration != nil && *m.status.DrainingGeneration >= m.status.CurrentGeneration {
			t.Fatalf("Inv_DrainingOlderThanCurrent: DrainingGen=%d, CurrentGen=%d",
				*m.status.DrainingGeneration, m.status.CurrentGeneration)
		}
	},

	"Inv_GenOrder": func(t invariantT, m *engineSim) {
		if m.status.ActiveGeneration > m.status.CurrentGeneration {
			t.Fatalf("Inv_GenOrder: ActiveGen=%d > CurrentGen=%d",
				m.status.ActiveGeneration, m.status.CurrentGeneration)
		}
	},

	// Gated on stsMatchesSpec exactly as the spec gates on StsMatchesSpec: mid
	// drift, a terminal phase legitimately lags the new spec, and catching up is
	// what drift detection is for. Once the current generation matches the spec,
	// a "stable" engine whose spec asks for zero replicas is a contract
	// violation users would see in status.
	"Inv_QuiescedPhaseMatchesSpec": func(t invariantT, m *engineSim) {
		if !isTerminalPhase(m.status.Phase) {
			return
		}
		// Quiesced also means the reconciler has caught up with the last spec
		// change. The spec gets that from StsMatchesSpec, because its
		// EnvChangeSpec always bumps specVer; Go has spec changes that produce
		// no template drift (replicas are applied in place by design), so the
		// terminal phase can legitimately name the old intent for one reconcile.
		// See engineSim.specDirty.
		if m.specDirty {
			return
		}
		sts := m.api.stses[m.status.CurrentGeneration]
		if sts == nil || !stsMatchesSpec(sts, &m.spec, testInstanceInfo(), m.classInfo) {
			return
		}
		wantStopped := m.spec.Replicas == 0
		gotStopped := m.status.Phase == computev1alpha1.PhaseStopped
		if gotStopped != wantStopped {
			t.Fatalf("Inv_QuiescedPhaseMatchesSpec: phase=%s but spec.Replicas=%d (quiesced on a matching STS)",
				m.status.Phase, m.spec.Replicas)
		}
	},

	// Go-only. The spec models resource deletion inside the reconciler actions,
	// so it has no separate conjunct for it; on the Go side the GC is a distinct
	// step (gcOrphanedResources) worth asserting on its own.
	"Inv_NoOrphanedResources": func(t invariantT, m *engineSim) {
		if !isTerminalPhase(m.status.Phase) {
			return
		}
		cur := m.status.CurrentGeneration
		for gen := range m.api.stses {
			if gen != cur {
				t.Fatalf("Inv_NoOrphanedResources: phase=%s but STS gen=%d survives (currentGen=%d)",
					m.status.Phase, gen, cur)
			}
		}
		for gen := range m.api.configMaps {
			if gen != cur {
				t.Fatalf("Inv_NoOrphanedResources: phase=%s but ConfigMap gen=%d survives (currentGen=%d)",
					m.status.Phase, gen, cur)
			}
		}
		for gen := range m.api.headlessSvcs {
			if gen != cur {
				t.Fatalf("Inv_NoOrphanedResources: phase=%s but HeadlessSvc gen=%d survives (currentGen=%d)",
					m.status.Phase, gen, cur)
			}
		}
	},
}

// goOnlyEngineInvariants are entries in the registry with no conjunct of their
// own in the spec's Safety predicate. Keeping the set explicit means
// TestEngineInvariantsMatchSpec can also catch the opposite drift: a registry
// key that the spec no longer has, i.e. an invariant renamed in the spec and
// left stale here.
var goOnlyEngineInvariants = map[string]string{
	"Inv_NoOrphanedResources": "the spec folds resource deletion into its reconciler actions; " +
		"Go has a separate GC step worth asserting on its own",
}

// engineSimSvcTargetGen reads the generation the cluster Service selects.
// Reports false when there is no Service, which several invariants treat as
// vacuous rather than as a violation.
func engineSimSvcTargetGen(t invariantT, m *engineSim) (int, bool) {
	if m.api.clusterSvc == nil {
		return 0, false
	}
	raw, ok := m.api.clusterSvc.Spec.Selector[LabelGeneration]
	if !ok {
		t.Fatalf("cluster Service is missing the %s selector label", LabelGeneration)
		return 0, false
	}
	gen, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("cluster Service has a non-numeric %s selector: %q", LabelGeneration, raw)
		return 0, false
	}
	return gen, true
}

// checkEngineInvariants runs every registered invariant. Sorted so a failing run
// reports the same invariant first every time, whichever harness called it.
func checkEngineInvariants(t invariantT, m *engineSim) {
	names := make([]string, 0, len(engineInvariants))
	for name := range engineInvariants {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		engineInvariants[name](t, m)
	}
}

// TestEngineInvariantsMatchSpec is the anti-drift guard: the spec's Safety
// conjuncts and the Go registry must name the same invariants, modulo the
// explicitly documented Go-only set.
func TestEngineInvariantsMatchSpec(t *testing.T) {
	required := make(map[string]bool, len(tlaRequiredInvariants))
	for _, name := range tlaRequiredInvariants {
		required[name] = true
		if _, ok := engineInvariants[name]; !ok {
			t.Errorf("formal/FireboltEngine.tla conjoins %s into Safety but the Go registry does not implement it.\n"+
				"Add it to engineInvariants so both harnesses check it. If it genuinely cannot be expressed "+
				"against engineSim, say why in a documented exemption rather than dropping it silently.", name)
		}
	}
	for name := range engineInvariants {
		if required[name] {
			continue
		}
		if _, ok := goOnlyEngineInvariants[name]; !ok {
			t.Errorf("the Go registry has %s, which is not a Safety conjunct and is not listed in "+
				"goOnlyEngineInvariants. Either it was renamed in the spec (fix the key) or it is Go-only "+
				"(document why).", name)
		}
	}
	if len(tlaRequiredInvariants) == 0 {
		t.Fatal("tlaRequiredInvariants is empty: the generator stopped parsing the spec's Safety predicate")
	}
	t.Logf("%d spec conjuncts, %d registered invariants (%d Go-only)",
		len(tlaRequiredInvariants), len(engineInvariants), len(goOnlyEngineInvariants))
}
