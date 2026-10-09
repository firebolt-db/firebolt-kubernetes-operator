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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/wakeagent"
)

func TestWakeSurvivesDemandCacheEviction(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	old := metav1.NewTime(now.Add(-time.Hour))
	engine := wakeHandoffEngine(now)
	scheme := wakeTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(engine).
		WithStatusSubresource(engine).Build()
	tracker := &WakeDemandTracker{Client: c}
	tracker.replace(map[demandKey]time.Time{{namespace: "ns", engine: "engine"}: now})
	r := &FireboltEngineReconciler{Client: c, Scheme: scheme,
		WakeDemand: tracker, MetricsRecorder: metrics.NoOpEngineRecorder{}}
	if _, err := r.runAutoStop(ctx, engine, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
		t.Fatal(err)
	}
	if engine.Spec.Replicas != 1 {
		t.Fatalf("engine did not wake: %+v", engine.Spec)
	}

	// The actual poller clears demand as soon as desired replicas become nonzero.
	tracker.pollOnce(ctx)
	if tracker.LastDemand("ns", "engine") != nil {
		t.Fatal("poller retained demand for a running engine")
	}

	// Startup can exceed the idle timeout while the original query is still held.
	// Read only persisted status, as a replacement operator would after a restart.
	readyAt := now.Add(time.Minute)
	idle := readyAt.Sub(old.Time)
	decision := decideAutoStopWithEngineIdle(&engine.Spec, engine.Spec.AutoStop,
		&engine.Status, AutoStopObservation{}, &idle, readyAt)
	if decision.ScaleAction || decision.DesiredReplicas != 1 {
		t.Fatalf("woken engine stopped before query handoff after demand cache eviction: %+v", decision)
	}
}

func wakeHandoffEngine(now time.Time) *computev1alpha1.FireboltEngine {
	old := metav1.NewTime(now.Add(-time.Hour))
	idleReplicas := int32(0)
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "engine", Namespace: "ns"},
		Spec: computev1alpha1.FireboltEngineSpec{AutoStop: &computev1alpha1.AutoStopSpec{
			Enabled: true, ActiveReplicas: 1, IdleReplicas: &idleReplicas,
			IdleTimeout: &metav1.Duration{Duration: 25 * time.Second},
		}},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase: computev1alpha1.PhaseStopped, LastActivityTime: &old,
		},
	}
}

func TestAcceptedWakeExpiresWithoutReplay(t *testing.T) {
	now := fixedNow()
	engine := wakeHandoffEngine(now)
	stamp := metav1.NewTime(now)
	engine.Status.LastWakeDemandTime = &stamp
	until := metav1.NewTime(stamp.Add(wakeProtectionDuration(engine.Spec.AutoStop.IdleTimeout.Duration)))
	engine.Status.WakeProtectionUntil = &until
	engine.Spec.Replicas = 1
	for _, age := range []time.Duration{time.Minute, wakeProtectionDuration(25*time.Second) - time.Second} {
		idle := time.Hour + age
		decision := decideAutoStopWithEngineIdle(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
			AutoStopObservation{WakeRequestedAt: &now}, &idle, now.Add(age))
		if decision.ScaleAction || decision.NewLastWakeDemandTime != nil {
			t.Fatalf("replayed wake changed replicas or renewed protection at %v: %+v", age, decision)
		}
	}
	expired := now.Add(wakeProtectionDuration(25 * time.Second))
	idle := time.Hour + wakeProtectionDuration(25*time.Second)
	decision := decideAutoStopWithEngineIdle(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
		AutoStopObservation{}, &idle, expired)
	if !decision.ScaleAction || decision.DesiredReplicas != 0 || decision.Reason != AutoStopReasonIdle {
		t.Fatalf("expired wake did not resume idle evaluation: %+v", decision)
	}
	engine.Spec.Replicas = 0
	decision = decideAutoStopWithEngineIdle(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
		AutoStopObservation{WakeRequestedAt: &now}, nil, expired.Add(time.Second))
	if decision.ScaleAction {
		t.Fatalf("old retained demand restarted an expired wake: %+v", decision)
	}
	fresh := expired.Add(2 * time.Second)
	decision = decideAutoStopWithEngineIdle(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
		AutoStopObservation{WakeRequestedAt: &fresh}, nil, fresh)
	if !decision.ScaleAction || decision.NewLastWakeDemandTime == nil || !decision.NewLastWakeDemandTime.Time.Equal(fresh) {
		t.Fatalf("new demand did not start another wake: %+v", decision)
	}
}

func TestAcceptedWakePreservesUserResize(t *testing.T) {
	now := fixedNow()
	engine := wakeHandoffEngine(now)
	stamp := metav1.NewTime(now)
	engine.Status.LastWakeDemandTime = &stamp
	until := metav1.NewTime(stamp.Add(wakeProtectionDuration(engine.Spec.AutoStop.IdleTimeout.Duration)))
	engine.Status.WakeProtectionUntil = &until
	engine.Spec.Replicas = 3
	decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
		AutoStopObservation{}, now.Add(time.Minute))
	if decision.ScaleAction || decision.DesiredReplicas != 3 {
		t.Fatalf("wake protection resized a running engine: %+v", decision)
	}
}

func TestWakeProtectionOutlastsGatewayHold(t *testing.T) {
	if wakeProtectionDuration(25*time.Second) <= wakeagent.DefaultHoldTimeout {
		t.Fatal("wake protection must outlast the gateway's hold deadline")
	}
}

func TestWakePersistenceBeforeScale(t *testing.T) {
	for _, failure := range []string{"status", "pruned status", "pruned deadline", "conflict", "spec"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			engine := wakeHandoffEngine(now)
			scheme := wakeTestScheme(t)
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(engine).
				WithStatusSubresource(engine).Build()
			injected := errors.New("injected write failure")
			c := interceptor.NewClient(base, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					if failure == "status" {
						return injected
					}
					if failure == "conflict" {
						fresh := &computev1alpha1.FireboltEngine{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(obj), fresh); err != nil {
							return err
						}
						fresh.Spec.Replicas = 3
						if err := c.Update(ctx, fresh); err != nil {
							return err
						}
					}
					if failure == "pruned deadline" {
						obj.(*computev1alpha1.FireboltEngine).Status.WakeProtectionUntil = nil
					}
					if failure == "pruned status" {
						obj.(*computev1alpha1.FireboltEngine).Status.LastWakeDemandTime = nil
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if failure == "spec" {
						return injected
					}
					return c.Update(ctx, obj, opts...)
				},
			})
			tracker := &WakeDemandTracker{}
			tracker.replace(map[demandKey]time.Time{{namespace: "ns", engine: "engine"}: now})
			r := &FireboltEngineReconciler{Client: c, Scheme: scheme, WakeDemand: tracker,
				MetricsRecorder: metrics.NoOpEngineRecorder{}}
			result, err := r.runAutoStop(ctx, engine, nil)
			if err == nil || result.Patched {
				t.Fatalf("failed write was accepted: result=%+v err=%v", result, err)
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
				t.Fatal(err)
			}
			wantReplicas := int32(0)
			if failure == "conflict" {
				wantReplicas = 3
			}
			if engine.Spec.Replicas != wantReplicas {
				t.Fatalf("failed acceptance changed replicas: got %d, want %d", engine.Spec.Replicas, wantReplicas)
			}
			if failure != "spec" {
				if failure != "pruned deadline" && engine.Status.LastWakeDemandTime != nil {
					t.Fatal("failed status write persisted demand")
				}
				return
			}
			// Restart after acceptance but before the replica write. No gateway cache survives.
			if engine.Status.LastWakeDemandTime == nil || engine.Status.WakeProtectionUntil == nil {
				t.Fatal("spec failure lost accepted demand")
			}
			restarted := &FireboltEngineReconciler{Client: base, Scheme: scheme, MetricsRecorder: metrics.NoOpEngineRecorder{}}
			if _, err := restarted.runAutoStop(ctx, engine, nil); err != nil {
				t.Fatal(err)
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
				t.Fatal(err)
			}
			if engine.Spec.Replicas != 1 {
				t.Fatal("restart lost the accepted wake")
			}
		})
	}
}

func TestFutureWakeCannotCreateProtection(t *testing.T) {
	now := fixedNow()
	engine := wakeHandoffEngine(now)
	future := now.Add(time.Hour)
	decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
		AutoStopObservation{WakeRequestedAt: &future}, now)
	if decision.ScaleAction || decision.NewLastWakeDemandTime != nil {
		t.Fatalf("future demand created an unbounded protection window: %+v", decision)
	}
}

func TestAcceptedWakeHonorsSchedule(t *testing.T) {
	now := referenceTime
	engine := wakeHandoffEngine(now)
	engine.Spec.Replicas = 1
	stamp := metav1.NewTime(now)
	engine.Status.LastWakeDemandTime = &stamp
	until := metav1.NewTime(stamp.Add(wakeProtectionDuration(engine.Spec.AutoStop.IdleTimeout.Duration)))
	engine.Status.WakeProtectionUntil = &until
	engine.Spec.AutoStop.ActiveReplicas = 3
	engine.Spec.AutoStop.Schedule = scheduleWindowCovering(now)
	decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status, AutoStopObservation{}, now.Add(time.Minute))
	if decision.DesiredReplicas != 3 || !decision.ScaleAction || decision.Reason != AutoStopReasonScheduleActive {
		t.Fatalf("wake protection suppressed scheduled capacity: %+v", decision)
	}
}

func TestAcceptedWakeRetainsActivityGrace(t *testing.T) {
	for _, failed := range []bool{true, false} {
		t.Run(fmt.Sprintf("scrapeFailed=%v", failed), func(t *testing.T) {
			now := referenceTime
			engine := wakeHandoffEngine(now)
			engine.Spec.Replicas = 1
			stamp := metav1.NewTime(now)
			engine.Status.LastWakeDemandTime = &stamp
			until := metav1.NewTime(stamp.Add(wakeProtectionDuration(engine.Spec.AutoStop.IdleTimeout.Duration)))
			engine.Status.WakeProtectionUntil = &until
			observedAt := now.Add(wakeProtectionDuration(25*time.Second) - time.Second)
			obs := AutoStopObservation{ScrapeFailed: failed}
			if !failed {
				obs.ActiveQueries = 1
			}
			decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status, obs, observedAt)
			if decision.NewLastActivityTime == nil || !decision.NewLastActivityTime.Time.Equal(observedAt) {
				t.Fatalf("wake protection suppressed activity grace: %+v", decision)
			}
			engine.Status.LastActivityTime = decision.NewLastActivityTime
			expiredAt := now.Add(wakeProtectionDuration(25 * time.Second))
			idle := wakeProtectionDuration(25 * time.Second)
			decision = decideAutoStopWithEngineIdle(&engine.Spec, engine.Spec.AutoStop, &engine.Status, AutoStopObservation{}, &idle, expiredAt)
			if decision.ScaleAction || decision.DesiredReplicas != 1 {
				t.Fatalf("expiry erased activity grace: %+v", decision)
			}
		})
	}
}

func TestWakeProtectionScalesWithIdleTimeout(t *testing.T) {
	for _, idle := range []time.Duration{time.Second, 25 * time.Second, 10 * time.Minute} {
		if got := wakeProtectionDuration(idle); got != 6*idle {
			t.Fatalf("idle=%s protection=%s", idle, got)
		}
	}
	if wakeProtectionDuration(time.Duration(math.MaxInt64)) != time.Duration(math.MaxInt64) {
		t.Fatal("protection overflowed")
	}
}

func TestAcceptedWakeProtectionOutlivesDemandFreshness(t *testing.T) {
	now := referenceTime
	engine := wakeHandoffEngine(now)
	engine.Spec.Replicas = 1
	engine.Spec.AutoStop.IdleTimeout = &metav1.Duration{Duration: 2 * time.Minute}
	stamp := metav1.NewTime(now)
	engine.Status.LastWakeDemandTime = &stamp
	until := metav1.NewTime(stamp.Add(wakeProtectionDuration(engine.Spec.AutoStop.IdleTimeout.Duration)))
	engine.Status.WakeProtectionUntil = &until
	decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
		AutoStopObservation{}, now.Add(DefaultAutoStopWakeTTL+time.Minute))
	if decision.ScaleAction || decision.DesiredReplicas != 1 {
		t.Fatalf("freshness expiry truncated accepted protection: %+v", decision)
	}
}

// Exercise accepted state through the API-shaped status rather than manufacturing
// a deadline from the policy under test.
func TestAcceptedWakeDeadlineSurvivesPolicyChanges(t *testing.T) {
	for _, configured := range []time.Duration{time.Second, 2 * time.Minute} {
		t.Run(configured.String(), func(t *testing.T) {
			now := referenceTime
			engine := wakeHandoffEngine(now)
			accepted := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
				AutoStopObservation{WakeRequestedAt: &now}, now)
			engine.Status.LastWakeDemandTime = accepted.NewLastWakeDemandTime
			engine.Status.WakeProtectionUntil = accepted.NewWakeProtectionUntil
			wantDeadline := now.Add(150 * time.Second)
			if accepted.NewWakeProtectionUntil == nil || !accepted.NewWakeProtectionUntil.Time.Equal(wantDeadline) {
				t.Fatalf("incorrect acceptance deadline: %+v", accepted)
			}
			// A policy update and restart before the replica write must still retry
			// the accepted wake, even when the new timeout is much shorter.
			engine = engine.DeepCopy()
			engine.Spec.AutoStop.IdleTimeout.Duration = configured
			decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
				AutoStopObservation{}, now.Add(time.Minute))
			if decision.DesiredReplicas != 1 || !decision.ScaleAction {
				t.Fatalf("policy edit lost pending accepted wake: %+v", decision)
			}
			engine.Spec.Replicas = 1
			for _, at := range []time.Time{now.Add(time.Minute), wantDeadline.Add(-time.Second)} {
				decision = computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
					AutoStopObservation{WakeRequestedAt: &now}, at)
				if decision.ScaleAction || decision.NewLastWakeDemandTime != nil || decision.NewWakeProtectionUntil != nil {
					t.Fatalf("policy edit or replay changed active protection: %+v", decision)
				}
			}
			decision = computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
				AutoStopObservation{}, wantDeadline)
			if !decision.ScaleAction || decision.DesiredReplicas != 0 {
				t.Fatalf("policy edit extended protection: %+v", decision)
			}
			engine.Spec.Replicas = 0
			for _, at := range []time.Time{wantDeadline.Add(time.Second), now.Add(10 * time.Minute)} {
				decision = computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
					AutoStopObservation{WakeRequestedAt: &now}, at)
				if decision.ScaleAction || decision.NewWakeProtectionUntil != nil {
					t.Fatalf("expired wake revived: %+v", decision)
				}
			}
			fresh := now.Add(11 * time.Minute)
			decision = computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
				AutoStopObservation{WakeRequestedAt: &fresh}, fresh)
			if !decision.ScaleAction || decision.NewWakeProtectionUntil == nil ||
				!decision.NewWakeProtectionUntil.Time.Equal(fresh.Add(6*configured)) {
				t.Fatalf("fresh demand did not use updated policy: %+v", decision)
			}
		})
	}
}

func TestWakeDeadlineSurvivesStatusSerialization(t *testing.T) {
	now := referenceTime
	engine := wakeHandoffEngine(now)
	engine.Spec.AutoStop.IdleTimeout.Duration = 1100 * time.Millisecond
	decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
		AutoStopObservation{WakeRequestedAt: &now}, now)
	engine.Status.LastWakeDemandTime = decision.NewLastWakeDemandTime
	engine.Status.WakeProtectionUntil = decision.NewWakeProtectionUntil
	data, err := json.Marshal(engine.Status)
	if err != nil {
		t.Fatal(err)
	}
	var restored computev1alpha1.FireboltEngineStatus
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.WakeProtectionUntil.Equal(decision.NewWakeProtectionUntil) ||
		!restored.WakeProtectionUntil.Time.Equal(now.Add(7*time.Second)) {
		t.Fatalf("fractional timeout deadline changed on persistence: %s", data)
	}
}

func TestWakeRecoversAfterDeadlinePruning(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached=%v", cached), func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)
			engine := wakeHandoffEngine(now)
			scheme := wakeTestScheme(t)
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(engine).
				WithStatusSubresource(engine).Build()
			pruning := interceptor.NewClient(base, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					obj.(*computev1alpha1.FireboltEngine).Status.WakeProtectionUntil = nil
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			})
			tracker := &WakeDemandTracker{}
			tracker.replace(map[demandKey]time.Time{{namespace: "ns", engine: "engine"}: now})
			r := &FireboltEngineReconciler{Client: pruning, Scheme: scheme, WakeDemand: tracker,
				MetricsRecorder: metrics.NoOpEngineRecorder{}}
			// Repeated attempts against an incomplete schema must keep failing closed.
			for range 2 {
				if _, err := r.runAutoStop(ctx, engine, nil); err == nil {
					t.Fatal("incomplete acceptance silently consumed demand")
				}
				if err := base.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
					t.Fatal(err)
				}
				if engine.Spec.Replicas != 0 || engine.Status.LastWakeDemandTime == nil || engine.Status.WakeProtectionUntil != nil {
					t.Fatalf("unexpected partially persisted state: %+v", engine)
				}
			}
			if !cached {
				tracker = &WakeDemandTracker{}
			}
			// The corrected schema and replacement reconciler must finish acceptance.
			r = &FireboltEngineReconciler{Client: base, Scheme: scheme, WakeDemand: tracker,
				MetricsRecorder: metrics.NoOpEngineRecorder{}}
			if _, err := r.runAutoStop(ctx, engine, nil); err != nil {
				t.Fatal(err)
			}
			if err := base.Get(ctx, client.ObjectKeyFromObject(engine), engine); err != nil {
				t.Fatal(err)
			}
			if engine.Spec.Replicas != 1 || engine.Status.WakeProtectionUntil == nil ||
				!engine.Status.WakeProtectionUntil.Time.Equal(now.Add(150*time.Second)) {
				t.Fatalf("recovery lost or renewed the original wake: %+v", engine)
			}
		})
	}
}

func TestIncompleteWakeStillRequiresFreshDemand(t *testing.T) {
	now := referenceTime
	for _, age := range []time.Duration{-time.Second, DefaultAutoStopWakeTTL, time.Hour} {
		engine := wakeHandoffEngine(now)
		stamp := metav1.NewTime(now.Add(-age))
		engine.Status.LastWakeDemandTime = &stamp
		decision := computeAutoStopDecision(&engine.Spec, engine.Spec.AutoStop, &engine.Status,
			AutoStopObservation{}, now)
		if decision.ScaleAction || decision.NewWakeProtectionUntil != nil {
			t.Fatalf("invalid partial record accepted at age %s: %+v", age, decision)
		}
	}
}
