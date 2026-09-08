// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package wakeagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corepb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func routingTestAgent(t *testing.T) (*Agent, routing.State, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != "old.ns.svc:3473" && r.Host != "new.ns.svc:3473" {
			t.Errorf("unexpected probe authority %q", r.Host)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(probe.Close)
	a := New(Config{PodUID: "pod", InstanceUID: "instance", RouteProbeURL: probe.URL, HoldTimeout: 100 * time.Millisecond})
	state := routing.NewState("instance")
	if _, err := routing.RegisterSession(&state, "pod", a.RoutingReport().SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.SetRoute(&state, "engine", routing.Route{EngineUID: "engine-uid", Generation: 1, Authority: "old.ns.svc:3473", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyRoutingState(state); err != nil {
		t.Fatal(err)
	}
	a.readiness.MarkSynced()
	a.readiness.setReady("service:old", true)
	a.readiness.setReady("service:new", true)
	return a, state, calls
}

func TestRoutingFencePreservesOldPermit(t *testing.T) {
	a, state, _ := routingTestAgent(t)
	old, err := a.acquireRoute(t.Context(), "engine")
	if err != nil {
		t.Fatal(err)
	}
	staleData, err := routing.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := routing.SetRoute(&state, "engine", routing.Route{EngineUID: "engine-uid", Generation: 2, Authority: "new.ns.svc:3473", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyRoutingState(state); err != nil {
		t.Fatal(err)
	}
	stale, err := routing.Decode(staleData)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyRoutingState(stale); err != nil {
		t.Fatal(err)
	}
	next, err := a.acquireRoute(t.Context(), "engine")
	if err != nil {
		t.Fatal(err)
	}
	if next.Authority != "new.ns.svc:3473" {
		t.Fatalf("stale assignment reopened old route: %+v", next)
	}
	retirement := state.Retirements[old.Key()]
	report := a.RoutingReport()
	if report.AppliedRevision != state.Revision || report.Outstanding[old.Key()].Active != 1 {
		t.Fatalf("fence erased outstanding grant: %+v", report)
	}
	if routing.CanRetire(retirement, map[string]routing.Report{"pod": report}, nil) {
		t.Fatal("retired with outstanding old grant")
	}
	a.finishPermit(old.Key(), true)
	if !routing.CanRetire(retirement, map[string]routing.Report{"pod": a.RoutingReport()}, nil) {
		t.Fatal("clean completion did not permit retirement")
	}
	a.finishPermit(next.Key(), true)
}

func TestRoutingProbeCacheInvalidatesAfterReadinessLoss(t *testing.T) {
	a, _, calls := routingTestAgent(t)
	for range 2 {
		route, err := a.acquireRoute(t.Context(), "engine")
		if err != nil {
			t.Fatal(err)
		}
		a.finishPermit(route.Key(), true)
	}
	if calls.Load() != 1 {
		t.Fatalf("steady route reprobed %d times", calls.Load())
	}
	a.readiness.setReady("service:old", false)
	if ready, version := a.readiness.readinessVersion("service:old"); ready || version != 0 {
		t.Fatalf("removed Service retained readiness history: ready=%v version=%d", ready, version)
	}
	a.readiness.setReady("service:old", true)
	route, err := a.acquireRoute(t.Context(), "engine")
	if err != nil {
		t.Fatal(err)
	}
	a.finishPermit(route.Key(), true)
	if calls.Load() != 2 {
		t.Fatal("readiness loss did not invalidate cached Envoy usability")
	}
}

func TestRoutingUnregisteredBootCannotAdmit(t *testing.T) {
	a, state, _ := routingTestAgent(t)
	restarted := New(Config{PodUID: "pod", InstanceUID: "instance", HoldTimeout: time.Millisecond})
	if err := restarted.ApplyRoutingState(state); err != nil {
		t.Fatal(err)
	}
	restarted.readiness.MarkSynced()
	restarted.readiness.setReady("service:old", true)
	if _, err := restarted.acquireRoute(t.Context(), "engine"); err == nil {
		t.Fatal("new boot admitted using predecessor registration")
	}
	if restarted.RoutingReport().Registered {
		t.Fatal("new boot registered without durable assignment")
	}
	report := httptest.NewRecorder()
	a.demandMux().ServeHTTP(report, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/routing", http.NoBody))
	var got routing.Report
	if err := json.Unmarshal(report.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.PodUID != "pod" || got.SessionID != a.RoutingReport().SessionID {
		t.Fatalf("bad report: %+v", got)
	}
}

func TestRoutingProbeRechecksFenceBeforeGrant(t *testing.T) {
	a, state, _ := routingTestAgent(t)
	started, release := make(chan struct{}), make(chan struct{})
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "old.ns.svc:3473" {
			close(started)
			<-release
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer probe.Close()
	a.cfg.RouteProbeURL = probe.URL
	done := make(chan routing.Route, 1)
	go func() { route, _ := a.acquireRoute(t.Context(), "engine"); done <- route }()
	<-started
	if _, err := routing.SetRoute(&state, "engine", routing.Route{EngineUID: "engine-uid", Generation: 2, Authority: "new.ns.svc:3473", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.ApplyRoutingState(state); err != nil {
		t.Fatal(err)
	}
	close(release)
	route := <-done
	if route.Authority != "new.ns.svc:3473" {
		t.Fatalf("probe raced fence: %+v", route)
	}
	a.finishPermit(route.Key(), true)
}

func testProcessorClient(t *testing.T, a *Agent) (extpb.ExternalProcessorClient, *grpc.Server) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	extpb.RegisterExternalProcessorServer(server, a)
	go func() { _ = server.Serve(listener) }()
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(); server.Stop() })
	return extpb.NewExternalProcessorClient(connection), server
}

func processorRequest() *extpb.ProcessingRequest {
	return &extpb.ProcessingRequest{Request: &extpb.ProcessingRequest_RequestHeaders{RequestHeaders: &extpb.HttpHeaders{
		Headers: &corepb.HeaderMap{Headers: []*corepb.HeaderValue{{Key: "x-firebolt-engine", RawValue: []byte("engine")}}},
	}}}
}

func TestProcessorCleanCloseAndTransportFailure(t *testing.T) {
	for _, clean := range []bool{true, false} {
		name := "transport_failure"
		if clean {
			name = "clean_close"
		}
		t.Run(name, func(t *testing.T) {
			a, state, _ := routingTestAgent(t)
			client, server := testProcessorClient(t, a)
			stream, err := client.Process(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(processorRequest()); err != nil {
				t.Fatal(err)
			}
			response, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if got := string(response.GetRequestHeaders().GetResponse().GetHeaderMutation().GetSetHeaders()[0].GetHeader().GetRawValue()); got != "old.ns.svc:3473" {
				t.Fatalf("unexpected authority %s", got)
			}
			key := state.Routes["engine"].Key()
			if a.RoutingReport().Outstanding[key].Active != 1 {
				t.Fatal("grant sent before accounting")
			}
			if clean {
				if err := stream.CloseSend(); err != nil {
					t.Fatal(err)
				}
				if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
					t.Fatalf("close returned %v", err)
				}
			} else {
				server.Stop()
			}
			deadline := time.Now().Add(time.Second)
			for a.RoutingReport().Outstanding[key].Active != 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			count := a.RoutingReport().Outstanding[key]
			if count.Active != 0 {
				t.Fatalf("active permit did not settle: %+v", count)
			}
			if clean && count.Unknown != 0 {
				t.Fatal("clean stream became unknown")
			}
			if !clean && count.Unknown != 1 {
				t.Fatal("transport failure fabricated clean completion")
			}
		})
	}
}

func TestProcessorCancelWhileWaitingDoesNotGrant(t *testing.T) {
	a, _, _ := routingTestAgent(t)
	a.readiness.setReady("service:old", false)
	client, _ := testProcessorClient(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	stream, err := client.Process(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(processorRequest()); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("canceled hold received route grant")
	}
	if len(a.RoutingReport().Outstanding) != 0 {
		t.Fatal("canceled hold leaked a permit")
	}
}

func TestAdmissionWaitsForUsableEnvoyRoute(t *testing.T) {
	a, _, _ := routingTestAgent(t)
	a.cfg.HoldTimeout = 20 * time.Millisecond
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer probe.Close()
	a.cfg.RouteProbeURL = probe.URL
	if _, err := a.acquireRoute(t.Context(), "engine"); err == nil {
		t.Fatal("ready EndpointSlice bypassed failed Envoy probe")
	}
	if len(a.RoutingReport().Outstanding) != 0 {
		t.Fatal("failed usability probe issued permit")
	}
	if !strings.Contains(a.demand.Render(), `engine="engine"`) {
		t.Fatal("failed admission did not record wake demand")
	}
}

func TestAdmissionShedsPastHoldCapacityWithoutPermit(t *testing.T) {
	a, _, _ := routingTestAgent(t)
	a.readiness.setReady("service:old", false)
	if !a.demand.AcquireHold("other", 1) {
		t.Fatal("could not occupy hold capacity")
	}
	defer a.demand.ReleaseHold("other")
	a.capacity.fallbackCap = 1
	if _, err := a.acquireRoute(t.Context(), "engine"); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("full hold cap returned %v", err)
	}
	if len(a.RoutingReport().Outstanding) != 0 {
		t.Fatal("shed request issued permit")
	}
	if !strings.Contains(a.demand.Render(), `engine="engine"`) {
		t.Fatal("shed request lost demand")
	}
}

func TestRoutingRejectsWrongInstance(t *testing.T) {
	a, state, _ := routingTestAgent(t)
	original := a.RoutingReport().AppliedRevision
	state.InstanceUID = "another-instance"
	state.Revision++
	if err := a.ApplyRoutingState(state); err == nil {
		t.Fatal("accepted foreign routing assignment")
	}
	if a.RoutingReport().AppliedRevision != original {
		t.Fatal("rejected state changed applied revision")
	}
}
