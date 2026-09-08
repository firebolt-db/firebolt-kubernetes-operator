//go:build envoy_integration && unix

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
	"crypto/tls"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

func TestEnvoyRuntimeProductionShutdownHook(t *testing.T) {
	for _, protocol := range []string{"http1", "http2"} {
		t.Run(protocol, func(t *testing.T) {
			f := startRuntimeFixture(t, true, nil, "--drain-time-s", "0", "--drain-strategy", "immediate")
			queries := &runtimeFixture{t: t, query: f.query, client: f.client}
			if protocol == "http2" {
				transport := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, address)
				}}
				t.Cleanup(transport.CloseIdleConnections)
				queries.client = &http.Client{Transport: transport, Timeout: 10 * time.Second}
			}
			requireRuntimeResponse(t, queries.post("WARM"), http.StatusOK, f.old.ip)
			active := make(chan runtimeResponse, 1)
			go func() { active <- queries.post("HOLD") }()
			waitRuntimeSignal(t, "active engine query", f.old.started.done)
			// A wake completing during shutdown must still be able to warm
			// Envoy through its loopback probe listener after public admission closes.
			f.probeGate.Store(true)
			f.parked.Store(true)
			parked := make(chan runtimeResponse, 1)
			go func() { parked <- queries.post(runtimeQuery) }()
			waitRuntimeSignal(t, "held wake request", f.holding.done)
			done := startGatewayRuntimeHook(t, f)
			runtimeEventually(t, "query listener closed by production hook", func() bool { return !gatewayRuntimeAccepts(t, f) })
			select {
			case err := <-done:
				t.Fatalf("hook ended while requests were held: %v", err)
			default:
			}
			f.release.release()
			requireRuntimeResponse(t, <-parked, http.StatusOK, f.old.ip)
			select {
			case err := <-done:
				t.Fatalf("hook ended while engine query was held: %v", err)
			default:
			}
			f.old.release.release()
			requireRuntimeResponse(t, <-active, http.StatusOK, f.old.ip)
			requireGatewayRuntimeHookDone(t, done)
			got := f.old.received()
			if len(got) != 3 || got[0] != "WARM" || got[1] != "HOLD" || got[2] != runtimeQuery {
				t.Fatalf("engine deliveries = %v; each accepted query must execute once", got)
			}
			if err := f.process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			waitRuntimeSignal(t, "Envoy exited after hook", f.exited)
		})
	}
}

func TestEnvoyRuntimeShutdownWithoutPublicListener(t *testing.T) {
	f := startRuntimeFixture(t, false, func(config map[string]any) {
		resources := config["static_resources"].(map[string]any)
		var listeners []any
		for _, raw := range resources["listeners"].([]any) {
			if raw.(map[string]any)["name"] != "listener" {
				listeners = append(listeners, raw)
			}
		}
		resources["listeners"] = listeners
	}, "--drain-time-s", "0", "--drain-strategy", "immediate")
	requireGatewayRuntimeHookDone(t, startGatewayRuntimeHook(t, f))
}

func startGatewayRuntimeHook(t *testing.T, f *runtimeFixture) <-chan error {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(f.admin, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	number, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	cmd := exec.CommandContext(ctx, "bash", "-c", gatewayPreStopScript(int32(number)))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { done <- cmd.Wait(); close(exited) }()
	t.Cleanup(func() { cancel(); <-exited })
	return done
}

func requireGatewayRuntimeHookDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown hook: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not finish after connections drained")
	}
}
