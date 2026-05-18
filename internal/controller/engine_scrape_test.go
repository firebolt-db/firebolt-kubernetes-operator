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
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// fakeScraper drives the consumers (isPodDrained, scrapePodActiveQueries)
// without standing up a network server.
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

// fixedPortPodIPScraper hijacks the http.Transport dialer so the
// production Scrape URL (PodIP:MetricsPort/MetricsPath) actually
// connects to httptest's random port.
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

// TestResolveMetricScrapeMode covers explicit, empty, and missing
// instance. The default is asserted via MetricScrapeModeDefault so a
// change to the default doesn't fan out across tests.
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

// TestNewPodMetricScraperDispatch pins the factory's mode -> concrete
// type mapping, including the fail-closed branch for unknown values.
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

	// A zero-valued *kubernetes.Clientset is enough — we only assert
	// the returned type. The nil-clientset path is in its own test.
	clientset := &kubernetes.Clientset{}

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
				Clientset:       clientset,
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

// TestUnknownModeScraperCarriesReason asserts the error includes both
// the mode token (so a CRD typo is visible) and the reason.
func TestUnknownModeScraperCarriesReason(t *testing.T) {
	s := &unknownModeScraper{mode: "ApiserverProxy", reason: "clientset not initialized"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
	_, err := s.Scrape(context.Background(), pod)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "ApiserverProxy") || !strings.Contains(err.Error(), "clientset") {
		t.Errorf("error should name both the mode and the reason, got %v", err)
	}
}

// TestNewPodMetricScraper_NilClientsetGuard regression-tests the
// typed-nil-in-interface gotcha: ApiserverProxy + nil Clientset must
// produce a fail-closed scraper whose Scrape errors cleanly rather
// than panicking on CoreV1().
func TestNewPodMetricScraper_NilClientsetGuard(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = computev1alpha1.AddToScheme(scheme)

	const ns = "ns"
	const instName = "instance"

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: instName},
	}
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{MetricScrapeMode: computev1alpha1.MetricScrapeModeApiserverProxy},
	}
	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(engine, inst).Build()

	r := &FireboltEngineReconciler{
		Client:          fc,
		Scheme:          scheme,
		MetricsRecorder: metrics.NoOpEngineRecorder{},
		// Clientset intentionally left nil.
	}
	s := r.newPodMetricScraper(context.Background(), engine)
	if _, ok := s.(*unknownModeScraper); !ok {
		t.Fatalf("want *unknownModeScraper for nil Clientset, got %T", s)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: ns}}
	if _, err := s.Scrape(context.Background(), pod); err == nil {
		t.Fatal("expected error from fail-closed scraper, got nil")
	}
}

// TestIsPodDrained_FakeScraper pins the drained boundary: running +
// suspended == 0 -> drained, anything else -> not drained.
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
