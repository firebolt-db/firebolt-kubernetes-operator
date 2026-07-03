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
	"fmt"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// AuthSigningKeyID is the JWT "kid" — and the Certificate/Secret name
// suffix — for Phase 1's single, non-rotating signing key. Rendered into
// every engine's instance.auth.local.signing_keys[].id (see
// engine_reconcile.go). A later rotation feature will mint additional,
// uniquely-suffixed IDs; this constant marks the one Phase 1 always uses.
const AuthSigningKeyID = "signing-1"

// authSigningCertDuration is set far beyond any practical operator or
// cluster lifetime so cert-manager does not renew the signing
// Certificate on its own schedule. packdb's SigningKeyManager reads
// signing keys only at process startup, so an uncoordinated renewal
// would make one engine sign tokens with a key its peers can't yet
// validate — see buildSigningCertificate's doc comment. Coordinated
// rotation is a planned addition (SigningKeyPolicy's doc comment), not
// yet implemented.
const authSigningCertDuration = 100 * 365 * 24 * time.Hour

// authSigningCertCommonName is the fixed X.509 subject used for every
// signing Certificate. Deliberately NOT derived from the instance name or
// namespace: RFC 5280's ub-common-name caps CommonName at 64 characters
// and cert-manager warns that exceeding it produces an invalid CSR,
// which realistic "<instance>-auth-signing.<namespace>" pairs can do.
// The value carries no meaning beyond satisfying "a valid Certificate
// requires at least one of CommonName, LiteralSubject, DNSName, or URI."
const authSigningCertCommonName = "firebolt-jwt-signing"

func signingCertificateName(instanceName string) string {
	return instanceName + SuffixAuthSigning
}

// ensureAuth provisions everything Instance-wide auth requires: it
// verifies the user-supplied admin password Secret exists (the operator
// never generates one — see AdminSpec's doc comment) and ensures the
// shared JWT signing keypair is provisioned via cert-manager, recording
// the result in instance.Status.Auth once ready.
//
// Unlike ensurePostgreSQL/ensureMetadataResources/ensureGatewayResources,
// a failure here must NOT block those components: none of them read
// spec.auth, and an instance with auth still provisioning (or
// misconfigured) should still bring up a usable metadata service and
// gateway. Reconcile logs this function's error and moves on; the
// AuthReady condition is the surfaced signal, not Reconcile's return
// value. Engines gate their own reconcile on Status.Auth being populated
// (see resolveInstanceInfo in engine_controller.go), so a still-
// provisioning auth branch simply delays engines starting, exactly like
// it delays nothing else.
func (r *FireboltInstanceReconciler) ensureAuth(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	auth := instance.Spec.Auth
	if auth == nil || !auth.Enabled {
		instance.Status.Auth = nil
		setInstanceCondition(instance, computev1alpha1.InstanceConditionAuthReady, metav1.ConditionTrue,
			"Disabled", "spec.auth is unset or disabled")
		return nil
	}

	// Defense-in-depth against a bypassed validating webhook, mirroring
	// validateInstanceTemplates' re-check of the pod-template rulesets:
	// the rest of this function assumes the invariants ValidateAuth
	// enforces (notably Local != nil whenever Enabled is true) and would
	// otherwise panic on a CR that reached the cluster without
	// admission.
	if errs := computev1alpha1.ValidateAuth(instance); len(errs) > 0 {
		err := errs.ToAggregate()
		setInstanceCondition(instance, computev1alpha1.InstanceConditionAuthReady, metav1.ConditionFalse,
			reasonAuthSpecInvalid, err.Error())
		return err
	}

	if err := r.checkAdminPasswordSecret(ctx, instance); err != nil {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionAuthReady, metav1.ConditionFalse,
			"AdminSecretMissing", err.Error())
		return err
	}

	ready, err := r.ensureSigningCertificate(ctx, instance)
	if err != nil {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionAuthReady, metav1.ConditionFalse,
			"SigningCertificateEnsureFailed", err.Error())
		return err
	}
	if !ready {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionAuthReady, metav1.ConditionFalse,
			"SigningKeyPending", "waiting for cert-manager to issue the JWT signing keypair")
		return nil
	}

	setInstanceCondition(instance, computev1alpha1.InstanceConditionAuthReady, metav1.ConditionTrue,
		"Ready", "admin credentials and JWT signing key are provisioned")
	return nil
}

// reasonAuthSpecInvalid mirrors reasonTemplateRejected's role for the
// pod-template defense-in-depth check, but for spec.auth.
const reasonAuthSpecInvalid = "AuthSpecInvalid"

// checkAdminPasswordSecret verifies the Secret referenced by
// spec.auth.local.admin.password exists and carries the configured key,
// without creating or modifying it. Mirrors checkExternalPostgresSecret's
// preflight shape.
func (r *FireboltInstanceReconciler) checkAdminPasswordSecret(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	ref := instance.Spec.Auth.Local.Admin.Password
	return checkSecretKeyPresent(ctx, r.Client, instance.Namespace, ref.Name, ref.Key, "admin password secret")
}

// checkSecretKeyPresent verifies a Secret exists in namespace and carries
// a non-empty value for key, without creating, modifying, or otherwise
// inspecting its contents. Shared by the instance controller's admin-
// password preflight (checkAdminPasswordSecret) and the engine
// controller's auth-secret preflight (resolveInstanceInfo in
// engine_controller.go) — both need the exact same "does the Secret this
// pod will mount actually exist and have the key it claims" check, on
// different reconciler receiver types. label names the Secret's role
// in error messages (e.g. "admin password secret", "signing key
// secret").
func checkSecretKeyPresent(ctx context.Context, cli client.Client, namespace, name, key, label string) error {
	var secret corev1.Secret
	err := cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret)
	if errors.IsNotFound(err) {
		return fmt.Errorf("%s %s/%s not found", label, namespace, name)
	}
	if err != nil {
		return fmt.Errorf("getting %s %s/%s: %w", label, namespace, name, err)
	}
	if len(secret.Data[key]) == 0 {
		return fmt.Errorf("%s %s/%s is missing or empty for key %q", label, namespace, name, key)
	}
	return nil
}

// ensureSigningCertificate applies the desired signing-key Certificate
// and, once cert-manager has issued it, records the resulting Secret in
// instance.Status.Auth. Returns ready=true only once the Secret exists
// and carries a non-empty tls.key.
//
// Secret presence plus a populated key is an adequate readiness proxy
// for a single non-rotating key — checking the Certificate's own Ready
// condition would be stricter but is not needed here, since nothing
// downstream reads this Certificate other than for its Secret.
//
// This function is deliberately not covered by the envtest suite: the
// envtest CRD set (config/crd/bases) does not include cert-manager's
// CRDs, so applying a Certificate would fail with "no matches for kind"
// against the fake apiserver. Coverage for the apply-and-issue path
// lives in e2e (test/testhelpers/utils.go already installs cert-manager
// for that suite); buildSigningCertificate below carries the
// unit-tested surface for this function's logic.
func (r *FireboltInstanceReconciler) ensureSigningCertificate(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	desired := buildSigningCertificate(instance)
	desired.TypeMeta = metav1.TypeMeta{APIVersion: certmanagerv1.SchemeGroupVersion.String(), Kind: "Certificate"}

	// GC note: the Certificate is cleaned up via this OwnerReference and
	// Kubernetes' built-in garbage collector, NOT via reconcileDelete's
	// manual label-based sweep — that sweep only covers core/apps/rbac
	// kinds envtest can always resolve, and unconditionally Listing a
	// cert-manager kind there would break every deletion test on a
	// cluster (like envtest) that has no cert-manager CRDs installed.
	// The target Secret cert-manager creates IS covered by that sweep,
	// because it carries LabelInstance via SecretTemplate.Labels above
	// and corev1.Secret is always resolvable.
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return false, err
	}
	if err := applySSA(ctx, r.Client, desired); err != nil {
		return false, fmt.Errorf("applying signing certificate: %w", err)
	}

	secretName := desired.Spec.SecretName
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: secretName}, &secret)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting signing key secret %s/%s: %w", instance.Namespace, secretName, err)
	}
	if len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		// cert-manager writes tls.crt/tls.key/ca.crt atomically once
		// issuance completes, so an existing Secret with no private key
		// means issuance is still in flight — not an error, just not
		// ready yet.
		return false, nil
	}

	createdAt := metav1.Now()
	if instance.Status.Auth != nil {
		for _, k := range instance.Status.Auth.SigningKeys {
			if k.ID == AuthSigningKeyID {
				createdAt = k.CreatedAt
				break
			}
		}
	}
	instance.Status.Auth = &computev1alpha1.AuthStatus{
		SigningKeys: []computev1alpha1.SigningKeyStatus{
			{ID: AuthSigningKeyID, SecretName: secretName, CreatedAt: createdAt},
		},
	}
	return true, nil
}

// buildSigningCertificate returns the desired cert-manager Certificate
// used to provision the JWT signing keypair every engine in this
// Instance shares. Kept as a pure function so its shape is unit-testable
// without envtest (see ensureSigningCertificate's doc comment for why
// envtest itself cannot exercise the apply).
//
// This Certificate is never used as a TLS server certificate — it exists
// solely so cert-manager mints and stores an X.509 keypair we then use
// for JWT signing. CommonName only needs to be a non-empty, syntactically
// valid subject; nothing validates it against a hostname.
//
// PrivateKey.Encoding is pinned to PKCS8: packdb loads signing keys with
// OpenSSL's generic PEM_read_bio_PrivateKey (SigningKeyManager.cpp),
// which parses PKCS1, SEC1, and PKCS8 transparently, but PKCS8 is the
// only one of the three that uses the same PEM header ("PRIVATE KEY")
// for both RSA and ECDSA — so a policy switch between the two algorithms
// here never trips a different, algorithm-specific parse path on the
// engine side. Verified against packdb source at plan time, not assumed.
//
// PrivateKey.RotationPolicy is pinned to Never, and Duration is set to
// authSigningCertDuration, so cert-manager does not renew this
// Certificate on its own — see that constant's doc comment for why an
// uncoordinated renewal would be unsafe.
func buildSigningCertificate(instance *computev1alpha1.FireboltInstance) *certmanagerv1.Certificate {
	policy := instance.Spec.Auth.Local.SigningKeys.CertManager
	name := signingCertificateName(instance.Name)
	labels := instanceLabels(instance.Name, "auth-signing")

	algorithm := certmanagerv1.RSAKeyAlgorithm
	if policy.Algorithm == "ECDSA" {
		algorithm = certmanagerv1.ECDSAKeyAlgorithm
	}

	issuerKind := resolveCertManagerIssuerKind(policy.IssuerRef.Kind)

	return &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: certmanagerv1.CertificateSpec{
			// A fixed, short subject rather than one derived from
			// instance/namespace names: RFC 5280's ub-common-name caps
			// CommonName at 64 characters, and cert-manager's own docs
			// warn issuance fails past that. instance+namespace name
			// pairs routinely exceed it, and the CN is semantically
			// meaningless here anyway — this Certificate exists only so
			// cert-manager mints a keypair, never as a TLS server cert.
			CommonName: authSigningCertCommonName,
			SecretName: name,
			// Copied onto the Secret cert-manager creates so the
			// instance controller's reconcileDelete label-based sweep
			// (which already deletes every Secret carrying LabelInstance)
			// cleans it up on instance deletion without any
			// cert-manager-specific GC code.
			SecretTemplate: &certmanagerv1.CertificateSecretTemplate{
				Labels: labels,
			},
			IssuerRef: cmmeta.IssuerReference{
				Name: policy.IssuerRef.Name,
				Kind: issuerKind,
			},
			Duration: &metav1.Duration{Duration: authSigningCertDuration},
			PrivateKey: &certmanagerv1.CertificatePrivateKey{
				RotationPolicy: certmanagerv1.RotationPolicyNever,
				Encoding:       certmanagerv1.PKCS8,
				Algorithm:      algorithm,
				Size:           int(policy.Size),
			},
		},
	}
}
