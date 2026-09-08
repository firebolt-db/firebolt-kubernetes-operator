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
	"context"
	"encoding/json"
	"errors"
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

	corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"

	"golang.org/x/net/dns/dnsmessage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
)

const runtimeQuery = "INSERT sentinel VALUES (1)"

type runtimeLatch struct {
	once sync.Once
	done chan struct{}
}

func newRuntimeLatch() *runtimeLatch { return &runtimeLatch{done: make(chan struct{})} }
func (l *runtimeLatch) release()     { l.once.Do(func() { close(l.done) }) }

type runtimeDNS struct {
	mu        sync.Mutex
	ips       []string
	code      dnsmessage.RCode
	blocked   *runtimeLatch
	requested *runtimeLatch
	conn      *net.UDPConn
	ctx       context.Context
}

func newRuntimeDNS(t *testing.T) *runtimeDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	d := &runtimeDNS{conn: conn, ctx: t.Context()}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			buf := make([]byte, 4096)
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				d.answer(buf[:n], peer)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close() // Closing the owned socket stops its receive loop.
		workers.Wait()
	})
	return d
}

func (d *runtimeDNS) set(ips []string, code dnsmessage.RCode) {
	d.mu.Lock()
	d.ips, d.code = ips, code
	d.mu.Unlock()
}

func (d *runtimeDNS) block(ips []string, code dnsmessage.RCode, l *runtimeLatch) *runtimeLatch {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.blocked = l
	d.ips, d.code = ips, code
	d.requested = newRuntimeLatch()
	return d.requested
}

func (d *runtimeDNS) answer(raw []byte, peer *net.UDPAddr) {
	var q dnsmessage.Message
	if err := q.Unpack(raw); err != nil {
		return // The fixture only responds to valid DNS queries.
	}
	d.mu.Lock()
	blocked := d.blocked
	requested := d.requested
	ips, code := append([]string(nil), d.ips...), d.code
	d.mu.Unlock()
	if blocked != nil {
		requested.release()
		select {
		case <-blocked.done:
		case <-d.ctx.Done():
			return
		}
	}
	r := dnsmessage.Message{
		Header: dnsmessage.Header{ID: q.ID, Response: true, Authoritative: true,
			RecursionDesired: q.RecursionDesired, RecursionAvailable: true, RCode: code},
		Questions: q.Questions,
	}
	for i := range q.Questions {
		question := &q.Questions[i]
		if question.Type != dnsmessage.TypeA || code != dnsmessage.RCodeSuccess {
			continue
		}
		for _, ip := range ips {
			r.Answers = append(r.Answers, dnsmessage.Resource{
				Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 5},
				Body:   &dnsmessage.AResource{A: [4]byte(net.ParseIP(ip).To4())},
			})
		}
	}
	encoded, err := r.Pack()
	if err != nil {
		return // All responses use the fixed address format above.
	}
	_, _ = d.conn.WriteToUDP(encoded, peer) // The resolver may close during fixture teardown.
}

type runtimeEngine struct {
	ip         string
	server     *httptest.Server
	ready      atomic.Bool
	resetOnce  atomic.Bool
	reject     atomic.Bool
	drained    atomic.Bool
	started    *runtimeLatch
	release    *runtimeLatch
	mu         sync.Mutex
	deliveries []string
}

func newRuntimeEngine(t *testing.T, ip string, port int) *runtimeEngine {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	e := &runtimeEngine{ip: ip, started: newRuntimeLatch(), release: newRuntimeLatch()}
	e.ready.Store(true)
	e.server = httptest.NewUnstartedServer(http.HandlerFunc(e.serveHTTP))
	e.server.Listener = listener
	e.server.Start()
	t.Cleanup(func() {
		e.release.release()
		e.server.Close()
	})
	return e
}

func (e *runtimeEngine) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health/ready" {
		if !e.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	e.mu.Lock()
	e.deliveries = append(e.deliveries, string(body))
	e.mu.Unlock()
	if e.resetOnce.CompareAndSwap(true, false) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close() // Deliberately terminate after receiving the complete request.
		}
		return
	}
	if string(body) == "HOLD" {
		e.started.release()
		select {
		case <-e.release.done:
		case <-r.Context().Done():
			return
		}
	}
	if e.reject.Load() || e.drained.Load() {
		if e.drained.Swap(false) {
			w.Header().Set("X-Firebolt-Drained", "1")
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = io.WriteString(w, e.ip) // Requests can disconnect during teardown or the reset test.
}

func (e *runtimeEngine) received() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.deliveries...)
}

type runtimeResponse struct {
	status int
	body   string
	err    error
}

type runtimeFixture struct {
	t         *testing.T
	process   *os.Process
	exited    <-chan struct{}
	dns       *runtimeDNS
	old       *runtimeEngine
	new       *runtimeEngine
	admin     string
	query     string
	client    *http.Client
	wake      *grpc.Server
	probe     string
	authority string
	probeGate atomic.Bool
	parked    atomic.Bool
	holding   *runtimeLatch
	release   *runtimeLatch
}

// startRuntimeFixture keeps rendered Lua, mandatory ext_proc configuration,
// route policy, DFP filters, probe listener and synthesized subclusters.
// Its admission double isolates Envoy routing and listener mechanics; production
// Agent accounting and stream cancellation are tested in wakeagent runtime tests.
// mutate is reserved for explicit negative controls in a test.
func startRuntimeFixture(t *testing.T, wake bool, mutate func(map[string]any), extraArgs ...string) *runtimeFixture {
	t.Helper()
	binary := os.Getenv("ENVOY_BINARY")
	if binary == "" {
		t.Fatal("ENVOY_BINARY must name the pinned Envoy executable (make test-envoy-integration)")
	}
	version, err := exec.CommandContext(t.Context(), binary, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "/"+strings.TrimPrefix(images.EnvoyTag, "v")+"/") {
		t.Fatalf("Envoy must match %s: %s (%v)", images.EnvoyTag, version, err)
	}
	f := &runtimeFixture{t: t, dns: newRuntimeDNS(t), holding: newRuntimeLatch(), release: newRuntimeLatch(),
		client: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}}
	f.old = newRuntimeEngine(t, "127.0.0.2", 0)
	port := f.old.server.Listener.Addr().(*net.TCPAddr).Port
	f.new = newRuntimeEngine(t, "127.0.0.3", port)
	f.dns.set([]string{f.old.ip}, dnsmessage.RCodeSuccess)
	f.authority = net.JoinHostPort("probe-service.runtime-test.svc.cluster.local", strconv.Itoa(port))
	processorListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.wake = grpc.NewServer()
	extpb.RegisterExternalProcessorServer(f.wake, &runtimeAdmission{fixture: f})
	go func() { _ = f.wake.Serve(processorListener) }()
	t.Cleanup(func() { f.release.release(); f.wake.Stop(); f.client.CloseIdleConnections() })
	var config map[string]any
	if err := yaml.Unmarshal([]byte(buildEnvoyConfigYAML(&computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture", Namespace: "runtime-test"},
	}, wake)), &config); err != nil {
		t.Fatal(err)
	}
	root := config["static_resources"].(map[string]any)
	for _, raw := range root["listeners"].([]any) {
		listener := raw.(map[string]any)
		address := listener["address"].(map[string]any)["socket_address"].(map[string]any)
		address["address"], address["port_value"] = "127.0.0.1", 0
		for _, chain := range listener["filter_chains"].([]any) {
			for _, filter := range chain.(map[string]any)["filters"].([]any) {
				hcm := filter.(map[string]any)["typed_config"].(map[string]any)
				for _, httpFilter := range hcm["http_filters"].([]any) {
					filter := httpFilter.(map[string]any)
					if filter["name"] == "envoy.filters.http.lua" {
						source := filter["typed_config"].(map[string]any)["default_source_code"].(map[string]any)
						source["inline_string"] = strings.ReplaceAll(source["inline_string"].(string), ":3473", ":"+strconv.Itoa(port))
					}
				}
			}
		}
	}
	resolver := dfpCluster(t, config)["typed_dns_resolver_config"].(map[string]any)["typed_config"].(map[string]any)
	resolver["resolvers"] = []any{map[string]any{"socket_address": map[string]any{"address": "127.0.0.1", "port_value": f.dns.conn.LocalAddr().(*net.UDPAddr).Port}}}
	resolver["use_resolvers_as_fallback"] = false
	for _, raw := range root["clusters"].([]any) {
		cluster := raw.(map[string]any)
		if cluster["name"] == "routing_agent" {
			endpoint := cluster["load_assignment"].(map[string]any)["endpoints"].([]any)[0].(map[string]any)["lb_endpoints"].([]any)[0].(map[string]any)["endpoint"].(map[string]any)
			endpoint["address"].(map[string]any)["socket_address"].(map[string]any)["port_value"] = processorListener.Addr().(*net.TCPAddr).Port
		}
	}
	dir := t.TempDir()
	adminPath := filepath.Join(dir, "admin.address")
	config["admin"].(map[string]any)["address"].(map[string]any)["socket_address"].(map[string]any)["port_value"] = 0
	if mutate != nil {
		mutate(config)
	}
	hasQueryListener := false
	for _, raw := range root["listeners"].([]any) {
		hasQueryListener = hasQueryListener || raw.(map[string]any)["name"] == "listener"
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "envoy.json")
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "envoy.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "-c", configPath, "--concurrency", "2", "--disable-hot-restart", "--log-level", "warning", "--admin-address-path", adminPath)
	cmd.Args = append(cmd.Args, extraArgs...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close() // No process owns the log when start fails.
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	f.process, f.exited = cmd.Process, processDone
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		f.old.release.release()
		f.release.release()
		_ = cmd.Process.Kill() // Ensure fixture teardown cannot wait for Envoy's drain timeout.
		<-processDone
		_ = log.Close() // All writers stopped above.
		if t.Failed() {
			body, err := os.ReadFile(logPath)
			if err == nil {
				const maxLog = 12000
				if len(body) > maxLog {
					body = body[len(body)-maxLog:]
				}
				t.Logf("Envoy log:\n%s", body)
			}
		}
	})
	runtimeEventually(t, "Envoy listeners ready", func() bool {
		select {
		case <-processDone:
			t.Fatalf("Envoy exited before listeners became ready: %v", processErr)
		default:
		}
		address, err := os.ReadFile(adminPath)
		if err != nil {
			return false
		}
		f.admin = "http://" + strings.TrimSpace(string(address))
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
		if response.err != nil || json.Unmarshal([]byte(response.body), &listeners) != nil {
			return false
		}
		foundQuery := false
		for _, listener := range listeners.Statuses {
			address := "http://" + net.JoinHostPort(listener.Address.Socket.Address, strconv.Itoa(listener.Address.Socket.Port))
			if listener.Name == "listener" {
				f.query = address
				foundQuery = true
			}
			if listener.Name == "routing_probe" {
				f.probe = address
			}
		}
		if hasQueryListener && foundQuery && f.probe != "" {
			return true
		}
		return !hasQueryListener && len(listeners.Statuses) > 0
	})
	return f
}

func (f *runtimeFixture) request(method, url, body string) runtimeResponse {
	r, err := http.NewRequestWithContext(f.t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		return runtimeResponse{err: err}
	}
	r.Header.Set("X-Firebolt-Engine", "probe")
	response, err := f.client.Do(r)
	if err != nil {
		return runtimeResponse{err: err}
	}
	defer func() { _ = response.Body.Close() }() // The response is fully read below.
	raw, err := io.ReadAll(response.Body)
	return runtimeResponse{status: response.StatusCode, body: string(raw), err: err}
}

func (f *runtimeFixture) get(path string) runtimeResponse {
	return f.request(http.MethodGet, f.admin+path, "")
}

func (f *runtimeFixture) post(body string) runtimeResponse {
	return f.request(http.MethodPost, f.query, body)
}

func runtimeEventually(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", description)
		}
	}
}

func requireRuntimeResponse(t *testing.T, got runtimeResponse, status int, body string) {
	t.Helper()
	if got.err != nil || got.status != status || got.body != body {
		t.Fatalf("response = (%d, %q, %v), want (%d, %q)", got.status, got.body, got.err, status, body)
	}
}

type runtimeHost struct {
	Address struct {
		Socket struct {
			Address string `json:"address"`
		} `json:"socket_address"`
	} `json:"address"`
	Health struct {
		Failed   bool `json:"failed_active_health_check"`
		Pending  bool `json:"pending_active_hc"`
		Removing bool `json:"pending_dynamic_removal"`
	} `json:"health_status"`
}

// hosts inspects the main-thread host set. Successful requests below separately
// exercise worker routing; an admin snapshot alone is not a convergence barrier.
func (f *runtimeFixture) hosts() (map[string]runtimeHost, bool) {
	response := f.get("/clusters?format=json")
	var clusters struct {
		Statuses []struct {
			Name  string        `json:"name"`
			Hosts []runtimeHost `json:"host_statuses"`
		} `json:"cluster_statuses"`
	}
	if response.err != nil || response.status != http.StatusOK || json.Unmarshal([]byte(response.body), &clusters) != nil {
		return nil, false
	}
	for _, cluster := range clusters.Statuses {
		if strings.HasPrefix(cluster.Name, "DFPCluster:probe-service.runtime-test.svc.cluster.local:") {
			hosts := make(map[string]runtimeHost, len(cluster.Hosts))
			for _, host := range cluster.Hosts {
				hosts[host.Address.Socket.Address] = host
			}
			return hosts, true
		}
	}
	return nil, false
}

func (f *runtimeFixture) stat(suffix string) uint64 {
	response := f.get("/stats?format=json")
	var stats struct {
		Stats []struct {
			Name  string `json:"name"`
			Value uint64 `json:"value"`
		} `json:"stats"`
	}
	if response.err != nil || response.status != http.StatusOK || json.Unmarshal([]byte(response.body), &stats) != nil {
		f.t.Fatalf("cannot read Envoy stats: %+v", response)
	}
	var sum uint64
	found := false
	for _, stat := range stats.Stats {
		if strings.HasPrefix(stat.Name, "cluster.DFPCluster") && strings.HasSuffix(stat.Name, suffix) {
			sum += stat.Value
			found = true
		}
	}
	if !found {
		f.t.Fatalf("DFP stat with suffix %q missing from %s", suffix, response.body)
	}
	return sum
}

// runtimeAdmission is an explicit test double. It provides a fixed destination
// and controllable wake boundary; it makes no claim about fleet withdrawal ACKs.
type runtimeAdmission struct {
	extpb.UnimplementedExternalProcessorServer
	fixture *runtimeFixture
}

func (a *runtimeAdmission) Process(stream extpb.ExternalProcessor_ProcessServer) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	if request.GetRequestHeaders() == nil {
		return errors.New("expected request headers")
	}
	f := a.fixture
	if f.parked.Load() {
		f.holding.release()
		select {
		case <-f.release.done:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
	if f.probeGate.Load() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			probe, err := http.NewRequestWithContext(stream.Context(), http.MethodGet, f.probe+"/health/ready", http.NoBody)
			if err != nil {
				return err
			}
			probe.Host = f.authority
			response, err := f.client.Do(probe)
			ready := false
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				ready = response.StatusCode == http.StatusOK
			}
			if ready {
				break
			}
			select {
			case <-ticker.C:
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
		}
	}
	mutation := &extpb.HeaderMutation{SetHeaders: []*corepb.HeaderValueOption{{
		Header: &corepb.HeaderValue{Key: ":authority", RawValue: []byte(f.authority)}, AppendAction: corepb.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}}}
	if err := stream.Send(&extpb.ProcessingResponse{Response: &extpb.ProcessingResponse_RequestHeaders{RequestHeaders: &extpb.HeadersResponse{Response: &extpb.CommonResponse{HeaderMutation: mutation, ClearRouteCache: true}}}}); err != nil {
		return err
	}
	for {
		request, err = stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if request.GetResponseHeaders() == nil {
			return errors.New("unexpected processing message")
		}
		if err := stream.Send(&extpb.ProcessingResponse{Response: &extpb.ProcessingResponse_ResponseHeaders{ResponseHeaders: &extpb.HeadersResponse{Response: &extpb.CommonResponse{}}}}); err != nil {
			return err
		}
	}
}
