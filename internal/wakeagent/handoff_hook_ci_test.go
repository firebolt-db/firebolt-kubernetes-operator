//go:build wakehandofftest

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
	"testing"
	"time"
)

func barrierControl(a *Agent, method string) int {
	rec := httptest.NewRecorder()
	a.demandMux().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, "/test/handoff?engine=engine", http.NoBody))
	return rec.Code
}

func TestHandoffBarrierPreventsFastReadyRelease(t *testing.T) {
	a := testAgent(t, time.Second)
	if got := barrierControl(a, http.MethodPost); got != http.StatusCreated {
		t.Fatal(got)
	}
	defer barrierControl(a, http.MethodDelete)
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		a.handleHold(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/hold?engine=engine", http.NoBody))
		done <- rec.Code
	}()
	waitFor(t, func() bool { return barrierControl(a, http.MethodGet) == http.StatusOK })
	a.readiness.setReady("engine", true)
	select {
	case code := <-done:
		t.Fatalf("ready engine bypassed armed barrier: %d", code)
	case <-time.After(20 * time.Millisecond):
	}
	if got := barrierControl(a, http.MethodDelete); got != http.StatusNoContent {
		t.Fatal(got)
	}
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("release did not unblock query")
	}
}

func TestHandoffBarrierHonorsOriginalDeadlineAndCancellation(t *testing.T) {
	for _, cancelClient := range []bool{false, true} {
		a := testAgent(t, 30*time.Millisecond)
		barrierControl(a, http.MethodPost)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan int, 1)
		go func() {
			rec := httptest.NewRecorder()
			a.handleHold(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/hold?engine=engine", http.NoBody))
			done <- rec.Code
		}()
		if cancelClient {
			cancel()
		}
		select {
		case code := <-done:
			if !cancelClient && code != http.StatusServiceUnavailable {
				t.Fatal(code)
			}
		case <-time.After(time.Second):
			t.Fatal("barrier extended deadline or ignored cancellation")
		}
		cancel()
		barrierControl(a, http.MethodDelete)
		if a.demand.PendingTotal() != 0 {
			t.Fatal("barrier leaked hold capacity")
		}
	}
}

func TestHandoffBarrierWaitsForSelectedAgentCache(t *testing.T) {
	a := New(Config{Namespace: "test-ns"})
	if code := barrierControl(a, http.MethodPost); code != http.StatusAccepted {
		t.Fatalf("unsynced agent admitted query: %d", code)
	}
	if code := barrierControl(a, http.MethodGet); code != http.StatusNotFound {
		t.Fatalf("unsynced attempt armed barrier: %d", code)
	}
	a.readiness.MarkSynced()
	a.readiness.setReady("engine", true)
	if code := barrierControl(a, http.MethodPost); code != http.StatusAccepted {
		t.Fatalf("stale ready cache admitted query: %d", code)
	}
	if code := barrierControl(a, http.MethodGet); code != http.StatusNotFound {
		t.Fatalf("ready attempt armed barrier: %d", code)
	}
	a.readiness.setReady("engine", false)
	if code := barrierControl(a, http.MethodPost); code != http.StatusCreated {
		t.Fatalf("converged stopped cache did not arm barrier: %d", code)
	}
	barrierControl(a, http.MethodDelete)
}
