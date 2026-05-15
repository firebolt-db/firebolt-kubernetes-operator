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

	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
)

const (
	testNamespace = "firebolt-e2e"

	clusterReadyTimeout      = 300 * time.Second
	clusterTransitionTimeout = 300 * time.Second
	resourceCleanupTimeout   = 120 * time.Second
	instanceReadyTimeout     = 300 * time.Second
	pollInterval             = 1 * time.Second
)

// Image references for the E2E suite, sourced from the variant-specific
// config/images/defaults.<variant>.env file embedded into the images package
// at compile time. With the implicit default (no extra build tag) the "dev"
// variant is in effect: testTag / metadataTag track the mutable `:dev`
// aliases (release-flavored builds of the dev branch). With `IMAGE_VARIANT=
// latest` they pin to release-* build tags. Either way the suite operates
// on release builds — the previous separately-published debug-flavored
// upgrade-target image is gone.
//
// newImageTag / newMetadataTag are NOT loaded from a registry — they are
// synthetic tag strings derived from the loaded image's tag with
// upgradeTagSuffix appended, materialised inside each kind node by re-
// tagging the already-loaded image during SynchronizedBeforeSuite. That
// keeps the disk footprint to a single load per image while preserving a
// distinct tag string for the image-switch specs to flip the pod template
// to. See docs/SDLC.md "Default image bumps" and README.md "Bumping
// Default Image Versions" for the variant rules.
const upgradeTagSuffix = "-uptest"

var (
	testImage      string
	testTag        string
	newImageTag    string
	metadataImage  string
	metadataTag    string
	newMetadataTag string
	postgresImage  string
	envoyImage     string
	envoyTag       string
	curlImage      string
)

func init() {
	testImage = images.Get("ENGINE_IMAGE")
	testTag = images.Get("ENGINE_TAG")
	newImageTag = testTag + upgradeTagSuffix
	metadataImage = images.Get("METADATA_IMAGE")
	metadataTag = images.Get("METADATA_TAG")
	newMetadataTag = metadataTag + upgradeTagSuffix
	postgresImage = images.Get("POSTGRES_IMAGE")
	envoyImage = images.Get("ENVOY_IMAGE")
	envoyTag = images.Get("ENVOY_TAG")
	curlImage = images.Get("CURL_IMAGE")
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

// SynchronizedBeforeSuite runs the one-time environment setup (CRD install,
// stale-resource cleanup, namespace (re)creation, image verification) on the
// primary Ginkgo process only, then has every parallel process build its own
// Kubernetes client. FireboltInstance/operator setup is intentionally NOT done
// here: each second-level Describe owns its own instance so parallel specs
// don't interfere with each other.
var _ = SynchronizedBeforeSuite(func() {
	log.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	ctx, cancelFunc = context.WithCancel(context.Background())

	Expect(testTag).NotTo(Equal(newImageTag),
		"testTag and newImageTag must differ; upgrade tests would be no-ops")
	Expect(metadataTag).NotTo(Equal(newMetadataTag),
		"metadataTag and newMetadataTag must differ; upgrade tests would be no-ops")

	fmt.Fprintf(GinkgoWriter, "E2E image variant: %s (engine=%s, metadata=%s, upgrade-tag suffix=%q)\n",
		images.Variant(),
		testImage+":"+testTag,
		metadataImage+":"+metadataTag,
		upgradeTagSuffix,
	)

	By("Setting up Kubernetes client (proc 1)")
	var err error
	k8sClient, err = newK8sClient()
	Expect(err).NotTo(HaveOccurred())

	By("Checking minimum Kubernetes version")
	ensureMinK8sVersion(k8sClient, 1, 28)

	By("Verifying no operator deployments are installed in the cluster")
	ensureNoOperatorDeployed(ctx, k8sClient)

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
		metadataImage + ":" + metadataTag,
		postgresImage,
		envoyImage + ":" + envoyTag,
		curlImage,
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

	By("Re-tagging engine/metadata images to materialise the upgrade-target tags")
	// The image-switch specs (engine + metadata) flip the pod template to a
	// different tag string to trigger a rollout. We do not load that tag
	// from a registry — instead, re-tag the already-loaded image inside
	// each kind node's containerd. ctr's tag is a metadata-only operation,
	// so this adds zero on-disk image weight compared to loading a second
	// image (which on the engine side is multiple GB). Must run on every
	// node, not just the control-plane, because kubelet/CRI resolves
	// images node-locally.
	retagPairs := [][2]string{
		{testImage + ":" + testTag, testImage + ":" + newImageTag},
		{metadataImage + ":" + metadataTag, metadataImage + ":" + newMetadataTag},
	}
	nodesOut, err := exec.Command("kind", "get", "nodes", "--name", kindCluster).CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("Failed to list kind nodes for cluster %q: %s", kindCluster, strings.TrimSpace(string(nodesOut))))
	}
	nodes := strings.Fields(strings.TrimSpace(string(nodesOut)))
	Expect(nodes).NotTo(BeEmpty(), "kind reported no nodes for cluster %s", kindCluster)
	for _, node := range nodes {
		for _, pair := range retagPairs {
			src, dst := pair[0], pair[1]
			out, err := exec.Command(
				"docker", "exec", node,
				"ctr", "-n", "k8s.io", "image", "tag", "--force", src, dst,
			).CombinedOutput()
			if err != nil {
				Fail(fmt.Sprintf("Failed to re-tag %s -> %s on node %s: %s",
					src, dst, node, strings.TrimSpace(string(out))))
			}
			fmt.Fprintf(GinkgoWriter, "Re-tagged %s -> %s on node %s\n", src, dst, node)
		}
	}
}, func() {
	// Runs on every parallel process (including proc 1). Build a client and
	// context so specs can talk to the API server.
	log.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	if ctx == nil {
		ctx, cancelFunc = context.WithCancel(context.Background())
	}

	if k8sClient != nil {
		return
	}

	var err error
	k8sClient, err = newK8sClient()
	Expect(err).NotTo(HaveOccurred())
})

var _ = SynchronizedAfterSuite(func() {
	// Runs on every process; cancel the process-local context so goroutines
	// using it wind down.
	if cancelFunc != nil {
		cancelFunc()
	}
}, func() {
	// Runs once on the primary process after every other process has finished.
	// Use a fresh context because the suite-wide ctx was cancelled in the
	// per-process function above.
	By("Cleaning up test namespace")
	if k8sClient != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		err := k8sClient.CoreV1().Namespaces().Delete(cleanupCtx, testNamespace, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			fmt.Fprintf(GinkgoWriter, "Warning: failed to delete namespace: %v\n", err)
		}
	}
})

// ensureNoOperatorDeployed fails the suite if any firebolt operator Deployment
// is already running in the cluster. The E2E suite runs its own in-process
// operators per test, so an externally-deployed operator (e.g. left over from
// `make local-deploy`) would fight with them over the same CRs and produce
// confusing, non-deterministic failures.
func ensureNoOperatorDeployed(ctx context.Context, cs *kubernetes.Clientset) {
	const selector = "control-plane=controller-manager"

	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	deps, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(listCtx, metav1.ListOptions{LabelSelector: selector})
	Expect(err).NotTo(HaveOccurred(), "Failed to list operator deployments")

	if len(deps.Items) > 0 {
		var found []string
		for _, d := range deps.Items {
			found = append(found, fmt.Sprintf("%s/%s", d.Namespace, d.Name))
		}
		Fail(fmt.Sprintf(
			"Refusing to run E2E tests: found operator deployment(s) in the cluster [%s]. "+
				"The suite runs its own in-process operators; uninstall any externally-deployed "+
				"operator first (e.g. `make local-undeploy` or `helm uninstall firebolt-operator`).",
			strings.Join(found, ", "),
		))
	}
}

// newK8sClient builds a Kubernetes clientset honoring KIND_CLUSTER.
func newK8sClient() (*kubernetes.Clientset, error) {
	overrides := &clientcmd.ConfigOverrides{}
	if kindCluster := os.Getenv("KIND_CLUSTER"); kindCluster != "" {
		overrides.CurrentContext = "kind-" + kindCluster
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		overrides,
	).ClientConfig()
	if err != nil {
		return nil, err
	}
	if config == nil {
		config = ctrl.GetConfigOrDie()
	}
	return kubernetes.NewForConfig(config)
}

// ensureMinK8sVersion aborts the suite if the cluster's Kubernetes version is
// below the required minimum. CEL transition rules (oldSelf) require 1.28+.
func ensureMinK8sVersion(cs *kubernetes.Clientset, minMajor, minMinor int) {
	info, err := cs.Discovery().ServerVersion()
	Expect(err).NotTo(HaveOccurred(), "Failed to fetch server version")

	var major, minor int
	_, _ = fmt.Sscanf(info.Major, "%d", &major)
	// Minor may contain trailing characters like "+" (e.g. "28+").
	_, _ = fmt.Sscanf(info.Minor, "%d", &minor)

	if major < minMajor || (major == minMajor && minor < minMinor) {
		Fail(fmt.Sprintf(
			"Kubernetes %s.%s is below the minimum required version %d.%d. "+
				"The operator CRDs use CEL transition rules (oldSelf) which require Kubernetes 1.28+. "+
				"Upgrade your cluster before running E2E tests.",
			info.Major, info.Minor, minMajor, minMinor,
		))
	}

	fmt.Fprintf(GinkgoWriter, "Kubernetes version %s.%s meets minimum requirement %d.%d\n",
		info.Major, info.Minor, minMajor, minMinor)
}

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
