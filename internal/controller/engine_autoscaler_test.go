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
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// fixedNow returns a deterministic UTC instant used by the autoscaler tests.
// Wednesday 2026-04-29 13:00 UTC, so day-of-week tests aren't ambiguous.
func fixedNow() time.Time {
	return time.Date(2026, time.April, 29, 13, 0, 0, 0, time.UTC)
}

func ptr[T any](v T) *T { return &v }

func enabledAutoscalingSpec() *computev1alpha1.AutoscalingSpec {
	return &computev1alpha1.AutoscalingSpec{
		Enabled:      true,
		MaxReplicas:  3,
		MinReplicas:  ptr(int32(0)),
		IdleTimeout:  &metav1.Duration{Duration: 30 * time.Minute},
		PollInterval: &metav1.Duration{Duration: time.Minute},
	}
}

func TestComputeAutoscalerDecision_DisabledIsNoOp(t *testing.T) {
	t.Parallel()
	spec := &computev1alpha1.FireboltEngineSpec{Replicas: 3}
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{}, AutoscalerObservation{}, fixedNow())
	if d.ScaleAction {
		t.Fatalf("expected no scale action, got DesiredReplicas=%d", d.DesiredReplicas)
	}
	if d.Reason != AutoscalerReasonDisabled {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonDisabled, d.Reason)
	}
	if d.NewLastActivityTime != nil {
		t.Fatal("disabled autoscaler must not stamp activity")
	}
}

func TestComputeAutoscalerDecision_ScheduleWinsOverIdleAndStopped(t *testing.T) {
	t.Parallel()

	now := fixedNow() // 13:00 UTC Wednesday
	as := enabledAutoscalingSpec()
	as.Schedule = []computev1alpha1.ScheduleWindow{
		{Start: "08:00", End: "18:00"}, // every day
	}

	cases := []struct {
		name       string
		replicas   int32
		wantScale  bool
		wantTarget int32
	}{
		{"stopped engine wakes via schedule", 0, true, 3},
		{"running engine pinned at max", 2, true, 3},
		{"already at max stays put", 3, false, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := &computev1alpha1.FireboltEngineSpec{
				Replicas:    tc.replicas,
				Autoscaling: as,
			}
			d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{}, AutoscalerObservation{}, now)
			if d.Reason != AutoscalerReasonScheduleActive {
				t.Fatalf("reason: want %q got %q", AutoscalerReasonScheduleActive, d.Reason)
			}
			if d.DesiredReplicas != tc.wantTarget {
				t.Fatalf("desired: want %d got %d", tc.wantTarget, d.DesiredReplicas)
			}
			if d.ScaleAction != tc.wantScale {
				t.Fatalf("scale action: want %v got %v", tc.wantScale, d.ScaleAction)
			}
		})
	}
}

func TestComputeAutoscalerDecision_StoppedHonorsScheduleDays(t *testing.T) {
	t.Parallel()

	// Wednesday 13:00 UTC; window is Mon-Fri 08:00-18:00 → matches.
	as := enabledAutoscalingSpec()
	as.Schedule = []computev1alpha1.ScheduleWindow{
		{Start: "08:00", End: "18:00", Days: []computev1alpha1.ScheduleDay{"Mon", "Tue", "Wed", "Thu", "Fri"}},
	}
	spec := &computev1alpha1.FireboltEngineSpec{Replicas: 0, Autoscaling: as}
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{}, AutoscalerObservation{}, fixedNow())
	if d.Reason != AutoscalerReasonScheduleActive || d.DesiredReplicas != 3 {
		t.Fatalf("Wednesday weekday window did not match: %+v", d)
	}

	// Restrict to weekend only — Wednesday should fall through to Stopped.
	as.Schedule[0].Days = []computev1alpha1.ScheduleDay{"Sat", "Sun"}
	d = computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{}, AutoscalerObservation{}, fixedNow())
	if d.Reason != AutoscalerReasonStopped || d.DesiredReplicas != 0 {
		t.Fatalf("weekend-only window must not match Wednesday: %+v", d)
	}
}

func TestComputeAutoscalerDecision_ActivityRefreshesLastActivity(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    3,
		Autoscaling: enabledAutoscalingSpec(),
	}
	priorActivity := metav1.NewTime(fixedNow().Add(-2 * time.Hour))
	status := &computev1alpha1.FireboltEngineStatus{LastActivityTime: &priorActivity}

	d := computeAutoscalerDecision(spec, status, AutoscalerObservation{ActiveQueries: 1}, fixedNow())

	if d.ScaleAction {
		t.Fatal("activity must not trigger a scale action")
	}
	if d.Reason != AutoscalerReasonActivity {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonActivity, d.Reason)
	}
	if d.NewLastActivityTime == nil {
		t.Fatal("expected activity to refresh LastActivityTime")
	}
	if !d.NewLastActivityTime.Time.Equal(fixedNow()) {
		t.Fatalf("LastActivityTime: want %v got %v", fixedNow(), d.NewLastActivityTime.Time)
	}
}

func TestComputeAutoscalerDecision_ScrapeFailedNeverScalesDown(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    3,
		Autoscaling: enabledAutoscalingSpec(),
	}
	// Stale lastActivity that would otherwise be far past idleTimeout.
	stale := metav1.NewTime(fixedNow().Add(-24 * time.Hour))
	status := &computev1alpha1.FireboltEngineStatus{LastActivityTime: &stale}

	d := computeAutoscalerDecision(spec, status, AutoscalerObservation{ScrapeFailed: true}, fixedNow())

	if d.ScaleAction {
		t.Fatal("scrape failure must not trigger scale-down")
	}
	if d.Reason != AutoscalerReasonScrapeFailed {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonScrapeFailed, d.Reason)
	}
	// Scrape failure must stamp LastActivityTime so a subsequent quiet
	// observation doesn't compute idleFor against pre-failure data and
	// scale down on a single sample. See multi-cycle test below.
	if d.NewLastActivityTime == nil || !d.NewLastActivityTime.Time.Equal(fixedNow()) {
		t.Fatalf("scrape failure must refresh LastActivityTime to now (got %v)", d.NewLastActivityTime)
	}
}

// TestComputeAutoscalerDecision_ExtendedScrapeFailureThenQuietHoldsScaleDown
// is the regression for a multi-cycle scenario the single-observation test
// above does not cover: activity at T0, scrape failures from T0+1m through
// T0+IdleTimeout+1m, then a successful quiet observation. Without
// LastActivityTime being refreshed during the failure window, idleFor would
// be computed from T0 and trigger an immediate scale-down on a single
// quiet sample that had no reliable preceding signal.
func TestComputeAutoscalerDecision_ExtendedScrapeFailureThenQuietHoldsScaleDown(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    3,
		Autoscaling: enabledAutoscalingSpec(), // IdleTimeout = 30m
	}
	t0 := time.Date(2026, time.April, 29, 12, 0, 0, 0, time.UTC)
	status := &computev1alpha1.FireboltEngineStatus{}

	// T0: activity observed, lastActivity stamped at T0.
	d := computeAutoscalerDecision(spec, status,
		AutoscalerObservation{ActiveQueries: 1}, t0)
	if d.NewLastActivityTime == nil {
		t.Fatal("expected activity at T0 to stamp lastActivity")
	}
	status.LastActivityTime = d.NewLastActivityTime

	// T0+1m..T0+45m: scrapes fail. None must scale down, all must
	// keep refreshing lastActivity so the quiet sample at T0+46m
	// computes idleFor from a recent timestamp, not from T0.
	for offset := time.Minute; offset <= 45*time.Minute; offset += time.Minute {
		d := computeAutoscalerDecision(spec, status,
			AutoscalerObservation{ScrapeFailed: true}, t0.Add(offset))
		if d.ScaleAction {
			t.Fatalf("scale-down at +%v during scrape-failure window", offset)
		}
		if d.NewLastActivityTime == nil {
			t.Fatalf("at +%v: scrape failure must keep stamping lastActivity", offset)
		}
		status.LastActivityTime = d.NewLastActivityTime
	}

	// T0+46m: scrapes recover, returns 0 queries. Must not scale down
	// because lastActivity was kept fresh through the failure window.
	d = computeAutoscalerDecision(spec, status,
		AutoscalerObservation{}, t0.Add(46*time.Minute))
	if d.ScaleAction {
		t.Fatal("first quiet observation after long scrape-failure window must not scale down")
	}
}

func TestComputeAutoscalerDecision_FirstQuietObservationAnchorsLastActivity(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    3,
		Autoscaling: enabledAutoscalingSpec(),
	}
	// LastActivityTime nil → first quiet observation should not scale,
	// but should anchor the timestamp so the idle clock starts ticking.
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{}, AutoscalerObservation{}, fixedNow())

	if d.ScaleAction {
		t.Fatal("first quiet observation must not scale down")
	}
	if d.NewLastActivityTime == nil || !d.NewLastActivityTime.Time.Equal(fixedNow()) {
		t.Fatal("first quiet observation must anchor LastActivityTime to now")
	}
}

func TestComputeAutoscalerDecision_IdleScalesDownToMin(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    3,
		Autoscaling: enabledAutoscalingSpec(),
	}
	// idle for 1h, IdleTimeout=30m → must scale down.
	last := metav1.NewTime(fixedNow().Add(-1 * time.Hour))
	status := &computev1alpha1.FireboltEngineStatus{LastActivityTime: &last}

	d := computeAutoscalerDecision(spec, status, AutoscalerObservation{}, fixedNow())

	if !d.ScaleAction || d.DesiredReplicas != 0 {
		t.Fatalf("expected scale-down to 0, got DesiredReplicas=%d ScaleAction=%v", d.DesiredReplicas, d.ScaleAction)
	}
	if d.Reason != AutoscalerReasonIdle {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonIdle, d.Reason)
	}
	if d.NewLastActivityTime != nil {
		t.Fatal("scale-down must not refresh LastActivityTime")
	}
}

func TestComputeAutoscalerDecision_IdleScalesToCustomMin(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    5,
		Autoscaling: enabledAutoscalingSpec(),
	}
	spec.Autoscaling.MinReplicas = ptr(int32(1))

	last := metav1.NewTime(fixedNow().Add(-1 * time.Hour))
	status := &computev1alpha1.FireboltEngineStatus{LastActivityTime: &last}

	d := computeAutoscalerDecision(spec, status, AutoscalerObservation{}, fixedNow())

	if !d.ScaleAction || d.DesiredReplicas != 1 {
		t.Fatalf("expected scale-down to 1, got DesiredReplicas=%d ScaleAction=%v", d.DesiredReplicas, d.ScaleAction)
	}
}

func TestComputeAutoscalerDecision_IdleAtMinDoesNothing(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    1,
		Autoscaling: enabledAutoscalingSpec(),
	}
	spec.Autoscaling.MinReplicas = ptr(int32(1))

	last := metav1.NewTime(fixedNow().Add(-2 * time.Hour))
	status := &computev1alpha1.FireboltEngineStatus{LastActivityTime: &last}

	d := computeAutoscalerDecision(spec, status, AutoscalerObservation{}, fixedNow())

	if d.ScaleAction {
		t.Fatal("already at min: must not scale")
	}
}

func TestComputeAutoscalerDecision_FreshWakeRequestScalesToMax(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    0,
		Autoscaling: enabledAutoscalingSpec(),
	}
	wake := fixedNow().Add(-30 * time.Second)
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{},
		AutoscalerObservation{WakeRequestedAt: &wake}, fixedNow())

	if !d.ScaleAction || d.DesiredReplicas != 3 {
		t.Fatalf("fresh wake must scale to MaxReplicas: %+v", d)
	}
	if d.Reason != AutoscalerReasonWakeRequested {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonWakeRequested, d.Reason)
	}
}

func TestComputeAutoscalerDecision_StaleWakeRequestIgnored(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    0,
		Autoscaling: enabledAutoscalingSpec(),
	}
	stale := fixedNow().Add(-2 * DefaultAutoscalerWakeTTL)
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{},
		AutoscalerObservation{WakeRequestedAt: &stale}, fixedNow())

	if d.ScaleAction {
		t.Fatal("stale wake annotation must not trigger scale-up")
	}
	if d.Reason != AutoscalerReasonStopped {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonStopped, d.Reason)
	}
}

func TestComputeAutoscalerDecision_WakeIgnoredWhenAutoscalingDisabled(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{Replicas: 0}
	wake := fixedNow().Add(-1 * time.Second)
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{},
		AutoscalerObservation{WakeRequestedAt: &wake}, fixedNow())

	if d.ScaleAction {
		t.Fatal("wake must be ignored when autoscaling is disabled")
	}
	if d.Reason != AutoscalerReasonDisabled {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonDisabled, d.Reason)
	}
}

func TestParseWakeAnnotation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		annots    map[string]string
		expectNil bool
	}{
		{"nil map", nil, true},
		{"empty value", map[string]string{AnnotationWakeRequested: ""}, true},
		{"missing key", map[string]string{"unrelated": "x"}, true},
		{"valid RFC3339", map[string]string{AnnotationWakeRequested: "2026-04-30T12:00:00Z"}, false},
		{"malformed", map[string]string{AnnotationWakeRequested: "not-a-time"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseWakeAnnotation(tc.annots)
			if (got == nil) != tc.expectNil {
				t.Fatalf("parseWakeAnnotation(%v): got nil=%v want nil=%v", tc.annots, got == nil, tc.expectNil)
			}
		})
	}
}

func TestComputeAutoscalerDecision_StoppedNoSchedule(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas:    0,
		Autoscaling: enabledAutoscalingSpec(),
	}
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{}, AutoscalerObservation{}, fixedNow())
	if d.ScaleAction || d.DesiredReplicas != 0 {
		t.Fatalf("stopped engine without schedule must remain at 0: %+v", d)
	}
	if d.Reason != AutoscalerReasonStopped {
		t.Fatalf("reason: want %q got %q", AutoscalerReasonStopped, d.Reason)
	}
}

func TestComputeAutoscalerDecision_PollIntervalDefault(t *testing.T) {
	t.Parallel()

	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas: 3,
		Autoscaling: &computev1alpha1.AutoscalingSpec{
			Enabled:     true,
			MaxReplicas: 3,
			// PollInterval intentionally unset
		},
	}
	d := computeAutoscalerDecision(spec, &computev1alpha1.FireboltEngineStatus{}, AutoscalerObservation{ActiveQueries: 1}, fixedNow())
	if d.RequeueAfter != DefaultAutoscalerPollInterval {
		t.Fatalf("requeue: want %v got %v", DefaultAutoscalerPollInterval, d.RequeueAfter)
	}
}

func TestScheduleActive_CrossesMidnight(t *testing.T) {
	t.Parallel()

	windows := []computev1alpha1.ScheduleWindow{
		{Start: "22:00", End: "02:00"},
	}
	cases := []struct {
		hour, min int
		want      bool
	}{
		{21, 59, false},
		{22, 0, true},
		{23, 30, true},
		{0, 0, true},
		{1, 59, true},
		{2, 0, false},
		{12, 0, false},
	}
	base := time.Date(2026, time.April, 29, 0, 0, 0, 0, time.UTC)
	for _, tc := range cases {
		now := base.Add(time.Duration(tc.hour)*time.Hour + time.Duration(tc.min)*time.Minute)
		got := scheduleActive(windows, now)
		if got != tc.want {
			t.Errorf("at %02d:%02d: want %v got %v", tc.hour, tc.min, tc.want, got)
		}
	}
}

// TestScheduleActive_MidnightCrossWithDaysFilter pins the contract on
// ScheduleWindow.Days for windows that cross midnight: the post-midnight
// tail belongs to the day on which Start fell, NOT the wall-clock weekday.
// The previous implementation filtered against today only, so a window
// like {Start: "22:00", End: "02:00", Days: ["Wed"]} was silently dropped
// at Thu 01:00 UTC even though that minute is part of Wednesday's window.
func TestScheduleActive_MidnightCrossWithDaysFilter(t *testing.T) {
	t.Parallel()

	// Wed 22:00 → Thu 02:00, restricted to Wednesdays.
	windows := []computev1alpha1.ScheduleWindow{
		{Start: "22:00", End: "02:00", Days: []computev1alpha1.ScheduleDay{"Wed"}},
	}

	// 2026-04-29 is a Wednesday; 04-30 is Thursday; 05-01 is Friday.
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"Wed 21:59 — before window", time.Date(2026, time.April, 29, 21, 59, 0, 0, time.UTC), false},
		{"Wed 22:00 — start", time.Date(2026, time.April, 29, 22, 0, 0, 0, time.UTC), true},
		{"Wed 23:30 — leading half", time.Date(2026, time.April, 29, 23, 30, 0, 0, time.UTC), true},
		{"Thu 00:00 — trailing half (anchor=Wed)", time.Date(2026, time.April, 30, 0, 0, 0, 0, time.UTC), true},
		{"Thu 01:30 — trailing half (anchor=Wed)", time.Date(2026, time.April, 30, 1, 30, 0, 0, time.UTC), true},
		{"Thu 02:00 — end exclusive", time.Date(2026, time.April, 30, 2, 0, 0, 0, time.UTC), false},
		{"Thu 22:00 — wall clock matches but anchor=Thu", time.Date(2026, time.April, 30, 22, 0, 0, 0, time.UTC), false},
		{"Fri 01:30 — wrong-day trailing", time.Date(2026, time.May, 1, 1, 30, 0, 0, time.UTC), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scheduleActive(windows, tc.now); got != tc.want {
				t.Errorf("scheduleActive at %v: want %v got %v", tc.now, tc.want, got)
			}
		})
	}
}

func TestScheduleActive_EmptyWindowNeverActive(t *testing.T) {
	t.Parallel()
	windows := []computev1alpha1.ScheduleWindow{{Start: "12:00", End: "12:00"}}
	if scheduleActive(windows, fixedNow()) {
		t.Fatal("start==end window must never be active")
	}
}

func TestScheduleActive_MalformedTimesIgnored(t *testing.T) {
	t.Parallel()
	windows := []computev1alpha1.ScheduleWindow{
		{Start: "ab:cd", End: "12:00"}, // malformed start
		{Start: "12:00", End: "99:99"}, // malformed end
		{Start: "08:00", End: "18:00"}, // valid catch-all
	}
	// fixedNow is 13:00 — should match the third window.
	if !scheduleActive(windows, fixedNow()) {
		t.Fatal("expected last (valid) window to match")
	}
}

func TestParseHHMM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"00:00", 0, true},
		{"23:59", 23*60 + 59, true},
		{"08:30", 8*60 + 30, true},
		{"24:00", 0, false},
		{"12:60", 0, false},
		{"1:23", 0, false},
		{"12-34", 0, false},
		{"abcde", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseHHMM(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("parseHHMM(%q) = (%d, %v); want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestWeekdayCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   time.Weekday
		want computev1alpha1.ScheduleDay
	}{
		{time.Monday, "Mon"},
		{time.Tuesday, "Tue"},
		{time.Wednesday, "Wed"},
		{time.Thursday, "Thu"},
		{time.Friday, "Fri"},
		{time.Saturday, "Sat"},
		{time.Sunday, "Sun"},
	}
	for _, tc := range cases {
		if got := weekdayCode(tc.in); got != tc.want {
			t.Errorf("weekdayCode(%v) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestRunAutoscaler_DisabledClearsStaleStatus verifies the API doc contract on
// LastActivityTime ("Cleared when autoscaling is disabled"). When autoscaling
// is disabled, runAutoscaler must clear LastActivityTime, set
// AutoscalerReason="Disabled", and preserve AutoscaledAt (which is documented
// as audit trail metadata).
func TestRunAutoscaler_DisabledClearsStaleStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	stale := metav1.NewTime(fixedNow().Add(-time.Hour))
	scaledAt := metav1.NewTime(fixedNow().Add(-30 * time.Minute))

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ns"},
		Spec: computev1alpha1.FireboltEngineSpec{
			Replicas: 3,
			// Autoscaling deliberately nil — represents "feature disabled".
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:            computev1alpha1.PhaseStable,
			LastActivityTime: &stale,
			AutoscaledAt:     &scaledAt,
			AutoscalerReason: AutoscalerReasonActivity, // stale token from a prior active cycle
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(engine).
		WithStatusSubresource(engine).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	if _, err := r.runAutoscaler(context.Background(), engine); err != nil {
		t.Fatalf("runAutoscaler: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "eng", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.Status.AutoscalerReason != AutoscalerReasonDisabled {
		t.Errorf("AutoscalerReason: want %q got %q", AutoscalerReasonDisabled, got.Status.AutoscalerReason)
	}
	if got.Status.LastActivityTime != nil {
		t.Errorf("LastActivityTime: want nil, got %v", got.Status.LastActivityTime)
	}
	if got.Status.AutoscaledAt == nil || !got.Status.AutoscaledAt.Equal(&scaledAt) {
		t.Errorf("AutoscaledAt should be preserved as audit metadata, got %v", got.Status.AutoscaledAt)
	}
}

// TestRunAutoscaler_DisabledIsIdempotent verifies that calling runAutoscaler
// repeatedly when autoscaling is disabled and status is already in the
// "disabled, clean" state does not produce spurious status writes.
func TestRunAutoscaler_DisabledIsIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ns", ResourceVersion: "1"},
		Spec:       computev1alpha1.FireboltEngineSpec{Replicas: 3},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:            computev1alpha1.PhaseStable,
			AutoscalerReason: AutoscalerReasonDisabled,
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(engine).
		WithStatusSubresource(engine).
		Build()

	r := &FireboltEngineReconciler{Client: fc, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}

	if _, err := r.runAutoscaler(context.Background(), engine); err != nil {
		t.Fatalf("runAutoscaler: %v", err)
	}

	got := &computev1alpha1.FireboltEngine{}
	if err := fc.Get(context.Background(), types.NamespacedName{Name: "eng", Namespace: "ns"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ResourceVersion != "1" {
		t.Errorf("expected no status write (resourceVersion unchanged), got %q", got.ResourceVersion)
	}
}
