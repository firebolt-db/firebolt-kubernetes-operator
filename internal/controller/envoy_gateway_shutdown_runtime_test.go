//go:build envoy_integration

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
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// The short drain timeout is a fixture input. Requests stay held independently
// of that timeout, so a timer alone cannot make either completion check pass.
func TestEnvoyRuntimeGatewayShutdown(t *testing.T) {
	for _, name := range []string{"signal_with_requests_pending", "close_listeners_then_complete_requests", "zero_drain_period_then_complete_requests"} {
		t.Run(name, func(t *testing.T) {
			explicitDrain := name != "signal_with_requests_pending"
			drainTime := "1"
			if name == "zero_drain_period_then_complete_requests" {
				drainTime = "0"
			}
			f := startRuntimeFixture(t, true, nil, "--drain-time-s", drainTime, "--drain-strategy", "immediate")
			requireRuntimeResponse(t, f.post("WARM"), http.StatusOK, f.old.ip)
			active := make(chan runtimeResponse, 1)
			go func() { active <- f.post("HOLD") }()
			waitRuntimeSignal(t, "engine received active query", f.old.started.done)
			f.parked.Store(true)
			parked := make(chan runtimeResponse, 1)
			go func() { parked <- f.post(runtimeQuery) }()
			waitRuntimeSignal(t, "wake agent received parked query", f.holding.done)
			runtimeEventually(t, "both queries counted as active downstream requests", func() bool {
				return gatewayRuntimeStat(t, f, "http.gateway.downstream_rq_active") == 2
			})
			requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/healthcheck/fail"), http.StatusOK, "OK\n")
			if explicitDrain {
				if drainTime == "0" {
					requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners?graceful"), http.StatusOK, "OK\n")
				} else {
					requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners?graceful&skip_exit"), http.StatusOK, "OK\n")
					requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners"), http.StatusOK, "OK\n")
				}
				runtimeEventually(t, "query listener no longer accepts connections", func() bool {
					return !gatewayRuntimeAccepts(t, f)
				})
				if gatewayRuntimeStat(t, f, "http.gateway.downstream_rq_active") != 2 {
					t.Fatal("listener closure discarded an active or parked query")
				}
				select {
				case response := <-active:
					t.Fatalf("active query ended before engine release: %+v", response)
				case response := <-parked:
					t.Fatalf("parked query ended before wake release: %+v", response)
				default:
				}
				f.release.release()
				requireRuntimeResponse(t, <-parked, http.StatusOK, f.old.ip)
				f.old.release.release()
				requireRuntimeResponse(t, <-active, http.StatusOK, f.old.ip)
				runtimeEventually(t, "downstream connections finished", func() bool {
					return gatewayRuntimeStat(t, f, "http.gateway.downstream_cx_active") == 0
				})
				got := f.old.received()
				if len(got) != 3 || got[0] != "WARM" || got[1] != "HOLD" || got[2] != runtimeQuery {
					t.Fatalf("engine deliveries = %v, want each accepted request exactly once", got)
				}
			}
			if err := f.process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			waitRuntimeSignal(t, "Envoy exited", f.exited)
			if !explicitDrain {
				for _, response := range []runtimeResponse{<-active, <-parked} {
					if response.err == nil {
						t.Fatalf("expected transport interruption from terminating pending requests, got %+v", response)
					}
					t.Logf("SIGTERM interrupted pending request: %v", response.err)
				}
				if got := f.old.received(); len(got) != 2 {
					t.Fatalf("parked query reached engine before wake release: %v", got)
				}
			}
		})
	}
}

func TestEnvoyRuntimeGatewayGracefulSkipExit(t *testing.T) {
	f := startRuntimeFixture(t, false, nil, "--drain-time-s", "1", "--drain-strategy", "immediate")
	requireRuntimeResponse(t, f.post("WARM"), http.StatusOK, f.old.ip)
	requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners?skip_exit"), http.StatusBadRequest, "skip_exit requires graceful\n")
	requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners?graceful&skip_exit"), http.StatusOK, "OK\n")
	// Wait beyond the configured drain period to inspect what skip_exit actually
	// leaves running. This wait belongs to the protocol-under-test, not cleanup.
	timer := time.NewTimer(1100 * time.Millisecond)
	defer timer.Stop()
	<-timer.C
	if !gatewayRuntimeAccepts(t, f) {
		t.Fatal("graceful skip_exit unexpectedly closed accepting sockets")
	}
	requireRuntimeResponse(t, f.post("AFTER_DRAIN_PERIOD"), http.StatusOK, f.old.ip)
	requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners"), http.StatusOK, "OK\n")
	runtimeEventually(t, "explicit stop closes accepting sockets", func() bool { return !gatewayRuntimeAccepts(t, f) })
	if response := f.get("/server_info"); response.err != nil || response.status != http.StatusOK {
		t.Fatalf("listener drain stopped the Envoy admin process: %+v", response)
	}
}

func TestEnvoyRuntimeGatewayNeedsWakeAgentThroughDrain(t *testing.T) {
	f := startRuntimeFixture(t, true, nil, "--drain-time-s", "1", "--drain-strategy", "immediate")
	// Mandatory admission fails closed. A parked query needs its processor
	// stream to remain alive until it can receive a valid route.
	f.dns.set(nil, dnsmessage.RCodeSuccess)
	f.parked.Store(true)
	parked := make(chan runtimeResponse, 1)
	go func() { parked <- f.post(runtimeQuery) }()
	waitRuntimeSignal(t, "wake agent received parked query", f.holding.done)
	requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners?graceful&skip_exit"), http.StatusOK, "OK\n")
	requireRuntimeResponse(t, gatewayRuntimeAdmin(f, "/drain_listeners"), http.StatusOK, "OK\n")
	f.wake.Stop()
	response := <-parked
	if response.err != nil || response.status != http.StatusInternalServerError {
		t.Fatalf("wake agent interruption response = %+v, want a failed held query", response)
	}
	if len(f.old.received()) != 0 || len(f.new.received()) != 0 {
		t.Fatal("query was sent to an engine after wake admission failed")
	}
	t.Logf("terminating wake admission during gateway drain returns %d: %s", response.status, response.body)
}

func gatewayRuntimeAdmin(f *runtimeFixture, path string) runtimeResponse {
	return f.request(http.MethodPost, f.admin+path, "")
}

func gatewayRuntimeAccepts(t *testing.T, f *runtimeFixture) bool {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(t.Context(), "tcp", strings.TrimPrefix(f.query, "http://"))
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return false
		}
		t.Fatalf("cannot probe query listener: %v", err)
	}
	_ = conn.Close() // Probe connection carries no request body.
	return true
}

func gatewayRuntimeStat(t *testing.T, f *runtimeFixture, name string) uint64 {
	t.Helper()
	response := f.get("/stats?format=json")
	var stats struct {
		Stats []struct {
			Name  string `json:"name"`
			Value uint64 `json:"value"`
		} `json:"stats"`
	}
	if response.err != nil || response.status != http.StatusOK || json.Unmarshal([]byte(response.body), &stats) != nil {
		t.Fatalf("cannot read Envoy stats: %+v", response)
	}
	for _, stat := range stats.Stats {
		if stat.Name == name {
			return stat.Value
		}
	}
	t.Fatalf("stat %q not found", name)
	return 0
}

func waitRuntimeSignal(t *testing.T, description string, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}
