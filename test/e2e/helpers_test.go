//go:build e2e
// +build e2e

/*
Copyright 2025.

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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/firebolt-analytics/core-operator/internal/controller"
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
	Suffix    string // Added to cluster names to avoid conflicts between light/heavy runs
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
	prefix     string
}

// operatorInstanceCounter is used to generate unique controller names
var operatorInstanceCounter atomic.Int64

// StartOperator starts an operator instance with the given prefix
func StartOperator(prefix string) (*OperatorInstance, error) {
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add corev1 to scheme: %w", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("failed to add appsv1 to scheme: %w", err)
	}

	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				testNamespace: {},
			},
		},
		// Use port 0 to let the system pick a free port (for parallel tests)
		Metrics: metricsserver.Options{
			BindAddress: "0", // Disable metrics server
		},
		HealthProbeBindAddress: "0", // Disable health probes
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	reconciler := &controller.ConfigMapReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		ConfigPrefix: prefix,
		Namespace:    testNamespace,
		RestConfig:   config,
		Clientset:    clientset,
	}

	// Use a unique controller name to avoid collisions when restarting operators
	controllerName := fmt.Sprintf("%s-%d", prefix, operatorInstanceCounter.Add(1))
	if err := reconciler.SetupWithManagerNamed(mgr, controllerName); err != nil {
		return nil, fmt.Errorf("failed to setup reconciler: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	instance := &OperatorInstance{
		mgr:        mgr,
		cancelFunc: cancel,
		prefix:     prefix,
	}

	instance.wg.Add(1)
	go func() {
		defer instance.wg.Done()
		defer GinkgoRecover()
		if err := mgr.Start(ctx); err != nil {
			fmt.Fprintf(GinkgoWriter, "Manager for prefix %s exited with error: %v\n", prefix, err)
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

// CreateClusterConfig creates a ConfigMap for a Core cluster
func CreateClusterConfig(ctx context.Context, name string, replicas int) error {
	return CreateClusterConfigWithRollout(ctx, name, replicas, "graceful")
}

// CreateClusterConfigWithRollout creates a ConfigMap for a Core cluster with a specific rollout strategy
func CreateClusterConfigWithRollout(ctx context.Context, name string, replicas int, rollout string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"replicas":           strconv.Itoa(replicas),
			"image":              testImage,
			"tag":                testTag,
			"imagePullPolicy":    "IfNotPresent",
			"cpu":                "100m",
			"memory":             "3Gi", // Note: 2Gi is insufficient for these tests (drain check query OOMs)
			"drainCheckInterval": "2s",
			"rollout":            rollout,
		},
	}

	_, err := k8sClient.CoreV1().ConfigMaps(testNamespace).Create(ctx, cm, metav1.CreateOptions{})
	return err
}

// UpdateClusterReplicas updates the replicas count in the cluster config
func UpdateClusterReplicas(ctx context.Context, name string, replicas int) error {
	cm, err := k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	cm.Data["replicas"] = strconv.Itoa(replicas)
	_, err = k8sClient.CoreV1().ConfigMaps(testNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// UpdateClusterImageTag updates the image tag in the cluster config
func UpdateClusterImageTag(ctx context.Context, name string, tag string) error {
	cm, err := k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}

	cm.Data["tag"] = tag
	_, err = k8sClient.CoreV1().ConfigMaps(testNamespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

// DeleteClusterConfig deletes the cluster ConfigMap
func DeleteClusterConfig(ctx context.Context, name string) error {
	return k8sClient.CoreV1().ConfigMaps(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// WaitForClusterReady waits for all pods in a cluster to be ready
func WaitForClusterReady(ctx context.Context, clusterName string, expectedReplicas int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("core-operator/cluster=%s", clusterName),
		})
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}

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
			return nil
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("timeout waiting for cluster %s to have %d ready pods", clusterName, expectedReplicas)
}

// WaitForClusterStable waits for the cluster status to be stable
func WaitForClusterStable(ctx context.Context, clusterName string, timeout time.Duration) error {
	statusName := clusterName + "-status"
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		cm, err := k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, statusName, metav1.GetOptions{})
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}

		stateStr, ok := cm.Data["state"]
		if !ok {
			time.Sleep(pollInterval)
			continue
		}

		var status struct {
			Phase string `json:"phase"`
		}
		if err := json.Unmarshal([]byte(stateStr), &status); err != nil {
			time.Sleep(pollInterval)
			continue
		}

		if status.Phase == "stable" {
			return nil
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("timeout waiting for cluster %s to be stable", clusterName)
}

// WaitForResourcesDeleted waits for all cluster resources to be deleted
func WaitForResourcesDeleted(ctx context.Context, clusterName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		// Check for pods
		pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("core-operator/cluster=%s", clusterName),
		})
		if err == nil && len(pods.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		// Check for services
		svcs, err := k8sClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("core-operator/cluster=%s", clusterName),
		})
		if err == nil && len(svcs.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		// Check for statefulsets
		stsList, err := k8sClient.AppsV1().StatefulSets(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("core-operator/cluster=%s", clusterName),
		})
		if err == nil && len(stsList.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		// Check for status configmap
		_, err = k8sClient.CoreV1().ConfigMaps(testNamespace).Get(ctx, clusterName+"-status", metav1.GetOptions{})
		if err == nil || !errors.IsNotFound(err) {
			time.Sleep(pollInterval)
			continue
		}

		// All resources deleted
		return nil
	}

	return fmt.Errorf("timeout waiting for resources of cluster %s to be deleted", clusterName)
}

// RunQuery executes a SQL query through the cluster service using port-forwarding.
// This ensures queries go through the stable service endpoint, which always
// points to the active generation during zero-downtime transitions.
func RunQuery(ctx context.Context, clusterName, query string) (string, error) {
	serviceName := clusterName + "-service"
	return executeQueryViaService(ctx, serviceName, query)
}

// executeQueryViaService port-forwards to the cluster service and executes an HTTP query
func executeQueryViaService(ctx context.Context, serviceName, query string) (string, error) {
	// Find a free local port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("failed to find free port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	// Use kubectl port-forward to the service (client-go doesn't directly support service port-forwarding)
	cmd := exec.Command("kubectl", "--context", "kind-dev-cluster", "port-forward",
		"-n", testNamespace, "svc/"+serviceName, fmt.Sprintf("%d:3473", localPort))
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start port-forward: %w", err)
	}

	// Ensure we clean up the port-forward process
	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	// Wait for port-forward to be ready (poll until we can connect)
	var connected bool
	for i := 0; i < 50; i++ { // Try for ~5 seconds
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			connected = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !connected {
		return "", fmt.Errorf("timeout waiting for port-forward to be ready")
	}

	// Execute HTTP query
	queryURL := fmt.Sprintf("http://127.0.0.1:%d/?query_label=e2e-test-query&output_format=JSON_Compact", localPort)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, queryURL, strings.NewReader(query))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")

	httpClient := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return string(body), nil
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

// BackgroundQueryRunner runs queries in the background and tracks failures
type BackgroundQueryRunner struct {
	clusterName    string
	query          string
	validator      QueryValidator
	stopCh         chan struct{}
	wg             sync.WaitGroup
	failureCount   atomic.Int32
	successCount   atomic.Int32
	mu             sync.Mutex
	failureReasons map[string]int // Tracks failure reasons and their counts
}

// NewBackgroundQueryRunner creates a new background query runner with automatic validator selection
func NewBackgroundQueryRunner(clusterName, query string) *BackgroundQueryRunner {
	validator := LightQueryValidator // Default for SELECT <number>
	if strings.Contains(query, "array_agg") {
		validator = HeavyQueryValidator
	}
	return NewBackgroundQueryRunnerWithValidator(clusterName, query, validator)
}

// NewBackgroundQueryRunnerWithValidator creates a background query runner with custom validator
func NewBackgroundQueryRunnerWithValidator(clusterName, query string, validator QueryValidator) *BackgroundQueryRunner {
	return &BackgroundQueryRunner{
		clusterName:    clusterName,
		query:          query,
		validator:      validator,
		stopCh:         make(chan struct{}),
		failureReasons: make(map[string]int),
	}
}

// Start starts running queries in the background
func (r *BackgroundQueryRunner) Start(ctx context.Context) {
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

// runQuery executes a single query and records the result
func (r *BackgroundQueryRunner) runQuery(ctx context.Context) {
	output, err := RunQuery(ctx, r.clusterName, r.query)
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

// recordFailure records a failure with its reason
func (r *BackgroundQueryRunner) recordFailure(category, detail string) {
	r.failureCount.Add(1)

	// Categorize the error for summary
	reason := category + ": " + r.categorizeError(detail)

	r.mu.Lock()
	r.failureReasons[reason]++
	r.mu.Unlock()

	fmt.Fprintf(GinkgoWriter, "Background query failed [%s]: %s\n", category, detail)
}

// categorizeError extracts a short category from the error detail
func (r *BackgroundQueryRunner) categorizeError(detail string) string {
	// Extract key error patterns for grouping
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
	case strings.Contains(detail, "port-forward"):
		return "port-forward error"
	default:
		// Truncate long messages
		if len(detail) > 50 {
			return detail[:50] + "..."
		}
		return detail
	}
}

// Stop stops the background query runner
func (r *BackgroundQueryRunner) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}

// GetStats returns the success and failure counts
func (r *BackgroundQueryRunner) GetStats() (successes, failures int32) {
	return r.successCount.Load(), r.failureCount.Load()
}

// GetFailureReasons returns a summary of failure reasons
func (r *BackgroundQueryRunner) GetFailureReasons() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Return a copy
	result := make(map[string]int)
	for k, v := range r.failureReasons {
		result[k] = v
	}
	return result
}

// PrintFailureSummary prints a summary of all failure reasons
func (r *BackgroundQueryRunner) PrintFailureSummary() {
	reasons := r.GetFailureReasons()
	if len(reasons) == 0 {
		return
	}

	fmt.Fprintf(GinkgoWriter, "\n=== Background Query Failure Summary ===\n")
	for reason, count := range reasons {
		fmt.Fprintf(GinkgoWriter, "  %s: %d\n", reason, count)
	}
	fmt.Fprintf(GinkgoWriter, "=========================================\n")
}

// Helper function to get rest.Config
func getRestConfig() (*rest.Config, error) {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

// Helper to get controller-runtime client
func getClient() (client.Client, error) {
	config, err := getRestConfig()
	if err != nil {
		return nil, err
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	return client.New(config, client.Options{Scheme: scheme})
}
