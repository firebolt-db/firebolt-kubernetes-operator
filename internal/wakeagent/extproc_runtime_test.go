//go:build envoy_integration

// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package wakeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	extpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	"google.golang.org/grpc"
)

type processorLatch struct {
	once sync.Once
	done chan struct{}
}

func newProcessorLatch() *processorLatch { return &processorLatch{done: make(chan struct{})} }
func (l *processorLatch) release()       { l.once.Do(func() { close(l.done) }) }

type delayedProcessorStream struct {
	grpc.ServerStream
	delay            *atomic.Bool
	granted, release *processorLatch
	eof              *processorLatch
}

func (s *delayedProcessorStream) RecvMsg(message any) error {
	err := s.ServerStream.RecvMsg(message)
	if errors.Is(err, io.EOF) {
		s.eof.release()
	}
	return err
}
func (s *delayedProcessorStream) SendMsg(message any) error {
	if response, ok := message.(*extpb.ProcessingResponse); ok && response.GetRequestHeaders() != nil && s.delay.CompareAndSwap(true, false) {
		s.granted.release()
		select {
		case <-s.release.done:
		case <-s.Context().Done():
			return s.Context().Err()
		}
	}
	return s.ServerStream.SendMsg(message)
}

type processorRuntime struct {
	eof                *processorLatch
	agent              *Agent
	state              routing.State
	processor          *grpc.Server
	query, admin       string
	client             *http.Client
	oldCalls, newCalls atomic.Int32
	oldHold            *processorLatch
	entered            *processorLatch
	delay              atomic.Bool
	granted, release   *processorLatch
}

func processorEventually(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out: %s", description)
}

func newProcessorRuntime(t *testing.T) *processorRuntime {
	t.Helper()
	binary, err := filepath.Abs("../../bin/envoy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Skip("pinned bin/envoy unavailable")
	}
	a, state, _ := routingTestAgent(t)
	a.cfg.HoldTimeout = time.Second
	f := &processorRuntime{agent: a, state: state, client: &http.Client{Timeout: 5 * time.Second}, oldHold: newProcessorLatch(), entered: newProcessorLatch(), granted: newProcessorLatch(), release: newProcessorLatch(), eof: newProcessorLatch()}
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := f.oldCalls.Add(1)
		f.entered.release()
		if strings.HasPrefix(r.URL.Path, "/retry") && call == 1 {
			w.Header().Set("X-Firebolt-Drained", "true")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/hold" {
			select {
			case <-f.oldHold.done:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "old")
	}))
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { f.newCalls.Add(1); _, _ = io.WriteString(w, "new") }))
	t.Cleanup(func() { f.oldHold.release(); old.Close(); next.Close() })
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.processor = grpc.NewServer(grpc.StreamInterceptor(func(server any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(server, &delayedProcessorStream{ServerStream: stream, delay: &f.delay, granted: f.granted, release: f.release, eof: f.eof})
	}))
	extpb.RegisterExternalProcessorServer(f.processor, a)
	go func() { _ = f.processor.Serve(listener) }()
	t.Cleanup(func() { f.release.release(); f.processor.Stop() })
	address := func(port int) map[string]any {
		return map[string]any{"socket_address": map[string]any{"address": "127.0.0.1", "port_value": port}}
	}
	cluster := func(name, target string, h2 bool) map[string]any {
		_, portText, splitErr := net.SplitHostPort(strings.TrimPrefix(target, "http://"))
		if splitErr != nil {
			t.Fatal(splitErr)
		}
		port, convertErr := strconv.Atoi(portText)
		if convertErr != nil {
			t.Fatal(convertErr)
		}
		c := map[string]any{"name": name, "type": "STATIC", "connect_timeout": "0.25s", "load_assignment": map[string]any{"cluster_name": name, "endpoints": []any{map[string]any{"lb_endpoints": []any{map[string]any{"endpoint": map[string]any{"address": address(port)}}}}}}}
		if h2 {
			c["http2_protocol_options"] = map[string]any{}
		}
		return c
	}
	retry := map[string]any{"retry_on": "connect-failure,refused-stream,reset-before-request,retriable-headers", "num_retries": 2,
		"retriable_headers":           []any{map[string]any{"name": "x-firebolt-drained", "string_match": map[string]any{"exact": "true"}}},
		"retry_back_off":              map[string]any{"base_interval": "1s", "max_interval": "1s"},
		"rate_limited_retry_back_off": map[string]any{"reset_headers": []any{map[string]any{"name": "Retry-After", "format": "SECONDS"}}, "max_interval": "1s"}}
	hosts := []any{}
	for _, name := range []string{"old", "new"} {
		hosts = append(hosts, map[string]any{"name": name, "domains": []string{name + ".ns.svc:3473"}, "routes": []any{map[string]any{"match": map[string]any{"prefix": "/"}, "route": map[string]any{"cluster": name, "timeout": "0s", "retry_policy": retry}}}})
	}
	processor := map[string]any{"name": "envoy.filters.http.ext_proc", "typed_config": map[string]any{
		"@type":              "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor",
		"grpc_service":       map[string]any{"envoy_grpc": map[string]any{"cluster_name": "admission"}, "timeout": "0s"},
		"failure_mode_allow": false, "message_timeout": "3s", "allow_mode_override": false,
		"processing_mode": map[string]any{"request_header_mode": "SEND", "response_header_mode": "SEND", "request_body_mode": "NONE", "response_body_mode": "NONE"},
		"mutation_rules":  map[string]any{"allow_all_routing": true},
	}}
	// Production places the health_check filter ahead of ext_proc, so its
	// /healthz local reply reaches the processor on the encode path only.
	healthCheck := map[string]any{"name": "envoy.filters.http.health_check", "typed_config": map[string]any{
		"@type":             "type.googleapis.com/envoy.extensions.filters.http.health_check.v3.HealthCheck",
		"pass_through_mode": false,
		"headers":           []any{map[string]any{"name": ":path", "string_match": map[string]any{"exact": "/healthz"}}},
	}}
	config := map[string]any{"admin": map[string]any{"address": address(0)},
		"layered_runtime": map[string]any{"layers": []any{map[string]any{"name": "routing", "static_layer": map[string]any{"envoy.reloadable_features.ext_proc_graceful_grpc_close": true}}}},
		"static_resources": map[string]any{"clusters": []any{cluster("old", old.URL, false), cluster("new", next.URL, false), cluster("admission", listener.Addr().String(), true)},
			"listeners": []any{map[string]any{"name": "query", "address": address(0), "filter_chains": []any{map[string]any{"filters": []any{map[string]any{"name": "envoy.filters.network.http_connection_manager", "typed_config": map[string]any{
				"@type": "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager", "stat_prefix": "gateway",
				"route_config": map[string]any{"name": "query", "virtual_hosts": hosts}, "http_filters": []any{healthCheck, processor, map[string]any{"name": "envoy.filters.http.router", "typed_config": map[string]any{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}},
			}}}}}}}}}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "envoy.json")
	adminPath := filepath.Join(dir, "admin")
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := os.Create(filepath.Join(dir, "envoy.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), binary, "-c", configPath, "--concurrency", "2", "--disable-hot-restart", "--log-level", "warning", "--admin-address-path", adminPath)
	command.Stdout, command.Stderr = logs, logs
	if err := command.Start(); err != nil {
		_ = logs.Close()
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = command.Wait(); close(exited) }()
	t.Cleanup(func() {
		f.oldHold.release()
		f.release.release()
		_ = command.Process.Kill()
		<-exited
		_ = logs.Close()
		if t.Failed() {
			data, _ := os.ReadFile(filepath.Join(dir, "envoy.log"))
			t.Log(string(data))
		}
	})
	processorEventually(t, "Envoy query listener", func() bool {
		data, err := os.ReadFile(adminPath)
		if err != nil {
			return false
		}
		f.admin = "http://" + strings.TrimSpace(string(data))
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.admin+"/listeners?format=json", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		response, err := f.client.Do(request)
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		var result struct {
			Statuses []struct {
				Address struct {
					Socket struct {
						Port int `json:"port_value"`
					} `json:"socket_address"`
				} `json:"local_address"`
			} `json:"listener_statuses"`
		}
		if json.NewDecoder(response.Body).Decode(&result) != nil || len(result.Statuses) == 0 {
			return false
		}
		f.query = "http://127.0.0.1:" + strconv.Itoa(result.Statuses[0].Address.Socket.Port)
		return true
	})
	return f
}

func (f *processorRuntime) advance(t *testing.T) {
	t.Helper()
	if _, err := routing.SetRoute(&f.state, "engine", routing.Route{EngineUID: "engine-uid", Generation: 2, Authority: "new.ns.svc:3473", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.agent.ApplyRoutingState(f.state); err != nil {
		t.Fatal(err)
	}
}
func (f *processorRuntime) request(ctx context.Context, path string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.query+path, strings.NewReader("SELECT sentinel"))
	if err != nil {
		return "", err
	}
	request.Host = "old.ns.svc:3473"
	request.Header.Set("X-Firebolt-Engine", "engine")
	response, err := f.client.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, data)
	}
	return string(data), nil
}
func (f *processorRuntime) waitNoActive(t *testing.T, key string) {
	t.Helper()
	processorEventually(t, "permit completion", func() bool { return f.agent.RoutingReport().Outstanding[key].Active == 0 })
}

func TestProductionProcessorRuntime(t *testing.T) {
	t.Run("healthz_local_reply_passes_without_permit", func(t *testing.T) {
		f := newProcessorRuntime(t)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.query+"/healthz", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "old.ns.svc:3473"
		response, err := f.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("health check answered %d", response.StatusCode)
		}
		if f.oldCalls.Load() != 0 || f.newCalls.Load() != 0 {
			t.Fatal("health check reached an upstream")
		}
		if len(f.agent.RoutingReport().Outstanding) != 0 {
			t.Fatal("health check issued a permit")
		}
	})
	t.Run("delayed_grant_remains_counted_after_fence", func(t *testing.T) {
		f := newProcessorRuntime(t)
		old := f.state.Routes["engine"].Key()
		f.delay.Store(true)
		result := make(chan string, 1)
		go func() { body, err := f.request(t.Context(), "/"); result <- fmt.Sprintf("%s/%v", body, err) }()
		select {
		case <-f.granted.done:
		case <-time.After(3 * time.Second):
			t.Fatal("grant not recorded")
		}
		f.advance(t)
		if f.agent.RoutingReport().Outstanding[old].Active != 1 {
			t.Fatal("fence erased delayed old grant")
		}
		body, err := f.request(t.Context(), "/")
		if err != nil || body != "new" {
			t.Fatalf("fresh request: %s %v", body, err)
		}
		f.release.release()
		if got := <-result; got != "old/<nil>" {
			t.Fatal(got)
		}
		f.waitNoActive(t, old)
		if f.agent.RoutingReport().Outstanding[old].Unknown != 0 {
			t.Fatal("normal completion left unknown")
		}
	})
	t.Run("cancel_before_delayed_grant_is_sent", func(t *testing.T) {
		f := newProcessorRuntime(t)
		old := f.state.Routes["engine"].Key()
		f.delay.Store(true)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := make(chan error, 1)
		go func() { _, err := f.request(ctx, "/"); result <- err }()
		select {
		case <-f.granted.done:
		case <-time.After(3 * time.Second):
			t.Fatal("grant not recorded")
		}
		cancel()
		<-result
		select {
		case <-f.eof.done:
		case <-time.After(3 * time.Second):
			t.Fatal("Envoy did not signal clean cancellation")
		}
		f.release.release()
		f.waitNoActive(t, old)
		if f.agent.RoutingReport().Outstanding[old].Unknown != 0 {
			t.Fatal("clean cancellation during admission became unknown")
		}
		if f.oldCalls.Load() != 0 {
			t.Fatal("request dispatched after admission cancellation")
		}
	})
	t.Run("retry_keeps_original_epoch", func(t *testing.T) {
		f := newProcessorRuntime(t)
		old := f.state.Routes["engine"].Key()
		result := make(chan string, 1)
		go func() { body, err := f.request(t.Context(), "/retry"); result <- fmt.Sprintf("%s/%v", body, err) }()
		<-f.entered.done
		f.advance(t)
		if f.agent.RoutingReport().Outstanding[old].Active != 1 {
			t.Fatal("retry lost old permit")
		}
		if got := <-result; got != "old/<nil>" {
			t.Fatal(got)
		}
		f.waitNoActive(t, old)
		if f.oldCalls.Load() != 2 || f.newCalls.Load() != 0 {
			t.Fatal("retry changed destination")
		}
	})
	t.Run("cancel_with_retry_pending", func(t *testing.T) {
		f := newProcessorRuntime(t)
		old := f.state.Routes["engine"].Key()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := f.request(ctx, "/retry"); done <- err }()
		<-f.entered.done
		processorEventually(t, "router retry scheduled", func() bool {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.admin+"/stats?filter=upstream_rq_retry$", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			response, err := f.client.Do(request)
			if err != nil {
				return false
			}
			defer func() { _ = response.Body.Close() }()
			data, _ := io.ReadAll(response.Body)
			return strings.Contains(string(data), "upstream_rq_retry: 1")
		})
		cancel()
		<-done
		f.waitNoActive(t, old)
		if f.agent.RoutingReport().Outstanding[old].Unknown != 0 {
			t.Fatal("clean cancellation marked unknown")
		}
		time.Sleep(1600 * time.Millisecond)
		if f.oldCalls.Load() != 1 {
			t.Fatal("retry dispatched after cancellation released permit")
		}
	})
	t.Run("processor_transport_loss_preserves_unknown", func(t *testing.T) {
		f := newProcessorRuntime(t)
		old := f.state.Routes["engine"].Key()
		done := make(chan error, 1)
		go func() { _, err := f.request(t.Context(), "/hold"); done <- err }()
		<-f.entered.done
		f.processor.Stop()
		f.waitNoActive(t, old)
		if f.agent.RoutingReport().Outstanding[old].Unknown != 1 {
			t.Fatal("lost transport fabricated completion")
		}
		if err := <-done; err == nil {
			t.Fatal("processor failure admitted successful response")
		}
	})
}
