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
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A loopback readiness probe initializes the DFP cluster and observes its DNS
// targets. A successful request and a main-thread host sample do not acknowledge
// route withdrawal on every Envoy worker or prevent a later stale DNS answer.
func TestEnvoyRuntimeReadinessProbeObservesDNS(t *testing.T) {
	f := startRuntimeFixture(t, false, func(config map[string]any) {
		addRuntimeReadinessProbe(t, config)
	})
	probeURL := withdrawalProbeURL(t, f)
	requireRuntimeResponse(t, f.request(http.MethodPost, probeURL, runtimeQuery), http.StatusNotFound, "")
	if _, exists := f.hosts(); exists {
		t.Fatal("cold gateway unexpectedly has an engine subcluster")
	}
	status, address := withdrawalProbe(t, f, probeURL)
	if status != http.StatusOK || address != f.old.ip {
		t.Fatalf("cold probe = (%d, %q), want healthy old address", status, address)
	}
	if withdrawalProbeAcknowledges(f, status, address) {
		t.Fatal("stale DNS incorrectly acknowledged replacement withdrawal")
	}
	if len(f.old.received()) != 0 || len(f.new.received()) != 0 {
		t.Fatal("readiness probe executed a query")
	}

	held := make(chan runtimeResponse, 1)
	go func() { held <- f.post("HOLD") }()
	select {
	case <-f.old.started.done:
	case <-time.After(10 * time.Second):
		t.Fatal("old engine did not receive long request")
	}

	dnsRelease := newRuntimeLatch()
	defer dnsRelease.release()
	requested := f.dns.block([]string{f.new.ip}, dnsmessage.RCodeSuccess, dnsRelease)
	select {
	case <-requested.done:
	case <-time.After(10 * time.Second):
		t.Fatal("Envoy did not request its next DNS refresh")
	}
	status, address = withdrawalProbe(t, f, probeURL)
	if status != http.StatusOK || address != f.old.ip || withdrawalProbeAcknowledges(f, status, address) {
		t.Fatalf("blocked DNS probe = (%d, %q), must remain old and unacknowledged", status, address)
	}
	dnsRelease.release()
	runtimeEventually(t, "probe routes to replacement and old hosts are withdrawn", func() bool {
		status, address := withdrawalProbe(t, f, probeURL)
		return withdrawalProbeAcknowledges(f, status, address)
	})
	select {
	case response := <-held:
		t.Fatalf("withdrawal interrupted the old in-flight request: %+v", response)
	default:
	}
	requireRuntimeResponse(t, f.post(runtimeQuery), http.StatusOK, f.new.ip)
	f.old.release.release()
	requireRuntimeResponse(t, <-held, http.StatusOK, f.old.ip)
	if old, next := f.old.received(), f.new.received(); len(old) != 1 || old[0] != "HOLD" || len(next) != 1 || next[0] != runtimeQuery {
		t.Fatalf("probe must not execute queries: old=%v new=%v", old, next)
	}
}

func TestEnvoyRuntimeWithdrawalProbeRejectsRetainedOldHost(t *testing.T) {
	f := startRuntimeFixture(t, false, func(config map[string]any) {
		addRuntimeReadinessProbe(t, config)
		dfpCluster(t, config)["ignore_health_on_host_removal"] = false
	})
	probeURL := withdrawalProbeURL(t, f)
	status, address := withdrawalProbe(t, f, probeURL)
	if status != http.StatusOK || address != f.old.ip {
		t.Fatalf("cold probe = (%d, %q), want healthy old address", status, address)
	}
	f.dns.set([]string{f.new.ip}, dnsmessage.RCodeSuccess)
	runtimeEventually(t, "a probe selects the replacement while old host remains retained", func() bool {
		status, address := withdrawalProbe(t, f, probeURL)
		if withdrawalProbeAcknowledges(f, status, address) {
			t.Fatal("a successful replacement probe alone must not acknowledge retained old hosts")
		}
		hosts, exists := f.hosts()
		old, oldPresent := hosts[f.old.ip]
		return exists && oldPresent && old.Health.Removing && status == http.StatusOK && address == f.new.ip
	})
	if len(f.old.received()) != 0 || len(f.new.received()) != 0 {
		t.Fatal("readiness probes executed a query")
	}
}

// This controlled resolver sequence demonstrates possibility, not the frequency
// of a stale answer after convergence in Kubernetes. A successful observation
// does not make subsequent DNS answers monotonic.
func TestEnvoyRuntimeWithdrawalProbeDNSRegression(t *testing.T) {
	f := startRuntimeFixture(t, false, func(config map[string]any) {
		addRuntimeReadinessProbe(t, config)
	})
	probeURL := withdrawalProbeURL(t, f)
	status, address := withdrawalProbe(t, f, probeURL)
	if status != http.StatusOK || address != f.old.ip {
		t.Fatalf("cold probe = (%d, %q), want healthy old address", status, address)
	}
	f.dns.set([]string{f.new.ip}, dnsmessage.RCodeSuccess)
	runtimeEventually(t, "replacement acknowledged before DNS regression", func() bool {
		status, address := withdrawalProbe(t, f, probeURL)
		return withdrawalProbeAcknowledges(f, status, address)
	})
	f.dns.set([]string{f.old.ip}, dnsmessage.RCodeSuccess)
	runtimeEventually(t, "a later old DNS answer invalidates the observation", func() bool {
		status, address := withdrawalProbe(t, f, probeURL)
		return status == http.StatusOK && address == f.old.ip && !withdrawalProbeAcknowledges(f, status, address)
	})
	runtimeEventually(t, "an actual query selects the old engine again", func() bool {
		response := f.post("AFTER_DNS_REGRESSION")
		if response.err != nil || response.status != http.StatusOK {
			t.Fatalf("query failed while both engines remain healthy: %+v", response)
		}
		return response.body == f.old.ip
	})
}

func addRuntimeReadinessProbe(t *testing.T, config map[string]any) {
	t.Helper()
	root := config["static_resources"].(map[string]any)
	listeners := root["listeners"].([]any)
	var probe map[string]any
	for _, raw := range listeners {
		listener := raw.(map[string]any)
		if listener["name"] != "listener" {
			continue
		}
		encoded, err := json.Marshal(listener)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &probe); err != nil {
			t.Fatal(err)
		}
	}
	if probe == nil {
		t.Fatal("production query listener missing")
	}
	probe["name"] = "withdrawal_probe"
	hcm := probe["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	hcm["stat_prefix"] = "withdrawal_probe"
	routeConfig := hcm["route_config"].(map[string]any)
	routeConfig["name"] = "withdrawal_probe"
	virtualHost := routeConfig["virtual_hosts"].([]any)[0].(map[string]any)
	route := virtualHost["routes"].([]any)[0].(map[string]any)
	route["match"] = map[string]any{
		"path":    "/health/ready",
		"headers": []any{map[string]any{"name": ":method", "string_match": map[string]any{"exact": "GET"}}},
	}
	route["response_headers_to_add"] = []any{map[string]any{
		"header":        map[string]any{"key": "x-withdrawal-upstream", "value": "%UPSTREAM_HOST%"},
		"append_action": "OVERWRITE_IF_EXISTS_OR_ADD",
	}}
	root["listeners"] = append(listeners, probe)
}

func withdrawalProbeURL(t *testing.T, f *runtimeFixture) string {
	t.Helper()
	response := f.get("/listeners?format=json")
	var listeners struct {
		Statuses []struct {
			Name    string `json:"name"`
			Address struct {
				Socket struct {
					Address string `json:"address"`
					Port    int    `json:"port_value"`
				} `json:"socket_address"`
			} `json:"local_address"`
		} `json:"listener_statuses"`
	}
	if response.err != nil || response.status != http.StatusOK {
		t.Fatalf("read probe listener: %+v", response)
	}
	if err := json.Unmarshal([]byte(response.body), &listeners); err != nil {
		t.Fatal(err)
	}
	for _, listener := range listeners.Statuses {
		if listener.Name == "withdrawal_probe" {
			return "http://" + net.JoinHostPort(listener.Address.Socket.Address, strconv.Itoa(listener.Address.Socket.Port)) + "/health/ready"
		}
	}
	t.Fatal("probe listener missing")
	return ""
}

func withdrawalProbe(t *testing.T, f *runtimeFixture, url string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Firebolt-Engine", "probe")
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }() // The response is drained below.
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	address, _, err := net.SplitHostPort(response.Header.Get("X-Withdrawal-Upstream"))
	if err != nil {
		t.Fatalf("probe upstream header = %q: %v", response.Header.Get("X-Withdrawal-Upstream"), err)
	}
	return response.StatusCode, address
}

func withdrawalProbeAcknowledges(f *runtimeFixture, status int, address string) bool {
	hosts, exists := f.hosts()
	_, oldPresent := hosts[f.old.ip]
	_, newPresent := hosts[f.new.ip]
	return status == http.StatusOK && address == f.new.ip && exists && newPresent && !oldPresent
}
