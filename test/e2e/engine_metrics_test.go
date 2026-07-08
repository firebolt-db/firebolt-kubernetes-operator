//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The operator's drain and autoStop logic trusts the engine to report
// in-flight work through firebolt_running_queries + firebolt_suspended_queries
// on the pod metrics port (internal/controller/constants.go). Unit tests cover
// the operator's side of that contract with faked metric bodies; this spec
// pins the engine's side: the running+suspended sum — the exact quantity
// isPodDrained and scrapePodActiveQueries consume — must rise while a query
// is in flight and settle back to its baseline once the load stops.
const (
	engineMetricsPort = 9090

	metricRunningQueries   = "firebolt_running_queries"
	metricSuspendedQueries = "firebolt_suspended_queries"
	// metricAnyStateQueries counts queries the engine holds in ANY state,
	// not just RUNNING/SUSPENDED. Tracked alongside the drain gauges so a
	// failure can distinguish "the engine never saw the queries" from "the
	// engine saw them but reported them outside RUNNING/SUSPENDED".
	metricAnyStateQueries = "firebolt_queries"

	// computeBoundQuery burns execution time with a negligible result set,
	// while HeavyQuery's in-flight time is dominated by streaming its
	// ~500MB result. The two shapes park an in-flight query in different
	// engine states, and the drain gauge must count both.
	// 300M rows keeps each execution multi-second but comfortably inside
	// RunQuery's 33s curl budget (~60M rows/s observed on the e2e engine).
	computeBoundQuery = "SELECT sum(x + 1) FROM generate_series(1, 300000000) g(x)"

	// metricRiseWindow bounds how long each load shape keeps queries in
	// flight while the gauge is polled. Continuous back-to-back queries
	// run the whole window, so a functioning gauge has many chances to be
	// observed above baseline.
	metricRiseWindow = 45 * time.Second
	// metricSettleTimeout bounds the wait for the gauge to return to its
	// baseline after the query load stops -- the exact condition the drain
	// check (isPodDrained) relies on to release pods.
	metricSettleTimeout = 60 * time.Second
	metricPollInterval  = 250 * time.Millisecond
)

// scrapeEnginePodMetrics fetches the Prometheus text exposition from an engine
// pod's metrics endpoint, curl-ing the pod IP from inside the cluster via the
// client pod (pod IPs are not routable from the host where this suite runs).
func scrapeEnginePodMetrics(ctx context.Context, clientPod, podIP string) (string, error) {
	url := fmt.Sprintf("http://%s:%d/metrics", podIP, engineMetricsPort)
	args := kubectlArgs("exec", clientPod, "-n", testNamespace, "--",
		"curl", "-sSf", "--connect-timeout", "2", "--max-time", "5", url)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("scraping %s: %w: %s", url, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// parseEngineGauge mirrors the operator's parsePrometheusGauge
// (internal/controller/engine_state.go) line by line: skip blank and "#"
// metadata lines, match the exact unlabeled sample name followed by a space,
// strip an optional trailing timestamp ("<value> [<ts>]"), parse as float,
// clamp to int64. The parser is duplicated here because the operator's is
// unexported; the semantics must stay aligned so this spec measures exactly
// the value the drain check consumes — any divergence (e.g. mishandling a
// timestamped sample) would make the test disagree with production drain.
func parseEngineGauge(body, name string) (int64, bool) {
	prefix := name + " "
	for _, line := range strings.Split(body, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimSpace(line[len(prefix):])
		if idx := strings.IndexByte(rest, ' '); idx >= 0 {
			rest = rest[:idx]
		}
		v, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return 0, false
		}
		return int64(v), true
	}
	return 0, false
}

// parseActiveQueries returns firebolt_running_queries +
// firebolt_suspended_queries — the drained/idle signal isPodDrained and
// scrapePodActiveQueries compute. ok is false when either gauge is missing
// or unparsable, matching the operator's fail-closed handling.
func parseActiveQueries(body string) (int64, bool) {
	running, okR := parseEngineGauge(body, metricRunningQueries)
	suspended, okS := parseEngineGauge(body, metricSuspendedQueries)
	if !okR || !okS {
		return 0, false
	}
	return running + suspended, true
}

// queryGaugeLines extracts the query-state samples from a metrics body so a
// failure message can show the interesting slice instead of the multi-
// thousand-line exposition.
func queryGaugeLines(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "firebolt_") && strings.Contains(line, "quer") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// gaugeLoadStats aggregates what was observed while a query load shape ran.
type gaugeLoadStats struct {
	succeeded, failed int64
	failureReasons    map[string]int
	sampleErrors      []string
	// maxActive is the maximum running+suspended sum sampled from a single
	// scrape — the quantity the pass/fail assertions use. The per-gauge
	// maxima below are diagnostics only.
	maxActive    int64
	maxRunning   int64
	maxSuspended int64
	maxAnyState  int64
	polls        int
	lastBody     string
}

// observeGaugesUnderLoad keeps the given query continuously in flight
// (back-to-back worker loops) for the whole window while polling the engine
// pod's metrics endpoint, recording the maxima of the query-state gauges.
func observeGaugesUnderLoad(ctx context.Context, clientPod, engineName, podIP, sql string, workers int, window time.Duration) gaugeLoadStats {
	stats := gaugeLoadStats{failureReasons: map[string]int{}}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var succeeded, failed atomic.Int64
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer GinkgoRecover()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := RunQuery(ctx, clientPod, engineName, sql); err != nil {
					failed.Add(1)
					mu.Lock()
					stats.failureReasons[categorizeQueryError(err.Error())]++
					if len(stats.sampleErrors) < 3 {
						msg := err.Error()
						if len(msg) > 300 {
							msg = msg[:300] + "..."
						}
						stats.sampleErrors = append(stats.sampleErrors, msg)
					}
					mu.Unlock()
				} else {
					succeeded.Add(1)
				}
			}
		}()
	}

	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		body, err := scrapeEnginePodMetrics(ctx, clientPod, podIP)
		if err == nil {
			stats.polls++
			stats.lastBody = body
			if v, ok := parseActiveQueries(body); ok && v > stats.maxActive {
				stats.maxActive = v
			}
			if v, ok := parseEngineGauge(body, metricRunningQueries); ok && v > stats.maxRunning {
				stats.maxRunning = v
			}
			if v, ok := parseEngineGauge(body, metricSuspendedQueries); ok && v > stats.maxSuspended {
				stats.maxSuspended = v
			}
			if v, ok := parseEngineGauge(body, metricAnyStateQueries); ok && v > stats.maxAnyState {
				stats.maxAnyState = v
			}
		}
		time.Sleep(metricPollInterval)
	}

	close(stop)
	wg.Wait()
	stats.succeeded = succeeded.Load()
	stats.failed = failed.Load()
	return stats
}

var _ = Describe("Firebolt Engine Metrics", func() {
	Describe("Engine Query Metrics", Ordered, func() {
		var (
			instanceName = "inst-metrics" + queryConfig.Suffix
			engineName   = "test-metrics" + queryConfig.Suffix + "-engine"
			clientPod    = "client-metrics" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance for engine metrics test")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up engine metrics test")
			defer TeardownTestInstance(ctx, lc)
			DeleteClientPod(ctx, clientPod)
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})

		It("should raise the active-query gauges (running+suspended) while queries are in flight", func() {
			By("Creating engine with 1 replica")
			Expect(CreateEngine(ctx, instanceName, engineName, 1)).To(Succeed())

			By("Waiting for engine to become ready")
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())

			By("Waiting for engine status to be stable")
			Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())

			By("Resolving the engine pod IP")
			pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: "firebolt.io/engine=" + engineName,
			})
			Expect(err).NotTo(HaveOccurred())
			podIP := ""
			for _, pod := range pods.Items {
				if pod.Status.Phase == corev1.PodRunning && pod.Status.PodIP != "" {
					podIP = pod.Status.PodIP
					break
				}
			}
			Expect(podIP).NotTo(BeEmpty(), "no running engine pod with an IP found")

			By("Scraping the idle baseline")
			body, err := scrapeEnginePodMetrics(ctx, clientPod, podIP)
			Expect(err).NotTo(HaveOccurred())
			baseline, ok := parseActiveQueries(body)
			Expect(ok).To(BeTrue(),
				"%s and/or %s missing from the engine metrics endpoint; the drain check fails closed without them.\nQuery-state gauges:\n%s",
				metricRunningQueries, metricSuspendedQueries, queryGaugeLines(body))
			GinkgoWriter.Printf("Idle baseline: active (running+suspended) = %d\n", baseline)

			// Both load shapes keep two workers looping their query
			// back-to-back so the engine is continuously busy for the whole
			// observation window regardless of individual query duration.
			By("Keeping compute-bound queries in flight while polling the gauge")
			computeStats := observeGaugesUnderLoad(ctx, clientPod, engineName, podIP, computeBoundQuery, 2, metricRiseWindow)
			By("Keeping streaming-heavy queries in flight while polling the gauge")
			// A single worker: two concurrent ~500MB aggregations exceed the
			// e2e engine's 1.73 GiB memory limit and OOM instead of running.
			streamStats := observeGaugesUnderLoad(ctx, clientPod, engineName, podIP, HeavyQuery, 1, metricRiseWindow)

			for _, phase := range []struct {
				name  string
				stats gaugeLoadStats
			}{{"compute-bound", computeStats}, {"streaming-heavy", streamStats}} {
				GinkgoWriter.Printf("%s load: %d succeeded, %d failed, %d polls; max active=%d (%s=%d, %s=%d), %s=%d\n",
					phase.name, phase.stats.succeeded, phase.stats.failed, phase.stats.polls,
					phase.stats.maxActive,
					metricRunningQueries, phase.stats.maxRunning,
					metricSuspendedQueries, phase.stats.maxSuspended,
					metricAnyStateQueries, phase.stats.maxAnyState)
				for reason, count := range phase.stats.failureReasons {
					GinkgoWriter.Printf("  query failure category: %q x%d\n", reason, count)
				}
				for _, msg := range phase.stats.sampleErrors {
					GinkgoWriter.Printf("  sample query error: %s\n", msg)
				}
			}

			Expect(computeStats.succeeded+streamStats.succeeded).To(BeNumerically(">", 0),
				"no query completed successfully (%d failed); cannot judge the gauge without real in-flight queries",
				computeStats.failed+streamStats.failed)

			maxObserved := computeStats.maxActive
			if streamStats.maxActive > maxObserved {
				maxObserved = streamStats.maxActive
			}
			Expect(maxObserved).To(BeNumerically(">", baseline),
				"active queries (%s+%s) never rose above the idle baseline (%d) while queries were continuously in flight "+
					"(compute-bound: %d completed, max %s=%d; streaming-heavy: %d completed, max %s=%d) -- "+
					"the drain check and autoStop cannot see in-flight work.\nQuery-state gauges in the last mid-load scrape:\n%s",
				metricRunningQueries, metricSuspendedQueries, baseline,
				computeStats.succeeded, metricAnyStateQueries, computeStats.maxAnyState,
				streamStats.succeeded, metricAnyStateQueries, streamStats.maxAnyState,
				queryGaugeLines(streamStats.lastBody))

			By("Waiting for the active-query gauges to settle back to the baseline")
			Eventually(func(g Gomega) {
				body, err := scrapeEnginePodMetrics(ctx, clientPod, podIP)
				g.Expect(err).NotTo(HaveOccurred())
				v, ok := parseActiveQueries(body)
				g.Expect(ok).To(BeTrue())
				g.Expect(v).To(BeNumerically("<=", baseline),
					"active queries (%s+%s) stuck above baseline after load stopped; isPodDrained would never release the pod",
					metricRunningQueries, metricSuspendedQueries)
			}, metricSettleTimeout, metricPollInterval).Should(Succeed())

			By("Deleting engine")
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())

			By("Waiting for all resources to be deleted")
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})
	})
})
