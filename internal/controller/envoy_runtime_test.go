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
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestEnvoyRuntimeHealthyDNSWithdrawal(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(fmt.Sprintf("retain_healthy_removed_host=%t", retain), func(t *testing.T) {
			var control func(map[string]any)
			if retain {
				control = func(config map[string]any) { dfpCluster(t, config)["ignore_health_on_host_removal"] = false }
			}
			f := startRuntimeFixture(t, false, control)
			requireRuntimeResponse(t, f.post("WARM"), http.StatusOK, f.old.ip)
			held := make(chan runtimeResponse, 1)
			go func() { held <- f.post("HOLD") }()
			select {
			case <-f.old.started.done:
			case <-time.After(10 * time.Second):
				t.Fatal("engine did not receive held request")
			}
			f.dns.set([]string{f.new.ip}, dnsmessage.RCodeSuccess)
			runtimeEventually(t, "replacement healthy and DNS withdrawal observed", func() bool {
				// Exercise actual routing throughout refresh, including the first
				// health check of the replacement. Neither backend stops serving.
				response := f.post("DURING")
				if response.err != nil || response.status != http.StatusOK {
					t.Fatalf("request failed during healthy DNS withdrawal: %+v", response)
				}
				hosts, exists := f.hosts()
				newHost, newExists := hosts[f.new.ip]
				oldHost, oldExists := hosts[f.old.ip]
				return exists && newExists && !newHost.Health.Failed && !newHost.Health.Pending &&
					((retain && oldExists && oldHost.Health.Removing) || (!retain && !oldExists))
			})
			// Host-set publication to workers is asynchronous. Establish routing
			// behavior with requests before checking the steady state below.
			runtimeEventually(t, "requests observe updated worker routing", func() bool {
				seen := make(map[string]bool)
				for range 10 {
					response := f.post("CONVERGE")
					if response.err != nil || response.status != http.StatusOK {
						t.Fatalf("request failed during worker convergence: %+v", response)
					}
					seen[response.body] = true
				}
				return seen[f.new.ip] && seen[f.old.ip] == retain
			})
			seen := make(map[string]bool)
			for range 40 {
				response := f.post("AFTER")
				if response.err != nil || response.status != http.StatusOK {
					t.Fatalf("request failed after DNS withdrawal: %+v", response)
				}
				seen[response.body] = true
			}
			if !seen[f.new.ip] || seen[f.old.ip] != retain {
				t.Fatalf("post-withdrawal backends = %v, retain old = %t", seen, retain)
			}
			f.old.release.release()
			requireRuntimeResponse(t, <-held, http.StatusOK, f.old.ip)
		})
	}
}

func TestEnvoyRuntimeHealthFailureAndPanic(t *testing.T) {
	f := startRuntimeFixture(t, false, nil)
	f.dns.set([]string{f.old.ip, f.new.ip}, dnsmessage.RCodeSuccess)
	response := f.post("WARM")
	if response.err != nil || response.status != http.StatusOK {
		t.Fatalf("warm request failed: %+v", response)
	}
	waitHealth := func(oldFailed, newFailed bool) {
		t.Helper()
		runtimeEventually(t, "both DNS-present hosts have expected health", func() bool {
			hosts, exists := f.hosts()
			oldHost, oldExists := hosts[f.old.ip]
			newHost, newExists := hosts[f.new.ip]
			return exists && oldExists && newExists && !oldHost.Health.Pending && !newHost.Health.Pending &&
				oldHost.Health.Failed == oldFailed && newHost.Health.Failed == newFailed
		})
	}
	waitHealth(false, false)
	f.old.ready.Store(false)
	waitHealth(true, false)
	runtimeEventually(t, "workers exclude the unhealthy host", func() bool {
		allHealthy := true
		for range 10 {
			response := f.post("HEALTH_CONVERGE")
			if response.err != nil || response.status != http.StatusOK {
				t.Fatalf("request failed while a healthy host remains: %+v", response)
			}
			allHealthy = allHealthy && response.body == f.new.ip
		}
		return allHealthy
	})
	for range 20 {
		requireRuntimeResponse(t, f.post("ONE_HEALTHY"), http.StatusOK, f.new.ip)
	}
	// Readiness fails while the query listeners remain available. The generated
	// cluster's default panic threshold permits routing to unhealthy hosts when
	// all hosts fail: health flags alone cannot prove a host is excluded.
	f.new.ready.Store(false)
	waitHealth(true, true)
	before := f.stat(".lb_healthy_panic")
	seen := make(map[string]bool)
	runtimeEventually(t, "workers route to both unhealthy hosts in panic mode", func() bool {
		response := f.post("PANIC")
		if response.err != nil || response.status != http.StatusOK {
			t.Fatalf("panic-mode request failed: %+v", response)
		}
		seen[response.body] = true
		return seen[f.old.ip] && seen[f.new.ip] && f.stat(".lb_healthy_panic") > before
	})
	f.old.ready.Store(true)
	f.new.ready.Store(true)
	waitHealth(false, false)
}

func TestEnvoyRuntimeDNSResponsesAndRepopulation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   dnsmessage.RCode
		retain bool
	}{
		{"empty", dnsmessage.RCodeSuccess, false},
		{"nxdomain", dnsmessage.RCodeNameError, false},
		{"servfail", dnsmessage.RCodeServerFailure, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startRuntimeFixture(t, false, nil)
			requireRuntimeResponse(t, f.post("WARM"), http.StatusOK, f.old.ip)
			before := f.stat(".update_failure")
			f.dns.set(nil, tc.code)
			if tc.retain {
				runtimeEventually(t, "failed DNS refresh", func() bool { return f.stat(".update_failure") > before })
				hosts, exists := f.hosts()
				if _, present := hosts[f.old.ip]; !exists || !present {
					t.Fatal("SERVFAIL discarded the existing host")
				}
				requireRuntimeResponse(t, f.post(runtimeQuery), http.StatusOK, f.old.ip)
			} else {
				runtimeEventually(t, "existing subcluster has zero hosts", func() bool {
					hosts, exists := f.hosts()
					return exists && len(hosts) == 0
				})
				runtimeEventually(t, "requests observe empty worker host sets", func() bool {
					allEmpty := true
					for range 10 {
						response := f.post("EMPTY_CONVERGE")
						if response.err == nil && response.status == http.StatusOK && response.body == f.old.ip {
							allEmpty = false
							continue
						}
						requireRuntimeResponse(t, response, http.StatusServiceUnavailable, "no healthy upstream")
					}
					return allEmpty
				})
				beforeDeliveries := len(f.old.received())
				requireRuntimeResponse(t, f.post(runtimeQuery), http.StatusServiceUnavailable, "no healthy upstream")
				if len(f.old.received()) != beforeDeliveries || len(f.new.received()) != 0 {
					t.Fatal("request reached an engine despite an empty host set")
				}
			}
			f.dns.set([]string{f.new.ip}, dnsmessage.RCodeSuccess)
			runtimeEventually(t, "requests use replacement after DNS repopulation", func() bool {
				allNew := true
				for range 10 {
					response := f.post("RECOVER")
					if response.err == nil && response.status == http.StatusOK && response.body == f.new.ip {
						continue
					}
					allNew = false
					if tc.retain && response.err == nil && response.status == http.StatusOK && response.body == f.old.ip {
						continue
					}
					requireRuntimeResponse(t, response, http.StatusServiceUnavailable, "no healthy upstream")
				}
				return allNew
			})
			for range 20 {
				requireRuntimeResponse(t, f.post("RECOVERED"), http.StatusOK, f.new.ip)
			}
		})
	}
}

func TestEnvoyRuntimeRequestDelivery(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		reset, unsafe, reject, drained bool
		status, deliveries             int
	}{
		{name: "reset_after_delivery", reset: true, status: http.StatusServiceUnavailable, deliveries: 1},
		{name: "unsafe_reset_negative_control", reset: true, unsafe: true, status: http.StatusOK, deliveries: 2},
		{name: "unmarked_503", reject: true, status: http.StatusServiceUnavailable, deliveries: 1},
		{name: "drained_before_work", drained: true, status: http.StatusOK, deliveries: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var control func(map[string]any)
			if tc.unsafe {
				control = func(config map[string]any) {
					rp := dfpRouteRetryPolicy(t, config)
					rp["retry_on"] = strings.ReplaceAll(rp["retry_on"].(string), "reset-before-request", "reset")
				}
			}
			f := startRuntimeFixture(t, false, control)
			f.old.resetOnce.Store(tc.reset)
			f.old.reject.Store(tc.reject)
			f.old.drained.Store(tc.drained)
			response := f.post(runtimeQuery)
			if response.err != nil || response.status != tc.status {
				t.Fatalf("response = %+v, want status %d", response, tc.status)
			}
			got := f.old.received()
			if len(got) != tc.deliveries {
				t.Fatalf("engine received %d requests, want %d: %v", len(got), tc.deliveries, got)
			}
			for _, body := range got {
				if body != runtimeQuery {
					t.Fatalf("engine received body %q, want %q", body, runtimeQuery)
				}
			}
			if len(f.new.received()) != 0 {
				t.Fatal("request reached an engine absent from DNS")
			}
		})
	}
}

func TestEnvoyRuntimeWarmEmptyWakeProbeGate(t *testing.T) {
	f := startRuntimeFixture(t, true, nil)
	requireRuntimeResponse(t, f.post("WARM"), http.StatusOK, f.old.ip)
	f.parked.Store(true)
	f.dns.set(nil, dnsmessage.RCodeSuccess)
	held := make(chan runtimeResponse, 1)
	go func() { held <- f.post(runtimeQuery) }()
	select {
	case <-f.holding.done:
	case <-time.After(10 * time.Second):
		t.Fatal("query did not reach wake admission")
	}
	runtimeEventually(t, "warmed subcluster exists with zero hosts", func() bool {
		hosts, exists := f.hosts()
		return exists && len(hosts) == 0
	})
	// The replacement is ready, but its DNS answer cannot reach Envoy until
	// the test explicitly releases the response gate.
	dnsRelease := newRuntimeLatch()
	t.Cleanup(dnsRelease.release)
	dnsRequested := f.dns.block([]string{f.new.ip}, dnsmessage.RCodeSuccess, dnsRelease)
	select {
	case <-dnsRequested.done:
	case <-time.After(10 * time.Second):
		t.Fatal("resolver did not request the frozen DNS answer")
	}
	f.probeGate.Store(true)
	f.release.release()
	// One second of controlled DNS latency gives the request a chance to resume
	// while discovery is unavailable. This is a bounded observation, not a
	// worker acknowledgement: a descheduled worker can hide early continuation.
	observation := time.NewTimer(time.Second)
	defer observation.Stop()
	select {
	case response := <-held:
		if len(f.old.received()) != 1 || len(f.new.received()) != 0 {
			t.Fatalf("query reached an engine before DNS release: old=%v new=%v", f.old.received(), f.new.received())
		}
		t.Fatalf("query completed before DNS release: (%d, %q, %v); want it held, then 200 after DNS release; engine query deliveries=0",
			response.status, response.body, response.err)
	case <-observation.C:
	}
	dnsRelease.release()
	requireRuntimeResponse(t, <-held, http.StatusOK, f.new.ip)
}
