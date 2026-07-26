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

package controller

import (
	"strings"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestVerifyCertManagerIssued_RejectsPrePlantedSecret is the regression test for
// the adoption hole: cert-manager's rotationPolicy reuses a private key that
// already exists in the target Secret, so a Secret created by anyone with
// namespace `create secrets` — before the operator ever applies its Certificate —
// would otherwise become the Instance's JWT signing key.
func TestVerifyCertManagerIssued_RejectsPrePlantedSecret(t *testing.T) {
	const certName = "inst-auth-signing"

	planted := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "ns-1"},
		Data: map[string][]byte{
			corev1.TLSPrivateKeyKey: []byte("attacker-chosen-key"),
			corev1.TLSCertKey:       []byte("attacker-chosen-cert"),
		},
	}
	if err := verifyCertManagerIssued(planted, certName); err == nil {
		t.Fatal("a Secret with no cert-manager annotations was accepted as signing key material")
	}

	// A Secret cert-manager wrote for a DIFFERENT Certificate that happens to
	// occupy this name is refused too.
	adopted := planted.DeepCopy()
	adopted.Annotations = map[string]string{certmanagerv1.CertificateNameKey: "some-other-cert"}
	if err := verifyCertManagerIssued(adopted, certName); err == nil {
		t.Fatal("a Secret issued for another Certificate was accepted")
	}

	// The genuine article passes.
	genuine := planted.DeepCopy()
	genuine.Annotations = map[string]string{certmanagerv1.CertificateNameKey: certName}
	if err := verifyCertManagerIssued(genuine, certName); err != nil {
		t.Fatalf("cert-manager's own output was rejected: %v", err)
	}
}

// TestInstanceProtectedSecretIsInstanceWide pins the cross-component fix: the
// predicate every template gate resolves through must cover the whole Instance,
// not just the Secrets one component's pod mounts.
func TestInstanceProtectedSecretIsInstanceWide(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Auth: &computev1alpha1.AuthSpec{
				Enabled: true,
				Local: &computev1alpha1.LocalAuthSpec{
					Admin: computev1alpha1.AdminSpec{
						Password: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: "admin-pw"},
							Key:                  "password",
						},
					},
				},
			},
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled:           true,
					ClientCASecretRef: &corev1.LocalObjectReference{Name: "client-ca-secret"},
				},
			},
		},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{SigningKeys: []computev1alpha1.SigningKeyStatus{
				{ID: "signing-1", SecretName: "inst-auth-signing"},
			}},
			EngineTLS:  &computev1alpha1.EngineTLSStatus{SecretName: "inst-engine-tls"},
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "inst-gateway-tls"},
		},
	}

	isProtected := instanceProtectedSecret(inst)

	// Every one of these was reachable from at least one template before the fix,
	// because each component was screened only against its own mounted set.
	for _, name := range []string{
		"admin-pw",          // gateway/metadata templates could mount this
		"inst-auth-signing", // gateway/metadata templates could mount this
		"inst-gateway-tls",  // engine/metadata templates could mount this
		"inst-engine-tls",   // gateway/metadata templates could mount this
		"client-ca-secret",  // engine/metadata templates could mount this
		"inst-engine-ca-bundle",
		"eng-g4-engine-tls", // any engine's per-generation serving key
		"other-eng-g9-engine-tls",
	} {
		if !isProtected(name) {
			t.Errorf("Secret %q must be protected on every template", name)
		}
	}

	for _, name := range []string{"", "my-own-secret", "inst-config"} {
		if isProtected(name) {
			t.Errorf("Secret %q must not be protected", name)
		}
	}
}

// TestBuildSigningCertificate_PinsUsagesAndBoundedLifetime pins two properties of
// the signing keypair: its certificate authenticates nothing (so it must not pick
// up serverAuth/clientAuth via cert-manager's defaults, which would make the key
// mounted into every engine a usable TLS client credential), and its lifetime is
// bounded rather than effectively infinite.
func TestBuildSigningCertificate_PinsUsagesAndBoundedLifetime(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: validAuthSpecForController()},
	}
	cert := buildSigningCertificate(inst, AuthSigningKeyID)

	if len(cert.Spec.Usages) != 1 || cert.Spec.Usages[0] != certmanagerv1.UsageDigitalSignature {
		t.Errorf("Usages = %v, want exactly [digital signature]", cert.Spec.Usages)
	}
	for _, forbidden := range []certmanagerv1.KeyUsage{
		certmanagerv1.UsageServerAuth, certmanagerv1.UsageClientAuth,
	} {
		for _, got := range cert.Spec.Usages {
			if got == forbidden {
				t.Errorf("Usages contains %q; the signing keypair must never be a TLS credential", forbidden)
			}
		}
	}
	if cert.Spec.Duration == nil || cert.Spec.Duration.Duration != DefaultCertDurationSigning {
		t.Errorf("Duration = %v, want the bounded default %v", cert.Spec.Duration, DefaultCertDurationSigning)
	}
	// The signing key must NOT rotate on cert-manager's schedule — only through
	// the operator's coordinated two-phase rotation.
	if cert.Spec.PrivateKey.RotationPolicy != certmanagerv1.RotationPolicyNever {
		t.Errorf("RotationPolicy = %q, want Never (rotation is operator-coordinated)",
			cert.Spec.PrivateKey.RotationPolicy)
	}
}

// TestEngineTLSAnchorIsNotANamespaceWildcard pins that the anchor certificate —
// whose key is mounted nowhere and which exists only to surface the issuer's
// ca.crt — cannot impersonate every service in the namespace.
func TestEngineTLSAnchorIsNotANamespaceWildcard(t *testing.T) {
	got := engineTLSAnchorDNSName("inst", "ns-1")
	if strings.Contains(got, "*") {
		t.Errorf("anchor SAN %q is a wildcard; its key would be a namespace-wide impersonation credential", got)
	}
	if !strings.HasSuffix(got, ".ns-1.svc.cluster.local") {
		t.Errorf("anchor SAN %q should still be a syntactically valid in-namespace name", got)
	}
}

// TestEngineOwnedSecret pins that the engine's deletion sweep needs proof of
// provenance, not just a label anyone in the namespace can stamp.
func TestEngineOwnedSecret(t *testing.T) {
	labelled := func(annotations map[string]string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:        "looks-legit",
			Annotations: annotations,
		}}
	}
	if engineOwnedSecret(labelled(nil), "eng") {
		t.Error("a Secret carrying only the engine label must not be deleted")
	}
	if engineOwnedSecret(labelled(map[string]string{
		certmanagerv1.CertificateNameKey: "other-eng-g1-engine-tls",
	}), "eng") {
		t.Error("another engine's cert-manager Secret must not be deleted by this engine")
	}
	if !engineOwnedSecret(labelled(map[string]string{
		certmanagerv1.CertificateNameKey: "eng" + SuffixGen + "1" + SuffixEngineTLS,
	}), "eng") {
		t.Error("this engine's own per-generation TLS Secret should be swept")
	}
}
