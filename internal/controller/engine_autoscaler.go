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
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// Autoscaler defaults applied when the corresponding spec fields are unset.
// Mirroring the kubebuilder defaults here keeps unit tests that build specs
// as Go literals consistent with CRD-loaded specs.
const (
	DefaultAutoscalerIdleTimeout  = 30 * time.Minute
	DefaultAutoscalerPollInterval = 1 * time.Minute
	DefaultAutoscalerMinReplicas  = int32(0)
)

// Autoscaler reason tokens written to status.autoscalerReason. Constants
// are defined in one place so reconciler code, tests, and documentation
// share the exact spelling.
const (
	AutoscalerReasonDisabled       = "Disabled"
	AutoscalerReasonScheduleActive = "ScheduleActive"
	AutoscalerReasonStopped        = "Stopped"
	AutoscalerReasonActivity       = "ActivityObserved"
	AutoscalerReasonScrapeFailed   = "ScrapeFailed"
	AutoscalerReasonIdle           = "Idle"
)

// AutoscalerObservation is the runtime input the autoscaler consumes each
// cycle, sourced from a metric scrape over the active generation's pods.
type AutoscalerObservation struct {
	// ActiveQueries is the sum of firebolt_running_queries +
	// firebolt_suspended_queries across the active generation. Set to 0
	// when the engine has zero replicas (no pods to scrape).
	ActiveQueries int64

	// ScrapeFailed indicates the metric scrape itself failed (network
	// error, missing metric, RBAC failure). When true the autoscaler
	// conservatively treats this as "activity observed" so a broken probe
	// never trips an unintended scale-down.
	ScrapeFailed bool
}

// AutoscalerDecision is the output of computeAutoscalerDecision and is
// applied by the reconciler: DesiredReplicas may be patched onto
// spec.replicas, NewLastActivityTime may be written to status.
type AutoscalerDecision struct {
	// DesiredReplicas is the value spec.replicas should converge to. Equal
	// to the current replica count when no scale change is needed.
	DesiredReplicas int32

	// ScaleAction is true when DesiredReplicas != current spec.replicas.
	ScaleAction bool

	// Reason is the token written to status.autoscalerReason.
	Reason string

	// RequeueAfter is the suggested delay before the next autoscaler
	// evaluation. Zero means "use the controller's default requeue".
	RequeueAfter time.Duration

	// NewLastActivityTime, when non-nil, is the value to write to
	// status.lastActivityTime. Nil means leave the existing value alone.
	NewLastActivityTime *metav1.Time
}

// computeAutoscalerDecision is the pure decision function. It is independent
// of the K8s client and of the wall clock so tests drive it with synthetic
// observations and timestamps.
//
// Precedence (intentional, top-down):
//
//  1. Autoscaling disabled or unset → no decision (DesiredReplicas left at
//     spec.Replicas, Reason=Disabled).
//  2. A Schedule window is active → pin replicas at MaxReplicas regardless
//     of activity. Schedule wins over both idle and stopped paths so an
//     "always-on during business hours" policy can wake a parked engine.
//  3. Engine is stopped (replicas=0) and no schedule window is active →
//     no scale change. Wake-up via gateway is handled separately in
//     commit 3 by an annotation read; the autoscaler's job here is to
//     stay out of the way.
//  4. Scrape failed or activity observed → refresh LastActivityTime, do
//     not scale. ScrapeFailed is grouped with Activity intentionally:
//     a broken probe must never look like quiet enough to scale down.
//  5. Quiet for >= IdleTimeout and replicas > MinReplicas → scale down
//     to MinReplicas.
//  6. Otherwise: no change, but anchor LastActivityTime on the first
//     quiet observation so the idle clock starts ticking from a known
//     point (a fresh engine gets one full IdleTimeout of grace before
//     its first scale-down).
func computeAutoscalerDecision(
	spec *computev1alpha1.FireboltEngineSpec,
	status *computev1alpha1.FireboltEngineStatus,
	obs AutoscalerObservation,
	now time.Time,
) AutoscalerDecision {
	if spec.Autoscaling == nil || !spec.Autoscaling.Enabled {
		return AutoscalerDecision{
			DesiredReplicas: spec.Replicas,
			Reason:          AutoscalerReasonDisabled,
		}
	}

	as := spec.Autoscaling
	pollInterval := DefaultAutoscalerPollInterval
	if as.PollInterval != nil && as.PollInterval.Duration > 0 {
		pollInterval = as.PollInterval.Duration
	}
	minReplicas := DefaultAutoscalerMinReplicas
	if as.MinReplicas != nil {
		minReplicas = *as.MinReplicas
	}

	if scheduleActive(as.Schedule, now) {
		return decisionWithScale(spec.Replicas, as.MaxReplicas, AutoscalerReasonScheduleActive, pollInterval)
	}

	if spec.Replicas == 0 {
		return AutoscalerDecision{
			DesiredReplicas: 0,
			Reason:          AutoscalerReasonStopped,
			RequeueAfter:    pollInterval,
		}
	}

	if obs.ScrapeFailed {
		// Refresh LastActivityTime alongside the no-scale decision: a
		// scrape-failure window must look just as un-idle to the next
		// successful poll as a window full of activity would. Without
		// this, T0 stamps lastActivity, scrapes fail for >IdleTimeout,
		// the first quiet success then computes idleFor from T0 and
		// scales down on a single observation that had no preceding
		// reliable signal. Stamping here makes the safety guarantee
		// hold across the whole failure window, not just within a
		// single decision.
		nowMeta := metav1.NewTime(now)
		return AutoscalerDecision{
			DesiredReplicas:     spec.Replicas,
			Reason:              AutoscalerReasonScrapeFailed,
			RequeueAfter:        pollInterval,
			NewLastActivityTime: &nowMeta,
		}
	}

	if obs.ActiveQueries > 0 {
		nowMeta := metav1.NewTime(now)
		return AutoscalerDecision{
			DesiredReplicas:     spec.Replicas,
			Reason:              AutoscalerReasonActivity,
			RequeueAfter:        pollInterval,
			NewLastActivityTime: &nowMeta,
		}
	}

	idleTimeout := DefaultAutoscalerIdleTimeout
	if as.IdleTimeout != nil && as.IdleTimeout.Duration > 0 {
		idleTimeout = as.IdleTimeout.Duration
	}

	if status.LastActivityTime == nil {
		nowMeta := metav1.NewTime(now)
		return AutoscalerDecision{
			DesiredReplicas:     spec.Replicas,
			Reason:              AutoscalerReasonActivity,
			RequeueAfter:        pollInterval,
			NewLastActivityTime: &nowMeta,
		}
	}

	idleFor := now.Sub(status.LastActivityTime.Time)
	if idleFor >= idleTimeout && spec.Replicas > minReplicas {
		return decisionWithScale(spec.Replicas, minReplicas, AutoscalerReasonIdle, pollInterval)
	}

	return AutoscalerDecision{
		DesiredReplicas: spec.Replicas,
		Reason:          AutoscalerReasonActivity,
		RequeueAfter:    pollInterval,
	}
}

func decisionWithScale(current, desired int32, reason string, requeue time.Duration) AutoscalerDecision {
	return AutoscalerDecision{
		DesiredReplicas: desired,
		ScaleAction:     desired != current,
		Reason:          reason,
		RequeueAfter:    requeue,
	}
}

// scheduleActive reports whether `now` falls inside any of the configured
// always-on windows, evaluated in UTC. An empty window list returns false.
//
// For midnight-crossing windows (End < Start) the post-midnight tail is
// anchored to the day on which Start fell, NOT the wall-clock day at `now`.
// e.g., a window {Start: "22:00", End: "02:00", Days: ["Wed"]} matches Thu
// 01:00 UTC because that minute belongs to Wednesday's window even though
// the calendar weekday at `now` reads Thu. The doc on ScheduleWindow.Days
// is the contract this implements.
func scheduleActive(windows []computev1alpha1.ScheduleWindow, now time.Time) bool {
	if len(windows) == 0 {
		return false
	}
	utc := now.UTC()
	today := weekdayCode(utc.Weekday())
	yesterday := weekdayCode(utc.Add(-24 * time.Hour).Weekday())
	minute := utc.Hour()*60 + utc.Minute()
	for _, w := range windows {
		startMin, ok := parseHHMM(w.Start)
		if !ok {
			continue
		}
		endMin, ok := parseHHMM(w.End)
		if !ok {
			continue
		}
		if !windowContains(startMin, endMin, minute) {
			continue
		}
		// Resolve the window's anchor day. Crosses midnight AND we're in
		// the post-midnight tail (minute < endMin) → window started
		// yesterday; otherwise → window started today.
		anchorDay := today
		if endMin < startMin && minute < endMin {
			anchorDay = yesterday
		}
		if dayMatches(w.Days, anchorDay) {
			return true
		}
	}
	return false
}

func dayMatches(days []computev1alpha1.ScheduleDay, today computev1alpha1.ScheduleDay) bool {
	if len(days) == 0 {
		return true
	}
	for _, d := range days {
		if d == today {
			return true
		}
	}
	return false
}

func weekdayCode(d time.Weekday) computev1alpha1.ScheduleDay {
	switch d {
	case time.Monday:
		return "Mon"
	case time.Tuesday:
		return "Tue"
	case time.Wednesday:
		return "Wed"
	case time.Thursday:
		return "Thu"
	case time.Friday:
		return "Fri"
	case time.Saturday:
		return "Sat"
	case time.Sunday:
		return "Sun"
	}
	return ""
}

// parseHHMM parses an "HH:MM" string into a minute-of-day offset (0-1439).
// Returns false on any structural malformity; the CRD pattern catches this
// at admission, this is a defense-in-depth parse for tests and edge cases.
func parseHHMM(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for i, c := range s {
		if i == 2 {
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// windowContains reports whether minute (0-1439) is inside [start, end). When
// end < start the window is treated as crossing midnight: it is inside on
// either side of 00:00. start == end is an empty window (returns false) so
// the user cannot accidentally configure a 24h pin via a degenerate window.
func windowContains(start, end, minute int) bool {
	if start == end {
		return false
	}
	if start < end {
		return minute >= start && minute < end
	}
	return minute >= start || minute < end
}

// autoscalerStepResult is what runAutoscaler reports back to Reconcile.
type autoscalerStepResult struct {
	// Decision is the raw decision the pure function produced; useful for
	// tests asserting on Reason without re-deriving it from status.
	Decision AutoscalerDecision
	// Patched is true when this step mutated spec.replicas. The caller
	// should expect a follow-up reconcile from the FireboltEngine watch.
	Patched bool
	// RequeueAfter is the suggested follow-up requeue delay; the caller
	// merges it with the main reconcile's RequeueAfter (smallest wins).
	RequeueAfter time.Duration
}

// runAutoscaler is the runtime entry point: it scrapes activity metrics,
// invokes computeAutoscalerDecision, and applies the decision to the
// cluster. Returns a no-op result when autoscaling is disabled or the
// engine is mid-rollout.
//
// Why only PhaseStable / PhaseStopped: scaling decisions during a
// transition would race with the blue-green flow. Patching spec.replicas
// while computeCreating is waiting for pods to come up would either
// abandon the in-flight generation (spec drift triggers a fresh bump in
// computeCreating) or, worse, cause the autoscaler and the rollout to
// fight over the same field. Letting transitions complete first keeps
// the state machine deterministic.
func (r *FireboltEngineReconciler) runAutoscaler(
	ctx context.Context,
	engine *computev1alpha1.FireboltEngine,
) (autoscalerStepResult, error) {
	if engine.Spec.Autoscaling == nil || !engine.Spec.Autoscaling.Enabled {
		// Autoscaler is off. Clear stale autoscaler-driven fields from a
		// previous active cycle so audit tooling never sees values that
		// no longer correspond to a running autoscaler. AutoscaledAt is
		// left untouched: the doc treats it as historical audit metadata.
		// LastActivityTime is cleared per its API doc contract.
		statusDirty := false
		if engine.Status.AutoscalerReason != AutoscalerReasonDisabled {
			engine.Status.AutoscalerReason = AutoscalerReasonDisabled
			statusDirty = true
		}
		if engine.Status.LastActivityTime != nil {
			engine.Status.LastActivityTime = nil
			statusDirty = true
		}
		if statusDirty {
			if err := r.updateStatus(ctx, engine); err != nil {
				return autoscalerStepResult{}, fmt.Errorf("autoscaler: clearing stale status: %w", err)
			}
		}
		return autoscalerStepResult{}, nil
	}
	if engine.Status.Phase != computev1alpha1.PhaseStable &&
		engine.Status.Phase != computev1alpha1.PhaseStopped {
		return autoscalerStepResult{}, nil
	}

	log := logf.FromContext(ctx).WithValues("engine", engine.Name, "component", "autoscaler")

	obs := AutoscalerObservation{}
	if engine.Spec.Replicas > 0 {
		active, failed := r.scrapeActiveQueries(ctx, engine)
		obs.ActiveQueries = active
		obs.ScrapeFailed = failed
	}

	decision := computeAutoscalerDecision(&engine.Spec, &engine.Status, obs, time.Now())

	result := autoscalerStepResult{
		Decision:     decision,
		RequeueAfter: decision.RequeueAfter,
	}

	// Order matters: r.Update writes the spec subresource and then
	// deserializes the API server's response back into the engine
	// pointer. The response carries the previously-stored Status, so
	// any in-memory Status fields set BEFORE r.Update would be silently
	// clobbered. Always do the spec write first, THEN apply autoscaler
	// status mutations, THEN r.updateStatus to persist them.
	if decision.ScaleAction {
		log.Info("Autoscaler scaling spec.replicas",
			"from", engine.Spec.Replicas,
			"to", decision.DesiredReplicas,
			"reason", decision.Reason,
		)
		engine.Spec.Replicas = decision.DesiredReplicas
		if err := r.Update(ctx, engine); err != nil {
			return result, fmt.Errorf("autoscaler: failed to patch spec.replicas: %w", err)
		}
		result.Patched = true
	}

	statusDirty := false
	if engine.Status.AutoscalerReason != decision.Reason {
		engine.Status.AutoscalerReason = decision.Reason
		statusDirty = true
	}
	if decision.NewLastActivityTime != nil {
		engine.Status.LastActivityTime = decision.NewLastActivityTime
		statusDirty = true
	}
	if decision.ScaleAction {
		nowMeta := metav1.Now()
		engine.Status.AutoscaledAt = &nowMeta
		statusDirty = true
	}

	if statusDirty {
		if err := r.updateStatus(ctx, engine); err != nil {
			return result, fmt.Errorf("autoscaler: failed to update status: %w", err)
		}
	}

	return result, nil
}

// scrapeActiveQueries sums firebolt_running_queries + firebolt_suspended_queries
// across the active generation's running pods. Returns (sum, scrapeFailed):
// scrapeFailed=true means the result is unreliable and the autoscaler should
// treat this poll as "activity observed" rather than scaling down.
//
// We treat "spec.replicas > 0 but no running pods yet" as scrapeFailed for the
// same reason: a half-rolled generation must not be misread as quiet.
func (r *FireboltEngineReconciler) scrapeActiveQueries(
	ctx context.Context,
	engine *computev1alpha1.FireboltEngine,
) (int64, bool) {
	log := logf.FromContext(ctx).WithValues("engine", engine.Name, "component", "autoscaler")
	if engine.Status.ActiveGeneration < 0 {
		return 0, true
	}

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(engine.Namespace),
		client.MatchingLabels{
			LabelEngine:     engine.Name,
			LabelGeneration: strconv.Itoa(engine.Status.ActiveGeneration),
		}); err != nil {
		log.Info("List pods failed, treating poll as activity",
			"error", err.Error())
		return 0, true
	}

	var total int64
	sawRunning := false
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		sawRunning = true
		n, err := r.scrapePodActiveQueries(ctx, pod)
		if err != nil {
			log.Info("Pod scrape failed, treating poll as activity",
				"pod", pod.Name, "error", err.Error())
			return 0, true
		}
		total += n
	}
	if !sawRunning {
		return 0, true
	}
	return total, false
}

// scrapePodActiveQueries fetches firebolt_running_queries +
// firebolt_suspended_queries from a single pod via the API server's
// /proxy subresource. Mirrors isPodDrained so the autoscaler shares the
// drain check's RBAC and reachability story.
func (r *FireboltEngineReconciler) scrapePodActiveQueries(ctx context.Context, pod *corev1.Pod) (int64, error) {
	if r.Clientset == nil {
		return 0, errors.New("clientset not initialized")
	}

	raw, err := r.Clientset.CoreV1().RESTClient().Get().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(fmt.Sprintf("%s:%d", pod.Name, MetricsPort)).
		SubResource("proxy").
		Suffix(MetricsPath).
		DoRaw(ctx)
	if err != nil {
		return 0, fmt.Errorf("scraping metrics from pod %s: %w", pod.Name, err)
	}

	running, runningOK := parsePrometheusGauge(raw, MetricRunningQueries)
	suspended, suspendedOK := parsePrometheusGauge(raw, MetricSuspendedQueries)
	if !runningOK || !suspendedOK {
		return 0, fmt.Errorf("activity metrics missing from pod %s (running=%t suspended=%t)",
			pod.Name, runningOK, suspendedOK)
	}
	return running + suspended, nil
}
