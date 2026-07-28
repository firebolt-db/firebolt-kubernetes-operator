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
)

// Continuous query load for the specs that assert the operator does NOT act on
// a busy engine — the drain hold and the autoStop busy-hold. Both watch a signal
// derived from the same query-liveness gauges, so both need load with no gaps in
// it, and both were written with a load generator that could not provide that.
//
// The load loop runs INSIDE the client pod under a single long-lived kubectl
// exec, and that is the entire point. Re-issuing a query from the test process
// costs a kubectl-exec round trip — API-server SPDY handshake plus a container
// process spawn — during which no query is running on the engine and
// firebolt_running_queries is legitimately 0. The operator samples that gauge
// instantaneously, so a scrape landing in one of those holes correctly concludes
// the engine is idle: the drain releases its generation, or autoStop scales down.
// Both specs then fail on behaviour that is correct against what the operator
// actually observed. Two overlapping workers, which is what both specs used
// before, only lower the odds of both being in a hole at the same instant; they
// cannot remove them, and a contended runner widens every hole. In-pod,
// re-issuing costs a local process spawn and loadWorkers of them overlap.
//
// This matters because both specs assert something STRONGER than the contract
// they are protecting. The contract is "do not act on an engine with in-flight
// queries"; the assertion is "do not act for the whole hold window while I load
// it". Those coincide only while the load is gapless — during an idle instant
// the operator is entitled to act. Fixing the premise is what keeps the strong
// assertion honest. Weakening the assertion instead (only failing when the
// operator acts at a moment a sample happened to show work in flight) would let
// an operator that never checks at all pass whenever it got lucky with timing,
// and the luck lives exactly where the flake does.
const (
	// loadWorkers is how many queries the in-pod loop keeps in flight at once.
	//
	// Deliberately 2, not more: the client pod has a 16Mi memory limit, and two
	// is the peak concurrency both specs already reached (one curl per exec, two
	// execs), so this cannot OOM a pod that was fine before. Raising it would
	// mean raising CreateClientPod's limit, which every spec shares. Concurrency
	// is not what fixes the gap — running in-pod is.
	loadWorkers = 2
	// loadStopFile is touched inside the client pod to end the loop. Each spec
	// has its own client pod, so a fixed path cannot collide between specs.
	loadStopFile = "/tmp/e2e-load-stop"
	// loadBackstop bounds the in-pod loop's own lifetime, in seconds, in case the
	// test dies before it can stop the loop. It must exceed the longest wait any
	// caller holds load across — the drain spec's rolloutToDrainingTimeout (300s)
	// plus its hold window — since load has to stay up for all of it.
	loadBackstop = 420
	// loadDrainTimeout bounds waiting for the loop to notice the stop file: a
	// worker may be mid-query, and curl's own --max-time is 33s.
	loadDrainTimeout = 60 * time.Second
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

// engineServiceQueryURL is the URL RunQuery targets: the engine's ClusterIP
// Service. Kept in step with RunQuery by hand — a load loop that talked to a
// different endpoint than the spec's own queries would be a silent divergence.
func engineServiceQueryURL(engineName string) string {
	return fmt.Sprintf("http://%s-service.%s.svc.cluster.local:3473/?query_label=e2e-load&output_format=JSON_Compact",
		engineName, testNamespace)
}

// enginePodQueryURL is the URL RunQueryAgainstPodIP targets: one pod directly,
// bypassing the Service. The drain spec needs this to keep load on the OLD
// generation after the selector has flipped away from it.
func enginePodQueryURL(podIP string) string {
	return fmt.Sprintf("http://%s/?query_label=e2e-load&output_format=JSON_Compact",
		net.JoinHostPort(podIP, "3473"))
}

// keepURLBusy keeps queries continuously in flight against one URL until the
// returned stop func is called, and reports how many completed.
//
// Stopping is a file the test touches, not a kill: killing kubectl does not reap
// the shell inside the container, and a loop left running would keep the engine
// busy and stall whatever the spec asserts next. The returned func is idempotent,
// so a DeferCleanup can guarantee the loop is stopped on any exit while the
// success path still calls it explicitly to get the counts.
func keepURLBusy(ctx context.Context, clientPod, url, query string) (stop func() (succeeded, failed int64)) {
	// Clear any stale stop file BEFORE the loop starts, and synchronously.
	//
	// This used to be an `rm -f` on the script's first line, which raced its own
	// stop signal: stop() can fire while the exec is still handshaking, and a
	// touch that landed first was then deleted by the script, so no worker ever
	// saw the signal and the loop ran to its backstop. Doing it here means any
	// touch necessarily happens after the clear, because stop() cannot be called
	// until this function has returned.
	clear := exec.CommandContext(ctx, "kubectl", kubectlArgs(
		"exec", clientPod, "-n", testNamespace, "--", "rm", "-f", loadStopFile)...)
	if err := clear.Run(); err != nil {
		GinkgoWriter.Printf("could not clear %s before starting load: %v\n", loadStopFile, err)
	}

	// POSIX sh — the client pod is curlimages/curl, which is Alpine/BusyBox.
	// The SQL arrives through the environment rather than being interpolated into
	// the script, so no quoting in the query can break the shell.
	script := fmt.Sprintf(`
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
`, loadStopFile, loadBackstop, loadWorkers, url)

	args := kubectlArgs("exec", clientPod, "-n", testNamespace, "--",
		"env", "QUERY="+query, "sh", "-c", script)
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

	var stopOnce sync.Once
	var succeeded, failed int64
	return func() (int64, int64) {
		stopOnce.Do(func() {
			touch := exec.CommandContext(ctx, "kubectl", kubectlArgs(
				"exec", clientPod, "-n", testNamespace, "--", "touch", loadStopFile)...)
			if err := touch.Run(); err != nil {
				GinkgoWriter.Printf("could not touch %s (loop will stop at its backstop): %v\n",
					loadStopFile, err)
			}
			select {
			case <-done:
			case <-time.After(loadDrainTimeout):
				GinkgoWriter.Printf("in-pod load loop did not exit within %s\n", loadDrainTimeout)
			}
			for _, line := range strings.Split(out.String(), "\n") {
				switch strings.TrimSpace(line) {
				case "OK":
					succeeded++
				case "FAIL":
					failed++
				}
			}
		})
		return succeeded, failed
	}
}

// loadHoldTracker records what the engine's query gauges read during a hold
// window, so a failure can say whether the spec's own premise held. Recording
// does not weaken anything: callers assert on the operator's behaviour
// unconditionally, and consult this only to explain a failure.
type loadHoldTracker struct {
	samples      []int64
	idle         int
	scrapeErrors int
}

// sample scrapes the pod's query gauges once and records the result. A failed or
// unparsable scrape is counted, not ignored: "busy at every sample" and "there
// were no samples" are different findings, and only the count separates them.
func (t *loadHoldTracker) sample(ctx context.Context, clientPod, podIP string) {
	body, err := scrapeEnginePodMetrics(ctx, clientPod, podIP)
	if err != nil {
		t.scrapeErrors++
		return
	}
	v, ok := parseActiveQueries(body)
	if !ok {
		t.scrapeErrors++
		return
	}
	t.samples = append(t.samples, v)
	if v == 0 {
		t.idle++
	}
}

// failure explains a hold-window failure in terms of whether the premise held,
// because the three causes need different responses and the output could not
// previously tell them apart: both specs put their anti-vacuity check after the
// hold assertion, so a failing run never reached it.
//
// acted describes what the operator did, in the spec's own terms — "released the
// draining generation", "scaled a busy engine down".
func (t *loadHoldTracker) failure(acted string) string {
	rendered := make([]string, 0, len(t.samples))
	for _, s := range t.samples {
		rendered = append(rendered, strconv.FormatInt(s, 10))
	}
	joined := strings.Join(rendered, ",")

	// Separate on purpose: reached when every scrape failed, which means the
	// premise was never OBSERVED, not that it held. Folding it in with "no idle
	// sample seen" would print "non-zero at every sample" over an empty list and
	// point at the operator — the misdiagnosis this exists to prevent.
	if len(t.samples) == 0 {
		return fmt.Sprintf("INCONCLUSIVE, and the premise was never observed: no usable gauge "+
			"sample was obtained during the hold window (%d scrape attempts failed or returned "+
			"unparsable metrics). Whether the engine was busy is unknown, so this run says "+
			"nothing about the operator either way. Fix the scrape path first.", t.scrapeErrors)
	}
	if t.idle > 0 {
		return fmt.Sprintf("INCONCLUSIVE, not a product failure: the load generator left the "+
			"engine idle — running+suspended read 0 on %d of %d samples [%s]. Acting on an idle "+
			"engine is permitted by the contract, so this run says nothing about the operator. "+
			"Fix the load generator, not the operator.", t.idle, len(t.samples), joined)
	}
	return fmt.Sprintf("LIKELY CONTRACT VIOLATION: the operator %s even though running+suspended "+
		"was non-zero at every one of %d samples [%s]; failed scrapes: %d. Sampling is coarser "+
		"than the operator's own poll, so an unobserved idle instant is not fully excluded, but "+
		"the operator acting on an engine with queries in flight is the first thing to rule out.",
		acted, len(t.samples), joined, t.scrapeErrors)
}
