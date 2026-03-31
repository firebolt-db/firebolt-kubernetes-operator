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
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	computev1alpha1 "github.com/firebolt-analytics/core-operator/api/v1alpha1"
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

// StartOperator starts an operator instance. The labelValue is used to scope the cache
// so multiple operator instances in the same namespace don't interfere.
func StartOperator(labelValue string) (*OperatorInstance, error) {
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
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		RestConfig: config,
		Clientset:  clientset,
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

// CreateEngine creates a FireboltEngine CR
func CreateEngine(ctx context.Context, name string, replicas int) error {
	return CreateEngineWithRollout(ctx, name, replicas, "graceful")
}

// CreateEngineWithRollout creates a FireboltEngine CR with a specific rollout strategy
func CreateEngineWithRollout(ctx context.Context, name string, replicas int, rollout string) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}

	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			Replicas: int32(replicas),
			Image: computev1alpha1.ImageSpec{
				Repository: testImage,
				Tag:        testTag,
				PullPolicy: corev1.PullIfNotPresent,
			},
			Resources: computev1alpha1.ResourceRequirements{
				CPU:    resource.MustParse("100m"),
				Memory: resource.MustParse("3Gi"),
			},
			DrainCheckInterval: &metav1.Duration{Duration: 2 * time.Second},
			Rollout:            computev1alpha1.RolloutStrategy(rollout),
			MetadataService: &computev1alpha1.MetadataServiceSpec{
				Image: &computev1alpha1.ImageSpec{
					Repository: pensieveImage,
					Tag:        pensieveTag,
					PullPolicy: corev1.PullIfNotPresent,
				},
			},
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

// WaitForEngineReady waits for the metadata service and all pods in an engine to be ready.
func WaitForEngineReady(ctx context.Context, engineName string, expectedReplicas int, timeout time.Duration) error {
	if err := WaitForMetadataServiceReady(ctx, engineName, timeout); err != nil {
		return fmt.Errorf("metadata service not ready: %w", err)
	}

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("firebolt.io/engine=%s,firebolt.io/generation", engineName),
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

	return fmt.Errorf("timeout waiting for engine %s to have %d ready pods", engineName, expectedReplicas)
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

// WaitForMetadataServiceReady waits for the metadata service (dedicated pensieve)
// Deployment to have at least 1 ready replica.
func WaitForMetadataServiceReady(ctx context.Context, engineName string, timeout time.Duration) error {
	metadataDeployName := engineName + "-metadata"
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		deploy, err := k8sClient.AppsV1().Deployments(testNamespace).Get(ctx, metadataDeployName, metav1.GetOptions{})
		if err != nil {
			time.Sleep(pollInterval)
			continue
		}

		if deploy.Status.ReadyReplicas >= 1 {
			return nil
		}

		time.Sleep(pollInterval)
	}

	return fmt.Errorf("timeout waiting for metadata service %s to become ready", metadataDeployName)
}

// WaitForResourcesDeleted waits for all engine resources to be deleted
func WaitForResourcesDeleted(ctx context.Context, engineName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	engineSelector := fmt.Sprintf("firebolt.io/engine=%s", engineName)

	for time.Now().Before(deadline) {
		pods, err := k8sClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: engineSelector,
		})
		if err == nil && len(pods.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		svcs, err := k8sClient.CoreV1().Services(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: engineSelector,
		})
		if err == nil && len(svcs.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		stsList, err := k8sClient.AppsV1().StatefulSets(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: engineSelector,
		})
		if err == nil && len(stsList.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		deployList, err := k8sClient.AppsV1().Deployments(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: engineSelector,
		})
		if err == nil && len(deployList.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		pvcList, err := k8sClient.CoreV1().PersistentVolumeClaims(testNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: engineSelector,
		})
		if err == nil && len(pvcList.Items) > 0 {
			time.Sleep(pollInterval)
			continue
		}

		return nil
	}

	return fmt.Errorf("timeout waiting for resources of engine %s to be deleted", engineName)
}

// RunQuery executes a SQL query through the engine service using port-forwarding.
func RunQuery(ctx context.Context, engineName, query string) (string, error) {
	serviceName := engineName + "-service"
	return executeQueryViaService(ctx, serviceName, query)
}

// executeQueryViaService port-forwards to the engine service and executes an HTTP query
func executeQueryViaService(ctx context.Context, serviceName, query string) (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("failed to find free port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	args := []string{"port-forward", "-n", testNamespace, "svc/" + serviceName, fmt.Sprintf("%d:3473", localPort)}
	if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
		args = append([]string{"--context", "kind-" + kindCluster}, args...)
	}
	cmd := exec.Command("kubectl", args...)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start port-forward: %w", err)
	}

	defer func() {
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
	}()

	var connected bool
	for i := 0; i < 50; i++ {
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
	engineName     string
	query          string
	validator      QueryValidator
	stopCh         chan struct{}
	wg             sync.WaitGroup
	failureCount   atomic.Int32
	successCount   atomic.Int32
	mu             sync.Mutex
	failureReasons map[string]int
}

// NewBackgroundQueryRunner creates a new background query runner with automatic validator selection
func NewBackgroundQueryRunner(engineName, query string) *BackgroundQueryRunner {
	validator := LightQueryValidator
	if strings.Contains(query, "array_agg") {
		validator = HeavyQueryValidator
	}
	return NewBackgroundQueryRunnerWithValidator(engineName, query, validator)
}

// NewBackgroundQueryRunnerWithValidator creates a background query runner with custom validator
func NewBackgroundQueryRunnerWithValidator(engineName, query string, validator QueryValidator) *BackgroundQueryRunner {
	return &BackgroundQueryRunner{
		engineName:     engineName,
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
	output, err := RunQuery(ctx, r.engineName, r.query)
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

	reason := category + ": " + r.categorizeError(detail)

	r.mu.Lock()
	r.failureReasons[reason]++
	r.mu.Unlock()

	fmt.Fprintf(GinkgoWriter, "Background query failed [%s]: %s\n", category, detail)
}

// categorizeError extracts a short category from the error detail
func (r *BackgroundQueryRunner) categorizeError(detail string) string {
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

// getCRDClient returns a controller-runtime client that knows about the CRD types
func getCRDClient() (client.Client, error) {
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
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}

	return client.New(config, client.Options{Scheme: scheme})
}

// getRestConfig returns the kubernetes REST config
func getRestConfig() (*rest.Config, error) {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}
