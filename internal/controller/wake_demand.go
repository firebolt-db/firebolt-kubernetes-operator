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
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/wakeagent"
)

// DefaultWakeDemandPollInterval is how often the operator asks the gateway
// agents which stopped engines have been asked for.
//
// Two seconds, not the autoStop loop's minute. It is also not tuned for
// tightness: total wake latency is dominated by engine cold start (tens of
// seconds), so halving the detection window buys a percent or two. The
// value is small enough to be invisible against that and large enough that
// polling a handful of gateway pods costs nothing measurable.
const DefaultWakeDemandPollInterval = 2 * time.Second

// wakeDemandScrapeTimeout bounds a single agent scrape. Short: an agent
// that cannot answer promptly is one whose demand we will pick up on the
// next tick anyway.
const wakeDemandScrapeTimeout = 3 * time.Second

// WakeDemandSource reports when the gateway last saw a request for an
// engine that had no ready endpoints.
//
// An interface so the engine reconciler can be unit-tested without a
// running poller, and so the no-op implementation below can stand in when
// wake-on-zero is disabled.
type WakeDemandSource interface {
	// LastDemand returns the most recent demand timestamp for the engine,
	// or nil when the gateway has not asked for it recently.
	LastDemand(namespace, engine string) *time.Time
}

// NoWakeDemand is the WakeDemandSource used when wake-on-zero is off. It
// reports no demand, ever, which leaves autoStop's behavior identical to
// what it was before wake existed.
type NoWakeDemand struct{}

// LastDemand implements WakeDemandSource.
func (NoWakeDemand) LastDemand(string, string) *time.Time { return nil }

type demandKey struct {
	namespace string
	engine    string
}

// WakeDemandTracker polls every gateway's wake agent and caches the
// per-engine demand timestamps they report.
//
// It runs as a manager Runnable rather than inside a reconcile because the
// signal is not tied to any one object's events: demand appears when a
// client sends a query, which no watch will tell us about. The operator is
// the only component in this path holding a write credential — the agents
// it polls are read-only — so the poll direction also fixes the trust
// direction. Nothing in a gateway pod ever calls the operator.
type WakeDemandTracker struct {
	Client    client.Client
	Clientset *kubernetes.Clientset

	// PollInterval defaults to DefaultWakeDemandPollInterval when zero.
	PollInterval time.Duration

	// Namespaces, when non-empty, restricts polling to these namespaces.
	// Mirrors the manager's own cache scoping so a namespaced install does
	// not try to list pods it cannot see.
	Namespaces []string

	// DemandPort is the agent port to scrape. Defaults to
	// gatewayWakeAgentDemandPort, which is the port the operator-rendered
	// sidecar listens on; the field exists so tests can point the poll at
	// a local server without loosening that contract in production.
	DemandPort int32

	// Events, when non-nil, receives one entry per engine whose demand
	// timestamp is newly observed. The engine controller watches it, so a
	// wake triggers a reconcile immediately instead of waiting for
	// auto-stop's own poll.
	//
	// Without this the 2s poll buys nothing: auto-stop reads the cache on
	// its pollInterval, which defaults to a minute, so a held query could
	// sit for most of that before the scale-up even started — on top of
	// the cold start it then has to wait through, against a 120s hold.
	Events chan event.GenericEvent

	mu     sync.RWMutex
	demand map[demandKey]time.Time
}

// NewWakeDemandTracker builds a tracker with its event channel wired.
func NewWakeDemandTracker(c client.Client, clientset *kubernetes.Clientset, namespaces []string) *WakeDemandTracker {
	return &WakeDemandTracker{
		Client:    c,
		Clientset: clientset,
		// Buffered so a burst of newly-demanded engines never blocks the
		// poll loop; the controller drains it, and a dropped duplicate
		// costs nothing because the next poll re-observes the same stamp.
		Events:     make(chan event.GenericEvent, 256),
		Namespaces: namespaces,
	}
}

// LastDemand implements WakeDemandSource.
func (t *WakeDemandTracker) LastDemand(namespace, engine string) *time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ts, ok := t.demand[demandKey{namespace: namespace, engine: engine}]
	if !ok {
		return nil
	}
	return &ts
}

// NeedLeaderElection reports false: every operator replica should track
// demand.
//
// The tracker only populates a cache; the write that follows is done by the
// engine reconciler, which is already leader-gated. Polling on standbys
// costs a few HTTP requests and means a leadership handover does not start
// with an empty demand map — which would otherwise drop wakes for one full
// autoStop cycle right after a failover.
func (t *WakeDemandTracker) NeedLeaderElection() bool { return false }

// Start implements manager.Runnable.
func (t *WakeDemandTracker) Start(ctx context.Context) error {
	interval := t.PollInterval
	if interval <= 0 {
		interval = DefaultWakeDemandPollInterval
	}
	log := logf.FromContext(ctx).WithName("wake-demand")
	log.Info("starting wake demand poller", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			t.pollOnce(ctx)
		}
	}
}

// pollOnce refreshes the demand cache from every gateway agent that could
// have something to say.
//
// Errors are logged at low verbosity and otherwise swallowed: a gateway
// that cannot be reached this tick will be reached on the next one, and a
// poll failure must never surface as an engine reconcile error.
func (t *WakeDemandTracker) pollOnce(ctx context.Context) {
	log := logf.FromContext(ctx).WithName("wake-demand")

	stopped, err := t.stoppedEngines(ctx)
	if err != nil {
		log.V(1).Info("listing engines failed, skipping this poll", "error", err.Error())
		return
	}
	namespaces := stopped.namespaces()
	if len(namespaces) == 0 {
		// Nothing is stopped, so nothing can need waking. Replacing the
		// cache with an empty map here is deliberate: leaving stale
		// entries would let a timestamp from before an engine started
		// re-trigger a wake after it is stopped again.
		t.replace(nil)
		return
	}

	fresh := make(map[demandKey]time.Time)
	for _, ns := range namespaces {
		pods, err := t.gatewayPods(ctx, ns)
		if err != nil {
			log.V(1).Info("listing gateway pods failed", "namespace", ns, "error", err.Error())
			continue
		}
		for i := range pods {
			pod := &pods[i]
			body, err := t.scrapeAgent(ctx, pod, t.scrapeMode(ctx, pod))
			if err != nil {
				log.V(1).Info("scraping wake agent failed",
					"namespace", ns, "pod", pod.Name, "error", err.Error())
				continue
			}
			// Gateway replicas each hold their own view, so the demand
			// for an engine is the most recent stamp any of them saw.
			for engine, ts := range parseWakeDemand(string(body)) {
				key := demandKey{namespace: ns, engine: engine}
				// Only engines that are actually stopped. The agent
				// stamps demand whenever an engine has no ready
				// endpoints, which also covers a running engine during
				// a node drain or a rolling restart — and auto-stop
				// honors wake above the idle check, so an unfiltered
				// stamp would pin such an engine at activeReplicas,
				// scaling a manually-sized engine DOWN in the middle of
				// its outage and freezing its idle timer for the TTL.
				if !stopped.has(key) {
					continue
				}
				if prev, ok := fresh[key]; !ok || ts.After(prev) {
					fresh[key] = ts
				}
			}
		}
	}
	t.replace(fresh)
}

// replace swaps in the new cache and announces every engine whose demand
// is newly observed or newer than what we had.
func (t *WakeDemandTracker) replace(fresh map[demandKey]time.Time) {
	t.mu.Lock()
	previous := t.demand
	t.demand = fresh
	t.mu.Unlock()

	if t.Events == nil {
		return
	}
	for key, ts := range fresh {
		if prev, ok := previous[key]; ok && !ts.After(prev) {
			continue
		}
		evt := event.GenericEvent{Object: &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: key.engine, Namespace: key.namespace},
		}}
		select {
		case t.Events <- evt:
		default:
			// Channel full: the controller is behind, and it will pick
			// this engine up from the cache on its next reconcile anyway.
		}
	}
}

// stoppedEngineSet is the set of engines currently at zero replicas.
type stoppedEngineSet map[demandKey]struct{}

func (s stoppedEngineSet) has(k demandKey) bool {
	_, ok := s[k]
	return ok
}

// namespaces returns the distinct namespaces holding a stopped engine.
func (s stoppedEngineSet) namespaces() []string {
	seen := make(map[string]struct{}, len(s))
	out := make([]string, 0, len(s))
	for k := range s {
		if _, ok := seen[k.namespace]; ok {
			continue
		}
		seen[k.namespace] = struct{}{}
		out = append(out, k.namespace)
	}
	return out
}

// stoppedEngines lists the engines at zero replicas, which both bounds the
// poll (a cluster with nothing stopped does no HTTP work) and filters what
// the agents report.
func (t *WakeDemandTracker) stoppedEngines(ctx context.Context) (stoppedEngineSet, error) {
	engines := &computev1alpha1.FireboltEngineList{}
	if err := t.Client.List(ctx, engines); err != nil {
		return nil, err
	}
	out := make(stoppedEngineSet)
	for i := range engines.Items {
		engine := &engines.Items[i]
		if engine.Spec.Replicas != 0 || !t.watches(engine.Namespace) {
			continue
		}
		out[demandKey{namespace: engine.Namespace, engine: engine.Name}] = struct{}{}
	}
	return out, nil
}

func (t *WakeDemandTracker) watches(namespace string) bool {
	if len(t.Namespaces) == 0 {
		return true
	}
	for _, ns := range t.Namespaces {
		if ns == namespace {
			return true
		}
	}
	return false
}

// gatewayPods returns the running gateway pods in a namespace.
//
// TRUST ASSUMPTION, stated because it is not obvious and cannot be fixed
// here: any pod in the namespace carrying this label and serving /demand is
// believed. Kubernetes does not validate ownerReferences and a pod creator
// may set both labels and serviceAccountName freely, so no amount of
// pod-identity checking makes this a boundary — a name or UID check would
// look like a fix without being one.
//
// The exposure is bounded. An attacker needs pod-create in the namespace,
// which already permits direct resource consumption; what spoofed demand
// adds is the ability to scale a stopped engine to activeReplicas, bounded
// by DefaultAutoStopWakeTTL, after which auto-stop parks it again. So the
// upside is cost abuse, not access: no engine spec is written, and the
// attacker gains nothing they could not get by creating pods directly.
//
// The real fix is authenticating the payload rather than identifying its
// sender — an operator-provisioned per-instance token the agent presents,
// or mTLS on the demand endpoint. Worth doing if wake ever gates something
// costlier than a scale-up. Tracked on FB-2553.
func (t *WakeDemandTracker) gatewayPods(ctx context.Context, namespace string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := t.Client.List(ctx, podList,
		client.InNamespace(namespace),
		client.MatchingLabels{LabelComponent: "gateway"},
	); err != nil {
		return nil, err
	}
	out := make([]corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		pod := podList.Items[i]
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		out = append(out, pod)
	}
	return out, nil
}

// wakeDemandHTTPClient mirrors metricsHTTPClient's settings and for the
// same reason: gateway pod IPs are reused across rollouts, so a cached idle
// connection can land on a pod other than the one just listed.
var wakeDemandHTTPClient = &http.Client{
	Timeout: wakeDemandScrapeTimeout,
	Transport: &http.Transport{
		DisableKeepAlives:     true,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		ResponseHeaderTimeout: 2 * time.Second,
	},
}

// scrapeMode resolves how to reach this gateway pod, from the
// spec.metricScrapeMode of the FireboltInstance that owns it.
//
// Reusing the engine scrape's own setting rather than inventing a second
// knob means the wake poll reaches gateway pods in exactly the
// environments the existing engine scrape already reaches engine pods —
// including clusters where the operator is not on the pod network, or
// where a NetworkPolicy denies direct pod-to-pod ingress. An operator who
// has already had to set ApiserverProxy for engine metrics does not have
// to discover a second reason to set it again.
//
// Any lookup failure falls back to the default: a transient cache miss
// should not silently change transport.
func (t *WakeDemandTracker) scrapeMode(ctx context.Context, pod *corev1.Pod) computev1alpha1.MetricScrapeMode {
	instanceName := pod.Labels[LabelInstance]
	if instanceName == "" {
		return MetricScrapeModeDefault
	}
	inst := &computev1alpha1.FireboltInstance{}
	key := types.NamespacedName{Name: instanceName, Namespace: pod.Namespace}
	if err := t.Client.Get(ctx, key, inst); err != nil || inst.Spec.MetricScrapeMode == "" {
		return MetricScrapeModeDefault
	}
	return inst.Spec.MetricScrapeMode
}

// scrapeAgent fetches one gateway pod's demand exposition over the
// transport the instance is configured for.
func (t *WakeDemandTracker) scrapeAgent(
	ctx context.Context,
	pod *corev1.Pod,
	mode computev1alpha1.MetricScrapeMode,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, wakeDemandScrapeTimeout)
	defer cancel()

	port := t.DemandPort
	if port == 0 {
		port = gatewayWakeAgentDemandPort
	}

	if mode == computev1alpha1.MetricScrapeModeApiserverProxy {
		if t.Clientset == nil {
			return nil, fmt.Errorf("clientset not initialized for %s mode", mode)
		}
		raw, err := t.Clientset.CoreV1().
			Pods(pod.Namespace).
			ProxyGet("http", pod.Name, strconv.Itoa(int(port)), "/demand", nil).
			DoRaw(ctx)
		if err != nil {
			return nil, fmt.Errorf("proxy scrape of %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		return raw, nil
	}

	// Plain HTTP: the demand endpoint carries no secrets, only engine
	// names the caller already supplied to the gateway.
	url := fmt.Sprintf("http://%s/demand", //nolint:revive // wake demand endpoint is plain HTTP by design
		net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := wakeDemandHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wake agent %s/%s returned %s", pod.Namespace, pod.Name, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// parseWakeDemand extracts engine → last-demand timestamps from the
// agent's Prometheus exposition.
//
// Deliberately narrow: it reads only wakeagent.MetricLastDemand and ignores
// every other series, so adding observability metrics to the agent cannot
// change what the operator acts on.
func parseWakeDemand(body string) map[string]time.Time {
	out := make(map[string]time.Time)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, wakeagent.MetricLastDemand+"{") {
			continue
		}
		engine, value, ok := parseLabeledSample(line, wakeagent.MetricLastDemand, "engine")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		out[engine] = time.Unix(secs, 0)
	}
	return out
}

// parseLabeledSample pulls the single label value and the sample value out
// of a line shaped `name{label="value"} 123`.
func parseLabeledSample(line, name, label string) (labelValue, sampleValue string, ok bool) {
	rest, found := strings.CutPrefix(line, name+"{"+label+`="`)
	if !found {
		return "", "", false
	}
	labelValue, rest, found = strings.Cut(rest, `"`)
	if !found || labelValue == "" {
		return "", "", false
	}
	rest, found = strings.CutPrefix(rest, "}")
	if !found {
		return "", "", false
	}
	sampleValue = strings.TrimSpace(rest)
	if sampleValue == "" {
		return "", "", false
	}
	return labelValue, sampleValue, true
}
