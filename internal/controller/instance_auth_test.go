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
	"context"
	"strings"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// authTestScheme mirrors classRefTestScheme (engine_classref_test.go) but
// additionally registers cert-manager, since instance_auth.go's fake-client
// tests reference *computev1alpha1.FireboltInstance without ever needing to
// construct a Certificate via the fake client (see the doc comment on
// ensureSigningCertificate for why that path is left to e2e) — registered
// anyway so SetControllerReference-adjacent helpers never hit a scheme gap.
func authTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}
	if err := certmanagerv1.AddToScheme(s); err != nil {
		t.Fatalf("certmanagerv1.AddToScheme: %v", err)
	}
	return s
}

// adminSecretNameForController is the admin password Secret name used by
// every validAuthSpecForController fixture below, and the name the
// TestCheckAdminPasswordSecret / TestEnsureAuth_MissingAdminSecretFailsBeforeTouchingSigningKeys
// sub-tests seed (or deliberately omit) in the fake client.
const adminSecretNameForController = "admin-creds"

// validAuthSpecForController returns an AuthSpec that satisfies
// ValidateAuth on its own (mirrors validAdminSpec/validSigningKeys in
// api/v1alpha1/fireboltinstance_webhook_test.go, duplicated here because
// those helpers are unexported to their package), so individual test
// cases below only need to override the one thing under test.
func validAuthSpecForController() *computev1alpha1.AuthSpec {
	return &computev1alpha1.AuthSpec{
		Enabled: true,
		Local: &computev1alpha1.LocalAuthSpec{
			Admin: computev1alpha1.AdminSpec{
				Password: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: adminSecretNameForController},
					Key:                  "password",
				},
			},
			SigningKeys: &computev1alpha1.SigningKeyPolicy{
				CertManager: computev1alpha1.CertManagerSpec{
					IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
				},
			},
		},
	}
}

func TestBuildSigningCertificate_DefaultsToRSAPKCS8NeverRotate(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: validAuthSpecForController()},
	}

	cert := buildSigningCertificate(instance)

	wantName := "inst" + SuffixAuthSigning
	if cert.Name != wantName {
		t.Errorf("Name = %q, want %q", cert.Name, wantName)
	}
	if cert.Namespace != "ns-1" {
		t.Errorf("Namespace = %q, want ns-1", cert.Namespace)
	}
	if cert.Spec.SecretName != wantName {
		t.Errorf("Spec.SecretName = %q, want %q (Certificate and Secret share a name)", cert.Spec.SecretName, wantName)
	}
	if cert.Spec.CommonName == "" {
		t.Error("Spec.CommonName is empty; packdb requires a non-empty subject/SAN")
	}
	if len(cert.Spec.CommonName) > 64 {
		t.Errorf("Spec.CommonName is %d chars, want <=64 (RFC 5280 ub-common-name; cert-manager warns "+
			"longer values can produce an invalid CSR)", len(cert.Spec.CommonName))
	}

	pk := cert.Spec.PrivateKey
	if pk == nil {
		t.Fatal("Spec.PrivateKey is nil")
	}
	if pk.Algorithm != certmanagerv1.RSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want RSA (CertManagerSpec.Algorithm was empty)", pk.Algorithm)
	}
	if pk.Encoding != certmanagerv1.PKCS8 {
		t.Errorf("Encoding = %q, want PKCS8 (verified compatible with packdb's PEM_read_bio_PrivateKey)", pk.Encoding)
	}
	if pk.RotationPolicy != certmanagerv1.RotationPolicyNever {
		t.Errorf("RotationPolicy = %q, want Never (packdb only reads signing keys at startup)", pk.RotationPolicy)
	}

	if cert.Spec.Duration == nil || cert.Spec.Duration.Duration != authSigningCertDuration {
		t.Errorf("Duration = %v, want %v (must be effectively-static so cert-manager never auto-renews)",
			cert.Spec.Duration, authSigningCertDuration)
	}

	if cert.Spec.IssuerRef.Name != "internal-ca" {
		t.Errorf("IssuerRef.Name = %q, want internal-ca", cert.Spec.IssuerRef.Name)
	}
	if cert.Spec.IssuerRef.Kind != "ClusterIssuer" {
		t.Errorf("IssuerRef.Kind = %q, want ClusterIssuer (default when unset)", cert.Spec.IssuerRef.Kind)
	}

	if cert.Labels[LabelInstance] != "inst" {
		t.Errorf("Labels[%s] = %q, want inst", LabelInstance, cert.Labels[LabelInstance])
	}
	if cert.Spec.SecretTemplate == nil || cert.Spec.SecretTemplate.Labels[LabelInstance] != "inst" {
		t.Errorf("SecretTemplate.Labels[%s] must carry LabelInstance so reconcileDelete's generic "+
			"Secret sweep cleans up the signing Secret on instance deletion", LabelInstance)
	}
}

// TestBuildSigningCertificate_CommonNameBoundedForLongNames pins down that
// CommonName never grows with the instance/namespace name: a realistic
// long name pair ("<instance>-auth-signing.<namespace>", derived the way
// an earlier version of this function computed it) exceeds RFC 5280's
// 64-character ub-common-name limit and produces an invalid CSR at
// issuance time — a failure invisible to every check except this one,
// since cert-manager only reports it asynchronously on the Certificate's
// status. See buildSigningCertificate's CommonName comment.
func TestBuildSigningCertificate_CommonNameBoundedForLongNames(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "firebolt-analytics-production-instance",
			Namespace: "prod-us-east-1-data-platform",
		},
		Spec: computev1alpha1.FireboltInstanceSpec{Auth: validAuthSpecForController()},
	}

	cert := buildSigningCertificate(instance)

	if len(cert.Spec.CommonName) > 64 {
		t.Fatalf("Spec.CommonName = %q (%d chars), want <=64", cert.Spec.CommonName, len(cert.Spec.CommonName))
	}
}

func TestBuildSigningCertificate_ECDSAAndExplicitIssuerKind(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Auth: &computev1alpha1.AuthSpec{
				Enabled: true,
				Local: &computev1alpha1.LocalAuthSpec{
					Admin:            computev1alpha1.AdminSpec{Password: corev1.SecretKeySelector{Key: "password"}},
					SigningAlgorithm: "ES256",
					SigningKeys: &computev1alpha1.SigningKeyPolicy{
						CertManager: computev1alpha1.CertManagerSpec{
							IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca", Kind: "Issuer"},
							Algorithm: "ECDSA",
							Size:      384,
						},
					},
				},
			},
		},
	}

	cert := buildSigningCertificate(instance)

	if cert.Spec.PrivateKey.Algorithm != certmanagerv1.ECDSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want ECDSA", cert.Spec.PrivateKey.Algorithm)
	}
	if cert.Spec.PrivateKey.Size != 384 {
		t.Errorf("Size = %d, want 384", cert.Spec.PrivateKey.Size)
	}
	if cert.Spec.IssuerRef.Kind != "Issuer" {
		t.Errorf("IssuerRef.Kind = %q, want Issuer (explicit namespaced issuer)", cert.Spec.IssuerRef.Kind)
	}
	// PKCS8/Never/Duration are algorithm-independent; spot-check PKCS8
	// stays pinned even on the ECDSA path.
	if cert.Spec.PrivateKey.Encoding != certmanagerv1.PKCS8 {
		t.Errorf("Encoding = %q, want PKCS8", cert.Spec.PrivateKey.Encoding)
	}
}

// TestAuthSigningVolumeName_MatchesReservedEngineVolumeName guards against
// AuthSigningKeyID and operatorauthority.go's reserved engine volume name
// silently diverging. api/v1alpha1 cannot import internal/controller (that
// would be a cycle), so operatorOwnedEngineVolumeNames reserves the Phase-1
// signing volume as a literal — EngineAuthSigningVolumeNamePrefix +
// "signing-1" — rather than computing it from AuthSigningKeyID. If
// AuthSigningKeyID ever changes without updating that literal, a user's pod
// template could declare a volumeMount with the real (now unreserved) auth
// signing volume name and collide with the operator's own mount undetected.
func TestAuthSigningVolumeName_MatchesReservedEngineVolumeName(t *testing.T) {
	got := authSigningVolumeName(AuthSigningKeyID)
	reserved := computev1alpha1.FireboltEngineClassPodTemplateRules.ReservedPrimaryVolumeMountNames
	for _, name := range reserved {
		if name == got {
			return
		}
	}
	t.Errorf("authSigningVolumeName(AuthSigningKeyID) = %q not present in reserved engine volume names %v; "+
		"update the literal in api/v1alpha1/operatorauthority.go's operatorOwnedEngineVolumeNames", got, reserved)
}

func TestCheckAdminPasswordSecret(t *testing.T) {
	sch := authTestScheme(t)
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: validAuthSpecForController()},
	}

	t.Run("missing secret is rejected", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		err := r.checkAdminPasswordSecret(context.Background(), instance)
		if err == nil {
			t.Fatal("expected error for missing admin secret, got nil")
		}
		if !strings.Contains(err.Error(), "admin-creds") {
			t.Errorf("error %q does not name the missing secret", err.Error())
		}
	})

	t.Run("secret present but missing key is rejected", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "admin-creds", Namespace: "ns-1"},
				Data:       map[string][]byte{"not-password": []byte("x")},
			},
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		err := r.checkAdminPasswordSecret(context.Background(), instance)
		if err == nil {
			t.Fatal("expected error for secret missing the configured key, got nil")
		}
	})

	t.Run("secret with populated key is accepted", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "admin-creds", Namespace: "ns-1"},
				Data:       map[string][]byte{"password": []byte("hunter2")},
			},
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		if err := r.checkAdminPasswordSecret(context.Background(), instance); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestEnsureAuth_NilOrDisabledClearsStatus(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	tests := []struct {
		name string
		auth *computev1alpha1.AuthSpec
	}{
		{name: "nil auth", auth: nil},
		{name: "explicitly disabled", auth: &computev1alpha1.AuthSpec{Enabled: false}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := &computev1alpha1.FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
				Spec:       computev1alpha1.FireboltInstanceSpec{Auth: tc.auth},
				Status: computev1alpha1.FireboltInstanceStatus{
					// Simulate auth having been enabled and provisioned in
					// a prior reconcile, then disabled: stale status must
					// be cleared, not left dangling for engines to read.
					Auth: &computev1alpha1.AuthStatus{
						SigningKeys: []computev1alpha1.SigningKeyStatus{{ID: "stale", SecretName: "stale-secret"}},
					},
				},
			}

			if err := r.ensureAuth(context.Background(), instance); err != nil {
				t.Fatalf("ensureAuth: unexpected error: %v", err)
			}
			if instance.Status.Auth != nil {
				t.Errorf("Status.Auth = %+v, want nil", instance.Status.Auth)
			}
			cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionAuthReady)
			if cond == nil {
				t.Fatal("AuthReady condition not set")
			}
			if cond.Status != metav1.ConditionTrue || cond.Reason != "Disabled" {
				t.Errorf("AuthReady = %s/%s, want True/Disabled", cond.Status, cond.Reason)
			}
		})
	}
}

// TestEnsureAuth_BypassedWebhookDoesNotPanic exercises the defense-in-depth
// re-validation: a CR with Enabled=true and Local=nil could only exist if
// the validating webhook was bypassed (ValidateAuth rejects it at
// admission), so this pins down that the controller reports the invalid
// spec instead of dereferencing instance.Spec.Auth.Local and panicking.
func TestEnsureAuth_BypassedWebhookDoesNotPanic(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			Auth: &computev1alpha1.AuthSpec{Enabled: true}, // Local is nil: invalid per ValidateAuth.
		},
	}

	err := r.ensureAuth(context.Background(), instance)
	if err == nil {
		t.Fatal("expected error for invalid auth spec, got nil")
	}

	cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionAuthReady)
	if cond == nil {
		t.Fatal("AuthReady condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != reasonAuthSpecInvalid {
		t.Errorf("AuthReady = %s/%s, want False/%s", cond.Status, cond.Reason, reasonAuthSpecInvalid)
	}
}

func TestEnsureAuth_MissingAdminSecretFailsBeforeTouchingSigningKeys(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: validAuthSpecForController()},
	}

	err := r.ensureAuth(context.Background(), instance)
	if err == nil {
		t.Fatal("expected error for missing admin secret, got nil")
	}

	cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionAuthReady)
	if cond == nil {
		t.Fatal("AuthReady condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "AdminSecretMissing" {
		t.Errorf("AuthReady = %s/%s, want False/AdminSecretMissing", cond.Status, cond.Reason)
	}
	// The admin-secret preflight must short-circuit before any signing-key
	// provisioning is attempted (that path additionally requires
	// cert-manager's CRDs to be installed against a real apiserver — see
	// ensureSigningCertificate's doc comment), so Status.Auth must stay nil.
	if instance.Status.Auth != nil {
		t.Errorf("Status.Auth = %+v, want nil: signing-key provisioning must not have been reached", instance.Status.Auth)
	}
}
