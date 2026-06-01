//go:build webhook_integration

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

// Integration suite that boots envtest with admission webhooks
// installed and a controller-runtime manager exposing the operator's
// validators + defaulter on the same cert. Build tag isolates this
// from the default `make test` run because the suite spawns an
// extra webhook server and provisions certs, which is heavier than
// the standard CRD-only envtest harness in suite_test.go.
//
// Run with `make test-webhook-integration` (sets the build tag and
// reuses the envtest binaries pinned by setup-envtest).
//
// What this exercises that nothing else does today: the wire
// behavior of every webhook the helm chart installs — defaulter
// fills in spec.id over the network, FireboltEngineClass validation rejects
// operator-owned-field writes over the network, and the deletion
// guard refuses DELETE while a bound engine exists over the network.
// Regressions in cert wiring, the rendered Service / WebhookConfig
// paths, or the validator setup would otherwise only surface at
// install time in a customer cluster.
package controller

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtimepkg "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	utilptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// webhookSuite holds the live envtest + manager state shared across
// the suite's tests. Built once in TestMain so the per-test cost is
// only the CR(s) each test creates.
type webhookSuite struct {
	env       *envtest.Environment
	cfg       *rest.Config
	cli       client.Client
	mgrCancel context.CancelFunc
}

var (
	suite    webhookSuite
	setupErr error
)

// TestMain bootstraps envtest with admission webhooks installed and
// starts the manager that serves them. controller-runtime's
// WebhookInstallOptions rewrites the configs we pass in to point at
// the cert + port envtest provisions, so the manifests we build
// programmatically here use placeholder Service references that get
// substituted at Start time.
//
// Setup errors are captured on the package-level setupErr so each
// test can surface them via t.Fatal — the linter (revive) forbids a
// bare os.Exit / log.Fatalf in TestMain since Go 1.15, and skipping
// m.Run on failure would mask the cause.
func TestMain(m *testing.M) {
	setupErr = setupWebhookSuite()
	defer teardownWebhookSuite()
	m.Run()
}

// requireWebhookSuite fails the calling test fast when TestMain's
// envtest + manager setup did not complete. One call site per test
// instead of an inline nil-check on suite.cli at every entry point.
func requireWebhookSuite(t *testing.T) {
	t.Helper()
	if setupErr != nil {
		t.Fatalf("webhook integration suite did not start: %v", setupErr)
	}
}

func setupWebhookSuite() error {
	scheme := runtimepkg.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return fmt.Errorf("clientgoscheme.AddToScheme: %w", err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		return fmt.Errorf("computev1alpha1.AddToScheme: %w", err)
	}

	mutating, validating := buildWebhookConfigs()

	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	suite.env = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{
			MutatingWebhooks:   []*admissionregistrationv1.MutatingWebhookConfiguration{mutating},
			ValidatingWebhooks: []*admissionregistrationv1.ValidatingWebhookConfiguration{validating},
		},
	}

	if dir := firstEnvtestBinaryDir(repoRoot); dir != "" {
		suite.env.BinaryAssetsDirectory = dir
	}

	cfg, err := suite.env.Start()
	if err != nil {
		return fmt.Errorf("envtest.Start: %w", err)
	}
	suite.cfg = cfg

	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("client.New: %w", err)
	}
	suite.cli = cli

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		WebhookServer: webhook.NewServer(webhook.Options{
			Host:    suite.env.WebhookInstallOptions.LocalServingHost,
			Port:    suite.env.WebhookInstallOptions.LocalServingPort,
			CertDir: suite.env.WebhookInstallOptions.LocalServingCertDir,
		}),
	})
	if err != nil {
		return fmt.Errorf("ctrl.NewManager: %w", err)
	}

	if err := computev1alpha1.SetupFireboltInstanceWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("setup FireboltInstance webhook: %w", err)
	}
	if err := computev1alpha1.SetupFireboltEngineClassWebhookWithManager(mgr); err != nil {
		return fmt.Errorf("setup FireboltEngineClass webhook: %w", err)
	}
	if err := computev1alpha1.SetupFireboltEngineWebhookWithManager(mgr, nil); err != nil {
		return fmt.Errorf("setup FireboltEngine webhook: %w", err)
	}

	mgrCtx, cancel := context.WithCancel(context.Background())
	suite.mgrCancel = cancel
	go func() {
		if err := mgr.Start(mgrCtx); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "manager exited: %v\n", err)
		}
	}()

	if err := waitForWebhookServer(
		suite.env.WebhookInstallOptions.LocalServingHost,
		suite.env.WebhookInstallOptions.LocalServingPort,
		20*time.Second,
	); err != nil {
		return fmt.Errorf("webhook server never came up: %w", err)
	}

	return nil
}

func teardownWebhookSuite() {
	if suite.mgrCancel != nil {
		suite.mgrCancel()
	}
	if suite.env != nil {
		if err := suite.env.Stop(); err != nil {
			// macOS apiserver may ignore SIGTERM (see the project-wide
			// note in controller/suite_test.go); we accept that error
			// here too so the suite shuts down cleanly on Darwin.
			if runtime.GOOS != "darwin" || !strings.Contains(err.Error(), "timeout waiting for process kube-apiserver") {
				_, _ = fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
			}
		}
	}
}

// buildWebhookConfigs returns Mutating and Validating webhook
// configurations covering every webhook the helm chart installs. The
// rule sets mirror the chart templates so envtest exercises the same
// surface customers run in production. ClientConfig.Service is a
// placeholder — envtest.WebhookInstallOptions rewrites it to a URL
// pointing at the local serving endpoint and stamps in the CABundle
// it generates from the in-memory cert.
func buildWebhookConfigs() (*admissionregistrationv1.MutatingWebhookConfiguration, *admissionregistrationv1.ValidatingWebhookConfiguration) {
	failPolicyFail := admissionregistrationv1.Fail
	failPolicyIgnore := admissionregistrationv1.Ignore
	sideEffectsNone := admissionregistrationv1.SideEffectClassNone
	matchPolicyEquivalent := admissionregistrationv1.Equivalent
	scopeNamespaced := admissionregistrationv1.NamespacedScope
	timeout := int32(10)

	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt-mutating"},
		Webhooks: []admissionregistrationv1.MutatingWebhook{
			{
				Name: "mfireboltinstance.compute.firebolt.io",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: "default",
						Name:      "webhook-service",
						Path:      utilptr.To("/mutate-compute-firebolt-io-v1alpha1-fireboltinstance"),
					},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"compute.firebolt.io"},
						APIVersions: []string{"v1alpha1"},
						Resources:   []string{"fireboltinstances"},
						Scope:       &scopeNamespaced,
					},
				}},
				FailurePolicy:           &failPolicyIgnore,
				SideEffects:             &sideEffectsNone,
				MatchPolicy:             &matchPolicyEquivalent,
				TimeoutSeconds:          &timeout,
				AdmissionReviewVersions: []string{"v1"},
			},
		},
	}

	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt-validating"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{
			{
				Name: "vfireboltinstance.compute.firebolt.io",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: "default",
						Name:      "webhook-service",
						Path:      utilptr.To("/validate-compute-firebolt-io-v1alpha1-fireboltinstance"),
					},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"compute.firebolt.io"},
						APIVersions: []string{"v1alpha1"},
						Resources:   []string{"fireboltinstances"},
						Scope:       &scopeNamespaced,
					},
				}},
				FailurePolicy:           &failPolicyFail,
				SideEffects:             &sideEffectsNone,
				MatchPolicy:             &matchPolicyEquivalent,
				TimeoutSeconds:          &timeout,
				AdmissionReviewVersions: []string{"v1"},
			},
			{
				Name: "vfireboltengineclass.compute.firebolt.io",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: "default",
						Name:      "webhook-service",
						Path:      utilptr.To("/validate-compute-firebolt-io-v1alpha1-fireboltengineclass"),
					},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
						admissionregistrationv1.Delete,
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"compute.firebolt.io"},
						APIVersions: []string{"v1alpha1"},
						Resources:   []string{"fireboltengineclasses"},
						Scope:       &scopeNamespaced,
					},
				}},
				FailurePolicy:           &failPolicyFail,
				SideEffects:             &sideEffectsNone,
				MatchPolicy:             &matchPolicyEquivalent,
				TimeoutSeconds:          &timeout,
				AdmissionReviewVersions: []string{"v1"},
			},
			{
				Name: "vfireboltengine.compute.firebolt.io",
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					Service: &admissionregistrationv1.ServiceReference{
						Namespace: "default",
						Name:      "webhook-service",
						Path:      utilptr.To("/validate-compute-firebolt-io-v1alpha1-fireboltengine"),
					},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create,
						admissionregistrationv1.Update,
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"compute.firebolt.io"},
						APIVersions: []string{"v1alpha1"},
						Resources:   []string{"fireboltengines"},
						Scope:       &scopeNamespaced,
					},
				}},
				FailurePolicy:           &failPolicyFail,
				SideEffects:             &sideEffectsNone,
				MatchPolicy:             &matchPolicyEquivalent,
				TimeoutSeconds:          &timeout,
				AdmissionReviewVersions: []string{"v1"},
			},
		},
	}
	return mutating, validating
}

// waitForWebhookServer polls the local webhook serving endpoint until
// it accepts a TLS handshake or the timeout elapses. The handshake
// suffices — we don't need to issue an admission request to confirm
// the server is ready; controller-runtime's manager opens the listener
// before Start unblocks, so dialing the port is a meaningful signal.
// We accept self-signed certs (envtest's CA is private) and bound
// every dial attempt with a short context so the poll loop is
// responsive without being noisy.
func waitForWebhookServer(host string, port int, timeout time.Duration) error {
	address := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	deadline := time.Now().Add(timeout)
	// InsecureSkipVerify is correct here: envtest mints a private CA
	// that we don't import; we only need to know the listener
	// accepts a TLS handshake, not validate the cert chain.
	dialer := &tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true}}
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := dialer.DialContext(dialCtx, "tcp", address)
		cancel()
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("webhook server at %s did not accept connections within %s", address, timeout)
}

// firstEnvtestBinaryDir mirrors the helper in suite_test.go so this
// suite can run from IDEs without exporting KUBEBUILDER_ASSETS.
func firstEnvtestBinaryDir(repoRoot string) string {
	base := filepath.Join(repoRoot, "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}

// TestWebhook_Defaulter_MintsULID verifies the mutating defaulter
// over the wire: a FireboltInstance Create that arrives with
// spec.id empty must come back with a 26-character ULID set by the
// webhook, not by the controller's reconcile fallback (which has not
// run in this test). Failure mode this catches: cert wiring, the
// rendered mutating webhook path, or the defaulter registration is
// broken in a way that lets writes through unmodified.
func TestWebhook_Defaulter_MintsULID(t *testing.T) {
	requireWebhookSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "defaulted", Namespace: "default"},
		// Spec.ID intentionally empty.
	}
	if err := suite.cli.Create(ctx, inst); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = suite.cli.Delete(context.Background(), inst) })

	got := &computev1alpha1.FireboltInstance{}
	if err := suite.cli.Get(ctx, client.ObjectKey{Name: inst.Name, Namespace: inst.Namespace}, got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Spec.ID) != 26 {
		t.Fatalf("spec.id length = %d, want 26 (ULID minted by mutating webhook): %q",
			len(got.Spec.ID), got.Spec.ID)
	}
}

// TestWebhook_FireboltEngineClass_RejectsOwnedField verifies the validating
// webhook over the wire: a FireboltEngineClass.spec.template carrying a
// path the operator owns end-to-end (engine container command in
// this case) must be rejected at admission with a field.Forbidden
// pointing at the offending coordinate. Same shape as the table
// rows in api/v1alpha1/fireboltengineclass_webhook_test.go, but driven
// through the apiserver.
func TestWebhook_FireboltEngineClass_RejectsOwnedField(t *testing.T) {
	requireWebhookSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	class := &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-class", Namespace: "default"},
		Spec: computev1alpha1.FireboltEngineClassSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    computev1alpha1.EngineContainerName,
						Image:   "ghcr.io/firebolt-db/engine:dev",
						Command: []string{"/bin/sh"},
					}},
				},
			},
		},
	}
	err := suite.cli.Create(ctx, class)
	if err == nil {
		_ = suite.cli.Delete(ctx, class)
		t.Fatal("Create: expected admission rejection for operator-owned command, got nil")
	}
	if !strings.Contains(err.Error(), "spec.template.spec.containers[0].command") {
		t.Errorf("Create error %q does not surface the offending field path", err.Error())
	}
}

// TestWebhook_FireboltEngineClass_RefusesDeleteWhileBound verifies the
// delete-time gate over the wire: a DELETE on a FireboltEngineClass that
// has at least one FireboltEngine referencing it via
// spec.engineClassRef must be refused with the bound-count message.
// The validator does a live List rather than reading
// status.boundEngines so it works without the FireboltEngineClass
// controller running.
func TestWebhook_FireboltEngineClass_RefusesDeleteWhileBound(t *testing.T) {
	requireWebhookSuite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	class := &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "bound-class", Namespace: "default"},
		Spec: computev1alpha1.FireboltEngineClassSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  computev1alpha1.EngineContainerName,
						Image: "ghcr.io/firebolt-db/engine:dev",
					}},
				},
			},
		},
	}
	if err := suite.cli.Create(ctx, class); err != nil {
		t.Fatalf("Create class: %v", err)
	}

	// Engine Create is a precondition of the deletion-guard test: the
	// validator counts FireboltEngines that reference the class, so we
	// need at least one binding. The validator looks the class up
	// synchronously in the same apiserver we just wrote to, so the
	// Get is strongly consistent and will find it. instanceRef is not
	// validated (only engineClassRef + resources are), so the
	// non-existent "any-instance" is fine. Any failure here is a real
	// regression in the engine validator or a transient apiserver
	// problem — neither should be masked as "unrelated."
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: "binder", Namespace: "default"},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef:    "any-instance",
			Replicas:       1,
			EngineClassRef: utilptr.To(class.Name),
		},
	}
	if err := suite.cli.Create(ctx, engine); err != nil {
		_ = suite.cli.Delete(ctx, class)
		t.Fatalf("Create engine (binding required for the deletion-guard assertion): %v", err)
	}
	t.Cleanup(func() {
		_ = suite.cli.Delete(context.Background(), engine)
		// Wait for the engine delete to land so the eventual class delete
		// at suite teardown isn't held by a stale binding.
		_ = waitForNotFound(context.Background(), suite.cli,
			client.ObjectKey{Name: engine.Name, Namespace: engine.Namespace},
			&computev1alpha1.FireboltEngine{}, 5*time.Second)
		_ = suite.cli.Delete(context.Background(), class)
	})

	err := suite.cli.Delete(ctx, class)
	if err == nil {
		t.Fatal("Delete class: expected admission rejection while engine binds, got nil")
	}
	if !strings.Contains(err.Error(), "FireboltEngine") {
		t.Errorf("Delete error %q does not mention the bound engine(s)", err.Error())
	}
}

// waitForNotFound polls Get until the named object returns NotFound
// or the deadline expires. Used in test cleanup so subsequent deletes
// don't race against the deletion-guard validator.
func waitForNotFound(ctx context.Context, cli client.Client, key client.ObjectKey, obj client.Object, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := cli.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s not gone after %s", key, timeout)
}
