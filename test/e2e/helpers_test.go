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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/oklog/ulid/v2"
	. "github.com/onsi/ginkgo/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-analytics/firebolt-kubernetes-operator/internal/controller"
)

// Test query constants
const (
	// LightQuery is a simple query for basic validation
	LightQuery = "SELECT 42"
	// HeavyQuery generates ~500MB of data to stress test the system
	HeavyQuery = "SELECT array_agg(x) FROM generate_series(1, 10000000) g(x)"

	// MinHeavyQueryOutputBytes is the minimum expected output size for heavy query (50MB)
	MinHeavyQueryOutputBytes = 50 * 1024 * 1024
)

// QueryMode determines which query type to use for tests
type QueryMode string

const (
	QueryModeLight QueryMode = "light"
	QueryModeHeavy QueryMode = "heavy"
)

// TestQueryConfig holds the query and validator for the current test mode
type TestQueryConfig struct {
	Query     string
	Validator QueryValidator
	Mode      QueryMode
	Suffix    string // Added to engine names to avoid conflicts between light/heavy runs
}

// QueryValidator validates the result of a query
type QueryValidator func(result interface{}) bool

// LightQueryValidator validates SELECT 42 returns "42"
func LightQueryValidator(result interface{}) bool {
	return fmt.Sprintf("%v", result) == "42"
}

// HeavyQueryValidator validates the heavy query returns at least 50MB of output
func HeavyQueryValidator(result interface{}) bool {
	resultStr := fmt.Sprintf("%v", result)
	// Must be an array with at least 50MB of data
	return strings.HasPrefix(resultStr, "[") && len(resultStr) >= MinHeavyQueryOutputBytes
}

// OperatorInstance represents a running operator instance
type OperatorInstance struct {
	mgr        manager.Manager
	cancelFunc context.CancelFunc
	wg         sync.WaitGroup
	crdClient  client.Client
}

// operatorInstanceCounter is used to generate unique controller names
var operatorInstanceCounter atomic.Int64

// StartOperator starts an engine operator scoped to the given instance name.
// The reconciler drops reconcile requests for any engine whose
// spec.instanceRef does not match instanceName, so multiple operator instances
// can coexist in the same namespace without stepping on each other.
func StartOperator(instanceName string) (*OperatorInstance, error) {
	config, err := getRestConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add client-go scheme: %w", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add computev1alpha1 to scheme: %w", err)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				testNamespace: {},
			},
		},
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	reconciler := &controller.FireboltEngineReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Namespace:      testNamespace,
		Clientset:      clientset,
		InstanceFilter: instanceName,
		DisableGC:      true,
	}

	controllerName := fmt.Sprintf("fireboltengine-%d", operatorInstanceCounter.Add(1))
	if err := reconciler.SetupWithManagerNamed(mgr, controllerName); err != nil {
		return nil, fmt.Errorf("failed to setup reconciler: %w", err)
	}

	crdClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create crd client: %w", err)
	}

	ctxOp, cancel := context.WithCancel(context.Background())

	instance := &OperatorInstance{
		mgr:        mgr,
		cancelFunc: cancel,
		crdClient:  crdClient,
	}

	instance.wg.Add(1)
	go func() {
		defer instance.wg.Done()
		defer GinkgoRecover()
		if err := mgr.Start(ctxOp); err != nil {
			fmt.Fprintf(GinkgoWriter, "Manager exited with error: %v\n", err)
		}
	}()

	// Wait for cache to sync
	time.Sleep(500 * time.Millisecond)

	return instance, nil
}

// Stop stops the operator instance
func (o *OperatorInstance) Stop() {
	if o.cancelFunc != nil {
		o.cancelFunc()
	}
	o.wg.Wait()
}

// CreateEngine creates a FireboltEngine CR bound to the given FireboltInstance.
func CreateEngine(ctx context.Context, instanceName, name string, replicas int) error {
	return CreateEngineWithRollout(ctx, instanceName, name, replicas, "graceful")
}

// CreateEngineWithRollout creates a FireboltEngine CR bound to the given
// FireboltInstance with a specific rollout strategy.
func CreateEngineWithRollout(ctx context.Context, instanceName, name string, replicas int, rollout string) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	drainCheckEnabled := false
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef: instanceName,
			Replicas:    int32(replicas),
			Image: &computev1alpha1.ImageSpec{
				Repository: testImage,
				Tag:        testTag,
				PullPolicy: corev1.PullIfNotPresent,
			},
			Resources: computev1alpha1.ResourceRequirements{
				CPU:    resource.MustParse("50m"),
				Memory: resource.MustParse("2Gi"),
			},
			DrainCheckEnabled:  &drainCheckEnabled,
			DrainCheckInterval: &metav1.Duration{Duration: 2 * time.Second},
			Rollout:            computev1alpha1.RolloutStrategy(rollout),
		},
	}

	return cl.Create(ctx, engine)
}

// UpdateEngineReplicas updates the replicas count in the engine CR (with retry on conflict)
func UpdateEngineReplicas(ctx context.Context, name string, replicas int) error {
	return retryOnConflict(ctx, name, func(engine *computev1alpha1.FireboltEngine) {
		engine.Spec.Replicas = int32(replicas)
	})
}

// UpdateEngineImageTag updates the image tag in the engine CR (with retry on conflict)
func UpdateEngineImageTag(ctx context.Context, name string, tag string) error {
	return retryOnConflict(ctx, name, func(engine *computev1alpha1.FireboltEngine) {
		engine.Spec.Image.Tag = tag
	})
}

// retryOnConflict retries an update on conflict errors
func retryOnConflict(ctx context.Context, name string, mutate func(*computev1alpha1.FireboltEngine)) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	for i := 0; i < 10; i++ {
		engine := &computev1alpha1.FireboltEngine{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, engine); err != nil {
			return err
		}
		mutate(engine)
		err := cl.Update(ctx, engine)
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("too many conflict retries updating engine %s", name)
}

// DeleteEngine deletes the engine CR
func DeleteEngine(ctx context.Context, name string) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
	}
	err = cl.Delete(ctx, engine)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// WaitForEngineReady waits for all pods in an engine to be ready AND for the
// engine service to have ready endpoint addresses. Checking both ensures that
// kube-proxy/iptables rules have been updated and the service is routable.
// On timeout it dumps detailed pod and event diagnostics.
func WaitForEngineReady(ctx context.Context, engineName string, expectedReplicas int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	selector := fmt.Sprintf("firebolt.io/engine=%s", engineName)
	serviceName := engineName + "-service"

	var lastPods *corev1.PodList
	for time.Now().Before(deadline) {
		pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}
		lastPods = pods

		readyCount := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				for _, cond := range pod.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						readyCount++
						break
					}
				}
			}
		}

		if readyCount == expectedReplicas {
			ep, epErr := k8sClient.CoreV1().Endpoints(testNamespace).Get(ctx, serviceName, metav1.GetOptions{})
			if epErr == nil {
				readyAddrs := 0
				for _, subset := range ep.Subsets {
					readyAddrs += len(subset.Addresses)
				}
				if readyAddrs > 0 {
					return nil
				}
			}
		}

		time.Sleep(pollInterval)
	}

	diag := fmt.Sprintf("timeout waiting for engine %s to have %d ready pods", engineName, expectedReplicas)
	if lastPods != nil {
		diag += fmt.Sprintf("\n  Total pods found: %d", len(lastPods.Items))
		for _, pod := range lastPods.Items {
			diag += fmt.Sprintf("\n  Pod %s: phase=%s", pod.Name, pod.Status.Phase)
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					diag += fmt.Sprintf(" container=%s waiting(reason=%s, message=%s)",
						cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
				}
				if cs.State.Terminated != nil {
					diag += fmt.Sprintf(" container=%s terminated(reason=%s, exit=%d)",
						cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
				}
				if cs.RestartCount > 0 {
					diag += fmt.Sprintf(" restarts=%d", cs.RestartCount)
				}
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Status != corev1.ConditionTrue {
					diag += fmt.Sprintf("\n    condition %s=%s: %s", cond.Type, cond.Status, cond.Message)
				}
			}
		}
	} else {
		diag += "\n  No pod listing was obtained"
	}

	if lastPods != nil {
		for _, pod := range lastPods.Items {
			isReady := false
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					isReady = true
				}
			}
			if !isReady && pod.Status.Phase == corev1.PodRunning {
				tailLines := int64(30)
				logOpts := &corev1.PodLogOptions{TailLines: &tailLines}
				req := k8sClient.CoreV1().Pods(testNamespace).GetLogs(pod.Name, logOpts)
				logStream, logErr := req.Stream(ctx)
				if logErr == nil {
					logBytes, _ := io.ReadAll(io.LimitReader(logStream, 4096))
					logStream.Close()
					if len(logBytes) > 0 {
						diag += fmt.Sprintf("\n  Logs for unready pod %s:\n%s", pod.Name, string(logBytes))
					}
				}
			}
		}
	}

	events, err := k8sClient.CoreV1().Events(testNamespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		var relevant []corev1.Event
		for _, ev := range events.Items {
			if strings.Contains(ev.InvolvedObject.Name, engineName) &&
				(ev.Type == "Warning" || ev.Reason == "FailedScheduling" || ev.Reason == "Failed" || ev.Reason == "BackOff") {
				relevant = append(relevant, ev)
			}
		}
		if len(relevant) > 0 {
			diag += "\n  Warning events:"
			for _, ev := range relevant {
				diag += fmt.Sprintf("\n    %s/%s: %s - %s (count=%d)",
					ev.InvolvedObject.Kind, ev.InvolvedObject.Name, ev.Reason, ev.Message, ev.Count)
			}
		}
	}

	return fmt.Errorf("%s", diag)
}

// WaitForEngineStable waits for the engine status phase to be "stable"
func WaitForEngineStable(ctx context.Context, engineName string, timeout time.Duration) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		engine := &computev1alpha1.FireboltEngine{}
		if err := cl.Get(ctx, types.NamespacedName{Name: engineName, Namespace: testNamespace}, engine); err != nil {
			time.Sleep(pollInterval)
			continue
		}

		if engine.Status.Phase == computev1alpha1.PhaseStable {
			return nil
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("timeout waiting for engine %s to be stable", engineName)
}

// WaitForResourcesDeleted waits for all engine resources to be deleted
func WaitForResourcesDeleted(ctx context.Context, engineName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("firebolt.io/engine=%s", engineName),
		})
		if err == nil && len(pods.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		svcs, err := k8sClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("firebolt.io/engine=%s", engineName),
		})
		if err == nil && len(svcs.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		stsList, err := k8sClient.AppsV1().StatefulSets(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("firebolt.io/engine=%s", engineName),
		})
		if err == nil && len(stsList.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		return nil
	}

	return fmt.Errorf("timeout waiting for resources of engine %s to be deleted", engineName)
}

// CreateClientPod creates a lightweight curl pod in the test namespace that can
// be used to query services from inside the cluster. The pod blocks forever so
// it stays running for the duration of the test.
func CreateClientPod(ctx context.Context, podName string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: testNamespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "curl",
				Image:           curlImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"sleep", "infinity"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
				},
			}},
		},
	}
	if _, err := k8sClient.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create client pod %s: %w", podName, err)
	}

	waitArgs := kubectlArgs("wait", "--for=condition=Ready", "pod/"+podName,
		"-n", testNamespace, "--timeout=60s")
	cmd := exec.CommandContext(ctx, "kubectl", waitArgs...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("client pod %s not ready: %w (%s)", podName, err, strings.TrimSpace(stderrBuf.String()))
	}
	return nil
}

// DeleteClientPod deletes the client pod created by CreateClientPod.
func DeleteClientPod(ctx context.Context, podName string) {
	args := kubectlArgs("delete", "pod", podName, "-n", testNamespace,
		"--ignore-not-found", "--grace-period=0", "--force")
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	_ = cmd.Run()
}

// execCurlQuery runs a curl command inside the given client pod and returns
// the response body. It targets the given in-cluster URL and POSTs the query.
//
// curl flags:
//   - -sS: silent progress, but still print errors to stderr so a non-zero
//     exit code carries a human-readable reason (e.g. "Operation timeout").
//   - --connect-timeout 2: fail fast if TCP connect stalls (e.g. kube-proxy
//     race during endpoint churn).
//   - --max-time 5: cap the entire request so a hung upstream doesn't block
//     the background runner indefinitely. The zero-downtime tests tolerate
//     no failures, so this budget exists only to surface bugs as failures
//     quickly rather than to hide transient latency.
//   - -w "%{stderr}...": append a timing breakdown to stderr after transfer
//     so failures carry DNS/connect/response timings that pinpoint the phase
//     that stalled.
func execCurlQuery(ctx context.Context, podName, url, query string, extraHeaders ...string) (string, error) {
	const curlTimingFmt = "%{stderr}timings: code=%{http_code} dns=%{time_namelookup}s " +
		"connect=%{time_connect}s starttransfer=%{time_starttransfer}s total=%{time_total}s\n"

	curlArgs := []string{
		"-sSf",
		"--connect-timeout", "2",
		"--max-time", "5",
		"-w", curlTimingFmt,
		"-X", "POST",
		"-H", "Content-Type: text/plain",
	}
	for _, h := range extraHeaders {
		curlArgs = append(curlArgs, "-H", h)
	}
	curlArgs = append(curlArgs, "-d", query, url)

	args := kubectlArgs("exec", podName, "-n", testNamespace, "--", "curl")
	args = append(args, curlArgs...)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("curl failed (exit %v): %s", err, strings.TrimSpace(stderrBuf.String()))
	}
	return stdoutBuf.String(), nil
}

// kubectlArgs prepends --context if KIND_CLUSTER is set.
func kubectlArgs(args ...string) []string {
	if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
		return append([]string{"--context", "kind-" + kindCluster}, args...)
	}
	return args
}

// RunQuery executes a SQL query against the engine's ClusterIP service from
// inside a client pod. The podName must reference a pod previously created
// with CreateClientPod.
func RunQuery(ctx context.Context, podName, engineName, query string) (string, error) {
	url := fmt.Sprintf("http://%s-service.%s.svc.cluster.local:3473/?query_label=e2e-test&output_format=JSON_Compact&advanced_mode=true",
		engineName, testNamespace)
	return execCurlQuery(ctx, podName, url, query)
}

// QueryResponse represents the JSON response from fb
type QueryResponse struct {
	Data [][]interface{} `json:"data"`
	Rows int             `json:"rows"`
}

// ParseQueryResult parses the JSON response from fb and returns the first value.
func ParseQueryResult(output string) (interface{}, error) {
	var resp QueryResponse
	if err := json.Unmarshal([]byte(output), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w, output: %s", err, output)
	}

	if len(resp.Data) == 0 || len(resp.Data[0]) == 0 {
		return nil, fmt.Errorf("no data in response: %s", output)
	}

	return resp.Data[0][0], nil
}

// categorizeQueryError extracts a short category from an error detail string.
func categorizeQueryError(detail string) string {
	switch {
	case strings.Contains(detail, "connection refused"):
		return "connection refused"
	case strings.Contains(detail, "timeout"):
		return "timeout"
	case strings.Contains(detail, "no endpoints available"):
		return "no endpoints"
	case strings.Contains(detail, "EOF"):
		return "EOF"
	case strings.Contains(detail, "connection reset"):
		return "connection reset"
	case strings.Contains(detail, "no ready pod"):
		return "no ready pod"
	case strings.Contains(detail, "context canceled"):
		return "context canceled"
	case strings.Contains(detail, "curl failed"):
		return "curl error"
	default:
		if len(detail) > 50 {
			return detail[:50] + "..."
		}
		return detail
	}
}

// dialMetadataViaPortForward establishes a gRPC connection to the metadata
// service by port-forwarding through kubectl. This is needed because the
// reconciler runs on the host and cannot resolve in-cluster DNS.
func dialMetadataViaPortForward(_ context.Context, instance *computev1alpha1.FireboltInstance) (*grpc.ClientConn, func(), error) {
	serviceName := instance.Name + controller.SuffixMetadataService
	servicePort := controller.MetadataServicePort

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to find free port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	args := []string{"port-forward", "-n", testNamespace,
		fmt.Sprintf("svc/%s", serviceName),
		fmt.Sprintf("%d:%d", localPort, servicePort)}
	if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
		args = append([]string{"--context", "kind-" + kindCluster}, args...)
	}
	cmd := exec.Command("kubectl", args...)
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("failed to start port-forward to %s: %w", serviceName, err)
	}

	// Wait for port-forward to be ready
	var connected bool
	for i := 0; i < 50; i++ {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 100*time.Millisecond)
		if err == nil {
			c.Close()
			connected = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !connected {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		return nil, nil, fmt.Errorf("timeout waiting for port-forward to %s to be ready", serviceName)
	}

	target := fmt.Sprintf("127.0.0.1:%d", localPort)
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		return nil, nil, fmt.Errorf("grpc dial to %s via port-forward: %w", serviceName, err)
	}

	cleanup := func() {
		_ = conn.Close()
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
	}

	return conn, cleanup, nil
}

// InstanceOperator represents a running instance operator (FireboltInstanceReconciler)
type InstanceOperator struct {
	mgr        manager.Manager
	cancelFunc context.CancelFunc
	wg         sync.WaitGroup
	crdClient  client.Client
}

// StartInstanceOperator starts a FireboltInstanceReconciler in its own manager
// scoped to the given instance name. The reconciler drops reconcile requests
// for any other FireboltInstance, so multiple instance operators can coexist
// in the same namespace.
func StartInstanceOperator(instanceName string) (*InstanceOperator, error) {
	config, err := getRestConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add client-go scheme: %w", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add computev1alpha1 to scheme: %w", err)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				testNamespace: {},
			},
		},
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create instance manager: %w", err)
	}

	reconciler := &controller.FireboltInstanceReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		DialMetadata: dialMetadataViaPortForward,
		NameFilter:   instanceName,
	}
	controllerName := fmt.Sprintf("fireboltinstance-%d", operatorInstanceCounter.Add(1))
	if err := reconciler.SetupWithManagerNamed(mgr, controllerName); err != nil {
		return nil, fmt.Errorf("failed to setup instance reconciler: %w", err)
	}

	crdClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create crd client: %w", err)
	}

	ctxOp, cancel := context.WithCancel(context.Background())
	inst := &InstanceOperator{
		mgr:        mgr,
		cancelFunc: cancel,
		crdClient:  crdClient,
	}

	inst.wg.Add(1)
	go func() {
		defer inst.wg.Done()
		defer GinkgoRecover()
		if err := mgr.Start(ctxOp); err != nil {
			fmt.Fprintf(GinkgoWriter, "Instance manager exited with error: %v\n", err)
		}
	}()

	time.Sleep(500 * time.Millisecond)
	return inst, nil
}

// Stop stops the instance operator
func (o *InstanceOperator) Stop() {
	if o.cancelFunc != nil {
		o.cancelFunc()
	}
	o.wg.Wait()
}

// CreateInstance creates a FireboltInstance CR with the given metadata images.
// The gateway (Envoy proxy) image is set from the test suite's envoyImage/envoyTag.
func CreateInstance(ctx context.Context, name, metadataImage, metadataTag string) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	replicas := int32(1)
	smallResources := &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("25m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: ulid.MustNew(ulid.Now(), rand.Reader).String(),
			Metadata: computev1alpha1.MetadataSpec{
				ComponentSpec: computev1alpha1.ComponentSpec{
					Replicas:  &replicas,
					Resources: smallResources,
					Image: &computev1alpha1.ImageSpec{
						Repository: metadataImage,
						Tag:        metadataTag,
						PullPolicy: corev1.PullIfNotPresent,
					},
				},
			},
			Gateway: computev1alpha1.GatewaySpec{
				ComponentSpec: computev1alpha1.ComponentSpec{
					Replicas:  &replicas,
					Resources: smallResources,
					Image: &computev1alpha1.ImageSpec{
						Repository: envoyImage,
						Tag:        envoyTag,
						PullPolicy: corev1.PullIfNotPresent,
					},
				},
			},
		},
	}

	return cl.Create(ctx, instance)
}

// WaitForInstanceReady waits for the FireboltInstance to reach the Ready phase.
// On timeout it dumps instance status diagnostics.
func WaitForInstanceReady(ctx context.Context, name string, timeout time.Duration) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	deadline := time.Now().Add(timeout)
	key := types.NamespacedName{Name: name, Namespace: testNamespace}

	var lastInst *computev1alpha1.FireboltInstance
	for time.Now().Before(deadline) {
		inst := &computev1alpha1.FireboltInstance{}
		if err := cl.Get(ctx, key, inst); err != nil {
			time.Sleep(pollInterval)
			continue
		}
		lastInst = inst

		if inst.Status.Phase == computev1alpha1.InstancePhaseReady {
			return nil
		}

		fmt.Fprintf(GinkgoWriter, "Instance %s: phase=%s metadata=%t gateway=%t account=%q\n",
			name, inst.Status.Phase, inst.Status.MetadataReady, inst.Status.GatewayReady, inst.Spec.ID)
		time.Sleep(5 * time.Second)
	}

	diag := fmt.Sprintf("timeout waiting for instance %s to become Ready", name)
	if lastInst != nil {
		diag += fmt.Sprintf("\n  Phase: %s", lastInst.Status.Phase)
		diag += fmt.Sprintf("\n  MetadataReady: %t", lastInst.Status.MetadataReady)
		diag += fmt.Sprintf("\n  GatewayReady: %t", lastInst.Status.GatewayReady)
		diag += fmt.Sprintf("\n  InstanceID: %q", lastInst.Spec.ID)
		diag += fmt.Sprintf("\n  MetadataEndpoint: %q", lastInst.Status.MetadataEndpoint)
		diag += fmt.Sprintf("\n  GatewayEndpoint: %q", lastInst.Status.GatewayEndpoint)
		for _, c := range lastInst.Status.Conditions {
			diag += fmt.Sprintf("\n  Condition %s=%s: %s", c.Type, c.Status, c.Message)
		}
	}

	pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("firebolt.io/instance=%s", name),
	})
	if err == nil && len(pods.Items) > 0 {
		diag += fmt.Sprintf("\n  Instance pods (%d):", len(pods.Items))
		for _, pod := range pods.Items {
			diag += fmt.Sprintf("\n    %s: phase=%s", pod.Name, pod.Status.Phase)
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					diag += fmt.Sprintf(" container=%s waiting(reason=%s, message=%s)",
						cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
				}
				if cs.State.Terminated != nil {
					diag += fmt.Sprintf(" container=%s terminated(reason=%s, exit=%d)",
						cs.Name, cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
				}
				if cs.RestartCount > 0 {
					diag += fmt.Sprintf(" restarts=%d", cs.RestartCount)
				}
			}
		}
	}

	return fmt.Errorf("%s", diag)
}

// TestInstanceLifecycle bundles everything a per-Describe FireboltInstance
// needs: its name, the in-process instance operator, and the in-process engine
// operator. Tests typically create one of these in BeforeAll and release it in
// AfterAll so parallel Describes stay isolated from each other.
type TestInstanceLifecycle struct {
	Name        string
	InstanceOp  *InstanceOperator
	EngineOp    *OperatorInstance
}

// SetupTestInstance starts an isolated instance operator, creates a
// FireboltInstance with the given name, waits for it to become Ready, and
// starts an engine operator bound to that instance. Returns a lifecycle
// handle that TeardownTestInstance consumes.
func SetupTestInstance(ctx context.Context, name string) (*TestInstanceLifecycle, error) {
	instanceOp, err := StartInstanceOperator(name)
	if err != nil {
		return nil, fmt.Errorf("start instance operator for %s: %w", name, err)
	}
	if err := CreateInstance(ctx, name, pensieveImage, pensieveTag); err != nil {
		instanceOp.Stop()
		return nil, fmt.Errorf("create instance %s: %w", name, err)
	}
	if err := WaitForInstanceReady(ctx, name, instanceReadyTimeout); err != nil {
		_ = DeleteInstance(ctx, name)
		instanceOp.Stop()
		return nil, fmt.Errorf("wait instance %s ready: %w", name, err)
	}
	engineOp, err := StartOperator(name)
	if err != nil {
		_ = DeleteInstance(ctx, name)
		instanceOp.Stop()
		return nil, fmt.Errorf("start engine operator for %s: %w", name, err)
	}
	return &TestInstanceLifecycle{Name: name, InstanceOp: instanceOp, EngineOp: engineOp}, nil
}

// TeardownTestInstance stops both operators for the lifecycle and deletes its
// FireboltInstance. Errors are reported via GinkgoWriter rather than returned
// so cleanup stays idempotent in AfterAll blocks.
func TeardownTestInstance(ctx context.Context, lc *TestInstanceLifecycle) {
	if lc == nil {
		return
	}
	if lc.EngineOp != nil {
		lc.EngineOp.Stop()
	}
	if err := DeleteInstance(ctx, lc.Name); err != nil {
		fmt.Fprintf(GinkgoWriter, "Warning: failed to delete instance %s: %v\n", lc.Name, err)
	}
	if lc.InstanceOp != nil {
		lc.InstanceOp.Stop()
	}
}

// DeleteInstance deletes a FireboltInstance CR
func DeleteInstance(ctx context.Context, name string) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
	}
	err = cl.Delete(ctx, instance)
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// --- Instance mutation helpers ---

// UpdateInstanceMetadataImage updates the metadata image tag on the FireboltInstance (with retry on conflict).
func UpdateInstanceMetadataImage(ctx context.Context, name, tag string) error {
	return retryOnInstanceConflict(ctx, name, func(inst *computev1alpha1.FireboltInstance) {
		if inst.Spec.Metadata.Image == nil {
			inst.Spec.Metadata.Image = &computev1alpha1.ImageSpec{}
		}
		inst.Spec.Metadata.Image.Tag = tag
	})
}

// UpdateInstanceGatewayReplicas updates the gateway replica count on the FireboltInstance (with retry on conflict).
func UpdateInstanceGatewayReplicas(ctx context.Context, name string, replicas int32) error {
	return retryOnInstanceConflict(ctx, name, func(inst *computev1alpha1.FireboltInstance) {
		inst.Spec.Gateway.Replicas = &replicas
	})
}

// retryOnInstanceConflict retries an update on conflict errors for FireboltInstance resources.
func retryOnInstanceConflict(ctx context.Context, name string, mutate func(*computev1alpha1.FireboltInstance)) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	for i := 0; i < 10; i++ {
		inst := &computev1alpha1.FireboltInstance{}
		if err := cl.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, inst); err != nil {
			return err
		}
		mutate(inst)
		err := cl.Update(ctx, inst)
		if err == nil {
			return nil
		}
		if !errors.IsConflict(err) {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("too many conflict retries updating instance %s", name)
}

// WaitForInstanceMetadataImage polls until the metadata deployment uses the expected image tag.
func WaitForInstanceMetadataImage(ctx context.Context, instanceName, expectedTag string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	deployName := instanceName + controller.SuffixMetadataService

	for time.Now().Before(deadline) {
		dep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, deployName, metav1.GetOptions{})
		if err == nil {
			desired := int32(1)
			if dep.Spec.Replicas != nil {
				desired = *dep.Spec.Replicas
			}
			for _, c := range dep.Spec.Template.Spec.Containers {
				if strings.Contains(c.Image, expectedTag) {
					if dep.Status.ReadyReplicas == desired && dep.Status.UpdatedReplicas == desired {
						return nil
					}
				}
			}
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("timeout waiting for metadata deployment %s to use tag %s", deployName, expectedTag)
}

// WaitForGatewayReplicas polls until the gateway deployment has the expected number of ready replicas.
func WaitForGatewayReplicas(ctx context.Context, instanceName string, expected int32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	deployName := instanceName + controller.SuffixGateway

	for time.Now().Before(deadline) {
		dep, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, deployName, metav1.GetOptions{})
		if err == nil && dep.Status.ReadyReplicas == expected {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("timeout waiting for gateway %s to reach %d ready replicas", deployName, expected)
}

// --- Gateway query helpers ---

// RunQueryViaGateway executes a SQL query through the Envoy gateway service
// from inside a client pod. The gateway routes the query to the specified
// engine based on the X-Firebolt-Engine header.
func RunQueryViaGateway(ctx context.Context, podName, instanceName, engineName, query string) (string, error) {
	serviceName := instanceName + controller.SuffixGateway
	url := fmt.Sprintf("http://%s.%s.svc.cluster.local:80/?query_label=e2e-gateway-test&output_format=JSON_Compact&advanced_mode=true",
		serviceName, testNamespace)
	return execCurlQuery(ctx, podName, url, query, "X-Firebolt-Engine: "+engineName)
}

// GatewayBackgroundQueryRunner runs queries through the gateway in the background.
type GatewayBackgroundQueryRunner struct {
	podName        string
	instanceName   string
	engineName     string
	query          string
	validator      QueryValidator
	stopCh         chan struct{}
	wg             sync.WaitGroup
	failureCount   atomic.Int32
	successCount   atomic.Int32
	stopped        atomic.Bool
	mu             sync.Mutex
	failureReasons map[string]int
}

// NewGatewayBackgroundQueryRunner creates a background query runner that routes queries through the gateway.
func NewGatewayBackgroundQueryRunner(podName, instanceName, engineName, query string) *GatewayBackgroundQueryRunner {
	validator := LightQueryValidator
	if strings.Contains(query, "array_agg") {
		validator = HeavyQueryValidator
	}
	return NewGatewayBackgroundQueryRunnerWithValidator(podName, instanceName, engineName, query, validator)
}

// NewGatewayBackgroundQueryRunnerWithValidator creates a gateway-routed
// background query runner with a caller-supplied validator. Zero-downtime
// tests must use this variant (directly or via NewGatewayBackgroundQueryRunner)
// because the gateway is the only entry point on which we promise zero
// downtime - direct engine-service clients are responsible for their own
// retry / endpoint-selection semantics.
func NewGatewayBackgroundQueryRunnerWithValidator(podName, instanceName, engineName, query string, validator QueryValidator) *GatewayBackgroundQueryRunner {
	return &GatewayBackgroundQueryRunner{
		podName:        podName,
		instanceName:   instanceName,
		engineName:     engineName,
		query:          query,
		validator:      validator,
		stopCh:         make(chan struct{}),
		failureReasons: make(map[string]int),
	}
}

// Start starts running queries in the background through the gateway.
func (r *GatewayBackgroundQueryRunner) Start(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer GinkgoRecover()

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-r.stopCh:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runQuery(ctx)
			}
		}
	}()
}

func (r *GatewayBackgroundQueryRunner) runQuery(ctx context.Context) {
	output, err := RunQueryViaGateway(ctx, r.podName, r.instanceName, r.engineName, r.query)
	if err != nil {
		r.recordFailure("query_error", err.Error())
		return
	}

	result, err := ParseQueryResult(output)
	if err != nil {
		r.recordFailure("parse_error", err.Error())
		return
	}

	if !r.validator(result) {
		r.recordFailure("validation_error", fmt.Sprintf("validation failed for result: %v", result))
		return
	}

	r.successCount.Add(1)
}

func (r *GatewayBackgroundQueryRunner) recordFailure(category, detail string) {
	r.failureCount.Add(1)

	reason := category + ": " + categorizeQueryError(detail)

	r.mu.Lock()
	r.failureReasons[reason]++
	r.mu.Unlock()

	fmt.Fprintf(GinkgoWriter, "Gateway background query failed [%s]: %s\n", category, detail)
}

// Stop stops the gateway background query runner. Safe to call multiple times.
func (r *GatewayBackgroundQueryRunner) Stop() {
	if r.stopped.CompareAndSwap(false, true) {
		close(r.stopCh)
	}
	r.wg.Wait()
}

// GetStats returns the success and failure counts.
func (r *GatewayBackgroundQueryRunner) GetStats() (successes, failures int32) {
	return r.successCount.Load(), r.failureCount.Load()
}

// GetFailureReasons returns a summary of failure reasons.
func (r *GatewayBackgroundQueryRunner) GetFailureReasons() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make(map[string]int)
	for k, v := range r.failureReasons {
		result[k] = v
	}
	return result
}

// PrintFailureSummary prints a summary of all failure reasons.
func (r *GatewayBackgroundQueryRunner) PrintFailureSummary() {
	reasons := r.GetFailureReasons()
	if len(reasons) == 0 {
		return
	}

	fmt.Fprintf(GinkgoWriter, "\n=== Gateway Background Query Failure Summary ===\n")
	for reason, count := range reasons {
		fmt.Fprintf(GinkgoWriter, "  %s: %d\n", reason, count)
	}
	fmt.Fprintf(GinkgoWriter, "=================================================\n")
}

// getCRDClient returns a controller-runtime client that knows about the CRD types
func getCRDClient() (client.Client, error) {
	config, err := getRestConfig()
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	return client.New(config, client.Options{Scheme: scheme})
}

// getRestConfig returns the kubernetes REST config.
// When KIND_CLUSTER is set it forces the context to kind-<cluster> so the
// tests never accidentally target a different cluster.
func getRestConfig() (*rest.Config, error) {
	overrides := &clientcmd.ConfigOverrides{}
	if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
		overrides.CurrentContext = "kind-" + kindCluster
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		overrides,
	).ClientConfig()
}
