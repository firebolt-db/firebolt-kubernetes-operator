//go:build unix

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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

type gatewayShutdownAdminReply struct {
	path   string
	status int
	body   string
}

type gatewayShutdownAdminCall struct {
	request *http.Request
	reply   chan gatewayShutdownAdminReply
}

func TestGatewayPreStopAdminProtocol(t *testing.T) {
	t.Parallel()
	const (
		zeroConnections   = "http.gateway.downstream_cx_active: 0\n"
		activeConnections = "http.gateway.downstream_cx_active: 2\n"
		publicListener    = "listener::0.0.0.0:8080\nstats_listener::0.0.0.0:9090\n"
		metricsListener   = "stats_listener::0.0.0.0:9090\n"
	)
	acknowledged := []gatewayShutdownAdminReply{
		{path: "/healthcheck/fail", status: http.StatusOK, body: "OK\n"},
		{path: "/drain_listeners", status: http.StatusOK, body: "OK\n"},
	}
	tests := []struct {
		name    string
		initial []gatewayShutdownAdminReply
		polls   []gatewayShutdownAdminReply
	}{
		{
			name: "health_failure_retries_before_draining",
			initial: []gatewayShutdownAdminReply{
				{path: "/healthcheck/fail", status: http.StatusServiceUnavailable, body: "Expected 200 but health transition failed\n"},
				{path: "/healthcheck/fail", status: http.StatusOK, body: "OK\n"},
				{path: "/drain_listeners", status: http.StatusOK, body: "OK\n"},
			},
			polls: []gatewayShutdownAdminReply{{path: "/stats", status: http.StatusOK, body: zeroConnections}},
		},
		{
			name: "drain_failure_retries_before_observing_connections",
			initial: []gatewayShutdownAdminReply{
				{path: "/healthcheck/fail", status: http.StatusOK, body: "OK\n"},
				{path: "/drain_listeners", status: http.StatusServiceUnavailable, body: "Expected 200 but listener drain failed\n"},
				{path: "/drain_listeners", status: http.StatusOK, body: "OK\n"},
			},
			polls: []gatewayShutdownAdminReply{
				{path: "/stats", status: http.StatusOK, body: zeroConnections},
			},
		},
		{
			name: "active_connections_keep_hook_running",
			polls: []gatewayShutdownAdminReply{
				{path: "/stats", status: http.StatusOK, body: activeConnections},
				{path: "/stats", status: http.StatusOK, body: "http.gateway.downstream_cx_active: 1\n"},
				{path: "/stats", status: http.StatusOK, body: zeroConnections},
			},
		},
		{
			name: "missing_gauge_does_not_acknowledge_existing_listener",
			polls: []gatewayShutdownAdminReply{
				{path: "/stats", status: http.StatusOK, body: ""},
				{path: "/listeners", status: http.StatusOK, body: publicListener},
				{path: "/stats", status: http.StatusOK, body: zeroConnections},
			},
		},
		{
			name: "no_public_listener_needs_no_connection_gauge",
			polls: []gatewayShutdownAdminReply{
				{path: "/stats", status: http.StatusOK, body: ""},
				{path: "/listeners", status: http.StatusOK, body: metricsListener},
			},
		},
		{
			name: "failed_statistics_are_not_drain_evidence",
			polls: []gatewayShutdownAdminReply{
				{path: "/stats", status: http.StatusServiceUnavailable, body: zeroConnections},
				{path: "/stats", status: http.StatusOK, body: zeroConnections},
			},
		},
		{
			name: "failed_listener_inspection_is_not_an_empty_listener_set",
			polls: []gatewayShutdownAdminReply{
				{path: "/stats", status: http.StatusOK, body: ""},
				{path: "/listeners", status: http.StatusServiceUnavailable, body: ""},
				{path: "/stats", status: http.StatusOK, body: ""},
				{path: "/listeners", status: http.StatusOK, body: metricsListener},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			steps := append([]gatewayShutdownAdminReply(nil), tt.initial...)
			if len(tt.initial) == 0 {
				steps = append(steps, acknowledged...)
			}
			steps = append(steps, tt.polls...)
			runGatewayShutdownAdminSequence(t, steps)
		})
	}
}

// Each response authorizes exactly the next observable action. Waiting for the
// next request catches premature hook exit without a sleep-based assertion.
func runGatewayShutdownAdminSequence(t *testing.T, steps []gatewayShutdownAdminReply) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("gateway pre-stop protocol test requires bash")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	calls := make(chan gatewayShutdownAdminCall)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := gatewayShutdownAdminCall{request: r, reply: make(chan gatewayShutdownAdminReply)}
		select {
		case calls <- call:
		case <-ctx.Done():
			return
		}
		select {
		case reply := <-call.reply:
			w.WriteHeader(reply.status)
			if _, err := fmt.Fprint(w, reply.body); err != nil && ctx.Err() == nil {
				t.Errorf("write admin response: %v", err)
			}
		case <-ctx.Done():
		}
	}))
	port := int32(server.Listener.Addr().(*net.TCPAddr).Port)
	cmd := exec.CommandContext(ctx, bash, "-c", gatewayPreStopScript(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel the process group so a failed assertion cannot leave cat or sleep
	// children holding the admin connection or the output pipes open.
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	done := make(chan struct{})
	var runErr error
	if err := cmd.Start(); err != nil {
		server.Close()
		t.Fatalf("start pre-stop hook: %v", err)
	}
	go func() {
		runErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		server.CloseClientConnections()
		server.Close()
		<-done
		if t.Failed() {
			t.Logf("pre-stop output: %s", output.String())
		}
	})
	for i, step := range steps {
		select {
		case call := <-calls:
			method := http.MethodGet
			if step.path == "/healthcheck/fail" || step.path == "/drain_listeners" {
				method = http.MethodPost
			}
			if call.request.Method != method || call.request.URL.Path != step.path {
				t.Fatalf("step %d: got %s %s, want %s %s", i, call.request.Method, call.request.URL, method, step.path)
			}
			if step.path == "/drain_listeners" && (!call.request.URL.Query().Has("inboundonly") || !call.request.URL.Query().Has("graceful") || call.request.URL.Query().Has("skip_exit")) {
				t.Fatalf("drain must preserve non-inbound listeners and accepted requests: %s", call.request.URL)
			}
			select {
			case call.reply <- step:
			case <-ctx.Done():
				t.Fatalf("step %d: hook exceeded deadline", i)
			}
		case <-done:
			t.Fatalf("hook exited before step %d (%s): %v", i, step.path, runErr)
		case <-ctx.Done():
			t.Fatalf("step %d (%s): hook exceeded deadline", i, step.path)
		}
	}
	select {
	case <-done:
		if runErr != nil {
			t.Fatalf("hook failed after drain acknowledgement: %v", runErr)
		}
	case call := <-calls:
		t.Fatalf("hook kept polling after drain acknowledgement: %s %s", call.request.Method, call.request.URL)
	case <-ctx.Done():
		t.Fatal("hook did not exit after drain acknowledgement")
	}
}

// The hook's contract when the admin API never becomes healthy is to retain
// the process for the kubelet to kill at the Pod deadline: any self-exit here
// would let a broken admin surface as a clean, instant shutdown that drops
// connections. Pins the retain path against a future `set -e` or `exit 1`.
func TestGatewayPreStopRetainsProcessWhenAdminNeverHealthy(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	port := int32(server.Listener.Addr().(*net.TCPAddr).Port)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", gatewayPreStopScript(port))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pre-stop hook: %v", err)
	}
	err := cmd.Wait()
	if ctx.Err() == nil {
		t.Fatalf("hook exited on its own (err=%v) while the admin API never became healthy; it must retain the process for the Pod deadline", err)
	}
	if err == nil {
		t.Fatal("hook reported success after being killed at the deadline")
	}
}
