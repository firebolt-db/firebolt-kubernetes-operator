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
	"slices"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// markCertReadyForGeneration marks the cert-manager Certificate ns/name Ready
// for its current generation, simulating cert-manager completing issuance.
// FB-896 #4 gates TLS/auth readiness on this (not just Secret-key presence), so
// tests that seed a Secret and expect readiness must also mark the Certificate
// ready. Requires the fake client to be built
// WithStatusSubresource(&certmanagerv1.Certificate{}) so the status survives
// the next reconcile's server-side apply of the (unchanged) Certificate spec.
func markCertReadyForGeneration(t *testing.T, cli client.Client, ns, name string) {
	t.Helper()
	var cert certmanagerv1.Certificate
	if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &cert); err != nil {
		t.Fatalf("getting certificate %q to mark ready: %v", name, err)
	}
	cert.Status.Conditions = []certmanagerv1.CertificateCondition{{
		Type:               certmanagerv1.CertificateConditionReady,
		Status:             cmmeta.ConditionTrue,
		ObservedGeneration: cert.Generation,
	}}
	if err := cli.Status().Update(context.Background(), &cert); err != nil {
		t.Fatalf("marking certificate %q ready: %v", name, err)
	}
}

// validEngineTLSSpecForController returns a TLSSpec that satisfies
// ValidateTLS on its own, mirroring validAuthSpecForController.
func validEngineTLSSpecForController() *computev1alpha1.TLSSpec {
	return &computev1alpha1.TLSSpec{
		Engine: &computev1alpha1.TLSListenerSpec{
			Enabled: true,
			CertManager: &computev1alpha1.CertManagerSpec{
				IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
			},
		},
	}
}

func TestBuildEngineTLSCertificate_DefaultsToRSAPKCS8NeverRotate(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{TLS: validEngineTLSSpecForController()},
	}

	cert := buildEngineTLSCertificate(instance)

	wantName := "inst" + SuffixEngineTLS
	if cert.Name != wantName {
		t.Errorf("Name = %q, want %q", cert.Name, wantName)
	}
	if cert.Namespace != "ns-1" {
		t.Errorf("Namespace = %q, want ns-1", cert.Namespace)
	}
	if cert.Spec.SecretName != wantName {
		t.Errorf("Spec.SecretName = %q, want %q (Certificate and Secret share a name)", cert.Spec.SecretName, wantName)
	}

	wantDNSNames := []string{"*.ns-1.svc.cluster.local", "localhost"}
	if !slices.Equal(cert.Spec.DNSNames, wantDNSNames) {
		t.Errorf("DNSNames = %v, want %v", cert.Spec.DNSNames, wantDNSNames)
	}

	if !slices.Contains(cert.Spec.Usages, certmanagerv1.UsageServerAuth) {
		t.Errorf("Usages = %v, want to contain %q (this cert is presented in a real TLS handshake)",
			cert.Spec.Usages, certmanagerv1.UsageServerAuth)
	}

	pk := cert.Spec.PrivateKey
	if pk == nil {
		t.Fatal("Spec.PrivateKey is nil")
	}
	if pk.Algorithm != certmanagerv1.RSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want RSA (CertManagerSpec.Algorithm was empty)", pk.Algorithm)
	}
	if pk.Encoding != certmanagerv1.PKCS8 {
		t.Errorf("Encoding = %q, want PKCS8", pk.Encoding)
	}
	if pk.RotationPolicy != certmanagerv1.RotationPolicyNever {
		t.Errorf("RotationPolicy = %q, want Never", pk.RotationPolicy)
	}

	if cert.Spec.Duration == nil || cert.Spec.Duration.Duration != engineTLSCertDuration {
		t.Errorf("Duration = %v, want %v (must be effectively-static so cert-manager never auto-renews)",
			cert.Spec.Duration, engineTLSCertDuration)
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
			"Secret sweep cleans up the engine TLS Secret on instance deletion", LabelInstance)
	}
}

func TestBuildEngineTLSCertificate_ECDSAAndExplicitIssuerKind(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{
					Enabled: true,
					CertManager: &computev1alpha1.CertManagerSpec{
						IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca", Kind: "Issuer"},
						Algorithm: "ECDSA",
						Size:      384,
					},
				},
			},
		},
	}

	cert := buildEngineTLSCertificate(instance)

	if cert.Spec.PrivateKey.Algorithm != certmanagerv1.ECDSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want ECDSA", cert.Spec.PrivateKey.Algorithm)
	}
	if cert.Spec.PrivateKey.Size != 384 {
		t.Errorf("Size = %d, want 384", cert.Spec.PrivateKey.Size)
	}
	if cert.Spec.IssuerRef.Kind != "Issuer" {
		t.Errorf("IssuerRef.Kind = %q, want Issuer (explicit namespaced issuer)", cert.Spec.IssuerRef.Kind)
	}
}

// TestEngineTLSSecretReady_RequiresCACert is the discriminating test for
// the gap found during review: readiness must require ca.crt, not just
// tls.crt/tls.key, because the gateway's trusted_ca points at ca.crt
// gated on this exact check (see engineTLSSecretReady's doc comment).
func TestEngineTLSSecretReady_RequiresCACert(t *testing.T) {
	tests := []struct {
		name string
		data map[string][]byte
		want bool
	}{
		{name: "empty secret", data: map[string][]byte{}, want: false},
		{
			name: "cert and key but no CA",
			data: map[string][]byte{
				corev1.TLSCertKey:       []byte("cert"),
				corev1.TLSPrivateKeyKey: []byte("key"),
			},
			want: false,
		},
		{
			name: "cert and CA but no key",
			data: map[string][]byte{
				corev1.TLSCertKey:    []byte("cert"),
				engineTLSCASecretKey: []byte("ca"),
			},
			want: false,
		},
		{
			name: "all three present",
			data: map[string][]byte{
				corev1.TLSCertKey:       []byte("cert"),
				corev1.TLSPrivateKeyKey: []byte("key"),
				engineTLSCASecretKey:    []byte("ca"),
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			secret := &corev1.Secret{Data: tc.data}
			if got := engineTLSSecretReady(secret); got != tc.want {
				t.Errorf("engineTLSSecretReady(%v) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestEnsureEngineTLS_NilOrDisabledClearsStatus(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	tests := []struct {
		name string
		tls  *computev1alpha1.TLSSpec
	}{
		{name: "nil tls", tls: nil},
		{name: "engine nil", tls: &computev1alpha1.TLSSpec{}},
		{name: "explicitly disabled", tls: &computev1alpha1.TLSSpec{Engine: &computev1alpha1.TLSListenerSpec{Enabled: false}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := &computev1alpha1.FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
				Spec:       computev1alpha1.FireboltInstanceSpec{TLS: tc.tls},
				Status: computev1alpha1.FireboltInstanceStatus{
					// Simulate engine TLS having been enabled and
					// provisioned in a prior reconcile, then disabled:
					// stale status must be cleared.
					EngineTLS: &computev1alpha1.EngineTLSStatus{SecretName: "stale-secret"},
				},
			}

			if err := r.ensureEngineTLS(context.Background(), instance); err != nil {
				t.Fatalf("ensureEngineTLS: unexpected error: %v", err)
			}
			if instance.Status.EngineTLS != nil {
				t.Errorf("Status.EngineTLS = %+v, want nil", instance.Status.EngineTLS)
			}
			cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionEngineTLSReady)
			if cond == nil {
				t.Fatal("EngineTLSReady condition not set")
			}
			if cond.Status != metav1.ConditionTrue || cond.Reason != "Disabled" {
				t.Errorf("EngineTLSReady = %s/%s, want True/Disabled", cond.Status, cond.Reason)
			}
		})
	}
}

// TestEnsureEngineTLS_BypassedWebhookDoesNotPanic exercises the
// defense-in-depth re-validation, mirroring
// TestEnsureAuth_BypassedWebhookDoesNotPanic: a CR with engine TLS
// enabled and no CertManager block could only exist if the validating
// webhook was bypassed.
func TestEnsureEngineTLS_BypassedWebhookDoesNotPanic(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{Enabled: true}, // CertManager is nil: invalid per ValidateTLS.
			},
		},
	}

	err := r.ensureEngineTLS(context.Background(), instance)
	if err == nil {
		t.Fatal("expected error for invalid TLS spec, got nil")
	}

	cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionEngineTLSReady)
	if cond == nil {
		t.Fatal("EngineTLSReady condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "TLSSpecInvalid" {
		t.Errorf("EngineTLSReady = %s/%s, want False/TLSSpecInvalid", cond.Status, cond.Reason)
	}
}

// TestEnsureEngineTLS_SecretRefRejected verifies the controller's
// defense-in-depth after FB-896 #1: engine bring-your-own Secret is no longer
// supported (the operator must issue per-generation certs whose SANs cover the
// engine pod hostnames), so ensureEngineTLS's ValidateTLS re-run rejects it
// even on a CR that reached the cluster with a bypassed webhook — rather than
// silently consuming a user Secret that can't satisfy packdb's verification.
func TestEnsureEngineTLS_SecretRefRejected(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{
					Enabled:   true,
					SecretRef: &corev1.LocalObjectReference{Name: "byo-engine-tls"},
				},
			},
		},
	}

	if err := r.ensureEngineTLS(context.Background(), instance); err == nil {
		t.Fatal("ensureEngineTLS accepted engine secretRef; want rejection (BYO not supported for the engine listener)")
	}
	if instance.Status.EngineTLS != nil {
		t.Errorf("Status.EngineTLS = %+v, want nil when the spec is rejected", instance.Status.EngineTLS)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionEngineTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "TLSSpecInvalid" {
		t.Errorf("EngineTLSReady = %+v, want False/TLSSpecInvalid", cond)
	}
}

// TestEnsureGatewayTLS_SecretRefConsumesUserSecret is the gateway counterpart:
// a client-facing listener only presents its cert, so tls.crt + tls.key
// suffice (no ca.crt), and again no Certificate is provisioned.
func TestEnsureGatewayTLS_SecretRefConsumesUserSecret(t *testing.T) {
	sch := authTestScheme(t)
	byoSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-gw-tls", Namespace: "ns-1"},
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("cert"),
			corev1.TLSPrivateKeyKey: []byte("key"),
		},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(byoSecret).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled:   true,
					SecretRef: &corev1.LocalObjectReference{Name: "byo-gw-tls"},
				},
			},
		},
	}

	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS: %v", err)
	}
	if instance.Status.GatewayTLS == nil || instance.Status.GatewayTLS.SecretName != "byo-gw-tls" {
		t.Errorf("Status.GatewayTLS = %+v, want SecretName byo-gw-tls", instance.Status.GatewayTLS)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != "Ready" {
		t.Errorf("GatewayTLSReady = %+v, want True/Ready", cond)
	}

	var certs certmanagerv1.CertificateList
	if err := cli.List(context.Background(), &certs); err != nil {
		t.Fatalf("listing certificates: %v", err)
	}
	if len(certs.Items) != 0 {
		t.Errorf("BYO path created %d cert-manager Certificate(s), want 0", len(certs.Items))
	}
}

// TestEnsureGatewayTLS_ClientCAPendingFailsClosed covers FB-896 #2: when mutual
// TLS is requested (clientCASecretRef) but the client-CA Secret is missing, the
// gateway must fail CLOSED. A stale non-nil Status.GatewayTLS left standing
// keeps gatewayDownstreamTLSReady true, so the previous one-way pods keep
// serving (maxUnavailable=0) and accept the very clients the new mTLS policy
// should reject. Clearing the status flips gatewayDownstreamTLSPending on so the
// fail-closed (listener-omission) path takes over. Uses a BYO server Secret to
// isolate this from the server-cert Certificate-readiness path (#4).
func TestEnsureGatewayTLS_ClientCAPendingFailsClosed(t *testing.T) {
	sch := authTestScheme(t)
	byoSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-gw-tls", Namespace: "ns-1"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(byoSecret).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled:           true,
					SecretRef:         &corev1.LocalObjectReference{Name: "byo-gw-tls"},
					ClientCASecretRef: &corev1.LocalObjectReference{Name: "gw-client-ca"}, // deliberately never seeded
				},
			},
		},
		// Simulate a previously-ready one-way gateway whose status is now stale.
		Status: computev1alpha1.FireboltInstanceStatus{
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "byo-gw-tls"},
		},
	}

	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS: %v", err)
	}
	if instance.Status.GatewayTLS != nil {
		t.Errorf("Status.GatewayTLS = %+v, want nil (cleared so the gateway fails closed while the client-CA is pending)", instance.Status.GatewayTLS)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse {
		t.Errorf("GatewayTLSReady = %+v, want False while the client-CA is pending", cond)
	}
}

// TestEnsureGatewayTLS_WaitsForCertificateReadyForCurrentGeneration covers
// FB-896 #4: a cert-manager-backed listener must not be reported ready on the
// mere presence of a Secret carrying tls.crt/tls.key — the Certificate must be
// Ready for its CURRENT generation. Otherwise a failed re-issuance (issuer/SAN
// change) leaves the previous certificate serving while GatewayTLSReady stays
// True indefinitely.
func TestEnsureGatewayTLS_WaitsForCertificateReadyForCurrentGeneration(t *testing.T) {
	sch := authTestScheme(t)
	certName := gatewayTLSCertificateName("inst")
	// The Secret carries valid key material (a prior issuance); the Certificate
	// the apply below creates starts with no Ready condition.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "ns-1"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&certmanagerv1.Certificate{}).WithObjects(secret).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{TLS: validGatewayTLSSpecForController()},
	}

	// Secret present, Certificate not Ready for its current generation → not ready.
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (cert not ready): %v", err)
	}
	if instance.Status.GatewayTLS != nil {
		t.Errorf("Status.GatewayTLS = %+v, want nil while the Certificate is not Ready for its current generation", instance.Status.GatewayTLS)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse {
		t.Errorf("GatewayTLSReady = %+v, want False while the Certificate is not Ready", cond)
	}

	// cert-manager finishes issuance: the Certificate goes Ready for its gen.
	markCertReadyForGeneration(t, cli, "ns-1", certName)
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (cert ready): %v", err)
	}
	if instance.Status.GatewayTLS == nil || instance.Status.GatewayTLS.SecretName != certName {
		t.Errorf("Status.GatewayTLS = %+v, want SecretName %q once the Certificate is Ready", instance.Status.GatewayTLS, certName)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("GatewayTLSReady = %+v, want True once the Certificate is Ready for its current generation", cond)
	}
}

// TestEnsureGatewayTLS_StaleReadyGenerationKeepsServingButReportsDegraded is
// the core FB-896 #4 case: a Ready=True condition left over from a PRIOR
// successful issuance — its ObservedGeneration now lagging the Certificate's
// current generation because a re-issuance (after a DNS-SAN/issuer change) is
// failing — must NOT count as ready. The operator keeps serving the still-valid
// old certificate (Status.GatewayTLS retained) but flips GatewayTLSReady to
// False so the degraded state is visible. Retaining the status here is the
// deliberate opposite of the client-CA fail-closed handling (#2): a failing
// server-cert re-issuance is not a reason to tear down a still-valid cert.
func TestEnsureGatewayTLS_StaleReadyGenerationKeepsServingButReportsDegraded(t *testing.T) {
	sch := authTestScheme(t)
	certName := gatewayTLSCertificateName("inst")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "ns-1"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&certmanagerv1.Certificate{}).WithObjects(secret).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{TLS: validGatewayTLSSpecForController()},
		// A previous generation is already provisioned and serving.
		Status: computev1alpha1.FireboltInstanceStatus{
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: certName},
		},
	}

	// Priming reconcile creates the Certificate (no Ready condition yet).
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (priming): %v", err)
	}
	// Simulate cert-manager reporting Ready for a PRIOR generation only: the
	// current desired generation's issuance has not succeeded.
	var cert certmanagerv1.Certificate
	if err := cli.Get(context.Background(), client.ObjectKey{Namespace: "ns-1", Name: certName}, &cert); err != nil {
		t.Fatalf("getting gateway certificate: %v", err)
	}
	cert.Status.Conditions = []certmanagerv1.CertificateCondition{{
		Type:               certmanagerv1.CertificateConditionReady,
		Status:             cmmeta.ConditionTrue,
		ObservedGeneration: cert.Generation - 1, // stale: last success was a prior generation
	}}
	if err := cli.Status().Update(context.Background(), &cert); err != nil {
		t.Fatalf("setting stale Ready condition: %v", err)
	}

	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (stale ready): %v", err)
	}
	// Keep serving the still-valid old cert: Status.GatewayTLS must be retained.
	if instance.Status.GatewayTLS == nil || instance.Status.GatewayTLS.SecretName != certName {
		t.Errorf("Status.GatewayTLS = %+v, want retained (%q) so the still-valid old cert keeps serving during a failed re-issuance",
			instance.Status.GatewayTLS, certName)
	}
	// ...but report degraded (not Ready) for the stale-generation success.
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse {
		t.Errorf("GatewayTLSReady = %+v, want False for a Ready condition observed on a prior generation", cond)
	}
}

// validGatewayTLSSpecForController returns a TLSSpec that satisfies
// ValidateTLS on its own, mirroring validEngineTLSSpecForController.
func validGatewayTLSSpecForController() *computev1alpha1.TLSSpec {
	return &computev1alpha1.TLSSpec{
		Gateway: &computev1alpha1.TLSListenerSpec{
			Enabled: true,
			CertManager: &computev1alpha1.CertManagerSpec{
				IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
			},
		},
	}
}

func TestBuildGatewayTLSCertificate_DefaultsToRSAPKCS8NeverRotateAndServiceDNSNames(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{TLS: validGatewayTLSSpecForController()},
	}

	cert := buildGatewayTLSCertificate(instance)

	wantName := "inst" + SuffixGatewayTLS
	if cert.Name != wantName {
		t.Errorf("Name = %q, want %q", cert.Name, wantName)
	}
	if cert.Namespace != "ns-1" {
		t.Errorf("Namespace = %q, want ns-1", cert.Namespace)
	}
	if cert.Spec.SecretName != wantName {
		t.Errorf("Spec.SecretName = %q, want %q (Certificate and Secret share a name)", cert.Spec.SecretName, wantName)
	}

	wantDNSNames := []string{
		"inst-gateway",
		"inst-gateway.ns-1",
		"inst-gateway.ns-1.svc",
		"inst-gateway.ns-1.svc.cluster.local",
	}
	if !slices.Equal(cert.Spec.DNSNames, wantDNSNames) {
		t.Errorf("DNSNames = %v, want %v (the operator must always be able to derive its own Service's in-cluster names)",
			cert.Spec.DNSNames, wantDNSNames)
	}

	if !slices.Contains(cert.Spec.Usages, certmanagerv1.UsageServerAuth) {
		t.Errorf("Usages = %v, want to contain %q (this cert is presented in a real TLS handshake)",
			cert.Spec.Usages, certmanagerv1.UsageServerAuth)
	}

	pk := cert.Spec.PrivateKey
	if pk == nil {
		t.Fatal("Spec.PrivateKey is nil")
	}
	if pk.Algorithm != certmanagerv1.RSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want RSA (CertManagerSpec.Algorithm was empty)", pk.Algorithm)
	}
	if pk.Encoding != certmanagerv1.PKCS8 {
		t.Errorf("Encoding = %q, want PKCS8", pk.Encoding)
	}
	if pk.RotationPolicy != certmanagerv1.RotationPolicyNever {
		t.Errorf("RotationPolicy = %q, want Never", pk.RotationPolicy)
	}

	if cert.Spec.Duration == nil || cert.Spec.Duration.Duration != gatewayTLSCertDuration {
		t.Errorf("Duration = %v, want %v (must be effectively-static so cert-manager never auto-renews)",
			cert.Spec.Duration, gatewayTLSCertDuration)
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
			"Secret sweep cleans up the gateway TLS Secret on instance deletion", LabelInstance)
	}
}

// TestBuildGatewayTLSCertificate_UserDNSNamesAppended is the
// discriminating test for the design decision documented on
// TLSListenerSpec.DNSNames: the operator cannot infer an
// externally-visible hostname (no operator-managed Ingress/LoadBalancer),
// so any name a client outside the cluster will present must come from
// the user and must be ADDED to, not replace, the always-present
// in-cluster Service names.
func TestBuildGatewayTLSCertificate_UserDNSNamesAppended(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled: true,
					CertManager: &computev1alpha1.CertManagerSpec{
						IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
					},
					DNSNames: []string{"firebolt.example.com"},
				},
			},
		},
	}

	cert := buildGatewayTLSCertificate(instance)

	wantDNSNames := []string{
		"inst-gateway",
		"inst-gateway.ns-1",
		"inst-gateway.ns-1.svc",
		"inst-gateway.ns-1.svc.cluster.local",
		"firebolt.example.com",
	}
	if !slices.Equal(cert.Spec.DNSNames, wantDNSNames) {
		t.Errorf("DNSNames = %v, want %v (user names must be appended to, not replace, the in-cluster names)",
			cert.Spec.DNSNames, wantDNSNames)
	}
}

func TestBuildGatewayTLSCertificate_ECDSAAndExplicitIssuerKind(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{
					Enabled: true,
					CertManager: &computev1alpha1.CertManagerSpec{
						IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca", Kind: "Issuer"},
						Algorithm: "ECDSA",
						Size:      384,
					},
				},
			},
		},
	}

	cert := buildGatewayTLSCertificate(instance)

	if cert.Spec.PrivateKey.Algorithm != certmanagerv1.ECDSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want ECDSA", cert.Spec.PrivateKey.Algorithm)
	}
	if cert.Spec.PrivateKey.Size != 384 {
		t.Errorf("Size = %d, want 384", cert.Spec.PrivateKey.Size)
	}
	if cert.Spec.IssuerRef.Kind != "Issuer" {
		t.Errorf("IssuerRef.Kind = %q, want Issuer (explicit namespaced issuer)", cert.Spec.IssuerRef.Kind)
	}
}

// TestGatewayTLSSecretReady_DoesNotRequireCACert is the discriminating
// test for the design decision documented on gatewayTLSSecretReady's doc
// comment: unlike engineTLSSecretReady, ca.crt must NOT be required here.
// The gateway only presents this certificate to inbound clients; it never
// validates a peer against it, so requiring ca.crt would wedge every
// gateway TLS rollout in CertificatePending forever on an issuer that
// does not populate it.
func TestGatewayTLSSecretReady_DoesNotRequireCACert(t *testing.T) {
	tests := []struct {
		name string
		data map[string][]byte
		want bool
	}{
		{name: "empty secret", data: map[string][]byte{}, want: false},
		{
			name: "cert but no key",
			data: map[string][]byte{
				corev1.TLSCertKey: []byte("cert"),
			},
			want: false,
		},
		{
			name: "cert and key, no CA",
			data: map[string][]byte{
				corev1.TLSCertKey:       []byte("cert"),
				corev1.TLSPrivateKeyKey: []byte("key"),
			},
			want: true,
		},
		{
			name: "cert, key, and CA present",
			data: map[string][]byte{
				corev1.TLSCertKey:       []byte("cert"),
				corev1.TLSPrivateKeyKey: []byte("key"),
				engineTLSCASecretKey:    []byte("ca"),
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			secret := &corev1.Secret{Data: tc.data}
			if got := gatewayTLSSecretReady(secret); got != tc.want {
				t.Errorf("gatewayTLSSecretReady(%v) = %v, want %v", tc.data, got, tc.want)
			}
		})
	}
}

func TestEnsureGatewayTLS_NilOrDisabledClearsStatus(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	tests := []struct {
		name string
		tls  *computev1alpha1.TLSSpec
	}{
		{name: "nil tls", tls: nil},
		{name: "gateway nil", tls: &computev1alpha1.TLSSpec{}},
		{name: "explicitly disabled", tls: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{Enabled: false}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance := &computev1alpha1.FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
				Spec:       computev1alpha1.FireboltInstanceSpec{TLS: tc.tls},
				Status: computev1alpha1.FireboltInstanceStatus{
					// Simulate gateway TLS having been enabled and
					// provisioned in a prior reconcile, then disabled:
					// stale status must be cleared.
					GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "stale-secret"},
				},
			}

			if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
				t.Fatalf("ensureGatewayTLS: unexpected error: %v", err)
			}
			if instance.Status.GatewayTLS != nil {
				t.Errorf("Status.GatewayTLS = %+v, want nil", instance.Status.GatewayTLS)
			}
			cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady)
			if cond == nil {
				t.Fatal("GatewayTLSReady condition not set")
			}
			if cond.Status != metav1.ConditionTrue || cond.Reason != "Disabled" {
				t.Errorf("GatewayTLSReady = %s/%s, want True/Disabled", cond.Status, cond.Reason)
			}
		})
	}
}

// TestEnsureGatewayTLS_BypassedWebhookDoesNotPanic exercises the
// defense-in-depth re-validation, mirroring
// TestEnsureEngineTLS_BypassedWebhookDoesNotPanic: a CR with gateway TLS
// enabled and no CertManager block could only exist if the validating
// webhook was bypassed.
func TestEnsureGatewayTLS_BypassedWebhookDoesNotPanic(t *testing.T) {
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Gateway: &computev1alpha1.TLSListenerSpec{Enabled: true}, // CertManager is nil: invalid per ValidateTLS.
			},
		},
	}

	err := r.ensureGatewayTLS(context.Background(), instance)
	if err == nil {
		t.Fatal("expected error for invalid TLS spec, got nil")
	}

	cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady)
	if cond == nil {
		t.Fatal("GatewayTLSReady condition not set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "TLSSpecInvalid" {
		t.Errorf("GatewayTLSReady = %s/%s, want False/TLSSpecInvalid", cond.Status, cond.Reason)
	}
}
