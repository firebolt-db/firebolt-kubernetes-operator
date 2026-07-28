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
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// This spec is the consumer-side half of the query-liveness contract pinned
// by engine_metrics_test.go: with drainCheckEnabled=true, a blue-green
// rollout must HOLD the old generation in draining while queries are in
// flight on its pods, and release it promptly once they finish. Every other
// spec in the suite runs with the drain check off, so before this spec the
// busy-pod-blocks-cleanup behavior had no end-to-end coverage at all — a
// drain signal that always read "drained" (as engine builds before
// 2026-07-07 produced) passed the whole suite.
//
// The instance uses metricScrapeMode=ApiserverProxy because the in-process
// operator scrapes from the host, where kind pod IPs are unreachable.
const (
	// drainHoldWindow is how long the spec requires the draining phase to
	// persist while old-generation pods are busy. With drainCheckInterval=2s
	// this spans several consecutive drain probes, so a single spurious
	// "drained" reading cannot pass.
	drainHoldWindow = 15 * time.Second
	// drainReleaseTimeout bounds draining -> cleaning -> stable after the
	// load stops (the last in-flight query may still need to finish).
	drainReleaseTimeout = 120 * time.Second
	// rolloutToDrainingTimeout bounds creating -> switching -> draining for
	// the new generation (pod schedule + engine boot + readiness).
	rolloutToDrainingTimeout = 300 * time.Second

	// drainRolloutTolerationKey is the no-op toleration the spec adds to
	// the pod template purely to trigger a blue-green rollout. No node
	// carries a matching taint, so scheduling is unaffected.
	drainRolloutTolerationKey = "firebolt.io/e2e-drain-under-load"

	// drainLoadWorkers is how many queries the in-pod loop keeps in flight
	// concurrently. Their execution windows overlap, so the union leaves no
	// instant where the engine has nothing running.
	//
	// Deliberately 2, not more: the client pod has a 16Mi memory limit, and two
	// is exactly the peak concurrency the previous shape already reached (one
	// curl per kubectl exec, two execs), so this cannot OOM a pod that was fine
	// before. Raising it would mean raising CreateClientPod's limit, which every
	// other spec shares. Concurrency is not what fixes the flake anyway — moving
	// the loop in-pod is, since that shrinks the hole between consecutive
	// queries from a kubectl-exec round-trip to a local process spawn.
	drainLoadWorkers = 2
	// drainLoadStopFile is touched inside the client pod to end the loop.
	drainLoadStopFile = "/tmp/e2e-drain-load-stop"
	// drainLoadBackstop bounds the in-pod loop's own lifetime, in seconds, in
	// case the test dies before it can touch the stop file. Must exceed
	// rolloutToDrainingTimeout + drainHoldWindow, since the load has to stay up
	// across the whole rollout wait and the hold.
	drainLoadBackstop = 420
	// drainLoadDrainTimeout bounds waiting for the in-pod loop to notice the
	// stop file: one worker may be mid-query, and curl's own --max-time is 33s.
	drainLoadDrainTimeout = 60 * time.Second
)

// lockedBuffer collects the exec's output. The command runs on its own
// goroutine, so writes must not race a read that happens after a timeout.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// keepPodBusy keeps queries continuously in flight against one pod IP until the
// returned stop func is called, and reports how many completed.
//
// The loop runs INSIDE the client pod under a single long-lived kubectl exec,
// and that is the entire point. Re-issuing a query from the test process costs a
// kubectl-exec round-trip — API-server SPDY handshake plus a container process
// spawn — during which no query is running on the engine and
// firebolt_running_queries is legitimately 0. The drain probe samples that gauge
// instantaneously, on every reconcile, so a scrape landing in one of those holes
// correctly concludes the drain is complete and releases the generation. The
// earlier shape here (two goroutines each looping kubectl exec) only lowered the
// odds of both workers being between queries at the same instant; it could not
// remove them, and a contended runner widens every hole, which is what made this
// spec fail across unrelated branches. In-pod, re-issuing costs a local curl
// spawn and drainLoadWorkers of them overlap, so the busy signal has no gap for
// the probe to find.
//
// This matters because the spec asserts something STRONGER than the drain
// contract. The contract is "do not release a generation with in-flight
// queries"; the assertion is "stay in draining for drainHoldWindow while I load
// it". Those coincide only while the load is genuinely gapless — during an idle
// instant the operator is entitled to release, so a gap makes the spec fail on
// correct behaviour. Fixing the premise is what keeps the strong assertion
// honest; weakening the assertion instead (only failing when a release coincides
// with an observed non-zero gauge) would let an operator that never checks at
// all pass whenever it got lucky with the timing.
//
// Stopping is a file the test touches, not a kill: killing kubectl does not reap
// the shell inside the container, and a loop left running would keep the pod busy
// and stall the release assertion that follows.
func keepPodBusy(ctx context.Context, clientPod, podIP string) (stop func() (succeeded, failed int64)) {
	url := fmt.Sprintf("http://%s/?query_label=e2e-drain-load&output_format=JSON_Compact",
		net.JoinHostPort(podIP, "3473"))

	// POSIX sh — the client pod is curlimages/curl, which is Alpine/BusyBox.
	// The SQL arrives through the environment rather than being interpolated
	// into the script, so no quoting in the query can break the shell.
	script := fmt.Sprintf(`
rm -f %[1]s
deadline=$(( $(date +%%s) + %[2]d ))
w=0
while [ $w -lt %[3]d ]; do
  w=$((w + 1))
  (
    while [ ! -f %[1]s ] && [ "$(date +%%s)" -lt "$deadline" ]; do
      if curl -sSf --connect-timeout 2 --max-time 33 -X POST \
          -H 'Content-Type: text/plain' --data-binary "$QUERY" %[4]q >/dev/null 2>&1; then
        echo OK
      else
        echo FAIL
      fi
    done
  ) &
done
wait
`, drainLoadStopFile, drainLoadBackstop, drainLoadWorkers, url)

	args := kubectlArgs("exec", clientPod, "-n", testNamespace, "--",
		"env", "QUERY="+computeBoundQuery, "sh", "-c", script)
	out := &lockedBuffer{}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = out
	cmd.Stderr = out

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer GinkgoRecover()
		_ = cmd.Run() // a non-zero exit is reported through the counts below
	}()

	return func() (int64, int64) {
		touch := exec.CommandContext(ctx, "kubectl", kubectlArgs(
			"exec", clientPod, "-n", testNamespace, "--", "touch", drainLoadStopFile)...)
		if err := touch.Run(); err != nil {
			GinkgoWriter.Printf("could not touch %s (loop will stop at its backstop): %v\n",
				drainLoadStopFile, err)
		}
		select {
		case <-done:
		case <-time.After(drainLoadDrainTimeout):
			GinkgoWriter.Printf("in-pod load loop did not exit within %s\n", drainLoadDrainTimeout)
		}
		var succeeded, failed int64
		for _, line := range strings.Split(out.String(), "\n") {
			switch strings.TrimSpace(line) {
			case "OK":
				succeeded++
			case "FAIL":
				failed++
			}
		}
		return succeeded, failed
	}
}

// drainHoldFailure explains a hold-window failure in terms of whether the
// spec's own premise held, because the two causes need opposite responses and
// the failure output could not previously tell them apart: the anti-vacuity
// check sat after the hold assertion, so a failing run never reached it.
func drainHoldFailure(samples []int64, idle int) string {
	rendered := make([]string, 0, len(samples))
	for _, s := range samples {
		rendered = append(rendered, strconv.FormatInt(s, 10))
	}
	joined := strings.Join(rendered, ",")
	if idle > 0 {
		return fmt.Sprintf("INCONCLUSIVE, not a product failure: the load generator left the "+
			"old-generation pod idle — running+suspended read 0 on %d of %d samples [%s]. "+
			"Releasing a drain during an idle instant is permitted by the contract, so this run "+
			"says nothing about the operator. Fix the load generator, not the operator.",
			idle, len(samples), joined)
	}
	return fmt.Sprintf("LIKELY CONTRACT VIOLATION: the draining generation was released even "+
		"though running+suspended was non-zero at every sample [%s]. Sampling is coarser than "+
		"the drain probe, so an unobserved idle instant is not fully excluded, but the operator "+
		"releasing a generation with queries in flight is the first thing to rule out.", joined)
}

var _ = Describe("Firebolt Engine Drain", func() {
	Describe("Drain Under Load", Ordered, func() {
		var (
			instanceName = "inst-drain" + queryConfig.Suffix
			engineName   = "test-drain" + queryConfig.Suffix + "-engine"
			clientPod    = "client-drain" + queryConfig.Suffix
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

		BeforeAll(func() {
			By("Setting up FireboltInstance with ApiserverProxy metric scraping")
			var err error
			lc, err = SetupTestInstanceWithScrapeMode(ctx, instanceName, computev1alpha1.MetricScrapeModeApiserverProxy)
			Expect(err).NotTo(HaveOccurred())
			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
		})

		AfterAll(func() {
			By("Cleaning up drain-under-load test")
			defer TeardownTestInstance(ctx, lc)
			DeleteClientPod(ctx, clientPod)
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})

		It("should hold the draining generation while queries are in flight and release it after", func() {
			By("Creating engine with the drain check enabled")
			Expect(CreateEngineWithDrainCheck(ctx, instanceName, engineName, 1)).To(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterReadyTimeout)).To(Succeed())

			By("Resolving the active-generation pod")
			_, activeGen, err := GetEngineGeneration(ctx, engineName)
			Expect(err).NotTo(HaveOccurred())
			oldPods, err := EnginePodsForGeneration(ctx, engineName, activeGen)
			Expect(err).NotTo(HaveOccurred())
			Expect(oldPods).To(HaveLen(1))
			oldPod := oldPods[0]
			Expect(oldPod.Status.PodIP).NotTo(BeEmpty())

			By("Starting continuous queries against the old-generation pod")
			stopLoad := keepPodBusy(ctx, clientPod, oldPod.Status.PodIP)

			By("Triggering a blue-green rollout via a no-op toleration")
			Expect(UpdateEngineScheduling(ctx, engineName, nil, []corev1.Toleration{{
				Key:      drainRolloutTolerationKey,
				Operator: corev1.TolerationOpExists,
				Effect:   corev1.TaintEffectNoSchedule,
			}}, nil)).To(Succeed())

			By("Waiting for the rollout to reach draining on the old generation")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseDraining)))
				g.Expect(engine.Status.DrainingGeneration).NotTo(BeNil())
				g.Expect(*engine.Status.DrainingGeneration).To(Equal(activeGen))
			}, rolloutToDrainingTimeout, pollInterval).Should(Succeed())

			By("Verifying the busy old generation is held in draining")
			// The gauge is sampled alongside the phase so a failure can say
			// whether the premise held. Recording it does not weaken the
			// assertion: the phase check below is unconditional, so any release
			// fails the spec whether or not a sample caught the pod busy.
			var activeSamples []int64
			idleSamples := 0
			Consistently(func(g Gomega) {
				if body, err := scrapeEnginePodMetrics(ctx, clientPod, oldPod.Status.PodIP); err == nil {
					if v, ok := parseActiveQueries(body); ok {
						activeSamples = append(activeSamples, v)
						if v == 0 {
							idleSamples++
						}
					}
				}

				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseDraining)),
					drainHoldFailure(activeSamples, idleSamples))
				g.Expect(engine.Status.DrainingGeneration).NotTo(BeNil())

				pod, err := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, oldPod.Name, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred(), "old-generation pod disappeared mid-drain")
				g.Expect(pod.DeletionTimestamp).To(BeNil(), "old-generation pod is terminating while busy")
			}, drainHoldWindow, 1*time.Second).Should(Succeed())

			// Belt-and-braces on the premise for a run that PASSED: a hold that
			// never saw the pod busy proved nothing, and would otherwise be
			// indistinguishable from a real one.
			Expect(activeSamples).NotTo(BeEmpty(), "no gauge sample was taken during the hold window")
			Expect(idleSamples).To(BeZero(),
				"the pod went idle during the hold window (%d of %d samples read 0); the hold "+
					"proved nothing even though it passed", idleSamples, len(activeSamples))

			By("Stopping the query load")
			succeeded, failed := stopLoad()
			GinkgoWriter.Printf("Old-generation load: %d succeeded, %d failed\n", succeeded, failed)
			Expect(succeeded).To(BeNumerically(">", 0),
				"no query completed against the old pod (%d failed); the hold assertion proved nothing", failed)

			By("Waiting for the drained generation to be released and cleaned")
			Eventually(func(g Gomega) {
				engine, err := GetEngine(ctx, engineName)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(string(engine.Status.Phase)).To(Equal(string(computev1alpha1.PhaseStable)))
				g.Expect(engine.Status.DrainingGeneration).To(BeNil())

				pods, err := EnginePodsForGeneration(ctx, engineName, activeGen)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pods).To(BeEmpty(), "old-generation pods survived drain completion")
			}, drainReleaseTimeout, pollInterval).Should(Succeed())

			By("Verifying the new generation serves queries")
			output, err := RunQuery(ctx, clientPod, engineName, queryConfig.Query)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(queryConfig.Validator(result)).To(BeTrue(), "Query result validation failed")

			By("Deleting engine")
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})
	})
})
