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
	"time"

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

// TestVerifySigningKeyProvenance_RejectsForgedAnnotation covers what the
// annotation alone cannot. cert-manager's certificate-name annotation is
// client-settable and the API server stores it verbatim, so an attacker who
// plants a Secret sets it themselves and verifyCertManagerIssued passes.
// creationTimestamp is the ordering the API server owns: it overwrites any
// client-supplied value, and cert-manager cannot write a target Secret before
// its Certificate exists.
func TestVerifySigningKeyProvenance_RejectsForgedAnnotation(t *testing.T) {
	const certName = "inst-auth-signing"
	certCreated := metav1.NewTime(time.Unix(2_000_000, 0))

	cert := &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "ns-1", CreationTimestamp: certCreated},
	}
	certPEM := mustGenSigningCertPEM()
	secretAt := func(sec int64) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:              certName,
				Namespace:         "ns-1",
				CreationTimestamp: metav1.NewTime(time.Unix(sec, 0)),
				// The attacker stamps this themselves; it proves nothing.
				Annotations: map[string]string{certmanagerv1.CertificateNameKey: certName},
			},
			Data: map[string][]byte{
				corev1.TLSPrivateKeyKey: []byte("key"),
				corev1.TLSCertKey:       certPEM,
			},
		}
	}

	planted := secretAt(1_999_999)
	if err := verifySigningKeyProvenance(planted, cert, nil); err == nil {
		t.Fatal("a Secret that predates its Certificate was accepted despite a correct annotation")
	}

	if err := verifySigningKeyProvenance(secretAt(2_000_001), cert, nil); err != nil {
		t.Fatalf("cert-manager's own output was rejected: %v", err)
	}
	// Equal seconds must pass: creationTimestamp is second-granular and the
	// honest path collides in that window.
	if err := verifySigningKeyProvenance(secretAt(2_000_000), cert, nil); err != nil {
		t.Fatalf("same-second issuance was rejected: %v", err)
	}

	// Recovery: a Certificate deleted and recreated leaves a Secret that legitimately
	// predates it. Allowed only for a key already witnessed in status.
	witnessed, err := signingKeyPublicKeyFingerprint(certPEM)
	if err != nil {
		t.Fatalf("fingerprinting the fixture cert: %v", err)
	}
	if err := verifySigningKeyProvenance(planted, cert, &witnessed); err != nil {
		t.Fatalf("a previously witnessed key was rejected on Certificate recreate: %v", err)
	}
	other := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := verifySigningKeyProvenance(planted, cert, &other); err == nil {
		t.Fatal("an unwitnessed key was accepted through the recovery path")
	}
}

// TestIsGeneratedEngineTLSSecretName_MatchesControllerNaming binds the shape
// match admission uses to the names the controller actually mints. The two live
// in different packages (admission cannot import the controller's suffix
// constants), so a rename on either side has to fail here rather than silently
// unprotect every engine serving key.
func TestIsGeneratedEngineTLSSecretName_MatchesControllerNaming(t *testing.T) {
	for _, gen := range []int{1, 2, 17, 1234} {
		name := genResourceName("engine-a", gen, SuffixEngineTLS)
		if !computev1alpha1.IsGeneratedEngineTLSSecretName(name) {
			t.Errorf("admission does not recognize %q as an engine serving-cert Secret", name)
		}
	}
	for _, name := range []string{
		"inst" + SuffixEngineTLS, // instance-wide anchor, covered by the exact set
		"my-own-engine-tls",      // unrelated user Secret
		"engine-a-g1-gateway-tls",
		"",
	} {
		if computev1alpha1.IsGeneratedEngineTLSSecretName(name) {
			t.Errorf("admission wrongly claims %q is an engine serving-cert Secret", name)
		}
	}
}

// TestSigningSecretProtectedBeforeStatusExists covers the first-apply window.
// The Instance reconciler validates pod templates before it provisions the
// signing key, so on a fresh Instance status.auth is nil and a name-only screen
// admits a template aliasing the key the operator is about to mint. Every gate
// has to protect the name from the start, not from the moment status catches up.
func TestSigningSecretProtectedBeforeStatusExists(t *testing.T) {
	fresh := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	}
	if fresh.Status.Auth != nil {
		t.Fatal("fixture is meant to have no auth status")
	}

	bootstrap := signingCertificateName(fresh.Name, AuthSigningKeyID)
	rotated := signingCertificateName(fresh.Name, signingKeyID(7))

	for _, name := range []string{bootstrap, rotated} {
		if !instanceProtectedSecret(fresh)(name) {
			t.Errorf("instance template gate leaves %q unprotected before status exists", name)
		}
		if !engineProtectedSecret(InstanceInfo{InstanceName: fresh.Name})(name) {
			t.Errorf("engine template gate leaves %q unprotected before status exists", name)
		}
		if errs := computev1alpha1.ValidateTLS(&computev1alpha1.FireboltInstance{
			ObjectMeta: fresh.ObjectMeta,
			Spec: computev1alpha1.FireboltInstanceSpec{TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled:   true,
					SecretRef: &corev1.LocalObjectReference{Name: name},
				},
			}},
		}); len(errs) == 0 {
			t.Errorf("spec.tls.gateway.secretRef accepted %q before status existed", name)
		}
	}

	// Another Instance's signing key is not this Instance's business either, but
	// it must not be swept in by this predicate — it is covered where that
	// Instance's own name is known.
	if instanceProtectedSecret(fresh)("other-auth-signing") {
		t.Error("the prefix match reaches beyond this Instance's own name")
	}
	if instanceProtectedSecret(fresh)("instx-auth-signing") {
		t.Error("the prefix match fired on a near-miss Instance name")
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
	labeled := func(annotations map[string]string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:        "looks-legit",
			Annotations: annotations,
		}}
	}
	if engineOwnedSecret(labeled(nil), "eng") {
		t.Error("a Secret carrying only the engine label must not be deleted")
	}
	if engineOwnedSecret(labeled(map[string]string{
		certmanagerv1.CertificateNameKey: "other-eng-g1-engine-tls",
	}), "eng") {
		t.Error("another engine's cert-manager Secret must not be deleted by this engine")
	}
	if !engineOwnedSecret(labeled(map[string]string{
		certmanagerv1.CertificateNameKey: "eng" + SuffixGen + "1" + SuffixEngineTLS,
	}), "eng") {
		t.Error("this engine's own per-generation TLS Secret should be swept")
	}
}
