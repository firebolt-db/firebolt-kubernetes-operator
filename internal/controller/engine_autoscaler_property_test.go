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

// Property tests for computeAutoscalerDecision. The decision function is
// pure (no K8s client, no real clock), so a uniform random-input rapid
// harness can drive it through every combination of:
//   - autoscaling on/off and the (MinReplicas, MaxReplicas, IdleTimeout,
//     PollInterval) tuning knobs
//   - status.LastActivityTime nil vs. age in (-1h, +1h] vs. now
//   - obs.ActiveQueries / obs.SuspendedQueries / obs.ScrapeFailed
//   - obs.WakeRequestedAt nil vs. age in (-2*TTL, +TTL]
//   - an optional Schedule window encompassing or not encompassing now
//
// After every draw, the test asserts the precedence-rule invariants
// listed in computeAutoscalerDecision's docstring. The previous coverage
// was example-based (engine_autoscaler_test.go), which fixes a single
// concrete (spec, obs, time) triple per case; the random sampler reaches
// the cross-product the example tests never enumerate, so a regression
// that re-orders precedence rules or drops a guard surfaces here even
// when no example happens to hit the broken combination.

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"pgregory.net/rapid"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// referenceTime is the synthetic "now" the harness anchors every draw to.
// Avoiding wall-clock time keeps reproductions deterministic across
// machines and CI runs.
var referenceTime = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func TestComputeAutoscalerDecision_Properties(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		in := drawAutoscalerInput(t)
		dec := computeAutoscalerDecision(in.spec, in.status, in.obs, referenceTime)

		assertAutoscalerWellFormed(t, in, dec)
		assertAutoscalerPrecedence(t, in, dec)
	})
}

// autoscalerInput is the materialized random draw the property checker
// consumes. Keeping spec/status/obs together makes the per-property
// assertions short and lets the rapid trace pinpoint which knob produced
// the failure.
type autoscalerInput struct {
	spec   *computev1alpha1.FireboltEngineSpec
	status *computev1alpha1.FireboltEngineStatus
	obs    AutoscalerObservation
	// Convenience copies of derived inputs the assertions need to inspect
	// without re-reading them from the struct chain.
	autoscalingEnabled bool
	minReplicas        int32
	maxReplicas        int32
	idleTimeout        time.Duration
	pollInterval       time.Duration
	scheduleActiveNow  bool
	wakeFresh          bool
}

func drawAutoscalerInput(t *rapid.T) autoscalerInput {
	enabled := rapid.Bool().Draw(t, "autoscalingEnabled")
	maxReplicas := int32(rapid.IntRange(1, 10).Draw(t, "maxReplicas"))
	minReplicas := int32(rapid.IntRange(0, int(maxReplicas)).Draw(t, "minReplicas"))
	idle := time.Duration(rapid.IntRange(1, 120).Draw(t, "idleMinutes")) * time.Minute
	poll := time.Duration(rapid.IntRange(10, 300).Draw(t, "pollSeconds")) * time.Second
	replicas := int32(rapid.IntRange(0, 10).Draw(t, "specReplicas"))

	// Schedule: zero or one window covering or not covering referenceTime.
	var schedule []computev1alpha1.ScheduleWindow
	scheduleActiveNow := false
	switch rapid.IntRange(0, 2).Draw(t, "scheduleShape") {
	case 1:
		schedule = scheduleWindowCovering(referenceTime)
		scheduleActiveNow = true
	case 2:
		schedule = scheduleWindowMissing(referenceTime)
	default:
		// no schedule (0)
	}

	as := &computev1alpha1.AutoscalingSpec{
		Enabled:      enabled,
		MaxReplicas:  maxReplicas,
		MinReplicas:  &minReplicas,
		IdleTimeout:  &metav1.Duration{Duration: idle},
		PollInterval: &metav1.Duration{Duration: poll},
		Schedule:     schedule,
	}

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    replicas,
		Autoscaling: as,
	}

	// status.LastActivityTime: nil or a stamp in (-2*idle, +0] of now.
	var status computev1alpha1.FireboltEngineStatus
	if rapid.Bool().Draw(t, "hasLastActivity") {
		ageSec := rapid.IntRange(0, int(2*idle.Seconds())).Draw(t, "lastActivityAgeSec")
		stamp := metav1.NewTime(referenceTime.Add(-time.Duration(ageSec) * time.Second))
		status.LastActivityTime = &stamp
	}

	active := int64(rapid.IntRange(0, 100).Draw(t, "activeQueries"))
	scrapeFailed := rapid.Bool().Draw(t, "scrapeFailed")
	var wakeAt *time.Time
	wakeFresh := false
	if rapid.Bool().Draw(t, "hasWakeStamp") {
		// Age either inside the TTL (fresh) or outside (stale). 0..2*TTL
		// covers both deterministically.
		ageSec := rapid.IntRange(0, int(2*DefaultAutoscalerWakeTTL.Seconds())).Draw(t, "wakeAgeSec")
		stamp := referenceTime.Add(-time.Duration(ageSec) * time.Second)
		wakeAt = &stamp
		wakeFresh = referenceTime.Sub(stamp) < DefaultAutoscalerWakeTTL
	}

	obs := AutoscalerObservation{
		ActiveQueries:   active,
		ScrapeFailed:    scrapeFailed,
		WakeRequestedAt: wakeAt,
	}

	return autoscalerInput{
		spec: spec, status: &status, obs: obs,
		autoscalingEnabled: enabled,
		minReplicas:        minReplicas,
		maxReplicas:        maxReplicas,
		idleTimeout:        idle,
		pollInterval:       poll,
		scheduleActiveNow:  scheduleActiveNow,
		wakeFresh:          wakeFresh,
	}
}

// scheduleWindowCovering returns a single window guaranteed to include
// `now` (which is always referenceTime, UTC). Days-of-week and
// start/end times are pinned around the reference timestamp; the window
// is wide enough that small implementation tweaks (e.g. inclusive vs.
// exclusive end) don't make the test flaky.
func scheduleWindowCovering(now time.Time) []computev1alpha1.ScheduleWindow {
	startHour := now.Hour() - 1
	endHour := now.Hour() + 1
	if startHour < 0 {
		startHour = 0
	}
	if endHour > 23 {
		endHour = 23
	}
	return []computev1alpha1.ScheduleWindow{{
		Days:  []computev1alpha1.ScheduleDay{shortWeekday(now.Weekday())},
		Start: formatHM(startHour, 0),
		End:   formatHM(endHour, 0),
	}}
}

// scheduleWindowMissing returns a single window guaranteed NOT to
// include `now`. Same shape as scheduleWindowCovering but on a
// different weekday so the window covers no overlap with the reference
// time.
func scheduleWindowMissing(now time.Time) []computev1alpha1.ScheduleWindow {
	otherDay := now.AddDate(0, 0, -1).Weekday()
	return []computev1alpha1.ScheduleWindow{{
		Days:  []computev1alpha1.ScheduleDay{shortWeekday(otherDay)},
		Start: "00:00",
		End:   "23:59",
	}}
}

// shortWeekday maps a Go time.Weekday into the 3-letter code
// ScheduleDay accepts (Mon, Tue, Wed, Thu, Fri, Sat, Sun).
func shortWeekday(w time.Weekday) computev1alpha1.ScheduleDay {
	codes := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	return computev1alpha1.ScheduleDay(codes[w])
}

func formatHM(h, m int) string {
	return pad2(h) + ":" + pad2(m)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + itoaSmall(n)
	}
	return itoaSmall(n)
}

// itoaSmall is a tiny non-allocating int→string for 0..99. The test
// rig pins hours and minutes so the general strconv.Itoa allocator
// would be overkill; the rapid shrinker prefers cheap helpers because
// every shrink retraces the whole property graph.
func itoaSmall(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return string(digits[n])
	}
	return string(digits[n/10]) + string(digits[n%10])
}

// ---------------------------------------------------------------------
// Property assertions
// ---------------------------------------------------------------------

// assertAutoscalerWellFormed checks the structural invariants every
// decision must satisfy regardless of input: non-negative replicas,
// ScaleAction matches DesiredReplicas-vs-current, recognized Reason.
func assertAutoscalerWellFormed(t *rapid.T, in autoscalerInput, dec AutoscalerDecision) {
	if dec.DesiredReplicas < 0 {
		t.Fatalf("DesiredReplicas = %d, want >= 0", dec.DesiredReplicas)
	}
	wantScale := dec.DesiredReplicas != in.spec.Replicas
	if dec.ScaleAction != wantScale {
		t.Fatalf("ScaleAction = %v, want %v (DesiredReplicas=%d, current=%d)",
			dec.ScaleAction, wantScale, dec.DesiredReplicas, in.spec.Replicas)
	}
	if !isKnownAutoscalerReason(dec.Reason) {
		t.Fatalf("Reason = %q is not in the known reason set", dec.Reason)
	}
}

// isKnownAutoscalerReason guards against silently introducing a new
// reason token without updating consumers (status surfacing, metrics,
// docs). Every reason the production code can emit must appear here.
func isKnownAutoscalerReason(r string) bool {
	switch r {
	case AutoscalerReasonDisabled,
		AutoscalerReasonScheduleActive,
		AutoscalerReasonStopped,
		AutoscalerReasonActivity,
		AutoscalerReasonScrapeFailed,
		AutoscalerReasonIdle,
		AutoscalerReasonWakeRequested,
		AutoscalerReasonInitializing:
		return true
	}
	return false
}

// assertAutoscalerPrecedence checks the precedence rules listed in
// computeAutoscalerDecision's docstring. Each rule is gated on the
// preconditions that make it the winning branch, so the assertion is
// vacuous when the winning rule is elsewhere; the rapid harness
// reaches every branch over enough draws.
func assertAutoscalerPrecedence(t *rapid.T, in autoscalerInput, dec AutoscalerDecision) {
	if !in.autoscalingEnabled {
		// Rule 1: disabled.
		if dec.Reason != AutoscalerReasonDisabled {
			t.Fatalf("autoscaling disabled, Reason = %q, want %q", dec.Reason, AutoscalerReasonDisabled)
		}
		if dec.DesiredReplicas != in.spec.Replicas {
			t.Fatalf("autoscaling disabled, DesiredReplicas = %d, want unchanged %d",
				dec.DesiredReplicas, in.spec.Replicas)
		}
		return
	}

	// Rule 2: wake stamp wins over everything except disabled.
	if in.wakeFresh {
		if dec.Reason != AutoscalerReasonWakeRequested {
			t.Fatalf("fresh WakeRequestedAt, Reason = %q, want %q", dec.Reason, AutoscalerReasonWakeRequested)
		}
		if dec.DesiredReplicas != in.maxReplicas {
			t.Fatalf("fresh WakeRequestedAt, DesiredReplicas = %d, want %d",
				dec.DesiredReplicas, in.maxReplicas)
		}
		return
	}

	// Rule 3: schedule active wins over idle / stopped.
	if in.scheduleActiveNow {
		if dec.Reason != AutoscalerReasonScheduleActive {
			t.Fatalf("scheduleActive, Reason = %q, want %q", dec.Reason, AutoscalerReasonScheduleActive)
		}
		if dec.DesiredReplicas != in.maxReplicas {
			t.Fatalf("scheduleActive, DesiredReplicas = %d, want %d",
				dec.DesiredReplicas, in.maxReplicas)
		}
		return
	}

	// Rule 4: stopped engine stays stopped when no schedule / wake.
	if in.spec.Replicas == 0 {
		if dec.Reason != AutoscalerReasonStopped {
			t.Fatalf("spec.Replicas==0, Reason = %q, want %q", dec.Reason, AutoscalerReasonStopped)
		}
		if dec.DesiredReplicas != 0 {
			t.Fatalf("spec.Replicas==0, DesiredReplicas = %d, want 0", dec.DesiredReplicas)
		}
		return
	}

	// Rule 5: scrape failed refreshes lastActivity AND keeps replicas.
	if in.obs.ScrapeFailed {
		if dec.Reason != AutoscalerReasonScrapeFailed {
			t.Fatalf("scrapeFailed, Reason = %q, want %q", dec.Reason, AutoscalerReasonScrapeFailed)
		}
		if dec.DesiredReplicas != in.spec.Replicas {
			t.Fatalf("scrapeFailed, DesiredReplicas = %d, want unchanged %d",
				dec.DesiredReplicas, in.spec.Replicas)
		}
		if dec.NewLastActivityTime == nil || !dec.NewLastActivityTime.Time.Equal(referenceTime) {
			t.Fatalf("scrapeFailed, NewLastActivityTime = %+v, want now (%v)",
				dec.NewLastActivityTime, referenceTime)
		}
		return
	}

	// Rule 6: active queries refresh lastActivity AND keep replicas.
	if in.obs.ActiveQueries > 0 {
		if dec.Reason != AutoscalerReasonActivity {
			t.Fatalf("ActiveQueries>0, Reason = %q, want %q", dec.Reason, AutoscalerReasonActivity)
		}
		if dec.DesiredReplicas != in.spec.Replicas {
			t.Fatalf("ActiveQueries>0, DesiredReplicas = %d, want unchanged %d",
				dec.DesiredReplicas, in.spec.Replicas)
		}
		if dec.NewLastActivityTime == nil || !dec.NewLastActivityTime.Time.Equal(referenceTime) {
			t.Fatalf("ActiveQueries>0, NewLastActivityTime = %+v, want now",
				dec.NewLastActivityTime)
		}
		return
	}

	// Quiet observation past this point. Two sub-cases:

	// Rule 7: first quiet observation initializes anchor.
	if in.status.LastActivityTime == nil {
		if dec.Reason != AutoscalerReasonInitializing {
			t.Fatalf("first quiet obs, Reason = %q, want %q", dec.Reason, AutoscalerReasonInitializing)
		}
		if dec.DesiredReplicas != in.spec.Replicas {
			t.Fatalf("first quiet obs, DesiredReplicas = %d, want unchanged", dec.DesiredReplicas)
		}
		if dec.NewLastActivityTime == nil || !dec.NewLastActivityTime.Time.Equal(referenceTime) {
			t.Fatalf("first quiet obs, NewLastActivityTime = %+v, want now",
				dec.NewLastActivityTime)
		}
		return
	}

	// Rule 8: idle-timeout reached AND above floor -> scale down.
	idleFor := referenceTime.Sub(in.status.LastActivityTime.Time)
	if idleFor >= in.idleTimeout && in.spec.Replicas > in.minReplicas {
		if dec.Reason != AutoscalerReasonIdle {
			t.Fatalf("idleFor=%s >= IdleTimeout=%s and replicas=%d > min=%d, Reason = %q, want %q",
				idleFor, in.idleTimeout, in.spec.Replicas, in.minReplicas,
				dec.Reason, AutoscalerReasonIdle)
		}
		if dec.DesiredReplicas != in.minReplicas {
			t.Fatalf("idle scale-down, DesiredReplicas = %d, want MinReplicas=%d",
				dec.DesiredReplicas, in.minReplicas)
		}
		return
	}

	// Rule 9: not yet idle OR already at floor -> no scale, reason
	// Activity (the production code reports "Activity" as a stable
	// resting reason when nothing else applies).
	if dec.Reason != AutoscalerReasonActivity {
		t.Fatalf("quiet-but-not-idle, Reason = %q, want %q", dec.Reason, AutoscalerReasonActivity)
	}
	if dec.DesiredReplicas != in.spec.Replicas {
		t.Fatalf("quiet-but-not-idle, DesiredReplicas = %d, want unchanged", dec.DesiredReplicas)
	}
}
