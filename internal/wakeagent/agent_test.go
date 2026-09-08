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

// Engine names come from an untrusted header and pruning is time-based, so
// a sustained stream of unique garbage names must hit a hard size bound
// rather than grow the maps until the container's memory limit does the
// bounding. Engines already tracked keep the stamp-before-shed contract:
// their stamps refresh even when the tracker is full.
func TestDemandTrackerCapsTrackedEngines(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	d := newDemandTracker(10*time.Minute, func() time.Time { return now })

	for i := 0; i < maxTrackedEngines; i++ {
		d.Stamp(fmt.Sprintf("engine-%d", i))
	}
	now = now.Add(time.Minute)

	d.Stamp("intruder")
	d.Stamp("engine-0") // known name: must refresh despite the full tracker

	d.mu.RLock()
	tracked := len(d.last)
	_, intruderKnown := d.last["intruder"]
	refreshed := d.last["engine-0"]
	dropped := d.dropped
	d.mu.RUnlock()

	if tracked != maxTrackedEngines {
		t.Errorf("tracked engines = %d, want capped at %d", tracked, maxTrackedEngines)
	}
	if intruderKnown {
		t.Error("stamp past the size cap was admitted")
	}
	if !refreshed.Equal(now) {
		t.Errorf("known engine stamp = %v, want refreshed to %v", refreshed, now)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}

	// A shed for an unadmitted name must not sneak the name into the shed
	// map either, and the drop has to be visible on the exposition.
	if d.AcquireHold("intruder", 0) {
		t.Fatal("AcquireHold admitted a request at a cap of 0")
	}
	d.mu.RLock()
	_, shedKnown := d.shed["intruder"]
	d.mu.RUnlock()
	if shedKnown {
		t.Error("shed count created an entry for an untracked engine")
	}
	if !strings.Contains(d.Render(), MetricDemandDroppedTotal+" 1") {
		t.Errorf("dropped stamps not visible in render:\n%s", d.Render())
	}
}

// fakeStore is a sliceStore backed by a literal slice.
type fakeStore []interface{}

func (f fakeStore) List() []interface{} { return f }

func slice(engine string, ready ...bool) *discoveryv1.EndpointSlice {
	s := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{serviceNameLabel: engine},
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
func TestRecomputeServiceAcrossMultipleSlices(t *testing.T) {
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
			if got := recomputeService(tc.store, tc.engine); got != tc.want {
				t.Errorf("recomputeService(%s) = %v, want %v", tc.engine, got, tc.want)
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
	// Demand exposition does not depend on routing registration.
	a.readiness.MarkSynced()
	return a
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
