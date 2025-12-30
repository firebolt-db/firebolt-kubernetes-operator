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
	testImage = "us-east4-docker.pkg.dev/shared-991415466295/firebolt-analytics/firebolt-core"
	testTag   = "release-4.31.0-pre.0.20251219132726.3032bf8968ff-amd64"

	// Timeouts
	clusterReadyTimeout      = 60 * time.Second
	clusterTransitionTimeout = 90 * time.Second
	resourceCleanupTimeout   = 60 * time.Second
	pollInterval             = 1 * time.Second
)

var (
	k8sClient  *kubernetes.Clientset
	ctx        context.Context
	cancelFunc context.CancelFunc
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	fmt.Fprintf(GinkgoWriter, "Starting Core Operator E2E test suite\n")
	RunSpecs(t, "Core Operator E2E Suite")
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

	By("Ensuring test namespace is ready")
	// Wait for namespace to be fully deleted if it's in Terminating state
	for {
		ns, err := k8sClient.CoreV1().Namespaces().Get(ctx, testNamespace, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			break // Namespace doesn't exist, we can create it
		}
		Expect(err).NotTo(HaveOccurred())
		if ns.Status.Phase == corev1.NamespaceTerminating {
			fmt.Fprintf(GinkgoWriter, "Waiting for namespace %s to finish terminating...\n", testNamespace)
			time.Sleep(1 * time.Second)
			continue
		}
		// Namespace exists and is not terminating, we can use it
		break
	}

	By("Creating test namespace")
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	_, err = k8sClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		Expect(err).NotTo(HaveOccurred())
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
