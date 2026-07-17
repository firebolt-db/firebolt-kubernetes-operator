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
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	cert := buildSigningCertificate(instance, AuthSigningKeyID)

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

	cert := buildSigningCertificate(instance, AuthSigningKeyID)

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

	cert := buildSigningCertificate(instance, AuthSigningKeyID)

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

// TestAuthSigningVolumeName_ReservedViaPrefix guards against
// authSigningVolumeName's naming scheme and
// EngineAuthSigningVolumeNamePrefix silently diverging. Rotation can
// mount a volume for any kid the operator has ever minted, so
// api/v1alpha1's isReservedVolumeMountName reserves all of them via a
// prefix check rather than an enumerated literal (api/v1alpha1 cannot
// import internal/controller to compute one, and no static enumeration
// of every possible kid could be complete anyway). This pins down that
// authSigningVolumeName's output always starts with that exact prefix,
// for both the first key and a hypothetical later-rotation key.
func TestAuthSigningVolumeName_ReservedViaPrefix(t *testing.T) {
	for _, kid := range []string{AuthSigningKeyID, "signing-42"} {
		got := authSigningVolumeName(kid)
		if !strings.HasPrefix(got, computev1alpha1.EngineAuthSigningVolumeNamePrefix) {
			t.Errorf("authSigningVolumeName(%q) = %q, want prefix %q",
				kid, got, computev1alpha1.EngineAuthSigningVolumeNamePrefix)
		}
	}
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

// --- Signing-key rotation: pure helper functions ---

func TestNormalizeSigningKeyPhases_BackfillsEmptyToActive(t *testing.T) {
	keys := []computev1alpha1.SigningKeyStatus{
		{ID: "signing-1", Phase: ""},
		{ID: "signing-2", Phase: computev1alpha1.SigningKeyValidationOnly},
	}
	normalizeSigningKeyPhases(keys)
	if keys[0].Phase != computev1alpha1.SigningKeyActive {
		t.Errorf("keys[0].Phase = %q, want Active (backfilled from empty, matching a pre-rotation Instance)", keys[0].Phase)
	}
	if keys[1].Phase != computev1alpha1.SigningKeyValidationOnly {
		t.Errorf("keys[1].Phase = %q, want unchanged ValidationOnly", keys[1].Phase)
	}
}

func TestActiveAndOtherSigningKey(t *testing.T) {
	keys := []computev1alpha1.SigningKeyStatus{
		{ID: "signing-2", Phase: computev1alpha1.SigningKeyValidationOnly},
		{ID: "signing-1", Phase: computev1alpha1.SigningKeyActive},
	}
	if active := activeSigningKey(keys); active == nil || active.ID != "signing-1" {
		t.Fatalf("activeSigningKey(keys) = %+v, want signing-1", active)
	}
	if other := otherSigningKey(keys); other == nil || other.ID != "signing-2" {
		t.Fatalf("otherSigningKey(keys) = %+v, want signing-2", other)
	}
	if activeSigningKey(nil) != nil {
		t.Error("activeSigningKey(nil) should be nil: a brand-new Instance has no key yet")
	}
	soleActive := []computev1alpha1.SigningKeyStatus{{ID: "signing-1", Phase: computev1alpha1.SigningKeyActive}}
	if otherSigningKey(soleActive) != nil {
		t.Error("otherSigningKey should be nil when only an Active key exists (no rotation in flight)")
	}
}

func TestSigningKeysForRender(t *testing.T) {
	tests := []struct {
		name string
		keys []computev1alpha1.SigningKeyStatus
		want []string
	}{
		{
			name: "active only",
			keys: []computev1alpha1.SigningKeyStatus{{ID: "signing-1", Phase: computev1alpha1.SigningKeyActive}},
			want: []string{"signing-1"},
		},
		{
			name: "active first regardless of slice order, validation-only included",
			keys: []computev1alpha1.SigningKeyStatus{
				{ID: "signing-2", Phase: computev1alpha1.SigningKeyValidationOnly},
				{ID: "signing-1", Phase: computev1alpha1.SigningKeyActive},
			},
			want: []string{"signing-1", "signing-2"},
		},
		{
			name: "removing key excluded from render",
			keys: []computev1alpha1.SigningKeyStatus{
				{ID: "signing-1", Phase: computev1alpha1.SigningKeyActive},
				{ID: "signing-2", Phase: computev1alpha1.SigningKeyRemoving},
			},
			want: []string{"signing-1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := signingKeysForRender(tc.keys)
			if len(got) != len(tc.want) {
				t.Fatalf("signingKeysForRender = %+v, want %d entries matching IDs %v", got, len(tc.want), tc.want)
			}
			for i, id := range tc.want {
				if got[i].ID != id {
					t.Errorf("got[%d].ID = %q, want %q", i, got[i].ID, id)
				}
			}
		})
	}
}

func TestPromoteSigningKey_FlipsPhasesAndSetsDemotedAt(t *testing.T) {
	active := &computev1alpha1.SigningKeyStatus{ID: "signing-1", Phase: computev1alpha1.SigningKeyActive}
	other := &computev1alpha1.SigningKeyStatus{ID: "signing-2", Phase: computev1alpha1.SigningKeyValidationOnly}

	promoteSigningKey(active, other)

	if other.Phase != computev1alpha1.SigningKeyActive {
		t.Errorf("other.Phase = %q, want Active", other.Phase)
	}
	if active.Phase != computev1alpha1.SigningKeyValidationOnly {
		t.Errorf("active.Phase = %q, want ValidationOnly", active.Phase)
	}
	if active.DemotedAt == nil {
		t.Error("active.DemotedAt not set after demotion")
	}
	if active.RetireEligibleAt != nil {
		t.Error("active.RetireEligibleAt must stay unset at promotion time: engines haven't rolled onto " +
			"the promoted config yet, so the retain window must not start counting until " +
			"stepSigningKeyRotation separately confirms that rollout")
	}
}

func TestRemoveSigningKey_DropsMatchingID(t *testing.T) {
	keys := []computev1alpha1.SigningKeyStatus{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := removeSigningKey(keys, "b")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("removeSigningKey(keys, \"b\") = %+v, want [a c]", got)
	}
}

func TestAuthStatusGeneration(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{}
	if g := authStatusGeneration(instance); g != 0 {
		t.Errorf("authStatusGeneration(nil Status.Auth) = %d, want 0", g)
	}
	instance.Status.Auth = &computev1alpha1.AuthStatus{SigningKeyGeneration: 3}
	if g := authStatusGeneration(instance); g != 3 {
		t.Errorf("authStatusGeneration = %d, want 3", g)
	}
}

func TestSigningKeyID_GenerationOneMatchesAuthSigningKeyID(t *testing.T) {
	if signingKeyID(1) != AuthSigningKeyID {
		t.Errorf("signingKeyID(1) = %q, want AuthSigningKeyID (%q)", signingKeyID(1), AuthSigningKeyID)
	}
	if signingKeyID(2) == signingKeyID(1) {
		t.Error("signingKeyID must produce a distinct ID per generation")
	}
}

func TestSigningCertificateName_Generation1KeepsLegacyName(t *testing.T) {
	legacy := "inst" + SuffixAuthSigning
	if got := signingCertificateName("inst", AuthSigningKeyID); got != legacy {
		t.Errorf("signingCertificateName(inst, AuthSigningKeyID) = %q, want %q: an Instance that never rotates "+
			"must keep its pre-rotation Certificate name across an operator upgrade", got, legacy)
	}
	if got, want := signingCertificateName("inst", "signing-2"), legacy+"-signing-2"; got != want {
		t.Errorf("signingCertificateName(inst, signing-2) = %q, want %q", got, want)
	}
}

// --- Signing-key rotation: enginesConvergedOn ---

// engineWithHash builds a minimal FireboltEngine carrying the given
// ObservedAuthHash and instance binding, for tests that need to simulate an
// engine at a particular point in its own rollout. Engines bind to their
// instance via spec.instanceRef (never a label), so that is what
// enginesConvergedOn filters on.
func engineWithHash(name, instanceRef, hash string) *computev1alpha1.FireboltEngine {
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ns-1",
		},
		Spec:   computev1alpha1.FireboltEngineSpec{InstanceRef: instanceRef},
		Status: computev1alpha1.FireboltEngineStatus{ObservedAuthHash: hash},
	}
}

// adminSecretForConvergence returns the admin password Secret the auth spec
// references. enginesConvergedOn now reads its ResourceVersion and folds it
// into the expected authHash (FB-896 #4), so every subtest must seed it.
func adminSecretForConvergence() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: adminSecretNameForController, Namespace: "ns-1"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	}
}

func TestEnginesConvergedOn(t *testing.T) {
	sch := authTestScheme(t)
	auth := validAuthSpecForController()
	keys := []computev1alpha1.SigningKeyStatus{
		{ID: "signing-1", SecretName: "inst" + SuffixAuthSigning, Phase: computev1alpha1.SigningKeyActive},
	}
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: auth},
	}

	// The expected hash must include the admin Secret's ResourceVersion
	// exactly as enginesConvergedOn reads it. A probe client (its own tracker,
	// admin Secret added first, same as each subtest below) yields the RV the
	// fake client will assign, so the precomputed engine ObservedAuthHash
	// matches what enginesConvergedOn computes.
	probe := fake.NewClientBuilder().WithScheme(sch).WithObjects(adminSecretForConvergence()).Build()
	var probeSecret corev1.Secret
	if err := probe.Get(context.Background(),
		client.ObjectKey{Namespace: "ns-1", Name: adminSecretNameForController}, &probeSecret); err != nil {
		t.Fatalf("probing admin secret RV: %v", err)
	}
	expected := authHash(&ResolvedAuthInfo{
		Spec:               auth,
		SigningKeys:        signingKeysForRender(keys),
		AdminSecretVersion: probeSecret.ResourceVersion,
	})
	if expected == "" {
		t.Fatal("expected hash is empty; test fixture is wrong")
	}

	t.Run("no engines is vacuously converged", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(adminSecretForConvergence()).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		ok, err := r.enginesConvergedOn(context.Background(), instance, keys)
		if err != nil || !ok {
			t.Fatalf("enginesConvergedOn = (%v, %v), want (true, nil)", ok, err)
		}
	})

	t.Run("all engines matching hash converges", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			adminSecretForConvergence(),
			engineWithHash("e1", "inst", expected),
			engineWithHash("e2", "inst", expected),
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		ok, err := r.enginesConvergedOn(context.Background(), instance, keys)
		if err != nil || !ok {
			t.Fatalf("enginesConvergedOn = (%v, %v), want (true, nil)", ok, err)
		}
	})

	t.Run("one lagging engine blocks convergence", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			adminSecretForConvergence(),
			engineWithHash("e1", "inst", expected),
			engineWithHash("e2", "inst", "stale-hash"),
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		ok, err := r.enginesConvergedOn(context.Background(), instance, keys)
		if err != nil || ok {
			t.Fatalf("enginesConvergedOn = (%v, %v), want (false, nil)", ok, err)
		}
	})

	t.Run("engine belonging to a different instance is ignored", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			adminSecretForConvergence(),
			engineWithHash("e1", "other-inst", "irrelevant-hash"),
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		ok, err := r.enginesConvergedOn(context.Background(), instance, keys)
		if err != nil || !ok {
			t.Fatalf("enginesConvergedOn = (%v, %v), want (true, nil): an unrelated Instance's engine must not block", ok, err)
		}
	})

	t.Run("in-place admin password rotation (new RV) blocks convergence", func(t *testing.T) {
		// Engines still carry the old-RV hash; a rotated admin Secret bumps the
		// RV so enginesConvergedOn computes a different expected hash — the gate
		// must wait until engines roll onto the new password.
		rotated := adminSecretForConvergence()
		rotated.Data["password"] = []byte("rotated-pw")
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			rotated,
			engineWithHash("e1", "inst", expected),
		).Build()
		var rv corev1.Secret
		if err := cli.Get(context.Background(),
			client.ObjectKey{Namespace: "ns-1", Name: adminSecretNameForController}, &rv); err != nil {
			t.Fatalf("reading rotated admin secret: %v", err)
		}
		newExpected := authHash(&ResolvedAuthInfo{
			Spec: auth, SigningKeys: signingKeysForRender(keys), AdminSecretVersion: rv.ResourceVersion,
		})
		if newExpected == expected {
			// Only meaningful if the fake client actually assigns a distinct RV;
			// if not, skip rather than assert a false negative.
			t.Skip("fake client did not assign a distinct ResourceVersion; rotation-detection assertion not exercised")
		}
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		ok, err := r.enginesConvergedOn(context.Background(), instance, keys)
		if err != nil || ok {
			t.Fatalf("enginesConvergedOn = (%v, %v), want (false, nil): a rotated admin password must block until engines roll", ok, err)
		}
	})
}

// --- Signing-key rotation: full lifecycle via ensureSigningKeys/stepSigningKeyRotation ---

// signingKeySecretFor returns a Secret matching what cert-manager would
// eventually write for the Certificate buildSigningCertificate(instance,
// kid) describes: a real cert-manager controller isn't running against
// the fake client in these tests, so tests seed this directly to simulate
// "cert-manager has already issued this key" wherever the state machine
// needs to observe readiness.
func signingKeySecretFor(instance *computev1alpha1.FireboltInstance, kid string) *corev1.Secret {
	name := signingCertificateName(instance.Name, kid)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace},
		Data:       map[string][]byte{corev1.TLSPrivateKeyKey: []byte("fake-pem-" + kid)},
	}
}

func TestEnsureSigningKeys_Bootstrap_WaitsForSecretThenBecomesActive(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: validAuthSpecForController()},
	}

	ready, err := r.ensureSigningKeys(context.Background(), instance)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Fatal("expected ready=false before cert-manager has issued the Secret")
	}
	if instance.Status.Auth == nil || len(instance.Status.Auth.SigningKeys) != 1 {
		t.Fatalf("Status.Auth.SigningKeys = %+v, want exactly one key tracked (Certificate applied, Secret pending)",
			instance.Status.Auth)
	}
	key := instance.Status.Auth.SigningKeys[0]
	if key.ID != AuthSigningKeyID || key.Phase != computev1alpha1.SigningKeyActive {
		t.Errorf("key = %+v, want ID=%q Phase=Active", key, AuthSigningKeyID)
	}
	if !key.CreatedAt.IsZero() {
		t.Error("CreatedAt must stay unset until the Secret is actually observed ready")
	}

	// Simulate cert-manager finishing issuance, then reconcile again.
	if err := cli.Create(context.Background(), signingKeySecretFor(instance, AuthSigningKeyID)); err != nil {
		t.Fatalf("seeding signing key secret: %v", err)
	}
	ready, err = r.ensureSigningKeys(context.Background(), instance)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Fatal("expected ready=true once the Secret carries a private key")
	}
	if instance.Status.Auth.SigningKeys[0].CreatedAt.IsZero() {
		t.Error("CreatedAt must be set once the key is observed ready")
	}
}

// TestEnsureSigningKeys_AlgSizeChangeMintsFreshKey covers FB-896 finding #7: an
// active key issued as P-384 whose policy now asks for P-256 cannot be updated
// in place (rotationPolicy:Never), so the rotation machine must mint a fresh
// NAMED key carrying the new curve rather than accept the stale key as ready.
// TestEnsureSigningKeys_AlgSizeDriftDoesNotMint verifies that a change to the
// signing algorithm/size is NOT migrated in place by minting a fresh key
// (FB-896 round-4 #1). packdb exposes one global signing_algorithm and cannot
// serve two key curves at once, so such a change is rejected at admission
// (validateImmutableSigningKey) rather than migrated. Were the webhook
// bypassed, the controller must fail static — keep the existing active key —
// rather than mint a new-curve key that would produce an invalid mixed-curve
// keyset. This is the inverse of the removed mint-on-drift behavior.
func TestEnsureSigningKeys_AlgSizeDriftDoesNotMint(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
	ctx := context.Background()

	auth := validAuthSpecForController()
	auth.Local.SigningAlgorithm = "ES256"
	auth.Local.SigningKeys.CertManager.Algorithm = "ECDSA"
	auth.Local.SigningKeys.CertManager.Size = 256 // spec now asks for P-256

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: auth},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{
				SigningKeyGeneration: 1,
				SigningKeys: []computev1alpha1.SigningKeyStatus{{
					ID:         AuthSigningKeyID,
					SecretName: signingCertificateName("inst", AuthSigningKeyID),
					CreatedAt:  metav1.NewTime(time.Now()),
					Phase:      computev1alpha1.SigningKeyActive,
					Algorithm:  "ECDSA",
					Size:       384, // the curve the active key was actually issued with
				}},
			},
		},
	}
	if err := cli.Create(ctx, signingKeySecretFor(instance, AuthSigningKeyID)); err != nil {
		t.Fatalf("seeding active key secret: %v", err)
	}

	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("ensureSigningKeys: %v", err)
	}
	keys := instance.Status.Auth.SigningKeys
	if len(keys) != 1 {
		t.Fatalf("SigningKeys = %+v, want 1 (drift must NOT mint a fresh key)", keys)
	}
	if active := activeSigningKey(keys); active == nil || active.ID != AuthSigningKeyID ||
		active.Algorithm != "ECDSA" || active.Size != 384 {
		t.Errorf("active key = %+v, want the original ECDSA/384 key left unchanged", active)
	}
}

// TestEnsureAuth_DisableRetainsGenerationCounter covers FB-896 round-4 #3:
// disabling auth must preserve the monotonic SigningKeyGeneration so a later
// re-enable mints a FRESH generation rather than resetting to 0 and reusing the
// leftover signing-1 key material (which cert-manager reuses behind the
// existing Secret name, resurrecting previously-issued tokens).
func TestEnsureAuth_DisableRetainsGenerationCounter(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
	ctx := context.Background()

	// Instance that has already rotated to generation 2 while enabled.
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: &computev1alpha1.AuthSpec{Enabled: false}},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{
				SigningKeyGeneration: 2,
				SigningKeys: []computev1alpha1.SigningKeyStatus{{
					ID: signingKeyID(2), Phase: computev1alpha1.SigningKeyActive,
				}},
			},
		},
	}

	if err := r.ensureAuth(ctx, instance); err != nil {
		t.Fatalf("ensureAuth (disable): %v", err)
	}
	if instance.Status.Auth == nil {
		t.Fatal("disable nil'd Status.Auth, losing the generation high-water mark")
	}
	if got := instance.Status.Auth.SigningKeyGeneration; got != 2 {
		t.Errorf("SigningKeyGeneration = %d, want 2 preserved across disable", got)
	}
	if got := len(instance.Status.Auth.SigningKeys); got != 0 {
		t.Errorf("SigningKeys not cleared on disable: %d entries", got)
	}
	// A subsequent bootstrap (re-enable) must pick generation 3, never 1/2.
	if got := authStatusGeneration(instance) + 1; got != 3 {
		t.Errorf("next bootstrap generation = %d, want 3 (fresh, no collision)", got)
	}
}

// TestEnsureSigningKeys_FullRotationLifecycle drives the entire rotation
// state machine — mint, promote, retain, remove — across simulated
// reconciles, using an Instance with no engines (so every convergence
// check is vacuously true) to isolate the state machine's own timing
// logic from enginesConvergedOn, which TestEnginesConvergedOn covers
// separately.
func TestEnsureSigningKeys_FullRotationLifecycle(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
	ctx := context.Background()

	auth := validAuthSpecForController()
	rotationInterval := metav1.Duration{Duration: time.Hour}
	retainDuration := metav1.Duration{Duration: time.Hour}
	auth.Local.SigningKeys.RotationInterval = &rotationInterval
	auth.Local.SigningKeys.RetainDuration = &retainDuration

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: auth},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{
				SigningKeyGeneration: 1,
				SigningKeys: []computev1alpha1.SigningKeyStatus{{
					ID:         AuthSigningKeyID,
					SecretName: signingCertificateName("inst", AuthSigningKeyID),
					// Already well past rotationInterval, so a rotation is due
					// from the very first reconcile.
					CreatedAt: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
					Phase:     computev1alpha1.SigningKeyActive,
				}},
			},
		},
	}
	if err := cli.Create(ctx, signingKeySecretFor(instance, AuthSigningKeyID)); err != nil {
		t.Fatalf("seeding initial signing key secret: %v", err)
	}

	// Step 1: rotation is due, but signing-2's Secret does not exist yet —
	// mintNextSigningKey must apply the Certificate without appending an
	// unready key to Status.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("step 1: unexpected error: %v", err)
	}
	if got := len(instance.Status.Auth.SigningKeys); got != 1 {
		t.Fatalf("step 1: SigningKeys has %d entries, want 1 (new key must not be tracked before its Secret is ready)", got)
	}

	// Step 2: seed signing-2's Secret (cert-manager "finishes issuing"),
	// reconcile again — the pending mint must now succeed and append it
	// ValidationOnly.
	if err := cli.Create(ctx, signingKeySecretFor(instance, "signing-2")); err != nil {
		t.Fatalf("seeding signing-2 secret: %v", err)
	}
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("step 2: unexpected error: %v", err)
	}
	keys := instance.Status.Auth.SigningKeys
	if len(keys) != 2 {
		t.Fatalf("step 2: SigningKeys = %+v, want 2 entries", keys)
	}
	other := otherSigningKey(keys)
	if other == nil || other.ID != "signing-2" || other.Phase != computev1alpha1.SigningKeyValidationOnly || other.DemotedAt != nil {
		t.Fatalf("step 2: other = %+v, want signing-2/ValidationOnly/DemotedAt=nil", other)
	}
	if instance.Status.Auth.SigningKeyGeneration != 2 {
		t.Errorf("step 2: SigningKeyGeneration = %d, want 2", instance.Status.Auth.SigningKeyGeneration)
	}

	// Step 3: no engines exist, so convergence is vacuous — the next
	// reconcile must promote signing-2 to Active and demote signing-1.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("step 3: unexpected error: %v", err)
	}
	keys = instance.Status.Auth.SigningKeys
	active := activeSigningKey(keys)
	other = otherSigningKey(keys)
	if active == nil || active.ID != "signing-2" {
		t.Fatalf("step 3: active = %+v, want signing-2", active)
	}
	if other == nil || other.ID != AuthSigningKeyID || other.Phase != computev1alpha1.SigningKeyValidationOnly || other.DemotedAt == nil {
		t.Fatalf("step 3: other = %+v, want %s/ValidationOnly with DemotedAt set", other, AuthSigningKeyID)
	}

	// Step 4: no engines exist, so convergence on the just-promoted config
	// is also vacuous — the next reconcile must confirm the promotion
	// rolled out and stamp RetireEligibleAt, WITHOUT yet touching Phase:
	// the retain window has not started counting before this point, and
	// this is the step that starts it. If the retain window instead
	// started at DemotedAt (the promotion decision, not its confirmed
	// rollout), a slow-rolling engine could still be signing with this
	// key after the window already began — see RetireEligibleAt's doc
	// comment for why that would silently reopen the exact validation gap
	// this whole rotation design exists to close.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("step 4: unexpected error: %v", err)
	}
	other = otherSigningKey(instance.Status.Auth.SigningKeys)
	if other == nil || other.Phase != computev1alpha1.SigningKeyValidationOnly || other.RetireEligibleAt == nil {
		t.Fatalf("step 4: other = %+v, want ValidationOnly with RetireEligibleAt now set", other)
	}

	// Step 5: RetainDuration (1h) has not elapsed since RetireEligibleAt
	// (just now) — nothing should change yet.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("step 5: unexpected error: %v", err)
	}
	if other := otherSigningKey(instance.Status.Auth.SigningKeys); other == nil || other.Phase != computev1alpha1.SigningKeyValidationOnly {
		t.Fatalf("step 5: other = %+v, want still ValidationOnly (RetainDuration not yet elapsed)", other)
	}

	// Step 6: backdate RetireEligibleAt past RetainDuration (simulating
	// time passing, without sleeping in the test) — the demoted key must
	// move to Removing, dropped from render but not yet deleted.
	for i := range instance.Status.Auth.SigningKeys {
		if instance.Status.Auth.SigningKeys[i].ID == AuthSigningKeyID {
			past := metav1.NewTime(time.Now().Add(-2 * time.Hour))
			instance.Status.Auth.SigningKeys[i].RetireEligibleAt = &past
		}
	}
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("step 6: unexpected error: %v", err)
	}
	other = otherSigningKey(instance.Status.Auth.SigningKeys)
	if other == nil || other.Phase != computev1alpha1.SigningKeyRemoving {
		t.Fatalf("step 6: other = %+v, want Phase=Removing", other)
	}
	if rendered := signingKeysForRender(instance.Status.Auth.SigningKeys); len(rendered) != 1 {
		t.Errorf("step 6: signingKeysForRender = %+v, want only the Active key (Removing excluded)", rendered)
	}

	// Step 7: still vacuously converged (no engines) — the Removing key's
	// Certificate/Secret must be deleted and its entry dropped entirely.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("step 7: unexpected error: %v", err)
	}
	keys = instance.Status.Auth.SigningKeys
	if len(keys) != 1 || keys[0].ID != "signing-2" {
		t.Fatalf("step 7: SigningKeys = %+v, want exactly [signing-2] (old key fully removed)", keys)
	}

	oldSecretName := signingCertificateName("inst", AuthSigningKeyID)
	var secret corev1.Secret
	err := cli.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: oldSecretName}, &secret)
	if !apierrors.IsNotFound(err) {
		t.Errorf("old signing key Secret %q still exists (err=%v), want deleted", oldSecretName, err)
	}
	var cert certmanagerv1.Certificate
	err = cli.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: oldSecretName}, &cert)
	if !apierrors.IsNotFound(err) {
		t.Errorf("old signing Certificate %q still exists (err=%v), want deleted", oldSecretName, err)
	}
}

// TestEnsureSigningKeys_LaggingEngineBlocksPromotionAndRetireEligible is
// the composition test for this feature's entire safety property: a real
// engine that hasn't rolled yet must concretely block both convergence
// gates (promote, and retire-eligible), not just cause
// enginesConvergedOn to return false in isolation. TestEnginesConvergedOn
// already covers that function directly; every other rotation-lifecycle
// test above runs with zero engines (vacuous convergence) specifically to
// isolate the state machine's own timing from this gate — so this test
// is the one place that exercises them together, in a shape close to
// what an actual slow-rolling engine looks like.
func TestEnsureSigningKeys_LaggingEngineBlocksPromotionAndRetireEligible(t *testing.T) {
	sch := authTestScheme(t)
	auth := validAuthSpecForController()
	rotationInterval := metav1.Duration{Duration: time.Hour}
	retainDuration := metav1.Duration{Duration: time.Hour}
	auth.Local.SigningKeys.RotationInterval = &rotationInterval
	auth.Local.SigningKeys.RetainDuration = &retainDuration

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: auth},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{
				SigningKeyGeneration: 2,
				SigningKeys: []computev1alpha1.SigningKeyStatus{
					{ID: AuthSigningKeyID, SecretName: signingCertificateName("inst", AuthSigningKeyID),
						Phase: computev1alpha1.SigningKeyActive},
					{ID: "signing-2", SecretName: signingCertificateName("inst", "signing-2"),
						Phase: computev1alpha1.SigningKeyValidationOnly},
				},
			},
		},
	}
	ctx := context.Background()
	cli := fake.NewClientBuilder().WithScheme(sch).WithStatusSubresource(&computev1alpha1.FireboltEngine{}).WithObjects(
		adminSecretForConvergence(),
		signingKeySecretFor(instance, AuthSigningKeyID),
		signingKeySecretFor(instance, "signing-2"),
	).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	// enginesConvergedOn folds the admin Secret's ResourceVersion into the
	// expected hash (FB-896 #4), so the engine's simulated ObservedAuthHash
	// must be computed the same way to converge.
	var adminSec corev1.Secret
	if err := cli.Get(ctx, client.ObjectKey{Namespace: "ns-1", Name: adminSecretNameForController}, &adminSec); err != nil {
		t.Fatalf("reading admin secret RV: %v", err)
	}
	adminRV := adminSec.ResourceVersion

	preHash := authHash(&ResolvedAuthInfo{Spec: auth, SigningKeys: signingKeysForRender(instance.Status.Auth.SigningKeys), AdminSecretVersion: adminRV})
	engine := engineWithHash("e1", "inst", "stale-hash-from-a-previous-generation")
	if err := cli.Create(ctx, engine); err != nil {
		t.Fatalf("seeding lagging engine: %v", err)
	}

	// The engine has not rolled onto [active=signing-1, vo=signing-2] yet
	// — promotion must not happen.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("unexpected error while engine is lagging: %v", err)
	}
	if other := otherSigningKey(instance.Status.Auth.SigningKeys); other == nil || other.DemotedAt != nil {
		t.Fatalf("promotion happened despite a lagging engine: other = %+v", other)
	}

	// The engine catches up (its own reconcile stamps ObservedAuthHash to
	// match what it actually rendered) — promotion may now proceed.
	engine.Status.ObservedAuthHash = preHash
	if err := cli.Status().Update(ctx, engine); err != nil {
		t.Fatalf("updating engine hash: %v", err)
	}
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("unexpected error after engine catches up: %v", err)
	}
	active := activeSigningKey(instance.Status.Auth.SigningKeys)
	other := otherSigningKey(instance.Status.Auth.SigningKeys)
	if active == nil || active.ID != "signing-2" || other == nil || other.DemotedAt == nil {
		t.Fatalf("promotion did not happen once the engine converged: active=%+v other=%+v", active, other)
	}
	if other.RetireEligibleAt != nil {
		t.Fatalf("RetireEligibleAt set before confirming the promotion itself rolled out: other = %+v", other)
	}

	// Promotion flips the expected hash (order-sensitive: active is now
	// signing-2). The engine is still on its pre-promotion hash, so it is
	// lagging again relative to this new target — retire-eligibility must
	// not be granted yet.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("unexpected error while engine lags the promotion rollout: %v", err)
	}
	if other := otherSigningKey(instance.Status.Auth.SigningKeys); other == nil || other.RetireEligibleAt != nil {
		t.Fatalf("RetireEligibleAt set despite the engine not having rolled onto the promoted config: other = %+v", other)
	}

	// The engine rolls onto the promoted config too — only now is it safe
	// to start the retain-duration countdown.
	postHash := authHash(&ResolvedAuthInfo{Spec: auth, SigningKeys: signingKeysForRender(instance.Status.Auth.SigningKeys), AdminSecretVersion: adminRV})
	if postHash == preHash {
		t.Fatal("pre- and post-promotion hashes must differ (active-first ordering flipped); test fixture is wrong")
	}
	engine.Status.ObservedAuthHash = postHash
	if err := cli.Status().Update(ctx, engine); err != nil {
		t.Fatalf("updating engine hash: %v", err)
	}
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("unexpected error after engine converges on the promotion: %v", err)
	}
	if other := otherSigningKey(instance.Status.Auth.SigningKeys); other == nil || other.RetireEligibleAt == nil {
		t.Fatalf("RetireEligibleAt not set once the engine converged on the promoted config: other = %+v", other)
	}
}
