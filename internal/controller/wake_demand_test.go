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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/wakeagent"
)

func TestParseWakeDemand(t *testing.T) {
	t.Parallel()
	body := `# HELP ` + wakeagent.MetricLastDemand + ` help text
# TYPE ` + wakeagent.MetricLastDemand + ` gauge
` + wakeagent.MetricLastDemand + `{engine="analytics"} 1700000000
` + wakeagent.MetricLastDemand + `{engine="reporting"} 1700000042
# TYPE ` + wakeagent.MetricPendingHolds + ` gauge
` + wakeagent.MetricPendingHolds + `{engine="analytics"} 3
`
	got := parseWakeDemand(body)
	if len(got) != 2 {
		t.Fatalf("parseWakeDemand() returned %d engines, want 2: %v", len(got), got)
	}
	if !got["analytics"].Equal(time.Unix(1_700_000_000, 0)) {
		t.Errorf("analytics = %v, want %v", got["analytics"], time.Unix(1_700_000_000, 0))
	}
	if !got["reporting"].Equal(time.Unix(1_700_000_042, 0)) {
		t.Errorf("reporting = %v, want %v", got["reporting"], time.Unix(1_700_000_042, 0))
	}
}

// The parser reads only the demand series. Anything else the agent exposes
// is observability, and adding to it must not change what the operator
// acts on.
func TestParseWakeDemandIgnoresOtherSeries(t *testing.T) {
	t.Parallel()
	body := wakeagent.MetricPendingHolds + `{engine="analytics"} 5
` + wakeagent.MetricSheddedTotal + `{engine="analytics"} 12
some_unrelated_metric{engine="analytics"} 1700000000
`
	if got := parseWakeDemand(body); len(got) != 0 {
		t.Errorf("parseWakeDemand() = %v, want no engines", got)
	}
}

func TestParseWakeDemandSkipsMalformedLines(t *testing.T) {
	t.Parallel()
	cases := []string{
		wakeagent.MetricLastDemand + `{engine="ok"}`,              // no value
		wakeagent.MetricLastDemand + `{engine=""} 1700000000`,     // empty label
		wakeagent.MetricLastDemand + `{engine="ok"} not-a-number`, // non-numeric
		wakeagent.MetricLastDemand + `{other="ok"} 1700000000`,    // wrong label
	}
	for _, line := range cases {
		t.Run(line, func(t *testing.T) {
			if got := parseWakeDemand(line + "\n"); len(got) != 0 {
				t.Errorf("parseWakeDemand(%q) = %v, want no engines", line, got)
			}
		})
	}
}

func TestWakeDemandTrackerLastDemandBeforeFirstPoll(t *testing.T) {
	t.Parallel()
	tr := &WakeDemandTracker{}
	if got := tr.LastDemand("ns", "engine"); got != nil {
		t.Errorf("LastDemand() = %v before any poll, want nil", got)
	}
}

func TestWakeDemandTrackerWatchesNamespace(t *testing.T) {
	t.Parallel()
	all := &WakeDemandTracker{}
	if !all.watches("anything") {
		t.Error("an unscoped tracker should watch every namespace")
	}
	scoped := &WakeDemandTracker{Namespaces: []string{"a", "b"}}
	if !scoped.watches("b") {
		t.Error("scoped tracker should watch a listed namespace")
	}
	if scoped.watches("c") {
		t.Error("scoped tracker should not watch an unlisted namespace")
	}
}

func stoppedEngine(namespace, name string, replicas int32) *computev1alpha1.FireboltEngine {
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: computev1alpha1.FireboltEngineSpec{
			Replicas:    replicas,
			InstanceRef: "inst",
		},
	}
}

func TestNamespacesWithStoppedEngines(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			stoppedEngine("ns-a", "down", 0),
			stoppedEngine("ns-a", "up", 2),
			stoppedEngine("ns-b", "also-up", 1),
			stoppedEngine("ns-c", "down", 0),
		).
		Build()
	tr := &WakeDemandTracker{Client: c}

	set, err := tr.stoppedEngines(context.Background())
	if err != nil {
		t.Fatalf("stoppedEngines() error: %v", err)
	}
	got := set.namespaces()
	want := map[string]bool{"ns-a": true, "ns-c": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want namespaces %v", got, want)
	}
	for _, ns := range got {
		if !want[ns] {
			t.Errorf("unexpected namespace %q in %v", ns, got)
		}
	}
}

// A namespaced install must not poll namespaces outside its cache, even if
// a stopped engine somehow shows up in a List.
func TestNamespacesWithStoppedEnginesRespectsScope(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			stoppedEngine("watched", "down", 0),
			stoppedEngine("unwatched", "down", 0),
		).
		Build()
	tr := &WakeDemandTracker{Client: c, Namespaces: []string{"watched"}}

	set, err := tr.stoppedEngines(context.Background())
	if err != nil {
		t.Fatalf("stoppedEngines() error: %v", err)
	}
	got := set.namespaces()
	if len(got) != 1 || got[0] != "watched" {
		t.Fatalf("got %v, want only [watched]", got)
	}
}

func gatewayPod(name, ip string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels: map[string]string{
				LabelComponent: "gateway",
				LabelInstance:  "inst",
			},
		},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip},
	}
}

func TestGatewayPodsSkipsUnscheduledAndPending(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			gatewayPod("running", "10.0.0.1", corev1.PodRunning),
			gatewayPod("pending", "", corev1.PodPending),
			gatewayPod("running-no-ip", "", corev1.PodRunning),
		).
		Build()
	tr := &WakeDemandTracker{Client: c}

	pods, err := tr.gatewayPods(context.Background(), "ns")
	if err != nil {
		t.Fatalf("gatewayPods() error: %v", err)
	}
	if len(pods) != 1 || pods[0].Name != "running" {
		t.Fatalf("gatewayPods() = %v, want only the running pod with an IP", podNames(pods))
	}
}

func podNames(pods []corev1.Pod) []string {
	out := make([]string, 0, len(pods))
	for i := range pods {
		out = append(out, pods[i].Name)
	}
	return out
}

// End-to-end over the loopback interface: a stopped engine, a gateway pod
// serving a demand exposition, and the tracker turning that into a
// timestamp the autoStop decision can read.
func TestPollOnceCachesDemandFromAgent(t *testing.T) {
	demandAt := time.Unix(1_700_000_000, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/demand" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "%s{engine=%q} %d\n", wakeagent.MetricLastDemand, "down", demandAt.Unix())
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.Listener.Addr().String())

	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			stoppedEngine("ns", "down", 0),
			gatewayPod("gw-0", host, corev1.PodRunning),
		).
		Build()

	tr := &WakeDemandTracker{Client: c, DemandPort: port}
	tr.pollOnce(context.Background())

	got := tr.LastDemand("ns", "down")
	if got == nil {
		t.Fatal("LastDemand() = nil, want the agent's timestamp")
	}
	if !got.Equal(demandAt) {
		t.Errorf("LastDemand() = %v, want %v", got, demandAt)
	}
}

// With nothing stopped there is nothing to wake, and stale demand must not
// survive: a timestamp recorded before an engine started would otherwise
// re-trigger a wake the moment it stopped again.
func TestPollOnceClearsCacheWhenNothingIsStopped(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(stoppedEngine("ns", "up", 3)).
		Build()

	tr := &WakeDemandTracker{Client: c}
	tr.replace(map[demandKey]time.Time{
		{namespace: "ns", engine: "up"}: time.Unix(1_700_000_000, 0),
	})

	tr.pollOnce(context.Background())

	if got := tr.LastDemand("ns", "up"); got != nil {
		t.Errorf("LastDemand() = %v after the engine came up, want nil", got)
	}
}

// An unreachable agent must not wedge the poll or surface as an error.
func TestPollOnceToleratesUnreachableAgent(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			stoppedEngine("ns", "down", 0),
			// 192.0.2.0/24 is TEST-NET-1: guaranteed unroutable.
			gatewayPod("gw-0", "192.0.2.1", corev1.PodRunning),
		).
		Build()

	tr := &WakeDemandTracker{Client: c}
	done := make(chan struct{})
	go func() {
		tr.pollOnce(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("pollOnce did not return against an unreachable agent")
	}
	if got := tr.LastDemand("ns", "down"); got != nil {
		t.Errorf("LastDemand() = %v, want nil when the agent is unreachable", got)
	}
}

func TestScrapeModeDefaultsWithoutInstance(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithScheme(wakeTestScheme(t)).Build()
	tr := &WakeDemandTracker{Client: c}

	pod := gatewayPod("gw-0", "10.0.0.1", corev1.PodRunning)
	if got := tr.scrapeMode(context.Background(), pod); got != MetricScrapeModeDefault {
		t.Errorf("scrapeMode() = %q with no instance, want %q", got, MetricScrapeModeDefault)
	}
}

// The wake poll must follow the instance's existing metricScrapeMode
// rather than inventing a second transport knob.
func TestScrapeModeFollowsInstance(t *testing.T) {
	t.Parallel()
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			MetricScrapeMode: computev1alpha1.MetricScrapeModeApiserverProxy,
		},
	}
	c := fake.NewClientBuilder().WithScheme(wakeTestScheme(t)).WithObjects(inst).Build()
	tr := &WakeDemandTracker{Client: c}

	pod := gatewayPod("gw-0", "10.0.0.1", corev1.PodRunning)
	if got := tr.scrapeMode(context.Background(), pod); got != computev1alpha1.MetricScrapeModeApiserverProxy {
		t.Errorf("scrapeMode() = %q, want %q", got, computev1alpha1.MetricScrapeModeApiserverProxy)
	}
}

// ApiserverProxy mode without a clientset is a configuration error, not a
// silent fall back to a transport the operator was told not to use.
func TestScrapeAgentRequiresClientsetForProxyMode(t *testing.T) {
	t.Parallel()
	tr := &WakeDemandTracker{}
	pod := gatewayPod("gw-0", "10.0.0.1", corev1.PodRunning)
	_, err := tr.scrapeAgent(context.Background(), pod, computev1alpha1.MetricScrapeModeApiserverProxy)
	if err == nil {
		t.Fatal("scrapeAgent() succeeded in proxy mode with no clientset")
	}
}

func splitHostPort(t *testing.T, addr string) (string, int32) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	var port int32
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	return host, port
}

func wakeTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("adding client-go scheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding compute scheme: %v", err)
	}
	return s
}

// The agent stamps demand whenever an engine has no ready endpoints, which
// also covers a RUNNING engine during a node drain or rolling restart. Wake
// is honored above the idle check, so an unfiltered stamp would pin such an
// engine at activeReplicas — scaling a manually-sized engine down in the
// middle of its outage. Only engines actually at zero may reach the cache.
func TestPollOnceIgnoresDemandForRunningEngines(t *testing.T) {
	demandAt := time.Unix(1_700_000_000, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The gateway reports both: one genuinely stopped, one merely
		// mid-restart with four replicas configured.
		fmt.Fprintf(w, "%s{engine=%q} %d\n", wakeagent.MetricLastDemand, "stopped", demandAt.Unix())
		fmt.Fprintf(w, "%s{engine=%q} %d\n", wakeagent.MetricLastDemand, "restarting", demandAt.Unix())
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.Listener.Addr().String())
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			stoppedEngine("ns", "stopped", 0),
			stoppedEngine("ns", "restarting", 4),
			gatewayPod("gw-0", host, corev1.PodRunning),
		).
		Build()

	tr := &WakeDemandTracker{Client: c, DemandPort: port}
	tr.pollOnce(context.Background())

	if got := tr.LastDemand("ns", "stopped"); got == nil {
		t.Error("LastDemand(stopped) = nil, want the agent's timestamp")
	}
	if got := tr.LastDemand("ns", "restarting"); got != nil {
		t.Errorf("LastDemand(restarting) = %v, want nil; a running engine "+
			"must not be scaled to activeReplicas by a readiness blip", got)
	}
}

// The 2s poll buys nothing unless it also wakes the controller: auto-stop
// reads the cache on its own pollInterval, a minute by default, so a held
// query would wait most of that before scale-up even started.
func TestPollOnceAnnouncesNewlyDemandedEngines(t *testing.T) {
	demandAt := time.Unix(1_700_000_000, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s{engine=%q} %d\n", wakeagent.MetricLastDemand, "down", demandAt.Unix())
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.Listener.Addr().String())
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			stoppedEngine("ns", "down", 0),
			gatewayPod("gw-0", host, corev1.PodRunning),
		).
		Build()

	tr := NewWakeDemandTracker(c, nil, nil)
	tr.DemandPort = port
	tr.pollOnce(context.Background())

	select {
	case evt := <-tr.Events:
		if evt.Object.GetName() != "down" || evt.Object.GetNamespace() != "ns" {
			t.Errorf("event for %s/%s, want ns/down", evt.Object.GetNamespace(), evt.Object.GetName())
		}
	default:
		t.Fatal("no reconcile event emitted for a newly-demanded engine")
	}
}

// Re-announcing an unchanged timestamp every 2s would spin the engine
// controller for as long as the stamp lives.
func TestPollOnceDoesNotReannounceUnchangedDemand(t *testing.T) {
	demandAt := time.Unix(1_700_000_000, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s{engine=%q} %d\n", wakeagent.MetricLastDemand, "down", demandAt.Unix())
	}))
	defer srv.Close()

	host, port := splitHostPort(t, srv.Listener.Addr().String())
	c := fake.NewClientBuilder().
		WithScheme(wakeTestScheme(t)).
		WithObjects(
			stoppedEngine("ns", "down", 0),
			gatewayPod("gw-0", host, corev1.PodRunning),
		).
		Build()

	tr := NewWakeDemandTracker(c, nil, nil)
	tr.DemandPort = port
	tr.pollOnce(context.Background())
	<-tr.Events

	tr.pollOnce(context.Background())
	select {
	case evt := <-tr.Events:
		t.Errorf("unchanged demand re-announced for %s", evt.Object.GetName())
	default:
	}
}
