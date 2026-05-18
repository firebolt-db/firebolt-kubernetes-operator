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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// MetricScrapeModeDefault is the mode used when the referenced
// FireboltInstance is missing, unreachable, or carries an empty
// spec.metricScrapeMode. Centralized so test fixtures and production
// agree on the fallback. See the docstring on
// FireboltInstanceSpec.MetricScrapeMode for why PodIP is the right
// default.
const MetricScrapeModeDefault = computev1alpha1.MetricScrapeModePodIP

// scrapeTimeout bounds a single pod metric scrape end-to-end (connect +
// request + body read). The drain probe and autoscaler are both wrapped
// in the controller-runtime reconcile context which already has its own
// deadline, but a per-scrape timeout protects the controller from a pod
// that accepts the TCP handshake and then stalls indefinitely (the
// classic "metrics handler holds a sync.Mutex during shutdown" failure
// mode). 10s is generous for /metrics, which is single-digit KB.
const scrapeTimeout = 10 * time.Second

// metricsHTTPClient is the http.Client used for direct PodIP scrapes.
// Package-level so the dial pool is shared across reconciles instead of
// being re-created on every scrape; controller-runtime tends to fan many
// reconciles out on the same controller, so a per-call client would burn
// ephemeral ports under heavy churn.
var metricsHTTPClient = &http.Client{
	Timeout: scrapeTimeout,
	Transport: &http.Transport{
		// Disable keep-alives: pod IPs are ephemeral across rollouts and
		// a stale idle connection to a recycled IP can hit a totally
		// different pod. The scrape rate per pod is low (one per
		// reconcile, with the autoscaler poll on top of that, both on
		// the order of seconds), so the lost keep-alive savings are
		// negligible compared to the correctness risk.
		DisableKeepAlives: true,
		DialContext: (&net.Dialer{
			Timeout: 3 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 3 * time.Second,
	},
}

// podMetricScraper fetches the raw Prometheus /metrics body from a
// single engine pod. The interface hides the transport mode (direct
// PodIP HTTP vs apiserver pods/proxy subresource) so consumers
// (checkDrainComplete, scrapeActiveQueries) iterate pods without
// repeating the mode switch on every call site.
//
// A scraper is intended to be constructed once per reconcile pass via
// FireboltEngineReconciler.newPodMetricScraper, which resolves the mode
// from the parent FireboltInstance. The same scraper is then handed to
// every pod-level helper that needs to read /metrics; the mode is
// already baked in, so those helpers do not see it.
//
// Implementations must be safe for concurrent use only within a single
// reconcile (we do not currently scrape pods in parallel, but factoring
// the contract as method-on-instance keeps that option open without
// retrofitting locks).
type podMetricScraper interface {
	// Scrape returns the raw bytes of the /metrics response from pod,
	// or an error describing why the fetch could not be completed. A
	// non-2xx response from the pod is surfaced as an error.
	Scrape(ctx context.Context, pod *corev1.Pod) ([]byte, error)

	// Mode reports which transport this scraper implements. Used only
	// for diagnostics (log lines, error messages); the dispatch happens
	// inside Scrape.
	Mode() computev1alpha1.MetricScrapeMode
}

// resolveMetricScrapeMode returns the metric scrape transport configured on
// the FireboltInstance referenced by engine.spec.instanceRef. The lookup
// goes through the controller-runtime cached client, so it does not add
// real apiserver traffic per scrape — both the engine and the instance
// are already watched.
//
// Behavior on missing / unset:
//
//   - Instance not found, or any other Get error -> default mode. We do
//     not propagate the error: a transient cache miss should not turn a
//     drain probe into a DrainProbeError. The instance gate in Reconcile
//     handles "instance truly missing" by blocking reconcile before the
//     scrape paths are reached for the phases that actually need the
//     instance; for PhaseDraining (which does not gate on instance) the
//     mode lookup is purely advisory, so the default is the right
//     fallback.
//   - Instance found but spec.metricScrapeMode == "" -> default mode.
//     This covers existing CRs created before the field existed.
//
// The default is intentionally MetricScrapeModePodIP, matching the CRD
// default and the in-cluster scraper convention. See the docstring on
// FireboltInstanceSpec.MetricScrapeMode for why.
func (r *FireboltEngineReconciler) resolveMetricScrapeMode(
	ctx context.Context,
	engine *computev1alpha1.FireboltEngine,
) computev1alpha1.MetricScrapeMode {
	inst := &computev1alpha1.FireboltInstance{}
	key := types.NamespacedName{Name: engine.Spec.InstanceRef, Namespace: engine.Namespace}
	if err := r.Get(ctx, key, inst); err != nil {
		return MetricScrapeModeDefault
	}
	if inst.Spec.MetricScrapeMode == "" {
		return MetricScrapeModeDefault
	}
	return inst.Spec.MetricScrapeMode
}

// newPodMetricScraper builds the per-reconcile scraper for an engine by
// resolving the transport mode once. Callers iterate pods against the
// returned scraper without re-resolving on every call.
//
// Returns a noopScraper on an unrecognized mode rather than nil so
// callers do not need a nil-check. The CRD enum validation makes this
// path unreachable in production; the safety net is for tests or
// hand-edited resources that bypass admission.
func (r *FireboltEngineReconciler) newPodMetricScraper(
	ctx context.Context,
	engine *computev1alpha1.FireboltEngine,
) podMetricScraper {
	mode := r.resolveMetricScrapeMode(ctx, engine)
	switch mode {
	case computev1alpha1.MetricScrapeModeApiserverProxy:
		return &apiserverProxyScraper{clientset: r.Clientset}
	case computev1alpha1.MetricScrapeModePodIP, "":
		return &podIPScraper{client: metricsHTTPClient}
	default:
		return &unknownModeScraper{mode: mode}
	}
}

// podIPScraper opens a direct HTTP connection from the controller pod
// to pod.Status.PodIP:MetricsPort. Requires the controller to run
// in-cluster on the pod network. This is the default transport and
// matches every standard in-cluster scraper (Prometheus PodMonitor,
// metrics-server, OpenTelemetry collector, kube-state-metrics).
type podIPScraper struct {
	client *http.Client
}

func (s *podIPScraper) Mode() computev1alpha1.MetricScrapeMode {
	return computev1alpha1.MetricScrapeModePodIP
}

func (s *podIPScraper) Scrape(ctx context.Context, pod *corev1.Pod) ([]byte, error) {
	if pod.Status.PodIP == "" {
		return nil, fmt.Errorf("pod %s has no PodIP yet", pod.Name)
	}
	// Engine /metrics is plain HTTP on MetricsPort; there is no TLS
	// material in the engine pod and Prometheus/PodMonitor scrapes it
	// the same way. The revive nolint here is deliberate.
	url := fmt.Sprintf("http://%s/%s", net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(MetricsPort)), //nolint:revive // engine metrics endpoint is plain HTTP by design
		trimLeadingSlash(MetricsPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building scrape request for %s: %w", pod.Name, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("scraping metrics from pod %s at %s: %w", pod.Name, url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		// Read up to a small bounded slice of the body for the
		// diagnostic. We do not want to read megabytes from a
		// misbehaving endpoint into a controller log line.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("scrape from pod %s returned %s: %s",
			pod.Name, resp.Status, string(body))
	}
	return io.ReadAll(resp.Body)
}

// apiserverProxyScraper routes the GET through the apiserver pods/proxy
// subresource. The apiserver enforces pods/proxy RBAC and dials the pod
// from its own ENI; this requires the cluster network to allow that
// path, which on EKS is NOT the default for MetricsPort. See the
// docstring on MetricScrapeModeApiserverProxy for when to use it.
type apiserverProxyScraper struct {
	clientset kubernetes.Interface
}

func (s *apiserverProxyScraper) Mode() computev1alpha1.MetricScrapeMode {
	return computev1alpha1.MetricScrapeModeApiserverProxy
}

func (s *apiserverProxyScraper) Scrape(ctx context.Context, pod *corev1.Pod) ([]byte, error) {
	if s.clientset == nil {
		return nil, errors.New("clientset not initialized")
	}
	return s.clientset.CoreV1().RESTClient().Get().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(fmt.Sprintf("%s:%d", pod.Name, MetricsPort)).
		SubResource("proxy").
		Suffix(MetricsPath).
		DoRaw(ctx)
}

// unknownModeScraper is the fail-closed branch for an unrecognized
// MetricScrapeMode. Production paths cannot reach this (CRD admission
// rejects unknown enum values) — it exists so a hand-edited or
// fixture-built FireboltInstance produces a loud error instead of being
// silently coerced into one of the real modes.
type unknownModeScraper struct {
	mode computev1alpha1.MetricScrapeMode
}

func (s *unknownModeScraper) Mode() computev1alpha1.MetricScrapeMode {
	return s.mode
}

func (s *unknownModeScraper) Scrape(_ context.Context, _ *corev1.Pod) ([]byte, error) {
	return nil, fmt.Errorf("unknown MetricScrapeMode %q", s.mode)
}

// trimLeadingSlash strips a single leading '/'. Used to normalize
// MetricsPath when concatenating into a base URL — MetricsPath has a
// leading slash by convention but we already emit one when joining
// host:port, so we drop the duplicate to avoid "http://...//metrics".
func trimLeadingSlash(s string) string {
	if s != "" && s[0] == '/' {
		return s[1:]
	}
	return s
}
