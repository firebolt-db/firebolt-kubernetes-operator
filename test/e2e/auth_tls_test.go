//go:build e2e

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
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
	"github.com/firebolt-db/firebolt-kubernetes-operator/test/testhelpers"
)

// This suite is the first automated end-to-end exercise of the FB-896 auth/TLS
// feature (signing keys, engine/gateway TLS). It relies on cert-manager and the
// CA ClusterIssuer installed by SynchronizedBeforeSuite (see e2e_suite_test.go).

// caClusterIssuerRef points every provisioned Certificate at the suite's CA
// ClusterIssuer (a CA-backed issuer — required so engine certs carry ca.crt).
func caClusterIssuerRef() computev1alpha1.CertManagerIssuerRef {
	return computev1alpha1.CertManagerIssuerRef{
		Name: testhelpers.E2ECAClusterIssuerName,
		Kind: "ClusterIssuer",
	}
}

// createAdminSecret creates the admin-password Secret an auth-enabled instance
// references. No suite helper exists for this, so it writes directly.
func createAdminSecret(ctx context.Context, name, key, password string) error {
	_, err := k8sClient.CoreV1().Secrets(testNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		StringData: map[string]string{key: password},
	}, metav1.CreateOptions{})
	return err
}

// waitForInstanceCondition polls until the named FireboltInstance status
// condition reaches want, or the timeout elapses. Needed because no suite helper
// gates on a specific instance condition, and AuthReady in particular is NOT
// rolled into the Ready phase.
func waitForInstanceCondition(ctx context.Context, name, condType string, want metav1.ConditionStatus) error {
	cl, err := getCRDClient()
	if err != nil {
		return err
	}
	key := types.NamespacedName{Name: name, Namespace: testNamespace}
	deadline := time.Now().Add(instanceReadyTimeout)
	last := "<absent>"
	for time.Now().Before(deadline) {
		inst := &computev1alpha1.FireboltInstance{}
		if getErr := cl.Get(ctx, key, inst); getErr != nil {
			return getErr
		}
		if c := apimeta.FindStatusCondition(inst.Status.Conditions, condType); c != nil {
			last = fmt.Sprintf("%s/%s", c.Status, c.Reason)
			if c.Status == want {
				return nil
			}
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("condition %s did not reach %s within %s (last: %s)", condType, want, instanceReadyTimeout, last)
}

// kubectlNames returns `kubectl get <kind> -l <selector> -o name` output for the
// test namespace, trimmed — empty string means no matching objects. Used to
// assert cert-manager Certificate / Secret cleanup without adding those types to
// a typed client here.
func kubectlNames(ctx context.Context, kind, selector string) (string, error) {
	args := kubectlArgs("get", kind, "-n", testNamespace, "-l", selector, "-o", "name")
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl get %s: %s", kind, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// engineTrustCAPEM reads the CA certificate the engine's serving cert chains to,
// from the suite CA secret. Written into the client pod so curl can verify the
// engine's TLS listener against it.
func engineTrustCAPEM(ctx context.Context) ([]byte, error) {
	s, err := k8sClient.CoreV1().Secrets("cert-manager").Get(ctx, "e2e-ca-tls", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	pem := s.Data[corev1.TLSCertKey]
	if len(pem) == 0 {
		return nil, fmt.Errorf("secret cert-manager/e2e-ca-tls has no %s", corev1.TLSCertKey)
	}
	return pem, nil
}

// writeFileInPod pipes content to a file inside a pod via `kubectl exec -i`.
func writeFileInPod(ctx context.Context, pod, path string, content []byte) error {
	args := kubectlArgs("exec", "-i", pod, "-n", testNamespace, "--", "sh", "-c", "cat > "+path)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = bytes.NewReader(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("writing %s in pod %s: %s", path, pod, strings.TrimSpace(string(out)))
	}
	return nil
}

// curlTLS runs `curl` against url from inside pod and returns nil iff curl exits
// 0 — i.e. the TLS handshake AND certificate verification succeeded. It uses
// -sS (not -f), so an HTTP-level error (e.g. 401 for a missing auth token) after
// a good handshake still counts as success: this probes the transport, not the
// query. When caPath != "" the cert is verified against that CA (a real check of
// the SAN + chain); when "" the system trust store is used (expected to fail for
// the suite's private CA).
func curlTLS(ctx context.Context, pod, url, caPath string) error {
	curlArgs := []string{"-sS", "--connect-timeout", "5", "--max-time", "15", "-o", "/dev/null"}
	if caPath != "" {
		curlArgs = append(curlArgs, "--cacert", caPath)
	}
	curlArgs = append(curlArgs, url)
	args := kubectlArgs("exec", pod, "-n", testNamespace, "--", "curl")
	args = append(args, curlArgs...)
	if out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("curl %s: %s", url, strings.TrimSpace(string(out)))
	}
	return nil
}

var _ = Describe("FireboltInstance auth + TLS", func() {
	Describe("engine and gateway provision and serve TLS with auth enabled", Ordered, func() {
		const (
			instanceName    = "inst-authtls"
			engineName      = "authtls-engine"
			clientPod       = "client-authtls"
			adminSecretName = "inst-authtls-admin"
			adminSecretKey  = "password"
			caPodPath       = "/tmp/e2e-ca.crt"
		)
		var lc *TestInstanceLifecycle

		instName := instanceName
		engName := engineName
		RegisterFailedSpecPodLogDump(&instName, &engName)

		// engineHTTPSURL targets the engine's TLS listener on the routing Service
		// FQDN — which is one of the per-generation cert's SANs — so a verifying
		// curl proves both the SAN (FB-896 #1) and the CA chain / bundle (#2).
		engineHTTPSURL := fmt.Sprintf("https://%s-service.%s.svc.cluster.local:%d/",
			engineName, testNamespace, controller.EngineHTTPQueryPort)

		BeforeAll(func() {
			By("creating the admin-password Secret")
			Expect(createAdminSecret(ctx, adminSecretName, adminSecretKey, "s3cret-e2e")).To(Succeed())

			By("creating an auth+TLS FireboltInstance and starting its operators")
			var err error
			lc, err = SetupTestInstanceWithMutate(ctx, instanceName, func(inst *computev1alpha1.FireboltInstance) {
				inst.Spec.Auth = &computev1alpha1.AuthSpec{
					Enabled: true,
					Local: &computev1alpha1.LocalAuthSpec{
						Admin: computev1alpha1.AdminSpec{
							Password: corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: adminSecretName},
								Key:                  adminSecretKey,
							},
						},
						SigningAlgorithm: "ES384",
						SigningKeys: &computev1alpha1.SigningKeyPolicy{
							CertManager: computev1alpha1.CertManagerSpec{
								IssuerRef: caClusterIssuerRef(),
								Algorithm: "ECDSA",
								Size:      384,
							},
						},
					},
				}
				inst.Spec.TLS = &computev1alpha1.TLSSpec{
					Engine: &computev1alpha1.TLSListenerSpec{
						Enabled:     true,
						CertManager: &computev1alpha1.CertManagerSpec{IssuerRef: caClusterIssuerRef(), Algorithm: "ECDSA", Size: 384},
					},
					Gateway: &computev1alpha1.TLSListenerSpec{
						Enabled:     true,
						CertManager: &computev1alpha1.CertManagerSpec{IssuerRef: caClusterIssuerRef(), Algorithm: "ECDSA", Size: 384},
					},
				}
			})
			Expect(err).NotTo(HaveOccurred())

			By("creating a client pod and a TLS-serving engine")
			Expect(CreateClientPod(ctx, clientPod)).To(Succeed())
			Expect(CreateEngine(ctx, instanceName, engineName, 1)).To(Succeed())
			Expect(WaitForEngineReady(ctx, engineName, 1, clusterReadyTimeout)).To(Succeed())
			Expect(WaitForEngineStable(ctx, engineName, clusterTransitionTimeout)).To(Succeed())
		})

		AfterAll(func() {
			DeleteClientPod(ctx, clientPod)
			// Best-effort: the #4 spec may have already deleted the engine.
			if err := DeleteEngine(ctx, engineName); err != nil {
				fmt.Fprintf(GinkgoWriter, "AfterAll: DeleteEngine(%s): %v\n", engineName, err)
			}
			TeardownTestInstance(ctx, lc)
			_ = k8sClient.CoreV1().Secrets(testNamespace).Delete(ctx, adminSecretName, metav1.DeleteOptions{})
		})

		It("provisions auth and TLS (AuthReady/EngineTLSReady/GatewayTLSReady all True)", func() {
			for _, cond := range []string{
				computev1alpha1.InstanceConditionAuthReady,
				computev1alpha1.InstanceConditionEngineTLSReady,
				computev1alpha1.InstanceConditionGatewayTLSReady,
			} {
				By("waiting for " + cond)
				Expect(waitForInstanceCondition(ctx, instanceName, cond, metav1.ConditionTrue)).To(Succeed())
			}
		})

		It("engine serves TLS with a valid per-generation certificate (FB-896 #1/#2)", func() {
			By("installing the CA into the client pod")
			ca, err := engineTrustCAPEM(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(writeFileInPod(ctx, clientPod, caPodPath, ca)).To(Succeed())

			By("curl over HTTPS verifying against the CA succeeds (proves SAN + CA chain)")
			Expect(curlTLS(ctx, clientPod, engineHTTPSURL, caPodPath)).To(Succeed())

			By("the same curl WITHOUT the CA fails (the cert is not system-trusted)")
			Expect(curlTLS(ctx, clientPod, engineHTTPSURL, "")).To(HaveOccurred())
		})

		It("labels the per-generation engine TLS Secret (FB-896 #4 cleanup premise)", func() {
			// Positive control for the deletion assertion below. The Certificate
			// is owner-referenced to the engine, so k8s GC removes it on delete
			// regardless of the round-4 sweep — the Secret is the one round-4 had
			// to sweep explicitly (cert-manager does not own-ref it), and the
			// sweep selects it by firebolt.io/engine. That selector only works if
			// cert-manager propagated SecretTemplate.Labels onto the Secret;
			// assert that here (engine up) so the later BeEmpty() has real teeth
			// instead of passing vacuously on an unlabeled, leaked Secret.
			sel := controller.LabelEngine + "=" + engineName
			Eventually(func() (string, error) {
				return kubectlNames(ctx, "secrets", sel)
			}).WithTimeout(clusterTransitionTimeout).WithPolling(pollInterval).ShouldNot(BeEmpty(),
				"per-generation engine TLS Secret is not labeled %s — the #4 cleanup sweep would miss it", controller.LabelEngine)
		})

		It("rejects an in-place signingAlgorithm change (FB-896 #1 — regression test for round-4 immutability)", func() {
			cl, err := getCRDClient()
			Expect(err).NotTo(HaveOccurred())
			key := types.NamespacedName{Name: instanceName, Namespace: testNamespace}
			// Retry via Eventually: the in-process operator updates status
			// concurrently, so a single Get+Update can hit a resourceVersion
			// conflict instead of the immutability rejection. Re-getting a fresh
			// object and re-applying converges on the terminal (non-conflict)
			// admission error. A compat-valid target (ES256 pairs with a P-256
			// key) isolates the immutability rule from the alg/size compat check.
			Eventually(func(g Gomega) {
				inst := &computev1alpha1.FireboltInstance{}
				g.Expect(cl.Get(ctx, key, inst)).To(Succeed())
				inst.Spec.Auth.Local.SigningAlgorithm = "ES256"
				inst.Spec.Auth.Local.SigningKeys.CertManager.Size = 256
				updErr := cl.Update(ctx, inst)
				g.Expect(updErr).To(HaveOccurred())
				g.Expect(updErr.Error()).To(ContainSubstring("immutable"))
			}).WithTimeout(30 * time.Second).WithPolling(pollInterval).Should(Succeed())
		})

		It("reclaims per-generation engine Certificates and Secrets on engine deletion (FB-896 #4)", func() {
			Expect(DeleteEngine(ctx, engineName)).To(Succeed())
			Expect(WaitForResourcesDeleted(ctx, engineName, resourceCleanupTimeout)).To(Succeed())

			sel := controller.LabelEngine + "=" + engineName
			Eventually(func() (string, error) {
				return kubectlNames(ctx, "certificates.cert-manager.io", sel)
			}).WithTimeout(resourceCleanupTimeout).WithPolling(pollInterval).Should(BeEmpty(),
				"per-generation engine Certificates leaked after engine deletion")
			Eventually(func() (string, error) {
				return kubectlNames(ctx, "secrets", sel)
			}).WithTimeout(resourceCleanupTimeout).WithPolling(pollInterval).Should(BeEmpty(),
				"per-generation engine TLS Secrets leaked after engine deletion")
		})
	})
})
