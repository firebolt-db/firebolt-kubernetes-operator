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
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
)

// drainEjectionEnabled gates the drain-ejection step below.
// Both required conditions are met as of ENGINE_TAG release-4.32.0-pre.0.20260423061425.6d49af2e16b4:
//  1. Engine exposes GET /health/ready on port 3473, returning 200 normally
//     and 503 on SIGTERM (so Envoy can distinguish healthy from draining):
//     https://github.com/firebolt-analytics/packdb/commit/e130589baddfd64a63720ae1eb294137940d1c7b
//  2. expected_statuses removed from the health_check in instance_gateway.go.
const drainEjectionEnabled = true

var _ = Describe("Envoy Gateway Health Checks", func() {
	// Verifies that Envoy's active HTTP health checks are running against engine
	// pods on port 3473 (/health/ready) and that those pods are reported healthy.
	// The check uses the Envoy admin API (port 9901) reached via kubectl
	// port-forward, because the admin is bound to 127.0.0.1 and is not reachable
	// via the pod-proxy subresource (which connects to the pod IP).
	Describe("Active Health Check on Engine HealthPort", Ordered, func() {
		var (
			instanceName = "inst-gw-hc"
			engineName   = "test-gw-hc-engine"
			clientPod    = "client-gw-hc"
			lc           *TestInstanceLifecycle
		)

		BeforeAll(func() {
			By("Setting up FireboltInstance")
			var err error
			lc, err = SetupTestInstance(ctx, instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Creating client pod")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())

			By("Creating 1-replica engine")
			Expect(CreateEngine(ctx, instanceName, engineName, 1)).To(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)).To(Succeed())

			By("Diagnosing engine /health/ready response on port 3473")
			engPodName, engIP, engErr := findEnginePod(engineName)
			if engErr == nil {
				GinkgoWriter.Printf("engine pod %s IP %s\n", engPodName, engIP)
				// use GET — same method Envoy health checks use
				args := kubectlArgs("exec", clientPod, "-n", testNamespace, "--",
					"curl", "-sv", "--max-time", "3",
					fmt.Sprintf("http://%s:3473/health/ready", engIP))
				cmd := exec.Command("kubectl", args...)
				out, _ := cmd.CombinedOutput()
				GinkgoWriter.Printf("GET 3473/health/ready:\n%s\n", string(out))
			}

			// A gateway query is required to trigger DFP sub-cluster creation;
			// health check counters only appear after the sub-cluster exists.
			By("Running a gateway query to trigger DFP sub-cluster creation")
			output, err := RunQueryViaGateway(ctx, clientPod, instanceName, engineName, LightQuery)
			Expect(err).NotTo(HaveOccurred())
			result, err := ParseQueryResult(output)
			Expect(err).NotTo(HaveOccurred())
			Expect(LightQueryValidator(result)).To(BeTrue(), "gateway query should return 42")
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			_ = DeleteEngine(ctx, engineName)
			_ = WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)
			TeardownTestInstance(ctx, lc)
		})

		It("should perform active health checks against the engine HealthPort and report success", func() {
			By("Finding the ready gateway pod")
			gwPodName, err := findGatewayPod(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Opening port-forward to Envoy admin API (port 9901)")
			adminBase, cleanupPF, err := startEnvoyAdminPortForward(gwPodName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanupPF)

			By("Dumping initial Envoy stats for diagnosis")
			time.Sleep(3 * time.Second) // let health checks fire a few times
			if s, serr := envoyAdminStats(adminBase); serr == nil {
				for _, line := range strings.Split(s, "\n") {
					if strings.Contains(line, "health") || strings.Contains(line, "cluster.") {
						GinkgoWriter.Println(line)
					}
				}
			}

			// Health check interval is 1s; within 15s we expect multiple successes.
			By("Waiting for Envoy to record at least one health_check.success")
			var stats string
			Eventually(func() (int, error) {
				var queryErr error
				stats, queryErr = envoyAdminStats(adminBase)
				if queryErr != nil {
					return 0, queryErr
				}
				return parseEnvoyHealthStat(stats, ".health_check.success"), nil
			}, 15*time.Second, 1*time.Second).Should(BeNumerically(">", 0),
				"Envoy should have performed at least one successful health check against the engine (port 3473 /health/ready)")

			By("Verifying health_check.success dominates (log failure count)")
			successes := parseEnvoyHealthStat(stats, ".health_check.success")
			failures := parseEnvoyHealthStat(stats, ".health_check.failure")
			GinkgoWriter.Printf("health_check: success=%d failure=%d\n", successes, failures)
			// A single transient failure at sub-cluster creation time (before
			// DNS resolves) is acceptable; what matters is that successes are
			// accumulating and failures are not growing beyond the startup transient.
			Expect(successes).To(BeNumerically(">=", failures),
				"sustained health check failures indicate /health/ready on port 3473 is not returning 2xx")

			By("Verifying no engine endpoint is flagged as unhealthy in /clusters")
			clusters, err := envoyAdminClusters(adminBase)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusters).NotTo(ContainSubstring("failed_active_hc"),
				"all engine endpoints should be considered healthy by Envoy active health checks")
		})

		It("should eject a terminating pod within one health check interval and clear the flag once a replacement is healthy", func() {
			if !drainEjectionEnabled {
				Skip("drain ejection requires engine to serve /health/ready on port 3473 " +
					"(200 normally, 503 on SIGTERM) and expected_statuses removed from health check config; " +
					"see drainEjectionEnabled comment above")
			}

			By("Finding the ready gateway pod")
			gwPodName, err := findGatewayPod(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Opening port-forward to Envoy admin API (port 9901)")
			adminBase, cleanupPF, err := startEnvoyAdminPortForward(gwPodName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanupPF)

			By("Recording the engine pod name and IP before deletion")
			podName, podIP, err := findEnginePod(engineName)
			Expect(err).NotTo(HaveOccurred())
			GinkgoWriter.Printf("engine pod %s has IP %s\n", podName, podIP)

			By("Recording health_check.failure count before deletion")
			statsBefore, statsErr := envoyAdminStats(adminBase)
			Expect(statsErr).NotTo(HaveOccurred())
			failuresBefore := parseEnvoyHealthStat(statsBefore, ".health_check.failure")
			GinkgoWriter.Printf("health_check.failure before deletion: %d\n", failuresBefore)

			By("Deleting the engine pod to trigger SIGTERM")
			gracePeriod := int64(30)
			err = k8sClient.CoreV1().Pods(testNamespace).Delete(ctx, podName, metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
			})
			Expect(err).NotTo(HaveOccurred())

			// With unhealthy_threshold: 1 and interval: 1s, Envoy's health check should
			// detect the 503 from /health/ready within one check cycle (~1s) of SIGTERM.
			// We verify via the health_check.failure counter delta rather than looking for
			// podIP+":3473" in /clusters: STRICT_DNS sub-clusters remove endpoints when
			// DNS propagates the deletion, which in Kind can happen before the health check
			// fires — so the pod IP may be gone from /clusters before we can observe it as
			// failed_active_hc. The cumulative failure counter doesn't disappear with the
			// endpoint and is the reliable signal here.
			By("Verifying Envoy detects the draining pod within one health check interval (failure counter)")
			Eventually(func() (int, error) {
				stats, err := envoyAdminStats(adminBase)
				if err != nil {
					return 0, err
				}
				return parseEnvoyHealthStat(stats, ".health_check.failure"), nil
			}, 3*time.Second, 500*time.Millisecond).Should(
				BeNumerically(">", failuresBefore),
				"Envoy health check should detect the draining pod's 503 on /health/ready within ~1s of SIGTERM")

			By("Waiting for the replacement pod to become ready")
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())

			By("Verifying the failed_active_hc flag is cleared once the replacement pod is healthy")
			clusters, err := envoyAdminClusters(adminBase)
			Expect(err).NotTo(HaveOccurred())
			Expect(clusters).NotTo(ContainSubstring("failed_active_hc"),
				"the replacement pod should be healthy with no failed_active_hc entries in /clusters")
		})
	})
})

// startEnvoyAdminPortForward starts a kubectl port-forward to the Envoy admin
// API (port 9901) on the given gateway pod and waits until the local port is
// reachable. Returns the base URL (e.g. "http://127.0.0.1:<port>") and a
// cleanup function that kills the port-forward process.
//
// Port-forward (not pod-proxy) is used because the Envoy admin is bound to
// 127.0.0.1 inside the container; the pod-proxy subresource connects to the
// pod IP and therefore receives a connection-refused error.
func startEnvoyAdminPortForward(gwPodName string) (baseURL string, cleanup func(), err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("find free local port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	args := kubectlArgs("port-forward", "-n", testNamespace,
		fmt.Sprintf("pod/%s", gwPodName),
		fmt.Sprintf("%d:9901", localPort))
	cmd := exec.Command("kubectl", args...)
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return "", nil, fmt.Errorf("start port-forward to %s:9901: %w", gwPodName, err)
	}

	cleanup = func() {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
	}

	// Wait up to 10s for the port to accept connections.
	deadline := time.Now().Add(10 * time.Second)
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	for time.Now().Before(deadline) {
		c, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			c.Close()
			return fmt.Sprintf("http://%s", addr), cleanup, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	cleanup()
	return "", nil, fmt.Errorf("timeout waiting for port-forward to %s:9901 to be ready", gwPodName)
}

// findEnginePod returns the name and pod IP of a running, ready engine pod.
func findEnginePod(engineName string) (name, ip string, err error) {
	pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", controller.LabelEngine, engineName),
	})
	if err != nil {
		return "", "", fmt.Errorf("list engine pods for %s: %w", engineName, err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return pod.Name, pod.Status.PodIP, nil
			}
		}
	}
	return "", "", fmt.Errorf("no ready engine pod found for engine %s", engineName)
}

// findGatewayPod returns the name of a running, ready gateway pod for the instance.
func findGatewayPod(instanceName string) (string, error) {
	pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=gateway",
			controller.LabelInstance, instanceName, controller.LabelComponent),
	})
	if err != nil {
		return "", fmt.Errorf("list gateway pods for %s: %w", instanceName, err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return pod.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no ready gateway pod found for instance %s", instanceName)
}

// envoyAdminGet fetches a path from the Envoy admin API at the given base URL.
func envoyAdminGet(baseURL, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("build request for %s%s: %w", baseURL, path, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s%s: %w", baseURL, path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read %s body: %w", path, err)
	}
	return string(body), nil
}

// envoyAdminStats fetches the Envoy admin /stats page from the given base URL.
func envoyAdminStats(baseURL string) (string, error) {
	return envoyAdminGet(baseURL, "/stats")
}

// envoyAdminClusters fetches the Envoy admin /clusters page from the given base URL.
func envoyAdminClusters(baseURL string) (string, error) {
	return envoyAdminGet(baseURL, "/clusters")
}

// parseEnvoyHealthStat sums all counters in the Envoy stats text whose key ends
// with suffix (e.g. ".health_check.success"). Envoy emits lines as "<key>: <value>".
func parseEnvoyHealthStat(stats, suffix string) int {
	total := 0
	for _, line := range strings.Split(stats, "\n") {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(parts[0]), suffix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			continue
		}
		total += n
	}
	return total
}
