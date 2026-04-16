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
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	testNamespace = "firebolt-e2e"
	testInstance  = "test-instance"

	clusterReadyTimeout      = 120 * time.Second
	clusterTransitionTimeout = 300 * time.Second
	resourceCleanupTimeout   = 120 * time.Second
	instanceReadyTimeout     = 300 * time.Second
	pollInterval             = 1 * time.Second
)

var (
	testImage       string
	testTag         string
	newImageTag     string
	pensieveImage   string
	pensieveTag     string
	newPensieveTag  string
	postgresImage   string
	gatewayImage    string
	gatewayTag      string
	newGatewayTag   string
	instanceOp      *InstanceOperator
)

func init() {
	defaults := loadDefaults()
	testImage = defaults["TEST_ENGINE_IMAGE"]
	testTag = defaults["TEST_ENGINE_TAG"]
	newImageTag = defaults["TEST_ENGINE_NEW_TAG"]
	pensieveImage = defaults["TEST_PENSIEVE_IMAGE"]
	pensieveTag = defaults["TEST_PENSIEVE_TAG"]
	newPensieveTag = defaults["TEST_PENSIEVE_NEW_TAG"]
	postgresImage = defaults["TEST_POSTGRES_IMAGE"]
	gatewayImage = defaults["TEST_GATEWAY_IMAGE"]
	gatewayTag = defaults["TEST_GATEWAY_TAG"]
	newGatewayTag = defaults["TEST_GATEWAY_NEW_TAG"]
}

// loadDefaults reads key=value pairs from defaults.env next to this source file.
func loadDefaults() map[string]string {
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "defaults.env")
	f, err := os.Open(path)
	if err != nil {
		panic(fmt.Sprintf("cannot open defaults.env: %v", err))
	}
	defer f.Close()

	m := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if err := scanner.Err(); err != nil {
		panic(fmt.Sprintf("cannot read defaults.env: %v", err))
	}
	return m
}

var (
	k8sClient  *kubernetes.Clientset
	ctx        context.Context
	cancelFunc context.CancelFunc
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	fmt.Fprintf(GinkgoWriter, "Starting Firebolt Engine E2E test suite\n")
	RunSpecs(t, "Firebolt Engine E2E Suite", Label("e2e"))
}

var _ = ReportAfterSuite("E2E Summary", func(report Report) {
	fmt.Fprintf(os.Stdout, "\n============================== E2E SUMMARY ==============================\n")
	fmt.Fprintf(os.Stdout, "  Total:   %d\n", report.PreRunStats.TotalSpecs)
	fmt.Fprintf(os.Stdout, "  Passed:  %d\n", report.SpecReports.CountWithState(types.SpecStatePassed))
	fmt.Fprintf(os.Stdout, "  Failed:  %d\n", report.SpecReports.CountWithState(types.SpecStateFailed))
	fmt.Fprintf(os.Stdout, "  Skipped: %d\n", report.SpecReports.CountWithState(types.SpecStateSkipped))
	fmt.Fprintf(os.Stdout, "  Pending: %d\n", report.SpecReports.CountWithState(types.SpecStatePending))
	fmt.Fprintf(os.Stdout, "  Duration: %s\n", report.RunTime.Truncate(time.Second))

	failed := report.SpecReports.WithState(types.SpecStateFailed)
	if len(failed) > 0 {
		fmt.Fprintf(os.Stdout, "\n  FAILED TESTS:\n")
		for _, spec := range failed {
			fmt.Fprintf(os.Stdout, "    ✗ %s (%s)\n", spec.FullText(), spec.RunTime.Truncate(time.Second))
			if spec.Failure.Message != "" {
				fmt.Fprintf(os.Stdout, "      %s\n", spec.Failure.Message)
				fmt.Fprintf(os.Stdout, "      %s\n", spec.Failure.Location)
			}
		}
	}

	passed := report.SpecReports.WithState(types.SpecStatePassed)
	if len(passed) > 0 {
		fmt.Fprintf(os.Stdout, "\n  PASSED TESTS:\n")
		for _, spec := range passed {
			fmt.Fprintf(os.Stdout, "    ✓ %s (%s)\n", spec.FullText(), spec.RunTime.Truncate(time.Second))
		}
	}

	fmt.Fprintf(os.Stdout, "==========================================================================\n\n")
})

var _ = BeforeSuite(func() {
	// Setup controller-runtime logger
	log.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancelFunc = context.WithCancel(context.Background())

	Expect(testTag).NotTo(Equal(newImageTag),
		"TEST_ENGINE_TAG and TEST_ENGINE_NEW_TAG must differ; upgrade tests would be no-ops")

	By("Setting up Kubernetes client")
	overrides := &clientcmd.ConfigOverrides{}
	if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
		overrides.CurrentContext = "kind-" + kindCluster
		fmt.Fprintf(GinkgoWriter, "Forcing kubeconfig context to kind-%s\n", kindCluster)
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		overrides,
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())

	// Use the config from controller-runtime if available
	if config == nil {
		config = ctrl.GetConfigOrDie()
	}

	k8sClient, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())

	By("Installing CRDs")
	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	crds := []string{
		"compute.firebolt.io_fireboltengines.yaml",
		"compute.firebolt.io_fireboltinstances.yaml",
	}
	for _, crd := range crds {
		crdPath := filepath.Join(projectRoot, "config", "crd", "bases", crd)
		output, err := exec.Command("kubectl", "apply", "-f", crdPath).CombinedOutput()
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "CRD install output for %s: %s\n", crd, string(output))
		}
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRD %s", crd)
	}

	By("Cleaning up stale resources from previous runs")
	cleanupStaleResources(ctx)

	By("Waiting for namespace to be fully deleted")
	for {
		_, err := k8sClient.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		fmt.Fprintf(GinkgoWriter, "Waiting for namespace %s to be deleted...\n", testNamespace)
		time.Sleep(1 * time.Second)
	}

	By("Creating fresh test namespace")
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	_, err = k8sClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())

	By("Verifying required container images are loaded in Kind cluster")
	kindCluster := os.Getenv("KIND_CLUSTER")
	if kindCluster == "" {
		kindCluster = "operator-test-e2e"
	}
	kindNode := kindCluster + "-control-plane"
	requiredImages := []string{
		testImage + ":" + testTag,
		testImage + ":" + newImageTag,
		pensieveImage + ":" + pensieveTag,
		pensieveImage + ":" + newPensieveTag,
		postgresImage,
		gatewayImage + ":" + gatewayTag,
		gatewayImage + ":" + newGatewayTag,
	}
	for _, img := range requiredImages {
		out, err := exec.Command("docker", "exec", kindNode, "crictl", "inspecti", img).CombinedOutput()
		if err != nil {
			fmt.Fprintf(GinkgoWriter, "crictl inspecti output for %s: %s\n", img, strings.TrimSpace(string(out)))
			Fail(fmt.Sprintf("Required image %q is not loaded in Kind cluster %q. "+
				"Load it with: kind load docker-image %s --name %s", img, kindCluster, img, kindCluster))
		}
		fmt.Fprintf(GinkgoWriter, "Verified image %s is available\n", img)
	}

	By("Starting instance operator")
	instanceOp, err = StartInstanceOperator()
	Expect(err).NotTo(HaveOccurred())

	By("Creating shared FireboltInstance")
	err = CreateInstance(ctx, testInstance, pensieveImage, pensieveTag, gatewayImage, gatewayTag)
	Expect(err).NotTo(HaveOccurred())

	By("Waiting for instance to become Ready")
	err = WaitForInstanceReady(ctx, testInstance, instanceReadyTimeout)
	Expect(err).NotTo(HaveOccurred())
	fmt.Fprintf(GinkgoWriter, "Instance %s is Ready\n", testInstance)
})

var _ = AfterSuite(func() {
	By("Deleting shared FireboltInstance")
	if err := DeleteInstance(ctx, testInstance); err != nil {
		fmt.Fprintf(GinkgoWriter, "Warning: failed to delete instance: %v\n", err)
	}

	By("Stopping instance operator")
	if instanceOp != nil {
		instanceOp.Stop()
	}

	By("Cleaning up test namespace")
	if k8sClient != nil {
		err := k8sClient.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			fmt.Fprintf(GinkgoWriter, "Warning: failed to delete namespace: %v\n", err)
		}
	}

	if cancelFunc != nil {
		cancelFunc()
	}
})

// cleanupStaleResources strips finalizers from CRDs left by a previous test
// run, then deletes the namespace. Without this, the namespace hangs in
// Terminating because no controller is running to process the finalizers.
func cleanupStaleResources(ctx context.Context) {
	// Strip finalizers from FireboltInstances
	patchNoFinalizers := []byte(`{"metadata":{"finalizers":null}}`)
	for _, kind := range []string{"fireboltinstances", "fireboltengines"} {
		args := []string{"get", kind, "-n", testNamespace, "-o", "jsonpath={.items[*].metadata.name}"}
		if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
			args = append([]string{"--context", "kind-" + kindCluster}, args...)
		}
		out, err := exec.Command("kubectl", args...).CombinedOutput()
		if err != nil {
			continue
		}
		names := strings.Fields(strings.TrimSpace(string(out)))
		for _, name := range names {
			patchArgs := []string{"patch", kind, name, "-n", testNamespace, "--type=merge", "-p", string(patchNoFinalizers)}
			if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
				patchArgs = append([]string{"--context", "kind-" + kindCluster}, patchArgs...)
			}
			if patchOut, patchErr := exec.Command("kubectl", patchArgs...).CombinedOutput(); patchErr != nil {
				fmt.Fprintf(GinkgoWriter, "Warning: failed to strip finalizers from %s/%s: %s\n", kind, name, string(patchOut))
			} else {
				fmt.Fprintf(GinkgoWriter, "Stripped finalizers from %s/%s\n", kind, name)
			}
		}
	}

	err := k8sClient.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		fmt.Fprintf(GinkgoWriter, "Warning: failed to delete namespace: %v\n", err)
	}
}
