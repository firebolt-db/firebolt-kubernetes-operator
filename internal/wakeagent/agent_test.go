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

package wakeagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsValidEngineName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"simple", "analytics", true},
		{"with digits and hyphen", "engine-01", true},
		{"single char", "e", true},
		{"empty", "", false},
		{"uppercase", "Analytics", false},
		{"dot would cross namespaces", "engine.other-ns", false},
		{"leading hyphen", "-engine", false},
		{"trailing hyphen", "engine-", false},
		{"underscore", "my_engine", false},
		{"slash would inject a path", "engine/../admin", false},
		{"64 chars", strings.Repeat("a", 64), false},
		{"63 chars", strings.Repeat("a", 63), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidEngineName(tc.in); got != tc.want {
				t.Errorf("isValidEngineName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestDemandTrackerStampAndRender(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := newDemandTracker(10*time.Minute, func() time.Time { return now })

	d.Stamp("beta")
	d.Stamp("alpha")

	out := d.Render()
	if !strings.Contains(out, `firebolt_gateway_wake_last_demand_timestamp_seconds{engine="alpha"} 1700000000`) {
		t.Errorf("alpha demand missing from render:\n%s", out)
	}
	if !strings.Contains(out, `firebolt_gateway_wake_last_demand_timestamp_seconds{engine="beta"} 1700000000`) {
		t.Errorf("beta demand missing from render:\n%s", out)
	}
	// Engines are sorted so the exposition is stable across scrapes.
	if strings.Index(out, `engine="alpha"`) > strings.Index(out, `engine="beta"`) {
		t.Errorf("engines not sorted in render:\n%s", out)
	}
}

func TestDemandTrackerEvictsStaleEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	d := newDemandTracker(5*time.Minute, clock)

	d.Stamp("old")
	now = now.Add(6 * time.Minute)
	d.Stamp("fresh")

	rows := d.Snapshot()
	if len(rows) != 1 || rows[0].engine != "fresh" {
		t.Fatalf("Snapshot() = %+v, want only the fresh engine", rows)
	}
}

// A held request keeps its engine's entry alive past the retention window:
// evicting an engine someone is actively waiting on would drop the very
// demand the operator needs to see.
func TestDemandTrackerKeepsHeldEngineBeyondRetention(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := newDemandTracker(5*time.Minute, func() time.Time { return now })

	d.Stamp("waiting")
	if !d.AcquireHold("waiting", 10) {
		t.Fatal("AcquireHold() = false on an empty tracker")
	}
	now = now.Add(time.Hour)

	rows := d.Snapshot()
	if len(rows) != 1 || rows[0].engine != "waiting" {
		t.Fatalf("Snapshot() = %+v, want the held engine retained", rows)
	}
}

func TestDemandTrackerHoldCap(t *testing.T) {
	d := newDemandTracker(time.Minute, time.Now)

	if !d.AcquireHold("a", 2) {
		t.Fatal("first AcquireHold rejected")
	}
	if !d.AcquireHold("b", 2) {
		t.Fatal("second AcquireHold rejected")
	}
	// Cap is global across engines, not per engine: the memory it protects
	// is the gateway pod's, which every engine's holds draw from.
	if d.AcquireHold("c", 2) {
		t.Fatal("third AcquireHold accepted past the cap")
	}
	if got := d.PendingTotal(); got != 2 {
		t.Fatalf("PendingTotal() = %d, want 2", got)
	}

	d.ReleaseHold("a")
	if !d.AcquireHold("c", 2) {
		t.Fatal("AcquireHold rejected after a slot was released")
	}
}

// A shed request must still register demand, otherwise a herd that
// overflows the cap could wake nothing at all.
func TestShedRequestStillCountsAsDemand(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := newDemandTracker(time.Minute, func() time.Time { return now })

	// Saturate with another engine's hold, then shed.
	if !d.AcquireHold("other", 1) {
		t.Fatal("AcquireHold rejected on an empty tracker")
	}
	d.Stamp("busy")
	if d.AcquireHold("busy", 1) {
		t.Fatal("AcquireHold accepted past a cap of 1")
	}

	rows := d.Snapshot()
	var found bool
	for _, r := range rows {
		if r.engine == "busy" {
			found = true
			if r.last.Unix() != now.Unix() {
				t.Errorf("shed engine last demand = %d, want %d", r.last.Unix(), now.Unix())
			}
		}
	}
	if !found {
		t.Fatalf("shed engine absent from snapshot: %+v", rows)
	}
}

func TestReadinessTrackerWaitChanReleasesOnReady(t *testing.T) {
	tr := newReadinessTracker()

	ch := tr.WaitChan("engine")
	select {
	case <-ch:
		t.Fatal("WaitChan fired before the engine was ready")
	default:
	}

	tr.setReady("engine", true)
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("WaitChan did not fire after setReady")
	}
	if !tr.IsReady("engine") {
		t.Error("IsReady() = false after setReady(true)")
	}
}

// Every waiter for the same engine must wake on one readiness edge; a
// per-caller channel that only released the first would strand the rest
// until their hold timeout.
func TestReadinessTrackerWakesAllWaiters(t *testing.T) {
	tr := newReadinessTracker()
	a := tr.WaitChan("engine")
	b := tr.WaitChan("engine")

	tr.setReady("engine", true)

	for i, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatalf("waiter %d not released", i)
		}
	}
}

func TestReadinessTrackerAlreadyReadyReturnsClosedChan(t *testing.T) {
	tr := newReadinessTracker()
	tr.setReady("engine", true)

	select {
	case <-tr.WaitChan("engine"):
	default:
		t.Fatal("WaitChan on a ready engine did not return a closed channel")
	}
}

func TestEngineFromSliceLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"engine service", map[string]string{serviceNameLabel: "analytics-service"}, "analytics"},
		{"non-engine service", map[string]string{serviceNameLabel: "my-gateway"}, ""},
		{"missing label", map[string]string{}, ""},
		{"bare suffix", map[string]string{serviceNameLabel: "-service"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineFromSliceLabels(tc.labels); got != tc.want {
				t.Errorf("engineFromSliceLabels(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// fakeStore is a sliceStore backed by a literal slice.
type fakeStore []interface{}

func (f fakeStore) List() []interface{} { return f }

func slice(engine string, ready ...bool) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{serviceNameLabel: engine + ServiceSuffix},
		},
	}
	for i := range ready {
		r := ready[i]
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{
			Conditions: discoveryv1.EndpointConditions{Ready: &r},
		})
	}
	return s
}

// A Service can be backed by several EndpointSlices, so readiness has to be
// derived from all of them: one slice going empty does not mean the engine
// has no endpoints.
func TestRecomputeEngineAcrossMultipleSlices(t *testing.T) {
	cases := []struct {
		name   string
		store  fakeStore
		engine string
		want   bool
	}{
		{"single ready slice", fakeStore{slice("a", true)}, "a", true},
		{"single empty slice", fakeStore{slice("a")}, "a", false},
		{"all endpoints not ready", fakeStore{slice("a", false, false)}, "a", false},
		{
			"one of two slices ready",
			fakeStore{slice("a"), slice("a", true)},
			"a", true,
		},
		{
			"other engine's slice ignored",
			fakeStore{slice("b", true)},
			"a", false,
		},
		{"no slices at all", fakeStore{}, "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recomputeEngine(tc.store, tc.engine); got != tc.want {
				t.Errorf("recomputeEngine(%s) = %v, want %v", tc.engine, got, tc.want)
			}
		})
	}
}

// A nil Ready condition means ready per the EndpointSlice API contract.
func TestSliceHasReadyEndpointNilCondition(t *testing.T) {
	s := &discoveryv1.EndpointSlice{
		Endpoints: []discoveryv1.Endpoint{{}},
	}
	if !sliceHasReadyEndpoint(s) {
		t.Error("sliceHasReadyEndpoint() = false for a nil Ready condition, want true")
	}
}

func TestParseEnvoyStat(t *testing.T) {
	body := "server.memory_allocated: 12345\nserver.memory_heap_size: 99999\n"
	got, ok := parseEnvoyStat(body, envoyAllocatedStat)
	if !ok || got != 12345 {
		t.Fatalf("parseEnvoyStat() = (%d, %v), want (12345, true)", got, ok)
	}
	if _, ok := parseEnvoyStat(body, "nope"); ok {
		t.Error("parseEnvoyStat() found a stat that is not present")
	}
	if _, ok := parseEnvoyStat("server.memory_allocated: abc\n", envoyAllocatedStat); ok {
		t.Error("parseEnvoyStat() accepted a non-numeric value")
	}
}

func TestCapacityLimiterFallsBackWithoutMemoryLimit(t *testing.T) {
	c := newCapacityLimiter(0, 2<<20, 256, "")
	if got := c.Cap(); got != 256 {
		t.Errorf("Cap() = %d, want the fallback 256", got)
	}
}

func TestCapacityLimiterDerivesFromMemoryLimit(t *testing.T) {
	const limit = 1 << 30 // 1 GiB
	const perHold = 2 << 20
	c := newCapacityLimiter(limit, perHold, 256, "")

	// No live reading yet, so the static budget applies:
	// 1 GiB * 0.25 / 2 MiB = 128.
	if got, want := c.Cap(), 128; got != want {
		t.Errorf("Cap() = %d, want %d", got, want)
	}
}

// The live reading must be able to tighten the cap, otherwise the dynamic
// half of the design does nothing.
func TestCapacityLimiterTightensUnderPressure(t *testing.T) {
	const limit = 1 << 30
	const perHold = 2 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Well above the assumed steady state, so headroom shrinks.
		_, _ = w.Write([]byte("server.memory_allocated: " + itoa(limit/2) + "\n"))
	}))
	defer srv.Close()

	c := newCapacityLimiter(limit, perHold, 256, srv.URL)
	before := c.Cap()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	after := c.Cap()
	if after >= before {
		t.Errorf("Cap() = %d after a high allocation reading, want less than %d", after, before)
	}
}

// A failed refresh must not change the cap: the admin listener being
// briefly unavailable is not a reason to stop holding requests.
func TestCapacityLimiterKeepsCapWhenRefreshFails(t *testing.T) {
	c := newCapacityLimiter(1<<30, 2<<20, 256, "http://127.0.0.1:1")
	before := c.Cap()
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() succeeded against a dead endpoint")
	}
	if after := c.Cap(); after != before {
		t.Errorf("Cap() = %d after a failed refresh, want %d", after, before)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func testAgent(t *testing.T, holdTimeout time.Duration) *Agent {
	t.Helper()
	a := New(Config{
		Namespace:   "test-ns",
		HoldTimeout: holdTimeout,
		FallbackCap: 8,
	})
	// Every test below exercises steady state, where the cache has synced.
	// The unsynced path has its own test.
	a.readiness.MarkSynced()
	return a
}

// An agent that cannot watch EndpointSlices — a missing RBAC grant being
// the likely cause — must let queries through. Holding them instead would
// park every request for the full timeout and turn a lost wake capability
// into a lost data path.
func TestHandleHoldRoutesWhileCacheUnsynced(t *testing.T) {
	t.Parallel()
	a := New(Config{Namespace: "test-ns", HoldTimeout: time.Minute, FallbackCap: 8})

	rec := httptest.NewRecorder()
	a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=whatever", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 while the cache is unsynced", rec.Code)
	}
	if got := rec.Header().Get(DecisionHeader); got != DecisionUnsynced {
		t.Errorf("%s = %q, want %q", DecisionHeader, got, DecisionUnsynced)
	}
	if rows := a.demand.Snapshot(); len(rows) != 0 {
		t.Errorf("unsynced agent recorded demand: %+v", rows)
	}
}

// Engine names arrive in an untrusted header and are never checked against
// engines that exist, so an abandoned hold must not leave anything behind.
// The hold cap bounds concurrency, not rate.
func TestAbandonedHoldsDoNotLeakWaiters(t *testing.T) {
	t.Parallel()
	a := testAgent(t, 20*time.Millisecond)

	for i := 0; i < 50; i++ {
		rec := httptest.NewRecorder()
		a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			fmt.Sprintf("/hold?engine=ghost-%d", i), http.NoBody))
	}

	a.readiness.mu.RLock()
	waiters, refs := len(a.readiness.waiters), len(a.readiness.waiterRefs)
	a.readiness.mu.RUnlock()
	if waiters != 0 || refs != 0 {
		t.Errorf("waiters=%d waiterRefs=%d after 50 abandoned holds, want 0/0", waiters, refs)
	}
}

// Concurrent holds on one engine share a channel; the last one out drops it.
func TestWaiterRefcountReleasesOnLastWaiter(t *testing.T) {
	t.Parallel()
	tr := newReadinessTracker()
	tr.MarkSynced()

	a := tr.WaitChan("engine")
	b := tr.WaitChan("engine")
	tr.DoneWaiting("engine", a)

	tr.mu.RLock()
	stillThere := len(tr.waiters)
	tr.mu.RUnlock()
	if stillThere != 1 {
		t.Fatalf("waiters = %d after releasing one of two, want 1", stillThere)
	}

	tr.DoneWaiting("engine", b)
	tr.mu.RLock()
	gone := len(tr.waiters)
	tr.mu.RUnlock()
	if gone != 0 {
		t.Errorf("waiters = %d after the last release, want 0", gone)
	}
}

// A waiter release is keyed by channel identity, not by engine name. After
// setReady retires a channel, the engine can flap not-ready and a new hold
// can register a fresh channel under the same name; a woken waiter's
// deferred release from the previous generation must not decrement the new
// registration, or the new hold would be stranded until its timeout.
func TestDoneWaitingIgnoresRetiredChannel(t *testing.T) {
	t.Parallel()
	tr := newReadinessTracker()
	tr.MarkSynced()

	a := tr.WaitChan("engine")   // hold A parks
	tr.setReady("engine", true)  // A woken; its channel is retired
	tr.setReady("engine", false) // engine flaps away again
	b := tr.WaitChan("engine")   // hold B parks on a fresh channel
	tr.DoneWaiting("engine", a)  // A's deferred release finally runs

	tr.mu.RLock()
	waiters, refs := len(tr.waiters), tr.waiterRefs["engine"]
	tr.mu.RUnlock()
	if waiters != 1 || refs != 1 {
		t.Fatalf("waiters=%d refs=%d after a stale release, want 1/1", waiters, refs)
	}

	tr.setReady("engine", true) // B must be released, not time out
	select {
	case <-b:
	case <-time.After(time.Second):
		t.Fatal("second-generation waiter stranded by a stale release")
	}
}

// A wake that completes exactly at the hold deadline can lose the select
// race between the readiness edge and the timer. The engine is routable at
// that point, so the hold must answer 200, not 503.
func TestHandleHoldTimerRaceAnswersReadyEngine(t *testing.T) {
	t.Parallel()
	a := testAgent(t, 30*time.Millisecond)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=lastmoment", http.NoBody))
		done <- rec
	}()
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })

	// The engine becomes ready, but the waiter's channel close is never
	// observed — the timer arm wins the select, exactly as it can when both
	// events land together.
	a.readiness.mu.Lock()
	a.readiness.ready["lastmoment"] = struct{}{}
	a.readiness.mu.Unlock()

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 when the engine is ready at the deadline", rec.Code)
		}
		if got := rec.Header().Get(DecisionHeader); got != DecisionReleased {
			t.Errorf("%s = %q, want %q", DecisionHeader, got, DecisionReleased)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the hold deadline")
	}
}

// A release is an edge, not a guarantee: an engine can flap away again
// before the parked request wakes up. Routing it then just converts the
// wait into a confusing upstream error.
func TestHandleHoldRejectsWhenEngineFlapsAway(t *testing.T) {
	t.Parallel()
	a := testAgent(t, 5*time.Second)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=flappy", http.NoBody))
		done <- rec.Code
	}()
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })

	// Ready then immediately gone, before the waiter is scheduled.
	a.readiness.mu.Lock()
	a.readiness.ready["flappy"] = struct{}{}
	if ch, ok := a.readiness.waiters["flappy"]; ok {
		close(ch)
		delete(a.readiness.waiters, "flappy")
		delete(a.readiness.waiterRefs, "flappy")
	}
	delete(a.readiness.ready, "flappy")
	a.readiness.mu.Unlock()

	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 when the engine flapped away", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the flap")
	}
}

// Steady state: the engine is up, so the hop must return immediately and
// leave no demand behind for the operator to act on.
func TestHandleHoldReadyEngineReturnsImmediately(t *testing.T) {
	a := testAgent(t, time.Minute)
	a.readiness.setReady("up", true)

	rec := httptest.NewRecorder()
	a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=up", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rows := a.demand.Snapshot(); len(rows) != 0 {
		t.Errorf("a ready engine recorded demand: %+v", rows)
	}
}

func TestHandleHoldRejectsInvalidEngine(t *testing.T) {
	a := testAgent(t, time.Minute)
	rec := httptest.NewRecorder()
	a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=Bad.Name", http.NoBody))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The whole point of the sidecar: park the request, then release it the
// moment the engine's endpoints appear.
func TestHandleHoldParksThenReleases(t *testing.T) {
	a := testAgent(t, 5*time.Second)

	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=stopped", http.NoBody))
		done <- rec.Code
	}()

	// Wait for the hold to register before flipping readiness, so the test
	// exercises the wait path rather than the already-ready fast path.
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	a.readiness.setReady("stopped", true)

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held request was never released")
	}
	if a.demand.PendingTotal() != 0 {
		t.Errorf("PendingTotal() = %d after release, want 0", a.demand.PendingTotal())
	}
}

func TestHandleHoldTimesOut(t *testing.T) {
	a := testAgent(t, 50*time.Millisecond)

	rec := httptest.NewRecorder()
	a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=never", http.NoBody))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("timed-out hold has no Retry-After header")
	}
	// Demand outlives the abandoned request, which is what lets a slow
	// wake still succeed for the next caller.
	if rows := a.demand.Snapshot(); len(rows) != 1 {
		t.Errorf("Snapshot() = %+v, want the demand stamp retained", rows)
	}
}

func TestHandleHoldShedsPastCap(t *testing.T) {
	a := New(Config{Namespace: "test-ns", HoldTimeout: 5 * time.Second, FallbackCap: 1})
	a.readiness.MarkSynced()

	go func() {
		rec := httptest.NewRecorder()
		a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=first", http.NoBody))
	}()
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })

	rec := httptest.NewRecorder()
	a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=second", http.NoBody))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 once the cap is reached", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("shed response has no Retry-After header")
	}
	// One held request is enough to trigger the wake, but the shed one
	// must still have registered its own demand.
	var sawSecond bool
	for _, r := range a.demand.Snapshot() {
		if r.engine == "second" {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Error("shed engine did not register demand")
	}
}

// A client that hangs up mid-hold releases its slot but leaves the demand
// stamp, which is the behavior that makes wake independent of poll timing.
func TestHandleHoldClientDisconnectKeepsDemand(t *testing.T) {
	a := testAgent(t, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/hold?engine=impatient", http.NoBody)

	done := make(chan struct{})
	go func() {
		a.handleHold(httptest.NewRecorder(), req)
		close(done)
	}()
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}

	if a.demand.PendingTotal() != 0 {
		t.Errorf("PendingTotal() = %d, want the slot released", a.demand.PendingTotal())
	}
	if rows := a.demand.Snapshot(); len(rows) != 1 || rows[0].engine != "impatient" {
		t.Errorf("Snapshot() = %+v, want demand to survive the disconnect", rows)
	}
}

func TestDemandEndpointServesExposition(t *testing.T) {
	a := testAgent(t, time.Minute)
	a.demand.Stamp("analytics")

	rec := httptest.NewRecorder()
	a.demandMux().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/demand", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `engine="analytics"`) {
		t.Errorf("exposition missing the engine series:\n%s", rec.Body.String())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

// A cap of zero means shed everything, not "no limit". The capacity limiter
// returns zero precisely when Envoy is under memory pressure, so reading it
// as unlimited would disable admission control exactly when it is needed and
// let a flood of parked requests amplify the pressure that caused it.
func TestZeroCapShedsRatherThanAdmittingEverything(t *testing.T) {
	t.Parallel()
	d := newDemandTracker(time.Minute, time.Now)

	if d.AcquireHold("a", 0) {
		t.Fatal("AcquireHold accepted a request at a cap of 0")
	}
	if got := d.PendingTotal(); got != 0 {
		t.Errorf("PendingTotal() = %d after a rejected hold, want 0", got)
	}

	// Negative is the way to express "unlimited".
	if !d.AcquireHold("a", -1) {
		t.Error("AcquireHold rejected a request at a negative (unlimited) cap")
	}
}

// The limiter must be able to reach zero, otherwise the shed path above is
// unreachable in practice.
func TestCapacityLimiterReachesZeroUnderPressure(t *testing.T) {
	t.Parallel()
	const limit = 1 << 30
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Allocation far above the budget: headroom goes negative.
		_, _ = w.Write([]byte("server.memory_allocated: " + itoa(limit) + "\n"))
	}))
	defer srv.Close()

	c := newCapacityLimiter(limit, 2<<20, 256, srv.URL)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	if got := c.Cap(); got != 0 {
		t.Errorf("Cap() = %d at full allocation, want 0", got)
	}
}
