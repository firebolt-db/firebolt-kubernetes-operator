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
	"errors"
	"slices"
	"strings"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	appsv1 "k8s.io/api/apps/v1"
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

// markGatewayServingCurrentConfig seeds (or updates) the gateway Deployment so
// gatewayServingCurrentConfig reports true: its pod template carries the
// config-hash of whatever buildEnvoyConfigYAML currently yields for the instance,
// and its status shows a completed rollout. The rendered config depends on
// instance.Status.GatewayTLS, so this rolls out the FAIL-CLOSED config when the
// status is nil (the staging state, FB-896 #1) and the SECURE config once the
// status is populated (the FB-896 #5 serving gate) — call it in each phase to
// advance a staged tightening transition to completion.
func markGatewayServingCurrentConfig(t *testing.T, cli client.Client, r *FireboltInstanceReconciler, instance *computev1alpha1.FireboltInstance) {
	t.Helper()
	hash, err := r.gatewayConfigHash(context.Background(), instance, buildEnvoyConfigYAML(instance))
	if err != nil {
		t.Fatalf("computing gateway fail-closed config hash: %v", err)
	}
	name := instance.Name + SuffixGateway
	two := int32(2)
	spec := appsv1.DeploymentSpec{
		Replicas: &two,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelInstance: instance.Name}},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationConfigHash: hash}},
		},
	}
	status := appsv1.DeploymentStatus{ObservedGeneration: 1, UpdatedReplicas: 2, Replicas: 2, AvailableReplicas: 2}

	var existing appsv1.Deployment
	if err := cli.Get(context.Background(), client.ObjectKey{Namespace: instance.Namespace, Name: name}, &existing); err != nil {
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace, Generation: 1},
			Spec:       spec,
			Status:     status,
		}
		if err := cli.Create(context.Background(), dep); err != nil {
			t.Fatalf("creating gateway deployment: %v", err)
		}
		return
	}
	existing.Generation = 1
	existing.Spec = spec
	existing.Status = status
	if err := cli.Update(context.Background(), &existing); err != nil {
		t.Fatalf("updating gateway deployment: %v", err)
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

func TestBuildEngineTLSCertificate_DefaultsToECDSAP384PKCS8NeverRotate(t *testing.T) {
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
	if pk.Algorithm != certmanagerv1.ECDSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want ECDSA (unset CertManagerSpec.Algorithm resolves to the CRD default)", pk.Algorithm)
	}
	if pk.Size != 384 {
		t.Errorf("Size = %d, want 384 (unset CertManagerSpec.Size resolves to the CRD default)", pk.Size)
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

// TestBuildEngineTLSCertificate_ExplicitZeroSizeResolvesDefault: an explicit
// `size: 0`/`algorithm: ""` skips CRD defaulting but must still resolve to the
// validated default (ECDSA/384).
func TestBuildEngineTLSCertificate_ExplicitZeroSizeResolvesDefault(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{
					Enabled: true,
					CertManager: &computev1alpha1.CertManagerSpec{
						IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
						Algorithm: "",
						Size:      0,
					},
				},
			},
		},
	}

	cert := buildEngineTLSCertificate(instance)

	if cert.Spec.PrivateKey.Algorithm != certmanagerv1.ECDSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want ECDSA (unset resolves to default)", cert.Spec.PrivateKey.Algorithm)
	}
	if cert.Spec.PrivateKey.Size != 384 {
		t.Errorf("Size = %d, want 384 (explicit 0 must resolve to default, not pass through)", cert.Spec.PrivateKey.Size)
	}
}

// TestBuildEngineTLSCertificate_ExplicitRSAUnchanged: an explicit RSA/2048
// request is issued as RSA/2048.
func TestBuildEngineTLSCertificate_ExplicitRSAUnchanged(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			TLS: &computev1alpha1.TLSSpec{
				Engine: &computev1alpha1.TLSListenerSpec{
					Enabled: true,
					CertManager: &computev1alpha1.CertManagerSpec{
						IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "internal-ca"},
						Algorithm: "RSA",
						Size:      2048,
					},
				},
			},
		},
	}

	cert := buildEngineTLSCertificate(instance)

	if cert.Spec.PrivateKey.Algorithm != certmanagerv1.RSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want RSA", cert.Spec.PrivateKey.Algorithm)
	}
	if cert.Spec.PrivateKey.Size != 2048 {
		t.Errorf("Size = %d, want 2048", cert.Spec.PrivateKey.Size)
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

// TestEnsureEngineTLS_ConvergenceGatesReady covers FB-896 #4: once the anchor
// certificate is provisioned, EngineTLSReady must stay False/Converging until
// the engine fleet has actually rolled onto TLS (Reencrypting). The condition
// feeds the Instance Ready roll-up, which must not advertise Ready over a
// still-plaintext gateway→engine hop. With the fleet converged (or empty) it
// flips True/Ready.
func TestEnsureEngineTLS_ConvergenceGatesReady(t *testing.T) {
	sch := authTestScheme(t)
	certName := engineTLSCertificateName("inst")
	anchorSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "ns-1"},
			Data: map[string][]byte{
				corev1.TLSCertKey:       []byte("cert"),
				corev1.TLSPrivateKeyKey: []byte("key"),
				engineTLSCASecretKey:    []byte("ca"),
			},
		}
	}
	mkInstance := func() *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
			Spec:       computev1alpha1.FireboltInstanceSpec{TLS: validEngineTLSSpecForController()},
		}
	}
	provision := func(t *testing.T, cli client.Client, r *FireboltInstanceReconciler, inst *computev1alpha1.FireboltInstance) {
		t.Helper()
		if err := r.ensureEngineTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureEngineTLS (priming): %v", err)
		}
		markCertReadyForGeneration(t, cli, "ns-1", certName)
		if err := r.ensureEngineTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureEngineTLS (provisioned): %v", err)
		}
	}

	t.Run("converging while an engine is still on plaintext", func(t *testing.T) {
		unconverged := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: "ns-1"},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst"},
			// ObservedEngineTLSHash "" ⇒ not yet on the TLS-serving generation.
		}
		cli := fake.NewClientBuilder().WithScheme(sch).
			WithStatusSubresource(&certmanagerv1.Certificate{}).
			WithObjects(anchorSecret(), unconverged).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := mkInstance()
		provision(t, cli, r, inst)

		if inst.Status.EngineTLS == nil {
			t.Fatal("Status.EngineTLS = nil, want provisioned once the anchor cert is Ready")
		}
		if inst.Status.EngineTLS.Reencrypting {
			t.Error("Reencrypting = true, want false while an engine is still on plaintext")
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionEngineTLSReady); cond == nil ||
			cond.Status != metav1.ConditionFalse || cond.Reason != "Converging" {
			t.Errorf("EngineTLSReady = %+v, want False/Converging while the fleet has not converged (#4)", cond)
		}
	})

	t.Run("ready once the fleet has converged (no engines is vacuously converged)", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).
			WithStatusSubresource(&certmanagerv1.Certificate{}).
			WithObjects(anchorSecret()).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := mkInstance()
		provision(t, cli, r, inst)

		if inst.Status.EngineTLS == nil || !inst.Status.EngineTLS.Reencrypting {
			t.Errorf("Status.EngineTLS = %+v, want Reencrypting once the fleet is (vacuously) converged", inst.Status.EngineTLS)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionEngineTLSReady); cond == nil ||
			cond.Status != metav1.ConditionTrue || cond.Reason != "Ready" {
			t.Errorf("EngineTLSReady = %+v, want True/Ready once converged", cond)
		}
	})
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

	// FB-896 #1: enabling gateway TLS on a plaintext gateway is a *tightening*
	// transition (plaintext→one-way TLS), so the first reconcile stages
	// fail-closed rather than immediately serving the BYO cert.
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (staging): %v", err)
	}
	if instance.Status.GatewayTLS != nil {
		t.Errorf("Status.GatewayTLS = %+v, want nil while staging fail-closed on a plaintext→TLS tighten", instance.Status.GatewayTLS)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "StagingFailClosed" {
		t.Errorf("GatewayTLSReady = %+v, want False/StagingFailClosed while the fail-closed rollout is pending", cond)
	}

	// The fail-closed config rolls out fully (old plaintext pods gone); now the
	// secure listener may be served — Status.GatewayTLS is populated, but the
	// secure config has not rolled out yet, so readiness is withheld (#5).
	markGatewayServingCurrentConfig(t, cli, r, instance)
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (secure): %v", err)
	}
	if instance.Status.GatewayTLS == nil || instance.Status.GatewayTLS.SecretName != "byo-gw-tls" {
		t.Errorf("Status.GatewayTLS = %+v, want SecretName byo-gw-tls once fail-closed has rolled out", instance.Status.GatewayTLS)
	}
	if instance.Status.GatewayTLS != nil && instance.Status.GatewayTLS.Mode != computev1alpha1.GatewayTLSModeOneWay {
		t.Errorf("Status.GatewayTLS.Mode = %q, want %q", instance.Status.GatewayTLS.Mode, computev1alpha1.GatewayTLSModeOneWay)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "SecureRolloutPending" {
		t.Errorf("GatewayTLSReady = %+v, want False/SecureRolloutPending before the secure config has rolled out (#5)", cond)
	}

	// The secure config now rolls out on every pod → serve and report Ready.
	markGatewayServingCurrentConfig(t, cli, r, instance)
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (secure serving): %v", err)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionTrue || cond.Reason != "Ready" {
		t.Errorf("GatewayTLSReady = %+v, want True/Ready once the secure config is serving", cond)
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
	// Cert material is now ready, but enabling on a plaintext gateway is a
	// tightening transition (FB-896 #1), so this reconcile stages fail-closed.
	markCertReadyForGeneration(t, cli, "ns-1", certName)
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (cert ready, staging): %v", err)
	}
	if instance.Status.GatewayTLS != nil {
		t.Errorf("Status.GatewayTLS = %+v, want nil while staging fail-closed even though the cert is ready", instance.Status.GatewayTLS)
	}

	// Fail-closed rollout completes → status populated with the secure posture,
	// but the secure config has not rolled out yet, so readiness is withheld (#5).
	markGatewayServingCurrentConfig(t, cli, r, instance)
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (cert ready, secure): %v", err)
	}
	if instance.Status.GatewayTLS == nil || instance.Status.GatewayTLS.SecretName != certName {
		t.Errorf("Status.GatewayTLS = %+v, want SecretName %q once the Certificate is Ready and fail-closed has rolled out", instance.Status.GatewayTLS, certName)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "SecureRolloutPending" {
		t.Errorf("GatewayTLSReady = %+v, want False/SecureRolloutPending before the secure config has rolled out (#5)", cond)
	}

	// Secure config rolls out on every pod → Ready.
	markGatewayServingCurrentConfig(t, cli, r, instance)
	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS (secure serving): %v", err)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("GatewayTLSReady = %+v, want True once the Certificate is Ready and the secure config is serving", cond)
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
// TestMergeCACerts pins the deterministic, deduped bundle assembly: identical
// CAs collapse to one entry, whitespace is normalized, and output order is
// stable so an unchanged CA set yields byte-identical bytes (no spurious
// gateway roll).
func TestMergeCACerts(t *testing.T) {
	got := string(mergeCACerts([][]byte{
		[]byte("CA-B\n"), []byte("CA-A"), []byte("  CA-B  "), []byte("CA-A\n"), {}, []byte("CA-C"),
	}))
	want := "CA-A\nCA-B\nCA-C"
	if got != want {
		t.Errorf("mergeCACerts = %q, want %q (deduped, trimmed, sorted)", got, want)
	}
	// Determinism: a different input ORDER of the same set yields the same bytes.
	got2 := string(mergeCACerts([][]byte{[]byte("CA-C"), []byte("CA-A"), []byte("CA-B")}))
	if got2 != want {
		t.Errorf("mergeCACerts (reordered) = %q, want %q", got2, want)
	}
}

// TestCollectCACerts_CompleteSemantics pins the FB-896 #4 completeness contract:
// complete is true only when EVERY requested name was read with a non-empty
// ca.crt, so ensureEngineCABundle can tell a genuine set apart from a transiently
// partial one and refuse to publish a pruned bundle. A name that is NotFound or
// keyless is skipped but clears complete; a hard Get error is surfaced.
func TestCollectCACerts_CompleteSemantics(t *testing.T) {
	sch := authTestScheme(t)
	const ns = "ns-1"
	present := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ca-present", Namespace: ns},
		Data:       map[string][]byte{engineTLSCASecretKey: []byte("CA-A")},
	}
	keyless := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ca-keyless", Namespace: ns},
		Data:       map[string][]byte{},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(present, keyless).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	t.Run("all names read with ca.crt is complete", func(t *testing.T) {
		cas, complete, err := r.collectCACerts(context.Background(), ns, []string{"ca-present"})
		if err != nil {
			t.Fatalf("collectCACerts: %v", err)
		}
		if !complete {
			t.Error("complete = false, want true when every requested name carried ca.crt")
		}
		if len(cas) != 1 || string(cas[0]) != "CA-A" {
			t.Errorf("cas = %q, want [CA-A]", cas)
		}
	})

	t.Run("a NotFound name marks the read incomplete", func(t *testing.T) {
		cas, complete, err := r.collectCACerts(context.Background(), ns, []string{"ca-present", "ca-missing"})
		if err != nil {
			t.Fatalf("collectCACerts: %v", err)
		}
		if complete {
			t.Error("complete = true, want false when a requested name is NotFound")
		}
		if len(cas) != 1 {
			t.Errorf("cas = %q, want only the present CA collected", cas)
		}
	})

	t.Run("a keyless name marks the read incomplete", func(t *testing.T) {
		_, complete, err := r.collectCACerts(context.Background(), ns, []string{"ca-present", "ca-keyless"})
		if err != nil {
			t.Fatalf("collectCACerts: %v", err)
		}
		if complete {
			t.Error("complete = true, want false when a requested name carried no ca.crt")
		}
	})

	t.Run("no names is vacuously complete", func(t *testing.T) {
		cas, complete, err := r.collectCACerts(context.Background(), ns, nil)
		if err != nil {
			t.Fatalf("collectCACerts: %v", err)
		}
		if !complete || len(cas) != 0 {
			t.Errorf("cas=%q complete=%v, want empty/true for no requested names", cas, complete)
		}
	})
}

// TestEnsureEngineCABundle covers FB-896 #4 assembly and #3 pruning: the gateway
// trust bundle is the deduped union of every live engine generation's CA (the
// long-lived anchor is EXCLUDED so its pinned CA cannot linger — #3, with the
// anchor kept only as a fallback when no per-gen CA is present), it self-prunes
// as generations retire, and it is only maintained while engine upstream TLS is
// engaged.
func TestEnsureEngineCABundle(t *testing.T) {
	sch := authTestScheme(t)
	const ns = "ns-1"
	caSecret := func(name, ca string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{engineTLSCASecretKey: []byte(ca)},
		}
	}
	engine := func(gens ...int) *computev1alpha1.FireboltEngine {
		e := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: ns},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst"},
		}
		// gens = [current, active, draining?]
		e.Status.CurrentGeneration = gens[0]
		e.Status.ActiveGeneration = gens[1]
		if len(gens) > 2 {
			d := gens[2]
			e.Status.DrainingGeneration = &d
		}
		return e
	}
	reencrypting := func() *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: ns},
			Status: computev1alpha1.FireboltInstanceStatus{
				EngineTLS: &computev1alpha1.EngineTLSStatus{SecretName: "inst" + SuffixEngineTLS, Reencrypting: true},
			},
		}
	}
	readBundle := func(t *testing.T, cli client.Client) string {
		t.Helper()
		var s corev1.Secret
		if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: engineCABundleSecretName("inst")}, &s); err != nil {
			t.Fatalf("getting bundle secret: %v", err)
		}
		return string(s.Data[engineTLSCASecretKey])
	}

	t.Run("assembles deduped union of live generation CAs, excluding the anchor", func(t *testing.T) {
		// anchor=CA-ANCHOR (a distinct pinned CA that no live gen uses), gen1=CA-A,
		// gen2=CA-B. #3: the anchor CA must NOT appear in the bundle.
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			caSecret(genResourceName("eng", 1, SuffixEngineTLS), "CA-A"),
			caSecret(genResourceName("eng", 2, SuffixEngineTLS), "CA-B"),
			engine(2, 1), // current=2, active=1 (mid blue-green)
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		fps, err := r.ensureEngineCABundle(context.Background(), reencrypting())
		if err != nil {
			t.Fatalf("ensureEngineCABundle: %v", err)
		}
		bundle := readBundle(t, cli)
		if bundle != "CA-A\nCA-B" {
			t.Errorf("bundle = %q, want the deduped union of live gen CAs %q (anchor excluded)", bundle, "CA-A\nCA-B")
		}
		if strings.Contains(bundle, "CA-ANCHOR") {
			t.Errorf("bundle = %q, must not contain the pinned anchor CA (#3)", bundle)
		}
		// Returned fingerprints match the deduped CA set the engine gate checks.
		want := []string{caFingerprint("CA-A"), caFingerprint("CA-B")}
		slices.Sort(want)
		got := append([]string(nil), fps...)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("fingerprints = %v, want %v (one per distinct live gen CA)", got, want)
		}
	})

	t.Run("prunes a CA once its generation retires; anchor never lingers", func(t *testing.T) {
		// anchor=CA-ANCHOR (distinct, never trusted), gen1=CA-C, gen2=CA-B.
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			caSecret(genResourceName("eng", 1, SuffixEngineTLS), "CA-C"),
			caSecret(genResourceName("eng", 2, SuffixEngineTLS), "CA-B"),
			engine(2, 1),
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		if _, err := r.ensureEngineCABundle(context.Background(), reencrypting()); err != nil {
			t.Fatalf("ensureEngineCABundle (overlap): %v", err)
		}
		// Overlap: both live gens' CAs, and #3 keeps the anchor OUT.
		if bundle := readBundle(t, cli); bundle != "CA-B\nCA-C" {
			t.Fatalf("bundle = %q, want both live gen CAs during overlap (anchor excluded)", bundle)
		}

		// gen1 retires: its Secret is deleted and the engine advances to active=2.
		if err := cli.Delete(context.Background(), caSecret(genResourceName("eng", 1, SuffixEngineTLS), "CA-C")); err != nil {
			t.Fatalf("deleting retired gen1 secret: %v", err)
		}
		var e computev1alpha1.FireboltEngine
		if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "eng"}, &e); err != nil {
			t.Fatalf("get engine: %v", err)
		}
		e.Status.ActiveGeneration = 2
		if err := cli.Update(context.Background(), &e); err != nil {
			t.Fatalf("advancing engine: %v", err)
		}
		if _, err := r.ensureEngineCABundle(context.Background(), reencrypting()); err != nil {
			t.Fatalf("ensureEngineCABundle (pruned): %v", err)
		}
		// Only gen2's CA-B remains: CA-C pruned with its generation, and the anchor
		// CA-ANCHOR was never present to linger (#3).
		if bundle := readBundle(t, cli); bundle != "CA-B" {
			t.Errorf("bundle = %q, want only CA-B once gen1 retired (CA-C pruned, anchor excluded)", bundle)
		}
	})

	t.Run("falls back to the anchor when no live generation CA is present", func(t *testing.T) {
		// Re-encrypting, but the live generation's per-gen Secret is missing this
		// pass (a transient gap). #3 fallback: the anchor keeps the bundle non-empty
		// so the gateway is not left trusting nothing.
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			engine(2, 1), // live gens 1,2 — but neither per-gen Secret exists
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		if _, err := r.ensureEngineCABundle(context.Background(), reencrypting()); err != nil {
			t.Fatalf("ensureEngineCABundle (fallback): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-ANCHOR" {
			t.Errorf("bundle = %q, want the anchor CA as a fallback when no live gen CA is present", bundle)
		}
	})

	t.Run("skips when engine upstream TLS is not engaged", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(caSecret("inst"+SuffixEngineTLS, "CA-A")).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: ns}}
		if _, err := r.ensureEngineCABundle(context.Background(), inst); err != nil {
			t.Fatalf("ensureEngineCABundle: %v", err)
		}
		var s corev1.Secret
		if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: engineCABundleSecretName("inst")}, &s); err == nil {
			t.Error("bundle Secret created while engine upstream TLS is not engaged; want none")
		}
	})

	t.Run("preserves the last bundle when a live generation's Secret is transiently missing", func(t *testing.T) {
		// gen1=CA-A, gen2=CA-B, both live (engine 2,1). The first pass assembles
		// the union; the caller confirms it (RolledEngineTrustCAs). Then gen2's
		// per-gen Secret goes transiently NotFound while gen2 is STILL live: the
		// read is incomplete, so the bundle must NOT be pruned to CA-A alone — the
		// gen2 pods still serving CA-B would then fail the gateway handshake.
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			caSecret(genResourceName("eng", 1, SuffixEngineTLS), "CA-A"),
			caSecret(genResourceName("eng", 2, SuffixEngineTLS), "CA-B"),
			engine(2, 1),
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		inst := reencrypting()
		fps, err := r.ensureEngineCABundle(context.Background(), inst)
		if err != nil {
			t.Fatalf("ensureEngineCABundle (assemble): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-A\nCA-B" {
			t.Fatalf("bundle = %q, want the full union before the gap", bundle)
		}
		inst.Status.RolledEngineTrustCAs = fps // the gateway confirmed this set

		if err := cli.Delete(context.Background(), caSecret(genResourceName("eng", 2, SuffixEngineTLS), "CA-B")); err != nil {
			t.Fatalf("deleting gen2 secret: %v", err)
		}
		got, err := r.ensureEngineCABundle(context.Background(), inst)
		if err != nil {
			t.Fatalf("ensureEngineCABundle (incomplete read): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-A\nCA-B" {
			t.Errorf("bundle = %q, want the last good union preserved (CA-B not pruned while gen2 is still live)", bundle)
		}
		if !slices.Equal(got, fps) {
			t.Errorf("fingerprints = %v, want the confirmed set %v preserved on an incomplete read", got, fps)
		}
	})

	t.Run("prefers the existing bundle over the anchor fallback on a total transient gap", func(t *testing.T) {
		// A previously-assembled bundle exists (CA-A from a live gen). This pass no
		// live-gen Secret is readable, so the code would fall back to the anchor —
		// but an existing bundle must be preserved rather than collapsed to the
		// pinned anchor CA (which #3 excludes from the trusted set).
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			caSecret(genResourceName("eng", 1, SuffixEngineTLS), "CA-A"),
			engine(1, 1),
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		inst := reencrypting()
		fps, err := r.ensureEngineCABundle(context.Background(), inst)
		if err != nil {
			t.Fatalf("ensureEngineCABundle (assemble): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-A" {
			t.Fatalf("bundle = %q, want CA-A before the gap", bundle)
		}
		inst.Status.RolledEngineTrustCAs = fps

		if err := cli.Delete(context.Background(), caSecret(genResourceName("eng", 1, SuffixEngineTLS), "CA-A")); err != nil {
			t.Fatalf("deleting gen1 secret: %v", err)
		}
		if _, err := r.ensureEngineCABundle(context.Background(), inst); err != nil {
			t.Fatalf("ensureEngineCABundle (total gap): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-A" {
			t.Errorf("bundle = %q, want the existing CA-A preserved, not collapsed to the anchor CA-ANCHOR", bundle)
		}
	})

	t.Run("an incomplete read from one engine does not block a new CA from another", func(t *testing.T) {
		// engA's live gen Secret is transiently NotFound (the read is incomplete),
		// but engB contributes CA-B (already confirmed serving) AND a new CA-C. The
		// assembled set keeps every confirmed-serving CA and only ADDS CA-C, so the
		// write must proceed — suppressing it on !complete alone would wedge engB's
		// cutover gate on CA-C indefinitely.
		engA := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: "enga", Namespace: ns},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst"},
		}
		engA.Status.CurrentGeneration = 1
		engA.Status.ActiveGeneration = 1
		engB := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: "engb", Namespace: ns},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst"},
		}
		engB.Status.CurrentGeneration = 2
		engB.Status.ActiveGeneration = 1
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			// enga's gen1 Secret is deliberately absent (transient NotFound).
			caSecret(genResourceName("engb", 1, SuffixEngineTLS), "CA-B"),
			caSecret(genResourceName("engb", 2, SuffixEngineTLS), "CA-C"),
			engA, engB,
			// A previously-assembled bundle carrying only CA-B already exists — so a
			// blanket !complete suppression (which preserves whenever a bundle is
			// present) would keep CA-C out; the fingerprint-gated write must add it.
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:        engineCABundleSecretName("inst"),
					Namespace:   ns,
					Annotations: map[string]string{AnnotationEngineTrustCAs: caFingerprint("CA-B")},
				},
				Data: map[string][]byte{engineTLSCASecretKey: []byte("CA-B")},
			},
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		inst := reencrypting()
		inst.Status.RolledEngineTrustCAs = []string{caFingerprint("CA-B")} // CA-B confirmed serving
		fps, err := r.ensureEngineCABundle(context.Background(), inst)
		if err != nil {
			t.Fatalf("ensureEngineCABundle: %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-B\nCA-C" {
			t.Errorf("bundle = %q, want CA-B\\nCA-C: the new CA-C written despite engA's incomplete read", bundle)
		}
		if !slices.Contains(fps, caFingerprint("CA-C")) {
			t.Errorf("fingerprints = %v, want to contain the new CA-C fingerprint (additions not blocked)", fps)
		}
	})

	t.Run("preserve returns the physical bundle set, not a stale superset Status", func(t *testing.T) {
		// The physical bundle was legitimately pruned to [CA-B] last reconcile, but
		// Status.RolledEngineTrustCAs still lists [CA-A, CA-B] because the gateway is
		// mid-roll. A transient incomplete read this pass must NOT re-publish the
		// stale [CA-A, CA-B] — that would over-claim CA-A, letting a later
		// CA-A-signed generation pass the cutover gate while the gateway trusts only
		// CA-B and the handshake fails. The returned set must equal the bundle's own
		// recorded annotation.
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			engine(2, 2), // only gen2 live
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:        engineCABundleSecretName("inst"),
					Namespace:   ns,
					Annotations: map[string]string{AnnotationEngineTrustCAs: caFingerprint("CA-B")},
				},
				Data: map[string][]byte{engineTLSCASecretKey: []byte("CA-B")},
			},
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		inst := reencrypting()
		inst.Status.RolledEngineTrustCAs = []string{caFingerprint("CA-A"), caFingerprint("CA-B")} // stale superset

		fps, err := r.ensureEngineCABundle(context.Background(), inst)
		if err != nil {
			t.Fatalf("ensureEngineCABundle: %v", err)
		}
		want := []string{caFingerprint("CA-B")}
		if !slices.Equal(fps, want) {
			t.Errorf("fingerprints = %v, want the physical bundle set %v (not the stale superset RolledEngineTrustCAs)", fps, want)
		}
	})

	t.Run("a legacy unannotated bundle is never pruned by incomplete reads; a complete read heals it", func(t *testing.T) {
		// A pre-upgrade bundle carries {CA-A, CA-B} but has NO AnnotationEngineTrustCAs,
		// so its served set is unreadable. gen2 (CA-B) is still live. Incomplete reads
		// must NEVER rewrite it (the served set is unknown, so a write cannot be proven
		// non-pruning); only a complete read may rewrite and stamp the annotation.
		// Guards the vacuous-subset bug: anchoring the write on a Status that gets
		// cleared to nil on the legacy preserve path would let a later incomplete read
		// prune CA-B.
		bundleName := engineCABundleSecretName("inst")
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			caSecret("inst"+SuffixEngineTLS, "CA-ANCHOR"),
			caSecret(genResourceName("eng", 1, SuffixEngineTLS), "CA-A"),
			caSecret(genResourceName("eng", 2, SuffixEngineTLS), "CA-B"),
			engine(2, 1),
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: bundleName, Namespace: ns}, // no annotation
				Data:       map[string][]byte{engineTLSCASecretKey: []byte("CA-A\nCA-B")},
			},
		).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

		inst := reencrypting()
		inst.Status.RolledEngineTrustCAs = []string{caFingerprint("CA-A"), caFingerprint("CA-B")} // last published by old code

		// gen2 (CA-B) Secret goes transiently unreadable while gen2 is still live.
		if err := cli.Delete(context.Background(), caSecret(genResourceName("eng", 2, SuffixEngineTLS), "CA-B")); err != nil {
			t.Fatalf("deleting gen2 secret: %v", err)
		}

		// Reconcile 1: incomplete read of a legacy bundle → must NOT rewrite the
		// physical bundle, and must publish nothing (a legacy bundle's true set is
		// unknowable, so re-confirming a possibly-stale Status could over-claim).
		fps1, err := r.ensureEngineCABundle(context.Background(), inst)
		if err != nil {
			t.Fatalf("ensureEngineCABundle (reconcile 1): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-A\nCA-B" {
			t.Fatalf("bundle = %q after reconcile 1, want the legacy {CA-A,CA-B} preserved", bundle)
		}
		if len(fps1) != 0 {
			t.Fatalf("fps1 = %v, want nil (publish nothing for an unverifiable legacy bundle)", fps1)
		}
		inst.Status.RolledEngineTrustCAs = fps1 // simulate publishRolledEngineTrustCAs: clears the stale set

		// Reconcile 2: still incomplete, read yields only CA-A. The legacy bundle must
		// STILL not be pruned to {CA-A} — the case the vacuous-subset bug broke.
		if _, err := r.ensureEngineCABundle(context.Background(), inst); err != nil {
			t.Fatalf("ensureEngineCABundle (reconcile 2): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-A\nCA-B" {
			t.Errorf("bundle = %q after reconcile 2, want {CA-A,CA-B} still intact (CA-B not pruned)", bundle)
		}

		// Reconcile 3: gen2's Secret returns → a complete read heals the bundle and
		// stamps the annotation, making all future reads safe.
		if err := cli.Create(context.Background(), caSecret(genResourceName("eng", 2, SuffixEngineTLS), "CA-B")); err != nil {
			t.Fatalf("recreating gen2 secret: %v", err)
		}
		fps3, err := r.ensureEngineCABundle(context.Background(), inst)
		if err != nil {
			t.Fatalf("ensureEngineCABundle (reconcile 3): %v", err)
		}
		if bundle := readBundle(t, cli); bundle != "CA-A\nCA-B" {
			t.Errorf("bundle = %q after reconcile 3, want the full {CA-A,CA-B}", bundle)
		}
		var healed corev1.Secret
		if err := cli.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: bundleName}, &healed); err != nil {
			t.Fatalf("getting healed bundle: %v", err)
		}
		gotFPs := strings.Split(healed.Annotations[AnnotationEngineTrustCAs], ",")
		slices.Sort(gotFPs)
		wantFPs := []string{caFingerprint("CA-A"), caFingerprint("CA-B")}
		slices.Sort(wantFPs)
		if !slices.Equal(gotFPs, wantFPs) {
			t.Errorf("annotation = %q, want the sorted {CA-A,CA-B} fingerprints stamped after the complete read", healed.Annotations[AnnotationEngineTrustCAs])
		}
		if !slices.Contains(fps3, caFingerprint("CA-B")) {
			t.Errorf("fps3 = %v, want CA-B present after healing", fps3)
		}
	})
}

// TestPublishRolledEngineTrustCAs_PreservesOnAssemblyError covers FB-896 #6: when
// bundle assembly failed (bundleErr != nil, fingerprints nil), the last-confirmed
// Status.RolledEngineTrustCAs must be preserved rather than clobbered with nil —
// the previous bundle may still be serving, and dropping the set would wrongly
// block engine cutovers the gateway can still handle. The gateway rollout must
// not even be consulted on error (no Deployment is seeded here).
func TestPublishRolledEngineTrustCAs_PreservesOnAssemblyError(t *testing.T) {
	sch := authTestScheme(t)
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Status: computev1alpha1.FireboltInstanceStatus{
			EngineTLS:            &computev1alpha1.EngineTLSStatus{SecretName: "inst" + SuffixEngineTLS, Reencrypting: true},
			RolledEngineTrustCAs: []string{caFingerprint("CA-A"), caFingerprint("CA-B")},
		},
	}
	prior := append([]string(nil), inst.Status.RolledEngineTrustCAs...)
	cli := fake.NewClientBuilder().WithScheme(sch).Build() // no gateway Deployment
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	r.publishRolledEngineTrustCAs(context.Background(), inst, nil, errors.New("listing engines for CA bundle: boom"))

	if !slices.Equal(inst.Status.RolledEngineTrustCAs, prior) {
		t.Errorf("RolledEngineTrustCAs = %v, want preserved %v on assembly error (#6)", inst.Status.RolledEngineTrustCAs, prior)
	}
}

// TestEnsureGatewayTLS_StagesOnlyWhenTightening covers the FB-896 #1 posture
// machine: a transition to a *stricter* posture (one-way→mTLS) stages
// fail-closed and withholds readiness until the rollout completes, while a
// *loosening* (mTLS→one-way) or steady transition serves immediately with no
// staging. BYO Secrets isolate this from cert-manager Certificate readiness.
func TestEnsureGatewayTLS_StagesOnlyWhenTightening(t *testing.T) {
	sch := authTestScheme(t)
	serverSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "byo-gw-tls", Namespace: "ns-1"},
			Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
		}
	}
	clientCASecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-client-ca", Namespace: "ns-1"},
			Data:       map[string][]byte{engineTLSCASecretKey: []byte("ca")},
		}
	}
	mkInstance := func(clientCA bool, servedMode string) *computev1alpha1.FireboltInstance {
		gw := &computev1alpha1.TLSListenerSpec{Enabled: true, SecretRef: &corev1.LocalObjectReference{Name: "byo-gw-tls"}}
		if clientCA {
			gw.ClientCASecretRef = &corev1.LocalObjectReference{Name: "gw-client-ca"}
		}
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
			Spec:       computev1alpha1.FireboltInstanceSpec{TLS: &computev1alpha1.TLSSpec{Gateway: gw}},
		}
		if servedMode != "" {
			inst.Status.GatewayTLS = &computev1alpha1.GatewayTLSStatus{SecretName: "byo-gw-tls", Mode: servedMode}
			// A served posture means the gateway was already reported Ready. The
			// FB-896 #5 serving gate only re-checks on the transition OUT of a
			// not-ready state, so seeding this True lets a loosening/steady
			// transition stay Ready without re-rolling (the anti-flap behavior);
			// a genuine tighten resets it to False/StagingFailClosed regardless.
			apimeta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
				Type: computev1alpha1.InstanceConditionGatewayTLSReady, Status: metav1.ConditionTrue, Reason: "Ready",
			})
		}
		return inst
	}

	t.Run("one-way→mTLS tightens: stages fail-closed, then serves mTLS once rolled out", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(serverSecret(), clientCASecret()).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := mkInstance(true, computev1alpha1.GatewayTLSModeOneWay)

		// Tightening + no fail-closed rollout observed yet → withheld (staging).
		if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureGatewayTLS (staging): %v", err)
		}
		if inst.Status.GatewayTLS != nil {
			t.Errorf("Status.GatewayTLS = %+v, want nil while staging the one-way→mTLS tighten", inst.Status.GatewayTLS)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
			cond.Status != metav1.ConditionFalse || cond.Reason != "StagingFailClosed" {
			t.Errorf("GatewayTLSReady = %+v, want False/StagingFailClosed", cond)
		}

		// Fail-closed rollout completes → status populated with mTLS, but the
		// secure config has not rolled out yet, so readiness is still withheld (#5).
		markGatewayServingCurrentConfig(t, cli, r, inst)
		if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureGatewayTLS (mTLS): %v", err)
		}
		if inst.Status.GatewayTLS == nil || inst.Status.GatewayTLS.Mode != computev1alpha1.GatewayTLSModeMutual {
			t.Errorf("Status.GatewayTLS = %+v, want Mode %q once fail-closed rolled out", inst.Status.GatewayTLS, computev1alpha1.GatewayTLSModeMutual)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
			cond.Status != metav1.ConditionFalse || cond.Reason != "SecureRolloutPending" {
			t.Errorf("GatewayTLSReady = %+v, want False/SecureRolloutPending before the mTLS config rolls out (#5)", cond)
		}

		// The mTLS config rolls out on every pod → serve and report Ready.
		markGatewayServingCurrentConfig(t, cli, r, inst)
		if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureGatewayTLS (mTLS serving): %v", err)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Errorf("GatewayTLSReady = %+v, want True once mTLS is serving", cond)
		}
	})

	t.Run("mTLS→one-way loosens: serves immediately, no staging", func(t *testing.T) {
		// No gateway Deployment seeded: if this path staged, it would block forever.
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(serverSecret()).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := mkInstance(false, computev1alpha1.GatewayTLSModeMutual)

		if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureGatewayTLS: %v", err)
		}
		if inst.Status.GatewayTLS == nil || inst.Status.GatewayTLS.Mode != computev1alpha1.GatewayTLSModeOneWay {
			t.Errorf("Status.GatewayTLS = %+v, want Mode %q served immediately on a loosening transition", inst.Status.GatewayTLS, computev1alpha1.GatewayTLSModeOneWay)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Errorf("GatewayTLSReady = %+v, want True (loosening needs no staging)", cond)
		}
	})

	t.Run("steady one-way: idempotent, no staging", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(serverSecret()).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := mkInstance(false, computev1alpha1.GatewayTLSModeOneWay)

		if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureGatewayTLS: %v", err)
		}
		if inst.Status.GatewayTLS == nil || inst.Status.GatewayTLS.Mode != computev1alpha1.GatewayTLSModeOneWay {
			t.Errorf("Status.GatewayTLS = %+v, want retained one-way in steady state", inst.Status.GatewayTLS)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Errorf("GatewayTLSReady = %+v, want True in steady state", cond)
		}
	})
}

// TestEnsureGatewayTLS_ClientCASwapStages covers FB-896 #2: replacing the client
// CA (CA-A→CA-B) keeps the posture at MutualTLS on both sides, so the posture
// ordinal alone would miss it and roll without staging — leaving old pods
// trusting the retired CA-A during the roll. Comparing the recorded client-CA
// fingerprint catches the swap as a tightening and stages fail-closed; an
// unchanged CA does not.
func TestEnsureGatewayTLS_ClientCASwapStages(t *testing.T) {
	sch := authTestScheme(t)
	serverSecret := func() *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "byo-gw-tls", Namespace: "ns-1"},
			Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
		}
	}
	clientCASecret := func(ca string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "gw-client-ca", Namespace: "ns-1"},
			Data:       map[string][]byte{engineTLSCASecretKey: []byte(ca)},
		}
	}
	// An instance already serving mTLS against CA-A (fingerprint recorded, Ready).
	servingMTLS := func() *computev1alpha1.FireboltInstance {
		inst := &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
			Spec: computev1alpha1.FireboltInstanceSpec{TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{
				Enabled:           true,
				SecretRef:         &corev1.LocalObjectReference{Name: "byo-gw-tls"},
				ClientCASecretRef: &corev1.LocalObjectReference{Name: "gw-client-ca"},
			}}},
			Status: computev1alpha1.FireboltInstanceStatus{
				GatewayTLS: &computev1alpha1.GatewayTLSStatus{
					SecretName:          "byo-gw-tls",
					Mode:                computev1alpha1.GatewayTLSModeMutual,
					ClientCAFingerprint: caFingerprint("CA-A"),
				},
			},
		}
		apimeta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
			Type: computev1alpha1.InstanceConditionGatewayTLSReady, Status: metav1.ConditionTrue, Reason: "Ready",
		})
		return inst
	}

	t.Run("changed client CA stages fail-closed", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(serverSecret(), clientCASecret("CA-B")).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := servingMTLS()

		if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureGatewayTLS: %v", err)
		}
		if inst.Status.GatewayTLS != nil {
			t.Errorf("Status.GatewayTLS = %+v, want nil (staged fail-closed) on a client-CA swap", inst.Status.GatewayTLS)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
			cond.Status != metav1.ConditionFalse || cond.Reason != "StagingFailClosed" {
			t.Errorf("GatewayTLSReady = %+v, want False/StagingFailClosed on a client-CA swap (#2)", cond)
		}
	})

	t.Run("unchanged client CA does not stage", func(t *testing.T) {
		// No gateway Deployment seeded: staging or a serving re-check would block;
		// the already-Ready marker (#5) keeps it Ready without either.
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(serverSecret(), clientCASecret("CA-A")).Build()
		r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}
		inst := servingMTLS()

		if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
			t.Fatalf("ensureGatewayTLS: %v", err)
		}
		if inst.Status.GatewayTLS == nil || inst.Status.GatewayTLS.Mode != computev1alpha1.GatewayTLSModeMutual {
			t.Errorf("Status.GatewayTLS = %+v, want retained mTLS when the client CA is unchanged", inst.Status.GatewayTLS)
		}
		if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
			cond.Status != metav1.ConditionTrue {
			t.Errorf("GatewayTLSReady = %+v, want True (no swap, no staging)", cond)
		}
	})
}

// TestEnsureGatewayTLS_ReadyStableAcrossRenewal covers the FB-896 #5 anti-flap
// guard: once GatewayTLSReady is True, a later change that re-rolls the gateway
// (e.g. a server-cert renewal) must NOT drop the Instance out of Ready. The
// serving gate only applies on the transition OUT of a not-ready state; a
// zero-downtime renewal keeps old secure pods serving throughout, so re-gating
// on gatewayServingCurrentConfig would flap Ready for no hazard. Modeled here
// with NO gateway Deployment present (so a serving re-check would return false):
// the condition must stay True regardless.
func TestEnsureGatewayTLS_ReadyStableAcrossRenewal(t *testing.T) {
	sch := authTestScheme(t)
	serverSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-gw-tls", Namespace: "ns-1"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(serverSecret).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{
			Enabled:   true,
			SecretRef: &corev1.LocalObjectReference{Name: "byo-gw-tls"},
		}}},
		Status: computev1alpha1.FireboltInstanceStatus{
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: "byo-gw-tls", Mode: computev1alpha1.GatewayTLSModeOneWay},
		},
	}
	apimeta.SetStatusCondition(&inst.Status.Conditions, metav1.Condition{
		Type: computev1alpha1.InstanceConditionGatewayTLSReady, Status: metav1.ConditionTrue, Reason: "Ready",
	})

	if err := r.ensureGatewayTLS(context.Background(), inst); err != nil {
		t.Fatalf("ensureGatewayTLS: %v", err)
	}
	if cond := apimeta.FindStatusCondition(inst.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionTrue {
		t.Errorf("GatewayTLSReady = %+v, want True held across a renewal (no flap to SecureRolloutPending)", cond)
	}
}

// TestEnsureGatewayTLS_TightenDuringReissueFailsClosed covers FB-896 #1: when an
// operator tightens the posture (one-way→mTLS) in the SAME edit that forces a
// server-cert reissuance, the gateway must go fail-closed immediately rather than
// keep the old one-way listener accepting certificate-less clients while the new
// cert issues. The tightening decision and fail-closed staging run BEFORE the
// cert-readiness gate, so a Certificate not yet Ready for its current generation
// (reissuance in flight — modeled here by never marking the freshly-applied
// Certificate Ready) does not defer the tighten and leave the status standing.
func TestEnsureGatewayTLS_TightenDuringReissueFailsClosed(t *testing.T) {
	sch := authTestScheme(t)
	certName := gatewayTLSCertificateName("inst")
	serverSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "ns-1"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	clientCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-client-ca", Namespace: "ns-1"},
		Data:       map[string][]byte{engineTLSCASecretKey: []byte("CA-A")},
	}
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&certmanagerv1.Certificate{}).WithObjects(serverSecret, clientCA).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	gwSpec := validGatewayTLSSpecForController()
	gwSpec.Gateway.ClientCASecretRef = &corev1.LocalObjectReference{Name: "gw-client-ca"} // desired mTLS
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{TLS: gwSpec},
		// Already serving one-way TLS and reported Ready.
		Status: computev1alpha1.FireboltInstanceStatus{
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{SecretName: certName, Mode: computev1alpha1.GatewayTLSModeOneWay},
		},
	}
	apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type: computev1alpha1.InstanceConditionGatewayTLSReady, Status: metav1.ConditionTrue, Reason: "Ready",
	})

	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS: %v", err)
	}
	if instance.Status.GatewayTLS != nil {
		t.Errorf("Status.GatewayTLS = %+v, want nil (fail-closed) on a one-way→mTLS tighten during a reissuance", instance.Status.GatewayTLS)
	}
	if cond := apimeta.FindStatusCondition(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady); cond == nil ||
		cond.Status != metav1.ConditionFalse || cond.Reason != "StagingFailClosed" {
		t.Errorf("GatewayTLSReady = %+v, want False/StagingFailClosed while staging the tighten", cond)
	}
}

// TestEnsureGatewayTLS_TightenPreservesCreatedAt covers FB-896 #3: the status's
// CreatedAt is captured BEFORE the tightening path clears the status, so when a
// tighten clears and repopulates the status WITHIN A SINGLE reconcile pass it does
// not re-stamp CreatedAt to Now(). Modeled as a client-CA swap (CA-A→CA-B) whose
// fail-closed config is already fully rolled out, so the tighten completes and
// repopulates in one call. (Across a MULTI-pass tighten — where the first pass
// persists a nil status and returns staging — the completing pass necessarily
// re-stamps Now(); that is accepted for a fixed-lifetime cert and is not what this
// capture guards.)
func TestEnsureGatewayTLS_TightenPreservesCreatedAt(t *testing.T) {
	sch := authTestScheme(t)
	serverSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "byo-gw-tls", Namespace: "ns-1"},
		Data:       map[string][]byte{corev1.TLSCertKey: []byte("cert"), corev1.TLSPrivateKeyKey: []byte("key")},
	}
	clientCA := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gw-client-ca", Namespace: "ns-1"},
		Data:       map[string][]byte{engineTLSCASecretKey: []byte("CA-B")}, // the replacement CA
	}
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(serverSecret, clientCA).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	created := metav1.Unix(1000, 0)
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{TLS: &computev1alpha1.TLSSpec{Gateway: &computev1alpha1.TLSListenerSpec{
			Enabled:           true,
			SecretRef:         &corev1.LocalObjectReference{Name: "byo-gw-tls"},
			ClientCASecretRef: &corev1.LocalObjectReference{Name: "gw-client-ca"},
		}}},
		// Serving mTLS against the OLD CA-A, provisioned at a known past time.
		Status: computev1alpha1.FireboltInstanceStatus{
			GatewayTLS: &computev1alpha1.GatewayTLSStatus{
				SecretName:          "byo-gw-tls",
				CreatedAt:           created,
				Mode:                computev1alpha1.GatewayTLSModeMutual,
				ClientCAFingerprint: caFingerprint("CA-A"),
			},
		},
	}
	apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type: computev1alpha1.InstanceConditionGatewayTLSReady, Status: metav1.ConditionTrue, Reason: "Ready",
	})

	// Seed the gateway Deployment already serving the fail-closed config, so the
	// swap's tighten observes the rollout as complete and repopulates in one call.
	savedGatewayTLS := instance.Status.GatewayTLS
	instance.Status.GatewayTLS = nil
	markGatewayServingCurrentConfig(t, cli, r, instance)
	instance.Status.GatewayTLS = savedGatewayTLS

	if err := r.ensureGatewayTLS(context.Background(), instance); err != nil {
		t.Fatalf("ensureGatewayTLS: %v", err)
	}
	if instance.Status.GatewayTLS == nil {
		t.Fatal("Status.GatewayTLS = nil, want repopulated once the fail-closed swap rolled out")
	}
	if !instance.Status.GatewayTLS.CreatedAt.Equal(&created) {
		t.Errorf("CreatedAt = %v, want the prior %v preserved within a single-pass tighten (not re-stamped to Now())",
			instance.Status.GatewayTLS.CreatedAt, created)
	}
	if instance.Status.GatewayTLS.ClientCAFingerprint != caFingerprint("CA-B") {
		t.Errorf("ClientCAFingerprint = %q, want the new CA-B fingerprint recorded after the swap", instance.Status.GatewayTLS.ClientCAFingerprint)
	}
}

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

func TestBuildGatewayTLSCertificate_DefaultsToECDSAP384PKCS8NeverRotateAndServiceDNSNames(t *testing.T) {
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
	if pk.Algorithm != certmanagerv1.ECDSAKeyAlgorithm {
		t.Errorf("Algorithm = %q, want ECDSA (unset CertManagerSpec.Algorithm resolves to the CRD default)", pk.Algorithm)
	}
	if pk.Size != 384 {
		t.Errorf("Size = %d, want 384 (unset CertManagerSpec.Size resolves to the CRD default)", pk.Size)
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
