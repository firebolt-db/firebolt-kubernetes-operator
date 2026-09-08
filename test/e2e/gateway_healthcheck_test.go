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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
)

var _ = Describe("Envoy Gateway Health Checks", func() {
	// Verifies that Envoy's active HTTP health checks are running against engine
	// pods on port 3473 (/health/ready) and that those pods are reported healthy.
	// The check uses the Envoy admin API (port 9901) reached via kubectl
	// port-forward, because the admin is bound to 127.0.0.1 and is not reachable
	// via the pod-proxy subresource (which connects to the pod IP).
	Describe("Active Health Check on Engine Query Port", Ordered, func() {
		var (
			instanceName = "inst-gw-hc"
			engineName   = "test-gw-hc-engine"
			clientPod    = "client-gw-hc"
			lc           *TestInstanceLifecycle
		)
		RegisterFailedSpecPodLogDump(&instanceName, &engineName)

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
			defer TeardownTestInstance(ctx, lc)
			DeleteClientPod(ctx, clientPod)
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())
		})

		It("should discover the engine query endpoint and observe successful health checks", func() {
			By("Finding the ready gateway pod")
			gwPodName, err := findGatewayPod(instanceName)
			Expect(err).NotTo(HaveOccurred())

			By("Opening port-forward to Envoy admin API (port 9901)")
			adminBase, cleanupPF, err := startEnvoyAdminPortForward(gwPodName)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(cleanupPF)

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

			By("Dumping Envoy health/cluster stats for diagnosis")
			for _, line := range strings.Split(stats, "\n") {
				if strings.Contains(line, "health") || strings.Contains(line, "cluster.") {
					GinkgoWriter.Println(line)
				}
			}

			By("Verifying the expected engine endpoint is present and healthy")
			_, podIP, err := findEnginePod(engineName)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() (bool, error) {
				clusters, err := envoyAdminClusters(adminBase)
				if err != nil {
					return false, err
				}
				present, healthy := envoyEndpointHealth(clusters, engineName, podIP)
				return present && healthy, nil
			}, 15*time.Second, 500*time.Millisecond).Should(BeTrue(),
				"the expected engine endpoint must pass its active health check")
		})

		It("should consume and validate the engine query parameter before forwarding", func() {
			assertSuccessfulQuery := func(rawQuery, headerEngine string) {
				GinkgoHelper()
				status, body, err := gatewayQueryResponse(clientPod, instanceName, rawQuery, headerEngine)
				Expect(err).NotTo(HaveOccurred())
				Expect(status).To(Equal(http.StatusOK), "response body: %s", body)

				result, err := ParseQueryResult(body)
				Expect(err).NotTo(HaveOccurred())
				Expect(LightQueryValidator(result)).To(BeTrue(), "gateway query should return 42")
			}
			assertRejectedQuery := func(rawQuery, headerEngine string) {
				GinkgoHelper()
				status, body, err := gatewayQueryResponse(clientPod, instanceName, rawQuery, headerEngine)
				Expect(err).NotTo(HaveOccurred())
				Expect(status).To(Equal(http.StatusBadRequest), "response body: %s", body)
			}

			By("Routing with the query parameter while preserving downstream query settings")
			assertSuccessfulQuery(
				fmt.Sprintf("engine=%s&query_label=e2e-gateway-param&output_format=JSON_Compact", engineName),
				"",
			)

			By("URL-decoding the engine name before validation")
			encodedEngine := strings.ReplaceAll(engineName, "-", "%2D")
			assertSuccessfulQuery(
				fmt.Sprintf("engine=%s&output_format=JSON_Compact", encodedEngine),
				"",
			)

			By("Accepting matching header and query selectors")
			assertSuccessfulQuery(
				fmt.Sprintf("engine=%s&output_format=JSON_Compact", engineName),
				engineName,
			)

			By("Rejecting conflicting header and query selectors")
			assertRejectedQuery(
				fmt.Sprintf("engine=%s&output_format=JSON_Compact", engineName),
				"different-engine",
			)

			By("Rejecting duplicate engine query parameters")
			assertRejectedQuery(
				fmt.Sprintf("engine=%s&engine=%s&output_format=JSON_Compact", engineName, engineName),
				"",
			)

			By("Rejecting an encoded namespace traversal attempt")
			assertRejectedQuery("engine=other%2Enamespace&output_format=JSON_Compact", "")

			By("Rejecting an invalid header instead of falling back to the query selector")
			assertRejectedQuery(
				fmt.Sprintf("engine=%s&output_format=JSON_Compact", engineName),
				"invalid.header",
			)
		})

		It("should remove the deleted endpoint and discover a healthy replacement", func() {
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

			oldPod, err := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			By("Deleting the engine pod to trigger SIGTERM")
			gracePeriod := int64(30)
			err = k8sClient.CoreV1().Pods(testNamespace).Delete(ctx, podName, metav1.DeleteOptions{
				GracePeriodSeconds: &gracePeriod,
			})
			Expect(err).NotTo(HaveOccurred())

			// This single-pod deletion checks discovery and recovery. It does not
			// assert uninterrupted queries while the only engine pod is replaced.
			By("Waiting for a ready replacement pod with a different identity")
			var replacementIP string
			Eventually(func() (bool, error) {
				name, ip, err := findEnginePod(engineName)
				if err != nil {
					return false, err
				}
				pod, err := k8sClient.CoreV1().Pods(testNamespace).Get(ctx, name, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				if pod.UID == oldPod.UID {
					return false, nil
				}
				replacementIP = ip
				return true, nil
			}, clusterReadyTimeout, time.Second).Should(BeTrue())

			By("Waiting for Envoy to remove the old address and report the replacement healthy")
			Eventually(func() (bool, error) {
				clusters, err := envoyAdminClusters(adminBase)
				if err != nil {
					return false, err
				}
				oldPresent, _ := envoyEndpointHealth(clusters, engineName, podIP)
				present, healthy := envoyEndpointHealth(clusters, engineName, replacementIP)
				// Kubernetes may reuse the deleted pod's IP for its replacement.
				return (!oldPresent || podIP == replacementIP) && present && healthy, nil
			}, 15*time.Second, 500*time.Millisecond).Should(BeTrue(),
				"Envoy must discover the ready replacement and withdraw the deleted pod's address")
		})
	})
})

// gatewayQueryResponse sends a query through the instance gateway and returns
// both the HTTP status and body. Unlike execCurlQuery, it deliberately does not
// use curl --fail because routing-validation tests need to inspect 400 responses.
func gatewayQueryResponse(clientPod, instanceName, rawQuery, headerEngine string) (int, string, error) {
	const statusMarker = "\n__HTTP_STATUS__:"

	serviceName := instanceName + controller.SuffixGateway
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:80/?%s", serviceName, testNamespace, rawQuery)
	curlArgs := []string{
		"-sS",
		"--connect-timeout", "2",
		"--max-time", "15",
		"-w", statusMarker + "%{http_code}",
		"-X", "POST",
		"-H", "Content-Type: text/plain",
	}
	if headerEngine != "" {
		curlArgs = append(curlArgs, "-H", "X-Firebolt-Engine: "+headerEngine)
	}
	curlArgs = append(curlArgs, "-d", LightQuery, url)

	args := kubectlArgs("exec", clientPod, "-n", testNamespace, "--", "curl")
	args = append(args, curlArgs...)

	var stdout, stderr strings.Builder
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return 0, "", fmt.Errorf("curl gateway query: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	output := stdout.String()
	markerIndex := strings.LastIndex(output, statusMarker)
	if markerIndex < 0 {
		return 0, "", fmt.Errorf("curl gateway response missing status marker: %q", output)
	}
	status, err := strconv.Atoi(strings.TrimSpace(output[markerIndex+len(statusMarker):]))
	if err != nil {
		return 0, "", fmt.Errorf("parse gateway response status: %w", err)
	}
	return status, output[:markerIndex], nil
}

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
		if pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || !pod.DeletionTimestamp.IsZero() {
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
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s returned %s", path, resp.Status)
	}
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

type envoyClusterStatus struct {
	Name  string `json:"name"`
	Hosts []struct {
		Address struct {
			SocketAddress struct {
				Address string `json:"address"`
				Port    int32  `json:"port_value"`
			} `json:"socket_address"`
		} `json:"address"`
		Health *struct {
			FailedActive    bool   `json:"failed_active_health_check"`
			FailedOutlier   bool   `json:"failed_outlier_check"`
			Degraded        bool   `json:"failed_active_degraded_check"`
			PendingRemoval  bool   `json:"pending_dynamic_removal"`
			PendingCheck    bool   `json:"pending_active_hc"`
			Excluded        bool   `json:"excluded_via_immediate_hc_fail"`
			CheckTimeout    bool   `json:"active_hc_timeout"`
			DiscoveryHealth string `json:"eds_health_status"`
		} `json:"health_status"`
	} `json:"host_statuses"`
}

// envoyAdminClusters fetches the structured endpoint and health observations.
func envoyAdminClusters(baseURL string) ([]envoyClusterStatus, error) {
	body, err := envoyAdminGet(baseURL, "/clusters?format=json")
	if err != nil {
		return nil, err
	}
	return parseEnvoyClusters(body)
}

func parseEnvoyClusters(body string) ([]envoyClusterStatus, error) {
	var clusters struct {
		Statuses []envoyClusterStatus `json:"cluster_statuses"`
	}
	if err := json.Unmarshal([]byte(body), &clusters); err != nil {
		return nil, fmt.Errorf("parse Envoy clusters: %w", err)
	}
	return clusters.Statuses, nil
}

// envoyEndpointHealth requires a matching query endpoint and explicit health
// observation. An absent endpoint or health object is not proof of health.
func envoyEndpointHealth(clusters []envoyClusterStatus, engineName, ip string) (present, healthy bool) {
	wantCluster := fmt.Sprintf("DFPCluster:%s%s.%s.svc.cluster.local:%d", engineName, controller.SuffixService, testNamespace, controller.EngineHTTPQueryPort)
	for _, cluster := range clusters {
		if cluster.Name != wantCluster {
			continue
		}
		for _, host := range cluster.Hosts {
			if host.Address.SocketAddress.Address != ip || host.Address.SocketAddress.Port != controller.EngineHTTPQueryPort {
				continue
			}
			h := host.Health
			if h == nil {
				return true, false
			}
			discoveryHealthy := h.DiscoveryHealth == "" || h.DiscoveryHealth == "UNKNOWN" || h.DiscoveryHealth == "HEALTHY"
			return true, discoveryHealthy && !h.FailedActive && !h.FailedOutlier && !h.Degraded &&
				!h.PendingRemoval && !h.PendingCheck && !h.Excluded && !h.CheckTimeout
		}
	}
	return false, false
}

func TestEnvoyEndpointHealth(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cluster string
		hosts   string
		present bool
		healthy bool
	}{
		{name: "no endpoints", hosts: `[]`},
		{name: "different cluster", cluster: "DFPCluster:other-service.firebolt-e2e.svc.cluster.local:3473", hosts: `[{"address":{"socket_address":{"address":"127.0.0.1","port_value":3473}},"health_status":{"eds_health_status":"HEALTHY"}}]`},
		{name: "different endpoint", hosts: `[{"address":{"socket_address":{"address":"127.0.0.2","port_value":3473}},"health_status":{}}]`},
		{name: "missing health", hosts: `[{"address":{"socket_address":{"address":"127.0.0.1","port_value":3473}}}]`, present: true},
		{name: "healthy", hosts: `[{"address":{"socket_address":{"address":"127.0.0.1","port_value":3473}},"health_status":{"eds_health_status":"HEALTHY"}}]`, present: true, healthy: true},
		{name: "awaiting first check", hosts: `[{"address":{"socket_address":{"address":"127.0.0.1","port_value":3473}},"health_status":{"pending_active_hc":true}}]`, present: true},
		{name: "failed check", hosts: `[{"address":{"socket_address":{"address":"127.0.0.1","port_value":3473}},"health_status":{"failed_active_health_check":true}}]`, present: true},
		{name: "withdrawn but retained", hosts: `[{"address":{"socket_address":{"address":"127.0.0.1","port_value":3473}},"health_status":{"pending_dynamic_removal":true}}]`, present: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cluster := tt.cluster
			if cluster == "" {
				cluster = "DFPCluster:engine-service.firebolt-e2e.svc.cluster.local:3473"
			}
			clusters, err := parseEnvoyClusters(fmt.Sprintf(`{"cluster_statuses":[{"name":%q,"host_statuses":%s}]}`, cluster, tt.hosts))
			if err != nil {
				t.Fatal(err)
			}
			present, healthy := envoyEndpointHealth(clusters, "engine", "127.0.0.1")
			if present != tt.present || healthy != tt.healthy {
				t.Fatalf("endpoint observation = (%t, %t), want (%t, %t)", present, healthy, tt.present, tt.healthy)
			}
		})
	}
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
