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

// Deterministic exhaustive state-cover testing for the auto-stop decision
// function, mirroring the engine, instance and rotation harnesses.
//
// For every reachable state in the TLC state graph of formal/EngineWake.tla
// (regenerated via `make formal-gen`), this test materializes the inputs
// computeAutoStopDecision reads, runs it once, applies the decision exactly as
// runAutoStop does, and verifies the resulting state lies in the model's
// reconciler closure of the starting state.
//
// What this binds that the existing rapid harness
// (engine_autostop_property_test.go) does not: that harness draws inputs
// uniformly and checks hand-written precedence assertions, so it says nothing
// about which input combinations are REACHABLE, and its assertions are a second
// transcription of the same rules rather than a model. Here the reachable set
// comes from the model and the expected outcome comes from the model, so a
// re-ordered precedence rule fails as "not in the closure" rather than needing
// somebody to have foreseen it. The two are complementary: rapid explores the
// tuning knobs (arbitrary idleReplicas / activeReplicas / timeouts, schedule
// windows) that the model abstracts to three replica levels and one timeout.
//
// The model's OTHER half, WakeAgentHold.tla, has no binding here: its subject is
// the wake agent's in-memory waiter bookkeeping, which lives in
// internal/wakeagent behind unexported types. See formal/model-scope.tsv for
// that decision, stated as a row rather than left as a silence.

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// The model's clock, mapped onto real durations.
//
// EngineWake.cfg sets WakeTTL = 2 ticks and IdleTimeout = 2 ticks, so one tick
// is half the production wake TTL and the idle timeout is two of them. The
// mapping is deliberately derived from DefaultAutoStopWakeTTL rather than
// written out as a duration: the model's freshness test is `age < WakeTTL` and
// the production one is `now.Sub(stamp) < DefaultAutoStopWakeTTL`, and they have
// to agree ON THE BOUNDARY -- age 1 fresh, age 2 not -- or half the cover is
// checking the wrong side of a comparison.
//
// Nothing enforces that these tick counts still match the cfg, and nothing
// needs to: they are what converts a model age into an input, so a cfg that
// moved WakeTTL or IdleTimeout without moving them makes the Go decision
// disagree with the model and the closure assertion below fails on the states
// that straddle the boundary.
const (
	tlaWakeTTLTicks         = 2
	tlaWakeIdleTimeoutTicks = 2
	tlaWakeTick             = DefaultAutoStopWakeTTL / tlaWakeTTLTicks
	tlaWakeIdleTimeout      = tlaWakeIdleTimeoutTicks * tlaWakeTick
)

// Replica levels, matching EngineWake.cfg. UserReplicas is a count auto-stop
// never chooses, reachable in the model only as an initial condition.
const (
	tlaWakeIdleReplicas   int32 = 0
	tlaWakeActiveReplicas int32 = 1
)

// tlaWakeNow is the synthetic instant every case is evaluated at. Fixed rather
// than time.Now() so a failure reproduces, and off any duration boundary so
// nothing can pass by accidental rounding.
var tlaWakeNow = time.Date(2026, 6, 1, 12, 34, 56, 0, time.UTC)

// tlaWakeReasons maps the model's reason tokens to the production constants.
// Written out rather than assumed identical: the model names a decision, the
// status field carries a token, and the two agreeing character-for-character is
// a fact worth failing on if it stops being true.
var tlaWakeReasons = map[string]string{
	"Stopped":          AutoStopReasonStopped,
	"WakeRequested":    AutoStopReasonWakeRequested,
	"ScrapeFailed":     AutoStopReasonScrapeFailed,
	"ActivityObserved": AutoStopReasonActivity,
	"Idle":             AutoStopReasonIdle,
	"Initializing":     AutoStopReasonInitializing,
}

// tlaWakeSim is one materialized TLA+ state: everything computeAutoStopDecision
// reads, plus the two status fields it writes.
type tlaWakeSim struct {
	spec     *computev1alpha1.FireboltEngineSpec
	autoStop *computev1alpha1.AutoStopSpec
	status   *computev1alpha1.FireboltEngineStatus
	obs      AutoStopObservation
}

// tlaWakeStamp converts a model age in ticks to an instant, or nil for the
// model's -1 ("no such timestamp").
func tlaWakeStamp(age int) *time.Time {
	if age < 0 {
		return nil
	}
	t := tlaWakeNow.Add(-time.Duration(age) * tlaWakeTick)
	return &t
}

// tlaWakeAge is the inverse. Every materialized instant is an exact multiple of
// a tick away from tlaWakeNow, so this is exact rather than rounded.
func tlaWakeAge(t *time.Time) int {
	if t == nil {
		return -1
	}
	return int(tlaWakeNow.Sub(*t) / tlaWakeTick)
}

// materializeTLAWakeState builds the decision function's inputs for s.
//
// autoStop.enabled is always true and schedule is always empty: both branches
// return before the wake path is reached and the model leaves them out of scope
// on purpose (see EngineWake.tla). idleReplicas/activeReplicas are the model's
// two levels, and idleTimeout is its IdleTimeout in real time.
func materializeTLAWakeState(t *testing.T, s tlaWakeState) *tlaWakeSim {
	t.Helper()

	idleReplicas := tlaWakeIdleReplicas
	autoStop := &computev1alpha1.AutoStopSpec{
		Enabled:        true,
		ActiveReplicas: tlaWakeActiveReplicas,
		IdleReplicas:   &idleReplicas,
		IdleTimeout:    &metav1.Duration{Duration: tlaWakeIdleTimeout},
		PollInterval:   &metav1.Duration{Duration: tlaWakeTick},
	}
	spec := &computev1alpha1.FireboltEngineSpec{
		Replicas: int32(s.Replicas),
		AutoStop: autoStop,
	}

	reason, ok := tlaWakeReasons[s.Reason]
	if !ok {
		t.Fatalf("model reason %q has no production constant in tlaWakeReasons", s.Reason)
	}
	status := &computev1alpha1.FireboltEngineStatus{AutoStopReason: reason}
	if last := tlaWakeStamp(s.IdleAge); last != nil {
		stamp := metav1.NewTime(*last)
		status.LastActivityTime = &stamp
	}

	obs := AutoStopObservation{WakeRequestedAt: tlaWakeStamp(s.WakeAge)}
	switch s.Activity {
	case "quiet":
		// Zero-valued, which is also what runAutoStop leaves the observation as
		// for a parked engine: it does not scrape at zero replicas.
	case "busy":
		obs.ActiveQueries = 1
	case "scrapeFailed":
		// scrapeActiveQueries returns (0, true) on every failure path, so a
		// failed scrape never carries a query count.
		obs.ScrapeFailed = true
	default:
		t.Fatalf("unmappable model activity %q", s.Activity)
	}

	return &tlaWakeSim{spec: spec, autoStop: autoStop, status: status, obs: obs}
}

// project extracts the model's observable variables back out of the sim.
func (m *tlaWakeSim) project() tlaWakeState {
	activity := "quiet"
	switch {
	case m.obs.ScrapeFailed:
		activity = "scrapeFailed"
	case m.obs.ActiveQueries > 0:
		activity = "busy"
	}
	var last *time.Time
	if m.status.LastActivityTime != nil {
		last = &m.status.LastActivityTime.Time
	}
	return tlaWakeState{
		Replicas: int(m.spec.Replicas),
		WakeAge:  tlaWakeAge(m.obs.WakeRequestedAt),
		IdleAge:  tlaWakeAge(last),
		Activity: activity,
		Reason:   m.status.AutoStopReason,
	}
}

// apply writes a decision back exactly as runAutoStop does: the replica patch
// when the decision asks for one, then the two status fields.
//
// The order matters in production for a reason that does not apply here (the
// spec Update's response clobbers in-memory status), but keeping the same order
// keeps this a transcription of the caller rather than a second opinion about
// what the decision means.
func (m *tlaWakeSim) apply(decision AutoStopDecision) {
	if decision.ScaleAction {
		m.spec.Replicas = decision.DesiredReplicas
	}
	m.status.AutoStopReason = decision.Reason
	if decision.NewLastActivityTime != nil {
		m.status.LastActivityTime = decision.NewLastActivityTime
	}
}

// wakeInvariants maps each conjunct of EngineWake.tla's `Safety ==` to its Go
// counterpart. Keys are checked against the generated tlaWakeRequiredInvariants
// by TestWakeInvariantsMatchSpec, in both directions.
var wakeInvariants = map[string]func(t *testing.T, m *tlaWakeSim){
	// Partial in Go, as in the other harnesses: the spec's TypeOK asserts
	// membership in bounded sets that are a model artifact (the clock domain).
	// What carries over is that every value the decision function reads or
	// writes is one the state machine defines, and that no timestamp it holds
	// is dated in the future -- the agent stamps on arrival, the poller copies
	// what the agent had, and the reconciler anchors the idle clock at now.
	"TypeOK": func(t *testing.T, m *tlaWakeSim) {
		t.Helper()
		if m.spec.Replicas < 0 {
			t.Fatalf("TypeOK: spec.Replicas=%d is negative", m.spec.Replicas)
		}
		switch m.status.AutoStopReason {
		case AutoStopReasonStopped, AutoStopReasonWakeRequested, AutoStopReasonScrapeFailed,
			AutoStopReasonActivity, AutoStopReasonIdle, AutoStopReasonInitializing:
		default:
			t.Fatalf("TypeOK: autoStopReason %q is not a token this protocol writes",
				m.status.AutoStopReason)
		}
		if m.obs.WakeRequestedAt != nil && m.obs.WakeRequestedAt.After(tlaWakeNow) {
			t.Fatalf("TypeOK: wake demand is stamped in the future (%s > %s)",
				m.obs.WakeRequestedAt, tlaWakeNow)
		}
		if m.status.LastActivityTime != nil && m.status.LastActivityTime.After(tlaWakeNow) {
			t.Fatalf("TypeOK: lastActivityTime is in the future (%s > %s)",
				m.status.LastActivityTime, tlaWakeNow)
		}
	},

	// A parked engine is never scraped -- runAutoStop skips the scrape entirely
	// at zero replicas -- so its observation must carry neither queries nor a
	// scrape failure. Otherwise "quiet" would be indistinguishable from "not
	// observed" and the idle path could be entered on a scrape that never ran.
	"Inv_ScrapeOnlyWhenRunning": func(t *testing.T, m *tlaWakeSim) {
		t.Helper()
		if m.spec.Replicas > 0 {
			return
		}
		if m.obs.ActiveQueries != 0 || m.obs.ScrapeFailed {
			t.Fatalf("Inv_ScrapeOnlyWhenRunning: replicas=0 but observation carries "+
				"activeQueries=%d scrapeFailed=%t", m.obs.ActiveQueries, m.obs.ScrapeFailed)
		}
	},

	// The poller's filter, read from the decision function's side: while the
	// operator holds demand it WOULD act on, the engine is either parked or was
	// itself woken by that demand. A fresh stamp against a running engine that
	// was not woken means the filter to spec.replicas == 0 leaked, and the next
	// decision pins that engine at activeReplicas -- scaling a hand-sized
	// engine down in the middle of its own outage.
	"Inv_DemandOnlyForStoppedEngines": func(t *testing.T, m *tlaWakeSim) {
		t.Helper()
		if m.obs.WakeRequestedAt == nil ||
			tlaWakeNow.Sub(*m.obs.WakeRequestedAt) >= DefaultAutoStopWakeTTL {
			return
		}
		if m.spec.Replicas == 0 || m.status.AutoStopReason == AutoStopReasonWakeRequested {
			return
		}
		t.Fatalf("Inv_DemandOnlyForStoppedEngines: fresh wake demand (age %s) against a "+
			"running engine at %d replicas with reason %q",
			tlaWakeNow.Sub(*m.obs.WakeRequestedAt), m.spec.Replicas, m.status.AutoStopReason)
	},
}

func checkWakeInvariants(t *testing.T, m *tlaWakeSim) {
	t.Helper()
	for _, name := range mapKeys(wakeInvariants) {
		wakeInvariants[name](t, m)
	}
}

// TestWakeInvariantsMatchSpec is the anti-drift guard: the spec's Safety
// conjuncts and the Go registry must name the same invariants.
//
// There is no exemption list, and that is a claim rather than an omission:
// every conjunct of EngineWake.tla's Safety is expressible against the decision
// function's inputs. The two invariants that are NOT -- Inv_NoStrandedWaiter
// and Inv_WaiterRefsAccurate -- live in WakeAgentHold.tla, which deliberately
// has no fixture and no Go binding; formal/model-scope.tsv carries that as a
// row. If a conjunct is ever added here that genuinely cannot be bound, add the
// exemption mechanism then, with the reason.
func TestWakeInvariantsMatchSpec(t *testing.T) {
	if len(tlaWakeRequiredInvariants) == 0 {
		t.Fatal("tlaWakeRequiredInvariants is empty: the generator stopped parsing the spec's Safety predicate")
	}
	required := make(map[string]bool, len(tlaWakeRequiredInvariants))
	for _, name := range tlaWakeRequiredInvariants {
		required[name] = true
		if _, ok := wakeInvariants[name]; !ok {
			t.Errorf("formal/EngineWake.tla conjoins %s into Safety but wakeInvariants does not "+
				"implement it. Add it so the state cover checks it.", name)
		}
	}
	for _, name := range mapKeys(wakeInvariants) {
		if !required[name] {
			t.Errorf("wakeInvariants has %s, which is not a Safety conjunct of "+
				"formal/EngineWake.tla. It was probably renamed in the spec; fix the key rather "+
				"than leaving dead coverage.", name)
		}
	}
	t.Logf("%d spec conjuncts, %d registered invariants",
		len(tlaWakeRequiredInvariants), len(wakeInvariants))
}

// tlaWakeExpectedCases pins the size of the state cover; see the reasoning on
// the equivalent constant in engine_tla_state_test.go.
const tlaWakeExpectedCases = 166

func TestTLAWakeStateCover(t *testing.T) {
	if len(tlaWakeStateCases) != tlaWakeExpectedCases {
		t.Fatalf("fixture has %d cases, expected %d: the state space moved. Regenerate with `make formal-gen`, then update tlaWakeExpectedCases and say why in the commit",
			len(tlaWakeStateCases), tlaWakeExpectedCases)
	}
	for i := range tlaWakeStateCases {
		tc := tlaWakeStateCases[i]
		start := tlaWakeStatePool[tc.Start]
		name := fmt.Sprintf("case-%03d/replicas=%d/wakeAge=%d/idleAge=%d/%s/%s",
			i, start.Replicas, start.WakeAge, start.IdleAge, start.Activity, start.Reason)
		t.Run(name, func(t *testing.T) {
			m := materializeTLAWakeState(t, start)

			// Guard the fixture itself: if materialization does not reproduce
			// the starting state, every closure assertion below is meaningless.
			if got := m.project(); !tlaProjectionEqual(got, start) {
				t.Fatalf("materialization does not round-trip\n  want: %+v\n  got:  %+v", start, got)
			}
			checkWakeInvariants(t, m)

			m.apply(computeAutoStopDecision(m.spec, m.autoStop, m.status, m.obs, tlaWakeNow))

			checkWakeInvariants(t, m)

			actual := m.project()
			if !tlaClosureContains(tlaWakeStatePool, tc.Closure, actual) {
				t.Fatalf("result not in the TLA+ reconciler closure of the starting state\n  start:  %+v\n  actual: %+v\n  closure (%d states):\n%s",
					start, actual, len(tc.Closure), tlaFormatClosure(tlaWakeStatePool, tc.Closure))
			}
		})
	}
	t.Logf("wake state cover: ran %d cases", len(tlaWakeStateCases))
}
