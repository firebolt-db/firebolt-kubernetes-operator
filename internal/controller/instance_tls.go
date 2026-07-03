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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// engineTLSCertDuration is set far beyond any practical operator or
// cluster lifetime so cert-manager does not renew the engine TLS
// Certificate on its own schedule, mirroring authSigningCertDuration.
// packdb's HTTP server (like SigningKeyManager) reads its configured
// certificate_file/private_key_file only at process startup, so an
// uncoordinated renewal would sit in the Secret unused until an engine
// pod happened to restart for an unrelated reason — silently diverging
// from what cert-manager believes is the live certificate. A future
// rotation feature would need to plumb the Secret's content (not just
// its name) into engine_reconcile.go's drift hash before this can safely
// shrink.
const engineTLSCertDuration = 100 * 365 * 24 * time.Hour

func engineTLSCertificateName(instanceName string) string {
	return instanceName + SuffixEngineTLS
}

// engineTLSWildcardDNSName returns the namespace-wide wildcard SAN that
// covers every engine's stable routing Service in this namespace
// (buildClusterService: "<engine>-service.<namespace>.svc.cluster.local"),
// regardless of how many engines exist or what they are named. A single
// static wildcard avoids having to track the Instance's current engine
// set and reissue the certificate as engines are added, renamed, or
// removed — the same "operator-generated, no user-visible rotation
// surface" simplicity SigningKeyPolicy chose for the JWT signing key.
func engineTLSWildcardDNSName(namespace string) string {
	return "*." + namespace + ".svc.cluster.local"
}

// ensureEngineTLS provisions the engine-listener TLS server certificate
// via cert-manager, recording the result in instance.Status.EngineTLS
// once ready. Mirrors ensureAuth's shape and failure-isolation reasoning:
// neither postgres/metadata nor (mostly) the gateway read spec.tls, so a
// failure here must not block those components. Unlike auth, the gateway
// DOES read Status.EngineTLS (to re-encrypt upstream to engines — see
// buildEnvoyConfigYAML), so this must run before ensureGatewayResources
// in Reconcile.
func (r *FireboltInstanceReconciler) ensureEngineTLS(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	tls := instance.Spec.TLS
	if tls == nil || tls.Engine == nil || !tls.Engine.Enabled {
		instance.Status.EngineTLS = nil
		setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionTrue,
			"Disabled", "spec.tls.engine is unset or disabled")
		return nil
	}

	// Defense-in-depth against a bypassed validating webhook, mirroring
	// ensureAuth's re-check of ValidateAuth.
	if errs := computev1alpha1.ValidateTLS(instance); len(errs) > 0 {
		err := errs.ToAggregate()
		setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse,
			"TLSSpecInvalid", err.Error())
		return err
	}

	ready, err := r.ensureEngineTLSCertificate(ctx, instance)
	if err != nil {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse,
			"CertificateEnsureFailed", err.Error())
		return err
	}
	if !ready {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse,
			"CertificatePending", "waiting for cert-manager to issue the engine TLS certificate")
		return nil
	}

	setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionTrue,
		"Ready", "engine TLS certificate is provisioned")
	return nil
}

// engineTLSSecretReady reports whether secret carries everything the
// engine-TLS feature needs: tls.crt and tls.key (the certificate itself)
// AND ca.crt (the issuing CA).
//
// ca.crt is required here, not just tls.crt/tls.key, because the
// gateway's dynamic_forward_proxy transport_socket
// (buildDFPUpstreamTLSTransportSocket, instance_gateway.go) points
// trusted_ca at this exact key, gated on the very Status.EngineTLS this
// check controls. If an issuer never populates ca.crt (some ACME
// configurations, as opposed to a CA-backed Issuer/ClusterIssuer),
// marking the feature ready anyway would wire the gateway to a file that
// doesn't exist in the mounted Secret — an Envoy config-load failure
// that fails EVERY gateway pod's readiness, and since health checks ride
// the same transport_socket, marks every engine unhealthy too. Staying
// in CertificatePending forever in that case is the correct, visible
// failure mode: the operator requires a CA-backed issuer for engine TLS
// specifically because the gateway must be able to verify engines.
func engineTLSSecretReady(secret *corev1.Secret) bool {
	return len(secret.Data[corev1.TLSCertKey]) > 0 &&
		len(secret.Data[corev1.TLSPrivateKeyKey]) > 0 &&
		len(secret.Data[engineTLSCASecretKey]) > 0
}

// ensureEngineTLSCertificate applies the desired engine-TLS Certificate
// and, once cert-manager has issued it, records the resulting Secret in
// instance.Status.EngineTLS. Returns ready=true only once
// engineTLSSecretReady is satisfied.
//
// Not covered by the envtest suite for the same reason as
// ensureSigningCertificate: envtest has no cert-manager CRDs installed.
// buildEngineTLSCertificate and engineTLSSecretReady carry the
// unit-tested surface for this function's logic.
func (r *FireboltInstanceReconciler) ensureEngineTLSCertificate(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	desired := buildEngineTLSCertificate(instance)
	desired.TypeMeta = metav1.TypeMeta{APIVersion: certmanagerv1.SchemeGroupVersion.String(), Kind: "Certificate"}

	// GC note: same as ensureSigningCertificate — OwnerReference +
	// Kubernetes' garbage collector, not the manual label-based sweep in
	// reconcileDelete.
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return false, err
	}
	if err := applySSA(ctx, r.Client, desired); err != nil {
		return false, fmt.Errorf("applying engine TLS certificate: %w", err)
	}

	secretName := desired.Spec.SecretName
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: secretName}, &secret)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting engine TLS secret %s/%s: %w", instance.Namespace, secretName, err)
	}
	if !engineTLSSecretReady(&secret) {
		return false, nil
	}

	createdAt := metav1.Now()
	if instance.Status.EngineTLS != nil {
		createdAt = instance.Status.EngineTLS.CreatedAt
	}
	instance.Status.EngineTLS = &computev1alpha1.EngineTLSStatus{
		SecretName: secretName,
		CreatedAt:  createdAt,
	}
	return true, nil
}

// buildEngineTLSCertificate returns the desired cert-manager Certificate
// used to provision the TLS server certificate every engine in this
// Instance shares. Kept as a pure function so its shape is unit-testable
// without envtest (see ensureEngineTLSCertificate's doc comment).
//
// DNSNames carries only the namespace-wide wildcard (see
// engineTLSWildcardDNSName) plus "localhost": the wildcard covers every
// engine's stable routing Service that the gateway and external clients
// connect to, and "localhost" covers the engine web UI sidecar's
// same-pod loopback connection (see EngineWebBackendURL). No CommonName
// is set — DNSNames alone satisfies cert-manager's "at least one
// identity" requirement, and a SAN-only certificate is the modern (post
// CN-deprecation) convention for TLS server certs.
//
// Usages is pinned to ServerAuth: unlike buildSigningCertificate's
// Certificate (never presented in a TLS handshake), this one is, so an
// issuer enforcing strict key-usage/EKU policy needs the explicit hint.
//
// PrivateKey.Encoding/RotationPolicy/Duration mirror
// buildSigningCertificate's choices verbatim — see
// engineTLSCertDuration's doc comment for why disabling auto-renewal is
// the safe default here too, even though (unlike the signing key) a
// renewed TLS cert has no cross-engine validation hazard.
func buildEngineTLSCertificate(instance *computev1alpha1.FireboltInstance) *certmanagerv1.Certificate {
	policy := instance.Spec.TLS.Engine.CertManager
	name := engineTLSCertificateName(instance.Name)
	labels := instanceLabels(instance.Name, "engine-tls")

	algorithm := certmanagerv1.RSAKeyAlgorithm
	if policy.Algorithm == "ECDSA" {
		algorithm = certmanagerv1.ECDSAKeyAlgorithm
	}

	issuerKind := policy.IssuerRef.Kind
	if issuerKind == "" {
		issuerKind = "ClusterIssuer"
	}

	return &certmanagerv1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: certmanagerv1.CertificateSpec{
			SecretName: name,
			// Copied onto the Secret cert-manager creates so the
			// instance controller's reconcileDelete label-based sweep
			// cleans it up on instance deletion, same as the signing
			// certificate's SecretTemplate.
			SecretTemplate: &certmanagerv1.CertificateSecretTemplate{
				Labels: labels,
			},
			DNSNames: []string{
				engineTLSWildcardDNSName(instance.Namespace),
				"localhost",
			},
			Usages: []certmanagerv1.KeyUsage{certmanagerv1.UsageServerAuth},
			IssuerRef: cmmeta.IssuerReference{
				Name: policy.IssuerRef.Name,
				Kind: issuerKind,
			},
			Duration: &metav1.Duration{Duration: engineTLSCertDuration},
			PrivateKey: &certmanagerv1.CertificatePrivateKey{
				RotationPolicy: certmanagerv1.RotationPolicyNever,
				Encoding:       certmanagerv1.PKCS8,
				Algorithm:      algorithm,
				Size:           int(policy.Size),
			},
		},
	}
}
