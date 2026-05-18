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
	"net/http"
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
// newImageTag / newMetadataTag are NOT separate image content — they are
// synthetic tag strings derived from the loaded image's tag with
// upgradeTagSuffix appended. scripts/load-e2e-images.sh pushes the same
// engine / metadata content under both ${TAG} and ${TAG}-uptest into the
// local kind-registry; an OCI registry stores the second tag as a manifest
// pointing at existing blobs, so there is no extra layer transfer or kind
// node disk usage. Kind nodes resolve both tags transparently through the
// containerd hosts.toml mirror written by setup-kind-cluster.sh.
//
// upgradeTagSuffix MUST stay in sync with UPGRADE_TAG_SUFFIX in
// scripts/load-e2e-images.sh — the script is what materialises this tag
// in the registry, and a mismatch surfaces as an opaque ImagePullBackOff.
//
// See docs/SDLC.md "Default image bumps" and README.md "Bumping Default
// Image Versions" for the variant rules.
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
		"compute.firebolt.io_engineclasses.yaml",
		"compute.firebolt.io_fireboltengines.yaml",
		"compute.firebolt.io_fireboltinstances.yaml",
	}
	for _, crd := range crds {
		crdPath := filepath.Join(projectRoot, "config", "crd", "bases", crd)
		// --server-side: the EngineClass CRD is >600KB (the embedded
		// PodTemplateSpec schema), well past the 256KB cap kubectl's
		// client-side apply writes into the
		// last-applied-configuration annotation. Server-side apply does
		// not stamp that annotation, so it works at any size and is the
		// recommended path for large CRDs anyway.
		output, err := exec.Command("kubectl", "apply", "--server-side", "--force-conflicts", "-f", crdPath).CombinedOutput()
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

	By("Deploying floci (S3 emulator) and pre-creating the engine bucket")
	// The engine refuses to start without managed_storage pointing at object
	// storage when dedicated-pensieve mode is on. Stand up floci once for the
	// whole suite; engines created by CreateEngineWithRollout target it via
	// spec.customEngineConfig.firebolt.managed_storage.
	flociCtx, flociCancel := context.WithTimeout(ctx, flociSetupTimeout)
	defer flociCancel()
	Expect(setupFloci(flociCtx, testNamespace)).To(Succeed())

	By("Verifying required container images are published to the kind-registry mirror")
	// kind nodes mirror ghcr.io / docker.io through the local registry
	// started by scripts/setup-local-registry.sh. We verify image
	// availability by hitting the registry's manifest endpoint from the
	// host (where this test process runs). The previous `crictl inspecti`
	// check ran against the kind node's containerd content store, which
	// is empty at suite start under the registry-mirror flow — content
	// only lands on a node when a pod actually pulls.
	requiredImages := []string{
		testImage + ":" + testTag,
		testImage + ":" + newImageTag,
		metadataImage + ":" + metadataTag,
		metadataImage + ":" + newMetadataTag,
		postgresImage,
		envoyImage + ":" + envoyTag,
		curlImage,
	}
	registryEndpoint := os.Getenv("REGISTRY_HOST_ENDPOINT")
	if registryEndpoint == "" {
		registryEndpoint = "localhost:5001"
	}
	for _, img := range requiredImages {
		if err := verifyImageInRegistry(registryEndpoint, img); err != nil {
			Fail(fmt.Sprintf("Required image %q is not available via registry %s: %v. "+
				"Run `make load-test-images` to (re-)publish images.",
				img, registryEndpoint, err))
		}
		fmt.Fprintf(GinkgoWriter, "Verified image %s is available in registry %s\n", img, registryEndpoint)
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

// verifyImageInRegistry checks that an image (in "[host/]repo[:tag]" form) is
// available via the local kind-registry by HEADing the manifest endpoint. The
// path translation matches scripts/load-e2e-images.sh's `to_registry_path`:
// strip an explicit registry host, prepend "library/" for bare Docker Hub
// names. Any non-2xx response is treated as missing.
func verifyImageInRegistry(registryEndpoint, image string) error {
	repoPath, tag := registryRepoPathAndTag(image)
	url := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registryEndpoint, repoPath, tag)

	req, err := http.NewRequest(http.MethodHead, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	// Accept multiple manifest media types so the registry doesn't 404 a
	// well-formed manifest for which we forgot to advertise an Accept.
	for _, mt := range []string{
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	} {
		req.Header.Add("Accept", mt)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HEAD %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HEAD %s returned HTTP %d", url, resp.StatusCode)
	}
	return nil
}

// registryRepoPathAndTag splits "[host/]repo[:tag]" into (repoPath, tag),
// applying Docker's image normalisation so the result matches what
// scripts/load-e2e-images.sh pushes into the local registry. Tag defaults to
// "latest" when absent (matching Docker's resolution rules).
func registryRepoPathAndTag(image string) (string, string) {
	tag := "latest"
	repo := image
	// Split off the tag at the LAST colon so a host:port prefix like
	// "localhost:5001/foo:tag" parses correctly. We use the last "/" to
	// decide whether the colon belongs to a host:port (no "/" after it
	// means no path component, i.e. host:port) — but in practice this
	// helper is fed bare references like "ghcr.io/firebolt-db/engine:dev",
	// "postgres:16-alpine", or "envoyproxy/envoy:v1.37.2" where the last
	// colon is always the tag separator.
	if idx := strings.LastIndex(image, ":"); idx > strings.LastIndex(image, "/") {
		repo = image[:idx]
		tag = image[idx+1:]
	}

	firstSeg := repo
	if i := strings.Index(repo, "/"); i >= 0 {
		firstSeg = repo[:i]
	}
	switch {
	case strings.Contains(repo, "/") &&
		(strings.Contains(firstSeg, ".") || strings.Contains(firstSeg, ":") || firstSeg == "localhost"):
		// Has explicit host: strip it.
		return repo[strings.Index(repo, "/")+1:], tag
	case strings.Contains(repo, "/"):
		// org/name on Docker Hub: keep as is.
		return repo, tag
	default:
		// Bare name on Docker Hub: prepend library/.
		return "library/" + repo, tag
	}
}
