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
	"sync"
	"time"
)

// handoffTestBarrier exists only in the dedicated Helm CI build. Arm before
// sending a query, observe entry, then release after the stable idle window.
type handoffTestBarrier struct {
	mu    sync.Mutex
	gates map[string]*handoffTestGate
}
type handoffTestGate struct {
	release chan struct{}
	entered bool
}

func (b *handoffTestBarrier) register(mux *http.ServeMux, readiness *readinessTracker) {
	mux.HandleFunc("/test/handoff", func(w http.ResponseWriter, r *http.Request) {
		engine := r.URL.Query().Get("engine")
		if !isValidEngineName(engine) {
			http.Error(w, "invalid engine", http.StatusBadRequest)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		gate := b.gates[engine]
		switch r.Method {
		case http.MethodPost:
			// An Engine status transition is not an EndpointSlice cache barrier.
			// Admit the test only when this exact agent will take the hold path.
			if !readiness.Synced() || readiness.IsReady(engine) {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if gate != nil {
				http.Error(w, "already armed", http.StatusConflict)
				return
			}
			if b.gates == nil {
				b.gates = make(map[string]*handoffTestGate)
			}
			b.gates[engine] = &handoffTestGate{release: make(chan struct{})}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			switch {
			case gate == nil:
				w.WriteHeader(http.StatusNotFound)
			case gate.entered:
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusAccepted)
			}
		case http.MethodDelete:
			if gate != nil {
				close(gate.release)
				delete(b.gates, engine)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func (b *handoffTestBarrier) wait(ctx context.Context, engine string, deadline <-chan time.Time) routeWait {
	b.mu.Lock()
	gate := b.gates[engine]
	if gate != nil {
		gate.entered = true
	}
	b.mu.Unlock()
	if gate == nil {
		return routeWaitRoutable
	}
	select {
	case <-gate.release:
		return routeWaitRoutable
	case <-deadline:
		return routeWaitDeadline
	case <-ctx.Done():
		return routeWaitClientGone
	}
}
