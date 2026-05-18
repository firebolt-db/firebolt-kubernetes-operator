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
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
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
		name        string
		status      computev1alpha1.FireboltEngineStatus
		current     EngineState
		wantStatus  metav1.ConditionStatus
		wantReason  string
		wantMessage string
	}{
		{
			name: "Stable + instance ready + pods ready => True",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:            computev1alpha1.PhaseStable,
				ActiveGeneration: 3,
				Conditions:       []metav1.Condition{instanceReadyCond(metav1.ConditionTrue)},
			},
			current:    EngineState{CurrentPodsReady: true, CurrentPodTotal: 3, CurrentPodReady: 3},
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
			current:     EngineState{CurrentPodsReady: false, CurrentPodTotal: 3, CurrentPodReady: 1},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  "PodsNotReady",
			wantMessage: "generation 2 has 1 of 3 pod(s) ready",
		},
		{
			name: "Stopped phase => Stopped (not Rolling, not EngineReady)",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:            computev1alpha1.PhaseStopped,
				ActiveGeneration: 4,
				Conditions:       []metav1.Condition{instanceReadyCond(metav1.ConditionTrue)},
			},
			current:     EngineState{CurrentPodsReady: true, CurrentPodTotal: 0, CurrentPodReady: 0},
			wantStatus:  metav1.ConditionFalse,
			wantReason:  "Stopped",
			wantMessage: "Engine is stopped (spec.replicas is 0)",
		},
		{
			name: "InstanceNotReady beats Stopped",
			status: computev1alpha1.FireboltEngineStatus{
				Phase:      computev1alpha1.PhaseStopped,
				Conditions: []metav1.Condition{instanceReadyCond(metav1.ConditionFalse)},
			},
			current:    EngineState{CurrentPodsReady: true},
			wantStatus: metav1.ConditionFalse,
			wantReason: "InstanceNotReady",
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
			if tc.wantMessage != "" && got.Message != tc.wantMessage {
				t.Errorf("message: got %q, want %q", got.Message, tc.wantMessage)
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

func TestShouldSurfaceStatefulSetEvent(t *testing.T) {
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "engine-g0"}}

	readyCondFalse := func(reason string) metav1.Condition {
		return metav1.Condition{
			Type:   computev1alpha1.ConditionReady,
			Status: metav1.ConditionFalse,
			Reason: reason,
		}
	}

	tests := []struct {
		name             string
		conditions       []metav1.Condition
		current          EngineState
		expectedReplicas int32
		want             bool
	}{
		{
			name:             "no STS observed yet => skip",
			conditions:       []metav1.Condition{readyCondFalse("Rolling")},
			current:          EngineState{CurrentSTS: nil, CurrentPodTotal: 0},
			expectedReplicas: 3,
			want:             false,
		},
		{
			name:             "pod count matches replicas => skip (no missing pods)",
			conditions:       []metav1.Condition{readyCondFalse("PodsNotReady")},
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 3},
			expectedReplicas: 3,
			want:             false,
		},
		{
			name:             "Rolling + STS + missing pods => surface",
			conditions:       []metav1.Condition{readyCondFalse("Rolling")},
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 0},
			expectedReplicas: 3,
			want:             true,
		},
		{
			name:             "PodsNotReady + STS + missing pods => surface",
			conditions:       []metav1.Condition{readyCondFalse("PodsNotReady")},
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 1},
			expectedReplicas: 3,
			want:             true,
		},
		{
			name: "InstanceNotReady wins; do not mask higher-precedence reason",
			conditions: []metav1.Condition{
				readyCondFalse("InstanceNotReady"),
			},
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 0},
			expectedReplicas: 3,
			want:             false,
		},
		{
			name: "DrainCheckFailing wins; do not mask drain-probe diagnostic",
			conditions: []metav1.Condition{
				readyCondFalse("DrainCheckFailing"),
			},
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 0},
			expectedReplicas: 3,
			want:             false,
		},
		{
			name: "Ready=True => skip (engine is healthy)",
			conditions: []metav1.Condition{{
				Type:   computev1alpha1.ConditionReady,
				Status: metav1.ConditionTrue,
				Reason: "EngineReady",
			}},
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 0},
			expectedReplicas: 3,
			want:             false,
		},
		{
			name:             "missing Ready condition => skip",
			conditions:       nil,
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 0},
			expectedReplicas: 3,
			want:             false,
		},
		{
			name:             "zero expected replicas => skip (parked engine, no pods expected)",
			conditions:       []metav1.Condition{readyCondFalse("Stopped")},
			current:          EngineState{CurrentSTS: sts, CurrentPodTotal: 0},
			expectedReplicas: 0,
			want:             false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status := &computev1alpha1.FireboltEngineStatus{Conditions: tc.conditions}
			got := shouldSurfaceStatefulSetEvent(status, tc.current, tc.expectedReplicas)
			if got != tc.want {
				t.Errorf("shouldSurfaceStatefulSetEvent = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyStatefulSetEventToReadyCondition(t *testing.T) {
	baseStatus := func() *computev1alpha1.FireboltEngineStatus {
		return &computev1alpha1.FireboltEngineStatus{
			Conditions: []metav1.Condition{{
				Type:    computev1alpha1.ConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "Rolling",
				Message: "Engine is in creating phase",
			}},
		}
	}

	t.Run("FailedCreate event overrides Reason and Message", func(t *testing.T) {
		status := baseStatus()
		ev := &corev1.Event{
			InvolvedObject: corev1.ObjectReference{
				Kind: "StatefulSet",
				Name: "gamma-g0",
			},
			Type:    corev1.EventTypeWarning,
			Reason:  "FailedCreate",
			Message: `create Pod gamma-g0-0 in StatefulSet gamma-g0 failed error: pods "gamma-g0-0" is forbidden: error looking up service account firebolt-oss/firebolt-oss: serviceaccount "firebolt-oss" not found`,
			Count:   7,
		}
		applyStatefulSetEventToReadyCondition(status, ev, 42)

		cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady)
		if cond == nil {
			t.Fatal("expected Ready condition")
		}
		if cond.Reason != "FailedCreate" {
			t.Errorf("reason: got %q, want FailedCreate", cond.Reason)
		}
		if cond.Status != metav1.ConditionFalse {
			t.Errorf("status: got %s, want False", cond.Status)
		}
		if cond.ObservedGeneration != 42 {
			t.Errorf("observedGeneration: got %d, want 42", cond.ObservedGeneration)
		}
		if !strings.Contains(cond.Message, "gamma-g0") {
			t.Errorf("message missing STS name: %q", cond.Message)
		}
		if !strings.Contains(cond.Message, "serviceaccount") {
			t.Errorf("message missing underlying error: %q", cond.Message)
		}
		if !strings.Contains(cond.Message, "x7") {
			t.Errorf("message missing event count: %q", cond.Message)
		}
	})

	t.Run("count==1 omits the multiplier suffix", func(t *testing.T) {
		status := baseStatus()
		ev := &corev1.Event{
			InvolvedObject: corev1.ObjectReference{Kind: "StatefulSet", Name: "e-g0"},
			Type:           corev1.EventTypeWarning,
			Reason:         "FailedCreate",
			Message:        "boom",
			Count:          1,
		}
		applyStatefulSetEventToReadyCondition(status, ev, 1)
		cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady)
		if strings.Contains(cond.Message, "x1") {
			t.Errorf("did not expect count suffix for count==1: %q", cond.Message)
		}
	})

	t.Run("nil event is a no-op", func(t *testing.T) {
		status := baseStatus()
		applyStatefulSetEventToReadyCondition(status, nil, 1)
		cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady)
		if cond.Reason != "Rolling" {
			t.Errorf("expected Reason untouched; got %q", cond.Reason)
		}
	})

	t.Run("event with invalid Reason falls back to generic", func(t *testing.T) {
		status := baseStatus()
		ev := &corev1.Event{
			InvolvedObject: corev1.ObjectReference{Kind: "StatefulSet", Name: "e-g0"},
			Type:           corev1.EventTypeWarning,
			Reason:         "1-cannot-start-with-digit",
			Message:        "weird",
		}
		applyStatefulSetEventToReadyCondition(status, ev, 1)
		cond := apimeta.FindStatusCondition(status.Conditions, computev1alpha1.ConditionReady)
		if cond.Reason != reasonStatefulSetWarning {
			t.Errorf("expected fallback reason; got %q", cond.Reason)
		}
	})
}

func TestPickLatestWarning(t *testing.T) {
	now := time.Now()
	older := corev1.Event{
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "old failure",
		LastTimestamp: metav1.NewTime(now.Add(-5 * time.Minute)),
	}
	newer := corev1.Event{
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "new failure",
		LastTimestamp: metav1.NewTime(now),
	}
	normal := corev1.Event{
		Type:          corev1.EventTypeNormal,
		Reason:        "SuccessfulCreate",
		Message:       "ignored",
		LastTimestamp: metav1.NewTime(now.Add(time.Minute)),
	}
	blank := corev1.Event{
		Type:          corev1.EventTypeWarning,
		Reason:        "FailedCreate",
		Message:       "   ",
		LastTimestamp: metav1.NewTime(now.Add(2 * time.Minute)),
	}
	seriesOnly := corev1.Event{
		Type:      corev1.EventTypeWarning,
		Reason:    "FailedCreate",
		Message:   "series-style",
		EventTime: metav1.MicroTime{Time: now.Add(10 * time.Minute)},
	}

	t.Run("empty slice", func(t *testing.T) {
		if got := pickLatestWarning(nil); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})

	t.Run("Warning events sorted by recency, ignoring Normal and blank", func(t *testing.T) {
		got := pickLatestWarning([]corev1.Event{older, normal, newer, blank})
		if got == nil {
			t.Fatal("expected an event")
		}
		if got.Message != "new failure" {
			t.Errorf("want newest non-blank warning, got %q", got.Message)
		}
	})

	t.Run("EventTime is honored when LastTimestamp is zero", func(t *testing.T) {
		got := pickLatestWarning([]corev1.Event{newer, seriesOnly})
		if got == nil || got.Message != "series-style" {
			t.Fatalf("expected series-style event, got %+v", got)
		}
	})

	t.Run("returns a copy (caller cannot mutate the source slice)", func(t *testing.T) {
		input := []corev1.Event{newer}
		got := pickLatestWarning(input)
		got.Message = "mutated"
		if input[0].Message == "mutated" {
			t.Error("pickLatestWarning must return a defensive copy")
		}
	})
}

func TestSanitizeConditionReason(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", reasonStatefulSetWarning},
		{"FailedCreate", "FailedCreate"},
		{"FailedScheduling", "FailedScheduling"},
		{"BackOff", "BackOff"},
		{"camelCase_with_underscore", "camelCase_with_underscore"},
		{"1leading_digit", reasonStatefulSetWarning},
		{"has spaces", reasonStatefulSetWarning},
		{"has-dash", reasonStatefulSetWarning},
		{"trailing,", reasonStatefulSetWarning},
		{"trailing:", reasonStatefulSetWarning},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeConditionReason(tc.in); got != tc.want {
				t.Errorf("sanitizeConditionReason(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
