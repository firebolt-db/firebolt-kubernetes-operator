/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Phase 5 of the formal-verification plan:
// deterministic exhaustive state-cover testing for the FireboltInstance
// reconciler, mirroring the engine_tla_state_test.go harness.
//
// For every reachable state in the TLC state graph (regenerated via
// `make formal-gen`), this test materializes an instanceSim matching the
// state, runs one Reconcile, and verifies that the resulting state lies
// in the model's reconciler closure of the starting state — i.e. is a
// state TLC says is reachable from the start by zero or more consecutive
// reconciler-only transitions.

import (
	"fmt"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// tlaPhaseToGo maps the lowercase phase strings used in formal/FireboltInstance.tla
// to the capitalised computev1alpha1.InstancePhase constants. The spec's
// "uninitialized" maps to the empty Phase that the production code uses to
// distinguish "first reconcile, status not yet seeded" from any other state.
func tlaPhaseToGo(p string) computev1alpha1.InstancePhase {
	switch p {
	case "uninitialized":
		return ""
	case "provisioning":
		return computev1alpha1.InstancePhaseProvisioning
	case "ready":
		return computev1alpha1.InstancePhaseReady
	case "degraded":
		return computev1alpha1.InstancePhaseDegraded
	default:
		panic(fmt.Sprintf("unknown TLA+ phase %q", p))
	}
}

// goPhaseToTLA is the inverse of tlaPhaseToGo. Failed is mapped to a sentinel
// the test treats as "out of model": the spec excludes Failed from internal
// transitions, so observing it after a Reconcile is itself a test failure.
func goPhaseToTLA(p computev1alpha1.InstancePhase) string {
	switch p {
	case "":
		return "uninitialized"
	case computev1alpha1.InstancePhaseProvisioning:
		return "provisioning"
	case computev1alpha1.InstancePhaseReady:
		return "ready"
	case computev1alpha1.InstancePhaseDegraded:
		return "degraded"
	case computev1alpha1.InstancePhaseFailed:
		return "<failed-out-of-model>"
	default:
		return fmt.Sprintf("<unknown:%s>", p)
	}
}

// materializeTLAInstanceState constructs an instanceSim whose simulated state
// corresponds to the given TLA+ state.
func materializeTLAInstanceState(s tlaInstanceState) *instanceSim {
	m := &instanceSim{
		compAvail: map[string]bool{
			"postgres": s.PostgresAvail,
			"metadata": s.MetadataAvail,
			"gateway":  s.GatewayAvail,
		},
		instance: &computev1alpha1.FireboltInstance{},
	}
	m.instance.Name = propInstanceName
	m.instance.Namespace = propNamespace
	m.instance.Status.Phase = tlaPhaseToGo(s.Phase)
	// Pre-populate the per-component conditions to mirror what an instance in
	// this phase would carry mid-lifecycle. Without this, the very first
	// Reconcile against a Phase=Ready state would observe absent conditions
	// and route through the "Initializing" branch of setInstanceReadyRollup,
	// which is not what the model represents.
	if s.Phase != "uninitialized" {
		writeAvailCondition(m.instance,
			computev1alpha1.InstanceConditionMetadataReady,
			s.PostgresAvail && s.MetadataAvail)
		writeAvailCondition(m.instance,
			computev1alpha1.InstanceConditionGatewayReady,
			s.GatewayAvail)
		setInstanceReadyRollup(m.instance)
	}
	return m
}

// writeAvailCondition writes a per-component condition with the canonical
// Provisioning/Ready Reason vocabulary.
func writeAvailCondition(instance *computev1alpha1.FireboltInstance, condType string, ready bool) {
	status := metav1.ConditionFalse
	reason := "Provisioning"
	if ready {
		status = metav1.ConditionTrue
		reason = "Ready"
	}
	setInstanceCondition(instance, condType, status, reason, "")
}

// projectInstanceSim extracts the TLA+ observable variables from the sim.
func projectInstanceSim(m *instanceSim) tlaInstanceState {
	return tlaInstanceState{
		Phase:         goPhaseToTLA(m.instance.Status.Phase),
		PostgresAvail: m.compAvail["postgres"],
		MetadataAvail: m.compAvail["metadata"],
		GatewayAvail:  m.compAvail["gateway"],
	}
}

// instanceClosureContains reports whether `actual` is one of the TLA+ states
// the model considers reachable from the test's starting state via 0+
// reconciler-only transitions. closureIDs are indices into tlaInstanceStatePool.
func instanceClosureContains(closureIDs []int, actual tlaInstanceState) bool {
	for _, id := range closureIDs {
		if tlaInstanceStatePool[id] == actual {
			return true
		}
	}
	return false
}

// tlaInstanceInvariants mirrors the Check predicates in instance_property_test.go.
func tlaInstanceInvariants(t *testing.T, m *instanceSim) {
	t.Helper()
	s := m.instance.Status

	switch s.Phase {
	case "",
		computev1alpha1.InstancePhaseProvisioning,
		computev1alpha1.InstancePhaseReady,
		computev1alpha1.InstancePhaseDegraded:
		// ok
	case computev1alpha1.InstancePhaseFailed:
		t.Fatal("Failed phase reached via internal transitions; spec excludes it")
	default:
		t.Fatalf("invalid phase %q", s.Phase)
	}

	if s.Phase == computev1alpha1.InstancePhaseReady {
		ready := apimeta.FindStatusCondition(s.Conditions, computev1alpha1.InstanceConditionReady)
		if ready == nil {
			t.Fatal("Phase=Ready but ConditionReady is absent")
			return
		}
		if ready.Status != metav1.ConditionTrue {
			t.Fatalf("Phase=Ready but ConditionReady=%s (reason=%s)",
				ready.Status, ready.Reason)
		}
	}

	ready := apimeta.FindStatusCondition(s.Conditions, computev1alpha1.InstanceConditionReady)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		for _, c := range []string{
			computev1alpha1.InstanceConditionMetadataReady,
			computev1alpha1.InstanceConditionGatewayReady,
		} {
			cond := apimeta.FindStatusCondition(s.Conditions, c)
			if cond == nil || cond.Status != metav1.ConditionTrue {
				t.Fatalf("ConditionReady=True but %s=%v", c, cond)
			}
		}
	}
}

func TestTLAInstanceStateCover(t *testing.T) {
	for i := range tlaInstanceStateCases {
		tc := tlaInstanceStateCases[i]
		start := tlaInstanceStatePool[tc.Start]
		name := fmt.Sprintf("case-%02d/%s/p=%t/m=%t/g=%t",
			i, start.Phase,
			start.PostgresAvail, start.MetadataAvail, start.GatewayAvail)
		t.Run(name, func(t *testing.T) {
			m := materializeTLAInstanceState(start)

			// Mirror instanceSim.Reconcile in engine_property_test.go style:
			// init-seed branch when Phase is empty, otherwise the full
			// pipeline-projection + setInstanceReadyRollup + computePhase.
			if m.instance.Status.Phase == "" {
				m.instance.Status.Phase = computev1alpha1.InstancePhaseProvisioning
			} else {
				m.writeComponentConditions()
				setInstanceReadyRollup(m.instance)
				r := &FireboltInstanceReconciler{}
				m.instance.Status.Phase = r.computePhase(m.instance)
			}

			tlaInstanceInvariants(t, m)

			actual := projectInstanceSim(m)
			if !instanceClosureContains(tc.Closure, actual) {
				t.Fatalf("result not in TLA+ reconciler closure of starting state\n  start:    %+v\n  actual:   %+v\n  closure (%d states):\n%s",
					start, actual, len(tc.Closure), formatInstanceClosure(tc.Closure))
			}
		})
	}
	t.Logf("instance state cover: ran %d cases", len(tlaInstanceStateCases))
}

// formatInstanceClosure renders the first few entries of a closure index list
// for inclusion in a Fatalf message; pool indices are surfaced so errors
// point straight back into tlaInstanceStatePool.
func formatInstanceClosure(closureIDs []int) string {
	const limit = 8
	out := ""
	for i, id := range closureIDs {
		if i >= limit {
			out += fmt.Sprintf("    ... (%d more)\n", len(closureIDs)-limit)
			break
		}
		out += fmt.Sprintf("    [pool %d] %+v\n", id, tlaInstanceStatePool[id])
	}
	return out
}
