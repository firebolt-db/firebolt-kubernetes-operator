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
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func newInstance() *computev1alpha1.FireboltInstance {
	return &computev1alpha1.FireboltInstance{}
}

func TestSetInstanceCondition_SetsAllFields(t *testing.T) {
	inst := newInstance()
	inst.Generation = 7

	setInstanceCondition(inst, "X", metav1.ConditionTrue, "R", "M")

	got := apimeta.FindStatusCondition(inst.Status.Conditions, "X")
	if got == nil {
		t.Fatal("expected condition X to be set")
	}
	if got.Status != metav1.ConditionTrue || got.Reason != "R" || got.Message != "M" || got.ObservedGeneration != 7 {
		t.Fatalf("unexpected condition: %+v", got)
	}
}

func TestSetInstanceCondition_IdempotentTransitionTime(t *testing.T) {
	inst := newInstance()
	setInstanceCondition(inst, "X", metav1.ConditionTrue, "R", "M")
	first := apimeta.FindStatusCondition(inst.Status.Conditions, "X").LastTransitionTime

	setInstanceCondition(inst, "X", metav1.ConditionTrue, "R", "M")
	second := apimeta.FindStatusCondition(inst.Status.Conditions, "X").LastTransitionTime

	if !first.Equal(&second) {
		t.Fatalf("LastTransitionTime changed on no-op update: first=%v second=%v", first, second)
	}
}

func TestSetInstanceReadyRollup_TrueWhenAllComponentsTrue(t *testing.T) {
	inst := newInstance()
	for _, c := range []string{
		computev1alpha1.InstanceConditionPostgresReady,
		computev1alpha1.InstanceConditionMetadataReady,
		computev1alpha1.InstanceConditionGatewayReady,
	} {
		setInstanceCondition(inst, c, metav1.ConditionTrue, "Ready", "ok")
	}

	setInstanceReadyRollup(inst)

	ready := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True, got %+v", ready)
	}
	if ready.Reason != "AllComponentsReady" {
		t.Fatalf("unexpected reason: %q", ready.Reason)
	}
}

func TestSetInstanceReadyRollup_PropagatesFirstBlocker(t *testing.T) {
	// Pipeline order: Postgres → Metadata → Gateway.
	// When multiple components are False/missing, the roll-up must
	// surface the FIRST blocker so users see the root cause at the
	// headline condition.
	inst := newInstance()
	setInstanceCondition(inst, computev1alpha1.InstanceConditionPostgresReady,
		metav1.ConditionTrue, "Ready", "pg ok")
	setInstanceCondition(inst, computev1alpha1.InstanceConditionMetadataReady,
		metav1.ConditionFalse, "EnsureFailed", "metadata ensure failed: boom")

	setInstanceReadyRollup(inst)

	ready := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionReady)
	if ready == nil {
		t.Fatal("expected Ready condition to be set")
	}
	if ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False, got %v", ready.Status)
	}
	if ready.Reason != "EnsureFailed" {
		t.Fatalf("expected Reason from first blocker (MetadataReady), got %q", ready.Reason)
	}
	if !strings.Contains(ready.Message, computev1alpha1.InstanceConditionMetadataReady) {
		t.Fatalf("expected message to name MetadataReady, got %q", ready.Message)
	}
}

func TestSetInstanceReadyRollup_MissingComponentCountsAsNotReady(t *testing.T) {
	inst := newInstance()
	// Only Postgres is set; Metadata/Gateway are absent.
	setInstanceCondition(inst, computev1alpha1.InstanceConditionPostgresReady,
		metav1.ConditionTrue, "Ready", "pg ok")

	setInstanceReadyRollup(inst)

	ready := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected Ready=False for missing component, got %+v", ready)
	}
	if ready.Reason != "Initializing" {
		t.Fatalf("expected Reason=Initializing for missing component, got %q", ready.Reason)
	}
	if !strings.Contains(ready.Message, computev1alpha1.InstanceConditionMetadataReady) {
		t.Fatalf("expected message to name first missing component, got %q", ready.Message)
	}
}

// TestComputePhase_TracksConditionReady locks in the Phase ⇔ ConditionReady
// invariant introduced when computePhase was rewired off the boolean
// *Ready fields. The boolean fields are deliberately set to true in the
// "lying state" cases below to prove that Phase no longer follows them
// when the roll-up condition says otherwise.
func TestComputePhase_TracksConditionReady(t *testing.T) {
	cases := []struct {
		name      string
		prepare   func(*computev1alpha1.FireboltInstance)
		wantPhase computev1alpha1.InstancePhase
	}{
		{
			name: "all green -> Ready",
			prepare: func(inst *computev1alpha1.FireboltInstance) {
				inst.Status.MetadataReady = true
				inst.Status.GatewayReady = true
				for _, c := range []string{
					computev1alpha1.InstanceConditionPostgresReady,
					computev1alpha1.InstanceConditionMetadataReady,
					computev1alpha1.InstanceConditionGatewayReady,
				} {
					setInstanceCondition(inst, c, metav1.ConditionTrue, "Ready", "ok")
				}
				setInstanceReadyRollup(inst)
			},
			wantPhase: computev1alpha1.InstancePhaseReady,
		},
		{
			// Scenario A from the audit: external Postgres Secret deleted
			// after the instance reached Ready. Metadata pod keeps running
			// with mounted creds so MetadataReady/GatewayReady stay true,
			// but PostgresReady condition has flipped False. Old Phase
			// derivation would lie and keep reporting Ready; new
			// derivation flips to Degraded.
			name: "Postgres False but booleans still true -> Degraded (not a lie)",
			prepare: func(inst *computev1alpha1.FireboltInstance) {
				inst.Status.Phase = computev1alpha1.InstancePhaseReady
				inst.Status.MetadataReady = true
				inst.Status.GatewayReady = true
				setInstanceCondition(inst, computev1alpha1.InstanceConditionPostgresReady,
					metav1.ConditionFalse, "SecretPreflightFailed", "boom")
				setInstanceCondition(inst, computev1alpha1.InstanceConditionMetadataReady,
					metav1.ConditionTrue, "Ready", "ok")
				setInstanceCondition(inst, computev1alpha1.InstanceConditionGatewayReady,
					metav1.ConditionTrue, "Ready", "ok")
				setInstanceReadyRollup(inst)
			},
			wantPhase: computev1alpha1.InstancePhaseDegraded,
		},
		{
			name: "Not yet observed -> Provisioning",
			prepare: func(inst *computev1alpha1.FireboltInstance) {
				inst.Status.Phase = computev1alpha1.InstancePhaseProvisioning
				setInstanceReadyRollup(inst)
			},
			wantPhase: computev1alpha1.InstancePhaseProvisioning,
		},
		{
			name: "Failed is terminal even if condition Ready is True",
			prepare: func(inst *computev1alpha1.FireboltInstance) {
				inst.Status.Phase = computev1alpha1.InstancePhaseFailed
				for _, c := range []string{
					computev1alpha1.InstanceConditionPostgresReady,
					computev1alpha1.InstanceConditionMetadataReady,
					computev1alpha1.InstanceConditionGatewayReady,
				} {
					setInstanceCondition(inst, c, metav1.ConditionTrue, "Ready", "ok")
				}
				setInstanceReadyRollup(inst)
			},
			wantPhase: computev1alpha1.InstancePhaseFailed,
		},
	}

	r := &FireboltInstanceReconciler{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := newInstance()
			tc.prepare(inst)
			got := r.computePhase(inst)
			if got != tc.wantPhase {
				ready := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionReady)
				t.Fatalf("computePhase = %q, want %q (ConditionReady=%+v)", got, tc.wantPhase, ready)
			}
		})
	}
}

func TestSetInstanceReadyRollup_RecoversFromFalseToTrue(t *testing.T) {
	// After a failure flips Ready=False, a subsequent healthy pass must
	// flip it back to True rather than leaving a stale False.
	inst := newInstance()
	setInstanceCondition(inst, computev1alpha1.InstanceConditionPostgresReady,
		metav1.ConditionFalse, "EnsureFailed", "boom")
	setInstanceReadyRollup(inst)

	for _, c := range []string{
		computev1alpha1.InstanceConditionPostgresReady,
		computev1alpha1.InstanceConditionMetadataReady,
		computev1alpha1.InstanceConditionGatewayReady,
	} {
		setInstanceCondition(inst, c, metav1.ConditionTrue, "Ready", "ok")
	}
	setInstanceReadyRollup(inst)

	ready := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionReady)
	if ready == nil || ready.Status != metav1.ConditionTrue {
		t.Fatalf("expected Ready=True after recovery, got %+v", ready)
	}
}
