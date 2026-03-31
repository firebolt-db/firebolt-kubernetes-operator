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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
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
	// Test namespace
	testNamespace = "firebolt-e2e"

	// Test image - locally available in kind
	testImage    = "000000000000.dkr.ecr.us-east-1.amazonaws.com/firebolt-core"
	testTag      = "release-4.32.0-pre.0.20260331033249.e67bde0be1cd-amd64"
	newImageTag  = "latest"

	// Metadata service images
	pensieveImage = "000000000000.dkr.ecr.us-east-1.amazonaws.com/dedicated-pensieve"
	pensieveTag   = "4.32.0-pre.0.20260331033249.e67bde0be1cd"
	postgresImage = "postgres:16-alpine"

	// Timeouts
	clusterReadyTimeout      = 120 * time.Second
	clusterTransitionTimeout = 150 * time.Second
	resourceCleanupTimeout   = 90 * time.Second
	pollInterval             = 1 * time.Second
)

var (
	k8sClient  *kubernetes.Clientset
	ctx        context.Context
	cancelFunc context.CancelFunc
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	fmt.Fprintf(GinkgoWriter, "Starting Firebolt Engine E2E test suite\n")
	RunSpecs(t, "Firebolt Engine E2E Suite")
}

var _ = BeforeSuite(func() {
	// Setup controller-runtime logger
	log.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancelFunc = context.WithCancel(context.Background())

	By("Setting up Kubernetes client")
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())

	// Use the config from controller-runtime if available
	if config == nil {
		config = ctrl.GetConfigOrDie()
	}

	k8sClient, err = kubernetes.NewForConfig(config)
	Expect(err).NotTo(HaveOccurred())

	By("Installing FireboltEngine CRD")
	_, thisFile, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	crdPath := filepath.Join(projectRoot, "config", "crd", "bases", "compute.firebolt.io_fireboltengines.yaml")
	output, err := exec.Command("kubectl", "apply", "-f", crdPath).CombinedOutput()
	if err != nil {
		fmt.Fprintf(GinkgoWriter, "CRD install output: %s\n", string(output))
	}
	Expect(err).NotTo(HaveOccurred(), "Failed to install CRD")

	By("Deleting test namespace if it exists")
	err = k8sClient.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		Expect(err).NotTo(HaveOccurred())
	}

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
		kindCluster = "kind"
	}
	kindNode := kindCluster + "-control-plane"
	requiredImages := []string{
		testImage + ":" + testTag,
		testImage + ":" + newImageTag,
		pensieveImage + ":" + pensieveTag,
		postgresImage,
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
})

var _ = AfterSuite(func() {
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
