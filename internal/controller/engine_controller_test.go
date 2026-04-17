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
	stderrors "errors"
	"fmt"
	"strings"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

func instanceReadyCond(s metav1.ConditionStatus) metav1.Condition {
	return metav1.Condition{
		Type:   computev1alpha1.ConditionInstanceReady,
		Status: s,
		Reason: "Test",
	}
}

func TestSetReadyCondition(t *testing.T) {
	tests := []struct {
		name       string
		status     computev1alpha1.FireboltEngineStatus
		current    EngineState
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name: "Stable + instance ready + pods ready => True",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:            computev1alpha1.PhaseStable,
				ActiveGeneration: 3,
				Conditions:       []metav1.Condition{instanceReadyCond(metav1.ConditionTrue)},
			},
			current:    EngineState{CurrentPodsReady: true, CurrentPodCount: 3},
			wantStatus: metav1.ConditionTrue,
			wantReason: "EngineReady",
		},
		{
			name: "InstanceReady=False blocks Ready",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:      computev1alpha1.PhaseStable,
				Conditions: []metav1.Condition{instanceReadyCond(metav1.ConditionFalse)},
			},
			current:    EngineState{CurrentPodsReady: true},
			wantStatus: metav1.ConditionFalse,
			wantReason: "InstanceNotReady",
		},
		{
			name: "Missing InstanceReady condition => not Ready",
			status: computev1alpha1.FireboltEngineStatus{
				Phase: computev1alpha1.PhaseStable,
			},
			current:    EngineState{CurrentPodsReady: true},
			wantStatus: metav1.ConditionFalse,
			wantReason: "InstanceNotReady",
		},
		{
			name: "Creating phase => Rolling",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:      computev1alpha1.PhaseCreating,
				Conditions: []metav1.Condition{instanceReadyCond(metav1.ConditionTrue)},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "Rolling",
		},
		{
			name: "Draining phase => Rolling (Ready stays False during rollout)",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:      computev1alpha1.PhaseDraining,
				Conditions: []metav1.Condition{instanceReadyCond(metav1.ConditionTrue)},
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "Rolling",
		},
		{
			name: "Stable but pods not ready => PodsNotReady",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:            computev1alpha1.PhaseStable,
				ActiveGeneration: 2,
				Conditions:       []metav1.Condition{instanceReadyCond(metav1.ConditionTrue)},
			},
			current:    EngineState{CurrentPodsReady: false, CurrentPodCount: 1},
			wantStatus: metav1.ConditionFalse,
			wantReason: "PodsNotReady",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setReadyCondition(&tc.status, tc.current, 7)
			got := apimeta.FindStatusCondition(tc.status.Conditions, computev1alpha1.ConditionReady)
			if got == nil {
				t.Fatal("expected Ready condition to be set")
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status: got %s, want %s", got.Status, tc.wantStatus)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason: got %s, want %s", got.Reason, tc.wantReason)
			}
			if got.ObservedGeneration != 7 {
				t.Errorf("observedGeneration: got %d, want 7", got.ObservedGeneration)
			}
		})
	}
}

func TestSetDrainCheckFailingCondition(t *testing.T) {
	drainErr := &DrainProbeError{
		Generation: 5,
		PodName:    "my-engine-g5-0",
		Err:        stderrors.New("scraping metrics: connection refused"),
	}

	tests := []struct {
		name    string
		err     error
		wantSet bool
	}{
		{
			name:    "DrainProbeError is detected and condition is set",
			err:     drainErr,
			wantSet: true,
		},
		{
			name:    "Wrapped DrainProbeError is detected via errors.As",
			err:     fmt.Errorf("getEngineState failed: %w", fmt.Errorf("checkDrainComplete: %w", drainErr)),
			wantSet: true,
		},
		{
			name:    "Plain error leaves conditions untouched",
			err:     stderrors.New("listing pods: forbidden"),
			wantSet: false,
		},
		{
			name:    "nil error leaves conditions untouched",
			err:     nil,
			wantSet: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := &computev1alpha1.FireboltEngineStatus{}
			gotSet := setDrainCheckFailingCondition(status, tc.err, 11)
			if gotSet != tc.wantSet {
				t.Fatalf("return: got %v, want %v", gotSet, tc.wantSet)
			}
			cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady)
			if !tc.wantSet {
				if cond != nil {
					t.Fatalf("did not expect a Ready condition; got %+v", cond)
				}
				return
			}
			if cond == nil {
				t.Fatal("expected Ready condition to be set")
			}
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("status: got %s, want False", cond.Status)
			}
			if cond.Reason != "DrainCheckFailing" {
				t.Errorf("reason: got %s, want DrainCheckFailing", cond.Reason)
			}
			if cond.ObservedGeneration != 11 {
				t.Errorf("observedGeneration: got %d, want 11", cond.ObservedGeneration)
			}
			// The message should carry enough context to actually triage:
			// pod name, generation, underlying cause.
			if !strings.Contains(cond.Message, "my-engine-g5-0") ||
				!strings.Contains(cond.Message, "gen 5") ||
				!strings.Contains(cond.Message, "connection refused") {
				t.Errorf("message missing diagnostic context: %q", cond.Message)
			}
		})
	}
}

func TestSetDrainCheckFailingCondition_RecoversOnNextSetReady(t *testing.T) {
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:      computev1alpha1.PhaseDraining,
		Conditions: []metav1.Condition{instanceReadyCond(metav1.ConditionTrue)},
	}
	drainErr := &DrainProbeError{Generation: 2, PodName: "p", Err: stderrors.New("nope")}
	if !setDrainCheckFailingCondition(status, drainErr, 1) {
		t.Fatal("expected condition to be set")
	}
	if cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady); cond == nil || cond.Reason != "DrainCheckFailing" {
		t.Fatalf("expected DrainCheckFailing; got %+v", cond)
	}

	// Simulate the next successful reconcile: setReadyCondition runs
	// and must overwrite the DrainCheckFailing reason without any
	// explicit clear path. This is how the condition self-heals.
	setReadyCondition(status, EngineState{}, 1)
	cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("expected Ready condition")
	}
	if cond.Reason == "DrainCheckFailing" {
		t.Errorf("expected DrainCheckFailing to be overwritten; got %+v", cond)
	}
	if cond.Reason != "Rolling" {
		t.Errorf("phase is Draining; expected Rolling, got %s", cond.Reason)
	}
}
