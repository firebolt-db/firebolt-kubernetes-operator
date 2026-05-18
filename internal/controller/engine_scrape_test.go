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
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// fakeScraper is a podMetricScraper stub for unit-testing the consumers
// (isPodDrained, scrapePodActiveQueries) without standing up a network
// server. Tests that exercise the actual transports use httptest
// directly against podIPScraper.
type fakeScraper struct {
	mode computev1alpha1.MetricScrapeMode
	resp []byte
	err  error
}

func (f *fakeScraper) Mode() computev1alpha1.MetricScrapeMode {
	if f.mode == "" {
		return computev1alpha1.MetricScrapeModePodIP
	}
	return f.mode
}
func (f *fakeScraper) Scrape(_ context.Context, _ *corev1.Pod) ([]byte, error) {
	return f.resp, f.err
}

// hostPortFromURL splits an "http://host:port" URL into its host and
// numeric port components. The httptest server picks a random loopback
// port; tests need it to point the production podIPScraper at a Pod
// with PodIP=loopback and override MetricsPort indirectly by hosting
// the listener on the URL's port. Since podIPScraper hard-codes
// MetricsPort, we instead host the test server at MetricsPort directly
// when possible; tests that cannot bind that port use the override
// dial path provided by httptest's URL.
func hostPortFromURL(t *testing.T, u string) (string, int) {
	t.Helper()
	u = strings.TrimPrefix(u, "http://")
	h, p, err := net.SplitHostPort(u)
	if err != nil {
		t.Fatalf("split %q: %v", u, err)
	}
	pn, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parse port %q: %v", p, err)
	}
	return h, pn
}

// fixedPortPodIPScraper builds a podIPScraper whose http.Client is
// hijacked to dial a specific tcp endpoint regardless of the host:port
// in the request URL. This lets tests exercise the full URL-building
// path of the production Scrape method (host=PodIP, port=MetricsPort,
// path=MetricsPath) while still routing the request to httptest's
// random port.
func fixedPortPodIPScraper(realAddr string) *podIPScraper {
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, network, realAddr)
		},
	}
	return &podIPScraper{client: &http.Client{Transport: transport}}
}

func TestPodIPScraper_HappyPath(t *testing.T) {
	body := "firebolt_running_queries 0\nfirebolt_suspended_queries 0\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != MetricsPath {
			t.Errorf("path: want %q got %q", MetricsPath, req.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	host, _ := hostPortFromURL(t, server.URL)
	scraper := fixedPortPodIPScraper(strings.TrimPrefix(server.URL, "http://"))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine-0", Namespace: "ns"},
		Status:     corev1.PodStatus{PodIP: host, Phase: corev1.PodRunning},
	}
	raw, err := scraper.Scrape(context.Background(), pod)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if string(raw) != body {
		t.Errorf("body: want %q got %q", body, string(raw))
	}
	if scraper.Mode() != computev1alpha1.MetricScrapeModePodIP {
		t.Errorf("Mode: want PodIP got %q", scraper.Mode())
	}
}

func TestPodIPScraper_MissingPodIP(t *testing.T) {
	scraper := &podIPScraper{client: metricsHTTPClient}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine-0", Namespace: "ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	_, err := scraper.Scrape(context.Background(), pod)
	if err == nil {
		t.Fatal("expected error for empty PodIP, got nil")
	}
	if !strings.Contains(err.Error(), "no PodIP") {
		t.Errorf("error wording: %v", err)
	}
}

func TestPodIPScraper_Non200Surfaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "metrics handler busy", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	host, _ := hostPortFromURL(t, server.URL)
	scraper := fixedPortPodIPScraper(strings.TrimPrefix(server.URL, "http://"))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine-0", Namespace: "ns"},
		Status:     corev1.PodStatus{PodIP: host, Phase: corev1.PodRunning},
	}
	_, err := scraper.Scrape(context.Background(), pod)
	if err == nil {
		t.Fatal("expected non-200 to surface as error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected 503 in error, got %v", err)
	}
}

func TestTrimLeadingSlash(t *testing.T) {
	cases := map[string]string{
		"/metrics":  "metrics",
		"metrics":   "metrics",
		"//metrics": "/metrics",
		"":          "",
		"/":         "",
	}
	for in, want := range cases {
		if got := trimLeadingSlash(in); got != want {
			t.Errorf("trimLeadingSlash(%q) = %q; want %q", in, got, want)
		}
	}
}

// TestResolveMetricScrapeMode covers the three branches of the resolver:
// instance with an explicit mode, instance with an empty mode, instance
// missing entirely. The default is asserted via MetricScrapeModeDefault
// so a change to the default shows up as a single-line diff rather than
// a fan-out across tests.
func TestResolveMetricScrapeMode(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	const ns = "ns"
	const instName = "instance"

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: instName},
	}
	instanceWithMode := func(mode computev1alpha1.MetricScrapeMode) *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
			Spec:       computev1alpha1.FireboltInstanceSpec{MetricScrapeMode: mode},
		}
	}

	tests := []struct {
		name    string
		objects []client.Object
		want    computev1alpha1.MetricScrapeMode
	}{
		{
			name:    "instance missing falls back to default",
			objects: []client.Object{engine},
			want:    MetricScrapeModeDefault,
		},
		{
			name:    "instance with empty mode falls back to default",
			objects: []client.Object{engine, instanceWithMode("")},
			want:    MetricScrapeModeDefault,
		},
		{
			name:    "explicit PodIP is honored",
			objects: []client.Object{engine, instanceWithMode(computev1alpha1.MetricScrapeModePodIP)},
			want:    computev1alpha1.MetricScrapeModePodIP,
		},
		{
			name:    "explicit ApiserverProxy is honored",
			objects: []client.Object{engine, instanceWithMode(computev1alpha1.MetricScrapeModeApiserverProxy)},
			want:    computev1alpha1.MetricScrapeModeApiserverProxy,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.objects...).
				Build()
			r := &FireboltEngineReconciler{
				Client:          fc,
				Scheme:          scheme,
				MetricsRecorder: metrics.NoOpEngineRecorder{},
			}
			got := r.resolveMetricScrapeMode(context.Background(), engine)
			if got != tt.want {
				t.Errorf("resolveMetricScrapeMode = %q; want %q", got, tt.want)
			}
		})
	}
}

// TestNewPodMetricScraperDispatch covers the factory: it must produce
// the right concrete scraper for each mode and fall closed (with the
// error-bearing scraper) on an unrecognized value. CRD enum validation
// makes the unknown branch unreachable in production, but the safety
// net is asserted so a future refactor cannot silently change the
// fallback to "default mode" and hide a typo.
func TestNewPodMetricScraperDispatch(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	const ns = "ns"
	const instName = "instance"

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: instName},
	}
	makeInst := func(mode computev1alpha1.MetricScrapeMode) *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
			Spec:       computev1alpha1.FireboltInstanceSpec{MetricScrapeMode: mode},
		}
	}

	tests := []struct {
		name    string
		inst    *computev1alpha1.FireboltInstance
		wantTyp string
	}{
		{name: "PodIP", inst: makeInst(computev1alpha1.MetricScrapeModePodIP), wantTyp: "*controller.podIPScraper"},
		{name: "ApiserverProxy", inst: makeInst(computev1alpha1.MetricScrapeModeApiserverProxy), wantTyp: "*controller.apiserverProxyScraper"},
		{name: "default falls through to PodIP", inst: makeInst(""), wantTyp: "*controller.podIPScraper"},
		{name: "unknown mode falls closed", inst: makeInst("Bogus"), wantTyp: "*controller.unknownModeScraper"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(engine, tt.inst).
				Build()
			r := &FireboltEngineReconciler{
				Client:          fc,
				Scheme:          scheme,
				MetricsRecorder: metrics.NoOpEngineRecorder{},
			}
			s := r.newPodMetricScraper(context.Background(), engine)
			gotTyp := fmt.Sprintf("%T", s)
			if gotTyp != tt.wantTyp {
				t.Errorf("scraper type: want %q got %q", tt.wantTyp, gotTyp)
			}
		})
	}
}

func TestUnknownModeScraperFailsClosed(t *testing.T) {
	s := &unknownModeScraper{mode: "Bogus"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	_, err := s.Scrape(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), "unknown MetricScrapeMode") {
		t.Fatalf("expected unknown-mode error, got %v", err)
	}
}

// TestIsPodDrained_FakeScraper exercises the consumer wiring without
// touching the network: a fake scraper feeds canned bytes through the
// same parse path production uses. The two cases pin the boundary
// (suspended + running == 0 -> drained, anything > 0 -> not drained).
func TestIsPodDrained_FakeScraper(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine-0", Namespace: "ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}

	tests := []struct {
		name        string
		body        string
		wantDrained bool
		wantErr     bool
	}{
		{
			name:        "both zero -> drained",
			body:        "firebolt_running_queries 0\nfirebolt_suspended_queries 0\n",
			wantDrained: true,
		},
		{
			name:        "running non-zero -> not drained",
			body:        "firebolt_running_queries 2\nfirebolt_suspended_queries 0\n",
			wantDrained: false,
		},
		{
			name:        "suspended non-zero -> not drained",
			body:        "firebolt_running_queries 0\nfirebolt_suspended_queries 3\n",
			wantDrained: false,
		},
		{
			name:    "missing gauge -> error",
			body:    "firebolt_running_queries 0\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scraper := &fakeScraper{resp: []byte(tt.body)}
			drained, err := isPodDrained(context.Background(), scraper, pod)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err: want=%v got=%v", tt.wantErr, err)
			}
			if !tt.wantErr && drained != tt.wantDrained {
				t.Errorf("drained: want=%v got=%v", tt.wantDrained, drained)
			}
		})
	}
}

func TestScrapePodActiveQueries_FakeScraper(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "engine-0", Namespace: "ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
	scraper := &fakeScraper{resp: []byte("firebolt_running_queries 4\nfirebolt_suspended_queries 1\n")}
	n, err := scrapePodActiveQueries(context.Background(), scraper, pod)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if n != 5 {
		t.Errorf("activeQueries: want 5 got %d", n)
	}
}
