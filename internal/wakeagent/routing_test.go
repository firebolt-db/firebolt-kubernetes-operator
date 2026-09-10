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
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEngineAuthority(t *testing.T) {
	t.Parallel()
	got := EngineAuthority("analytics", "ns-1")
	want := "analytics-service.ns-1.svc.cluster.local:3473"
	if got != want {
		t.Errorf("EngineAuthority() = %q, want %q", got, want)
	}
}

// probeAgent is testAgent with a routing probe pointed at url.
func probeAgent(t *testing.T, holdTimeout time.Duration, url string) *Agent {
	t.Helper()
	a := New(Config{
		Namespace:     "test-ns",
		HoldTimeout:   holdTimeout,
		FallbackCap:   8,
		RouteProbeURL: url,
	})
	a.readiness.MarkSynced()
	return a
}

// startHold runs handleHold for the engine in the background and returns the
// channel its recorder arrives on once the handler finishes.
func startHold(t *testing.T, a *Agent, engine string) <-chan *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
			"/hold?engine="+engine, http.NoBody))
		done <- rec
	}()
	return done
}

// Ready endpoints alone must not release a hold: Envoy's own view of the
// engine can lag them, and a request released into that lag dies as a local
// 503 no retry policy can see. The hold stays parked while Envoy answers
// 503 through the probe, and releases promptly once it answers 200.
func TestHandleHoldWaitsForEnvoyRoutability(t *testing.T) {
	t.Parallel()
	var routable atomic.Bool
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if routable.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer probe.Close()

	a := probeAgent(t, 30*time.Second, probe.URL+"/health/ready")
	done := startHold(t, a, "stopped")
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	a.readiness.setReady("stopped", true)

	// Several probe ticks pass with Envoy still answering 503; the hold
	// must not release on the readiness edge alone.
	select {
	case rec := <-done:
		t.Fatalf("hold released with status %d while Envoy answered 503", rec.Code)
	case <-time.After(400 * time.Millisecond):
	}

	routable.Store(true)
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 once Envoy answers 200", rec.Code)
		}
		if got := rec.Header().Get(DecisionHeader); got != DecisionReleased {
			t.Errorf("%s = %q, want %q", DecisionHeader, got, DecisionReleased)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold not released after the probe turned 200")
	}
}

// The probe must present the engine's authority as Host — the exact string
// the Lua filter rewrites queries to — or Envoy would warm and observe a
// different dynamic-forward-proxy sub-cluster than the released query uses.
func TestRouteProbeCarriesEngineAuthority(t *testing.T) {
	t.Parallel()
	type seen struct{ host, path string }
	got := make(chan seen, 1)
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- seen{host: r.Host, path: r.URL.Path}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer probe.Close()

	a := probeAgent(t, 5*time.Second, probe.URL+"/health/ready")
	done := startHold(t, a, "analytics")
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	a.readiness.setReady("analytics", true)

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold not released by a probe that answers 200")
	}

	s := <-got
	if want := EngineAuthority("analytics", "test-ns"); s.host != want {
		t.Errorf("probe Host = %q, want %q", s.host, want)
	}
	if s.path != "/health/ready" {
		t.Errorf("probe path = %q, want /health/ready", s.path)
	}
}

// An empty RouteProbeURL is the escape hatch: no probe at all, release on
// endpoint readiness alone. The nil deadline channel would block forever,
// so returning at all proves the probe path was skipped rather than raced.
func TestAwaitRoutableSkipsWhenProbeDisabled(t *testing.T) {
	t.Parallel()
	a := testAgent(t, time.Minute) // no RouteProbeURL configured
	if got := a.awaitRoutable(context.Background(), "engine", nil); got != routeWaitRoutable {
		t.Errorf("awaitRoutable() = %v with probing disabled, want %v", got, routeWaitRoutable)
	}
}

// With probing disabled the hold path behaves exactly as it does without
// the probe feature: parked on readiness, released on the readiness edge.
func TestHandleHoldReleasesOnReadinessAloneWithoutProbeURL(t *testing.T) {
	t.Parallel()
	a := testAgent(t, 5*time.Second)
	done := startHold(t, a, "plain")
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	a.readiness.setReady("plain", true)

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get(DecisionHeader); got != DecisionReleased {
			t.Errorf("%s = %q, want %q", DecisionHeader, got, DecisionReleased)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold not released on the readiness edge with probing disabled")
	}
}

// A probe that never succeeds must not extend the hold: the deadline still
// fires, and since the engine's endpoints are ready the hold releases into
// them — the probe delays a release, it never converts one into an error,
// so an unroutable probe target degrades to the probe-disabled behavior.
func TestHandleHoldDeadlineReleasesReadyEngineWhenNeverRoutable(t *testing.T) {
	t.Parallel()
	var probes atomic.Int64
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer probe.Close()

	const holdTimeout = 400 * time.Millisecond
	a := probeAgent(t, holdTimeout, probe.URL+"/health/ready")
	start := time.Now()
	done := startHold(t, a, "stuck")
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	a.readiness.setReady("stuck", true)

	select {
	case rec := <-done:
		if elapsed := time.Since(start); elapsed < holdTimeout {
			t.Errorf("hold answered after %v, before the %v deadline", elapsed, holdTimeout)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for a ready engine at the deadline", rec.Code)
		}
		if got := rec.Header().Get(DecisionHeader); got != DecisionReleased {
			t.Errorf("%s = %q, want %q", DecisionHeader, got, DecisionReleased)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold not answered at the deadline despite a failing probe")
	}
	if probes.Load() == 0 {
		t.Error("probe target was never asked; the routability gate did not run")
	}
}

// An engine that flaps away while its release is still being probed gets the
// same terminal answer as one that never arrived: the deadline fires and the
// endpoints are gone, so the client sees the timeout 503.
func TestHandleHoldDeadlineErrorsWhenEngineGoneMidProbe(t *testing.T) {
	t.Parallel()
	var probes atomic.Int64
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer probe.Close()

	a := probeAgent(t, 500*time.Millisecond, probe.URL+"/health/ready")
	done := startHold(t, a, "flappy")
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	a.readiness.setReady("flappy", true)
	waitFor(t, func() bool { return probes.Load() > 0 })
	a.readiness.setReady("flappy", false)

	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 when the engine left mid-probe", rec.Code)
		}
		if got := rec.Header().Get(DecisionHeader); got != DecisionTimeout {
			t.Errorf("%s = %q, want %q", DecisionHeader, got, DecisionTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold not answered at the deadline")
	}
}

// Holds poll independently, so a wake herd would multiply into as many probe
// streams as parked requests — each one a fresh connection to an engine that
// just cold-started. Concurrent probes for one authority must coalesce into
// a single in-flight request.
func TestConcurrentHoldsShareOneProbeStream(t *testing.T) {
	t.Parallel()
	var (
		mu                sync.Mutex
		inFlight, maxSeen int
		routable          atomic.Bool
		enter             = func() {
			mu.Lock()
			inFlight++
			if inFlight > maxSeen {
				maxSeen = inFlight
			}
			mu.Unlock()
		}
		leave = func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}
	)
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		enter()
		defer leave()
		// Widen each probe so independent pollers would overlap.
		time.Sleep(20 * time.Millisecond)
		if routable.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer probe.Close()

	const holds = 8
	a := probeAgent(t, 10*time.Second, probe.URL+"/health/ready")
	var recs []<-chan *httptest.ResponseRecorder
	for i := 0; i < holds; i++ {
		recs = append(recs, startHold(t, a, "shared"))
	}
	waitFor(t, func() bool { return a.demand.PendingTotal() == holds })
	a.readiness.setReady("shared", true)

	// Let a few failing probe rounds pass with all holds awake, then open.
	time.Sleep(300 * time.Millisecond)
	routable.Store(true)

	for i, done := range recs {
		select {
		case rec := <-done:
			if rec.Code != http.StatusOK {
				t.Errorf("hold %d: status = %d, want 200", i, rec.Code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("hold %d never released", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if maxSeen != 1 {
		t.Errorf("max concurrent probe requests = %d, want 1 (coalesced)", maxSeen)
	}
}

// A routable verdict is final: endpoints flapping to not-ready between the
// probe's 200 and the hold response must not turn the release into a 503.
// The 200 traveled through Envoy after the flap began, so it is the fresher
// fact; the released query, not the hold, owns any later failure. This pins
// the deliberate absence of a readiness re-check on the success path.
func TestHandleHoldReleasesOnProbeSuccessDespiteEndpointFlap(t *testing.T) {
	t.Parallel()
	probeEntered := make(chan struct{})
	probeRelease := make(chan struct{})
	var entered sync.Once
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered.Do(func() { close(probeEntered) })
		<-probeRelease
		w.WriteHeader(http.StatusOK)
	}))
	defer probe.Close()

	a := probeAgent(t, 30*time.Second, probe.URL+"/health/ready")
	done := startHold(t, a, "flappy")
	waitFor(t, func() bool { return a.demand.PendingTotal() == 1 })
	a.readiness.setReady("flappy", true)

	// The hold woke on readiness and is now inside the probe; flap the
	// endpoints away before the probe answers.
	<-probeEntered
	a.readiness.setReady("flappy", false)
	close(probeRelease)

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("hold answered %d after a routable verdict, want 200", rec.Code)
		}
		if got := rec.Header().Get(DecisionHeader); got != DecisionReleased {
			t.Fatalf("decision = %q, want %q", got, DecisionReleased)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not answer after the probe returned 200")
	}
}
