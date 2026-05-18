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

// MetricScrapeModeDefault is the fallback when the parent FireboltInstance
// is missing or carries an empty spec.metricScrapeMode. See the docstring
// on FireboltInstanceSpec.MetricScrapeMode for why PodIP is the default.
const MetricScrapeModeDefault = computev1alpha1.MetricScrapeModePodIP

// scrapeTimeout bounds a single pod scrape end-to-end. The reconcile
// context already has its own deadline; this is defense against a pod
// that accepts the TCP handshake then stalls (metrics handler holding a
// sync.Mutex during shutdown is the classic case).
const scrapeTimeout = 10 * time.Second

// metricsHTTPClient is shared across reconciles so we don't burn
// ephemeral ports building a client per call. DisableKeepAlives because
// pod IPs are reused across rollouts and a cached idle conn can land on
// a different pod than the one we just listed.
var metricsHTTPClient = &http.Client{
	Timeout: scrapeTimeout,
	Transport: &http.Transport{
		DisableKeepAlives:     true,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		ResponseHeaderTimeout: 3 * time.Second,
	},
}

// podMetricScraper fetches the raw Prometheus /metrics body from one
// pod. The interface hides the transport (direct PodIP HTTP vs apiserver
// pods/proxy) so consumers iterate pods without repeating the mode
// switch. Build one per reconcile via newPodMetricScraper.
type podMetricScraper interface {
	Scrape(ctx context.Context, pod *corev1.Pod) ([]byte, error)
	// Mode is used only for diagnostics; dispatch happens inside Scrape.
	Mode() computev1alpha1.MetricScrapeMode
}

// resolveMetricScrapeMode reads spec.metricScrapeMode from the parent
// FireboltInstance via the cached client. Any Get error or empty value
// returns MetricScrapeModeDefault; we deliberately do not propagate the
// error because a transient cache miss must not surface as a
// DrainProbeError. The instance gate in Reconcile is the right place to
// block on a truly missing instance.
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

// newPodMetricScraper resolves the mode once and returns the matching
// implementation. Never returns nil; unrecognized modes and missing
// dependencies map to unknownModeScraper so callers can skip nil-checks.
func (r *FireboltEngineReconciler) newPodMetricScraper(
	ctx context.Context,
	engine *computev1alpha1.FireboltEngine,
) podMetricScraper {
	mode := r.resolveMetricScrapeMode(ctx, engine)
	switch mode {
	case computev1alpha1.MetricScrapeModeApiserverProxy:
		// Guard the typed nil here, where r.Clientset is still
		// *kubernetes.Clientset and the nil check actually fires.
		if r.Clientset == nil {
			return &unknownModeScraper{
				mode:   mode,
				reason: "clientset not initialized for ApiserverProxy mode",
			}
		}
		return &apiserverProxyScraper{clientset: r.Clientset}
	case computev1alpha1.MetricScrapeModePodIP, "":
		return &podIPScraper{client: metricsHTTPClient}
	default:
		return &unknownModeScraper{mode: mode}
	}
}

// podIPScraper dials pod.Status.PodIP:MetricsPort directly. Requires
// the controller to run in-cluster on the pod network — the default
// path, matching Prometheus / metrics-server / OpenTelemetry / KSM.
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
	// Engine /metrics is plain HTTP; Prometheus PodMonitor scrapes it
	// the same way. nolint:revive deliberate.
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
		// Bounded body read so a misbehaving endpoint cannot flood the log.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("scrape from pod %s returned %s: %s",
			pod.Name, resp.Status, string(body))
	}
	return io.ReadAll(resp.Body)
}

// apiserverProxyScraper routes the GET through the apiserver pods/proxy
// subresource. Opt-in via spec.metricScrapeMode=ApiserverProxy.
//
// clientset is the concrete *kubernetes.Clientset and NOT
// kubernetes.Interface. A typed-nil pointer boxed into an interface is
// not == nil (the classic Go gotcha), so widening this would turn the
// construction-time and runtime nil checks into no-ops and a missing
// clientset would panic on CoreV1() instead of returning an error.
type apiserverProxyScraper struct {
	clientset *kubernetes.Clientset
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

// unknownModeScraper is the fail-closed branch: every Scrape call errors.
// Used for unrecognized modes (unreachable under CRD validation) and for
// modes whose preconditions failed (e.g. ApiserverProxy without a
// clientset). reason is appended to the error when set.
type unknownModeScraper struct {
	mode   computev1alpha1.MetricScrapeMode
	reason string
}

func (s *unknownModeScraper) Mode() computev1alpha1.MetricScrapeMode {
	return s.mode
}

func (s *unknownModeScraper) Scrape(_ context.Context, _ *corev1.Pod) ([]byte, error) {
	if s.reason != "" {
		return nil, fmt.Errorf("unsupported MetricScrapeMode %q: %s", s.mode, s.reason)
	}
	return nil, fmt.Errorf("unknown MetricScrapeMode %q", s.mode)
}

// trimLeadingSlash drops one leading '/' so MetricsPath joined onto a
// host:port URL doesn't emit "http://...//metrics".
func trimLeadingSlash(s string) string {
	if s != "" && s[0] == '/' {
		return s[1:]
	}
	return s
}
