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

// defaultCertManagerIssuerKind is the cert-manager IssuerRef.Kind every
// Certificate-building function in this package falls back to when the
// user leaves CertManagerSpec.IssuerRef.Kind unset: a namespaced Issuer
// is the exception, not the norm, for an org-wide signing/TLS root, so
// ClusterIssuer is the more useful default.
const defaultCertManagerIssuerKind = "ClusterIssuer"

// resolveCertManagerIssuerKind returns kind unchanged when the user set
// it, or defaultCertManagerIssuerKind otherwise. Shared by every
// build*Certificate function (buildSigningCertificate, in instance_auth.go,
// plus the two below) so the "ClusterIssuer" default lives in exactly one
// place.
func resolveCertManagerIssuerKind(kind string) string {
	if kind == "" {
		return defaultCertManagerIssuerKind
	}
	return kind
}

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
		reason, msg := "CertificatePending", "waiting for cert-manager to issue the engine TLS certificate"
		if tls.Engine.SecretRef != nil {
			reason = "SecretPending"
			msg = "waiting for the referenced engine TLS secret to exist and carry tls.crt, tls.key, and ca.crt"
		}
		setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse, reason, msg)
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
	var secretName string
	if ref := instance.Spec.TLS.Engine.SecretRef; ref != nil {
		// Bring-your-own-Secret: the operator provisions nothing and owns
		// nothing here, it only consumes the user-supplied cert material.
		// The engineTLSSecretReady check below still applies (tls.crt,
		// tls.key, and ca.crt must all be present).
		secretName = ref.Name
	} else {
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
		secretName = desired.Spec.SecretName
	}

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

	issuerKind := resolveCertManagerIssuerKind(policy.IssuerRef.Kind)

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

// gatewayTLSCertDuration mirrors engineTLSCertDuration's reasoning: even
// though Envoy is not packdb (it may reload a filename-referenced TLS
// certificate on change rather than only at process startup), the
// operator has not verified that behavior against the exact Envoy version
// it ships, so cert-manager auto-renewal stays disabled here for the same
// "do not silently swap crypto material a running process hasn't been
// told about" reason. Coordinated rotation is a planned addition, not
// something to lean on an unverified hot-reload assumption for today.
const gatewayTLSCertDuration = 100 * 365 * 24 * time.Hour

func gatewayTLSCertificateName(instanceName string) string {
	return instanceName + SuffixGatewayTLS
}

// gatewayServiceDNSNames returns every in-cluster DNS form of the
// gateway's ClusterIP Service (see ensureGatewayService) that the
// operator can always compute without any user input: the bare name, the
// namespace-qualified name, and both full-FQDN suffixes. Covering all
// four means clients in the same namespace, clients in another namespace,
// and clients using the full FQDN all match a SAN on the certificate
// without needing to be told which form to use.
func gatewayServiceDNSNames(instance *computev1alpha1.FireboltInstance) []string {
	name := instance.Name + SuffixGateway
	return []string{
		name,
		name + "." + instance.Namespace,
		name + "." + instance.Namespace + ".svc",
		name + "." + instance.Namespace + ".svc.cluster.local",
	}
}

// gatewayTLSDNSNames returns the full SAN list for the gateway's
// downstream TLS certificate: the in-cluster Service names the operator
// always knows, plus any externally-visible names the user supplied via
// spec.tls.gateway.dnsNames (see TLSListenerSpec.DNSNames's doc comment
// for why the operator cannot derive those on its own).
func gatewayTLSDNSNames(instance *computev1alpha1.FireboltInstance) []string {
	names := gatewayServiceDNSNames(instance)
	return append(names, instance.Spec.TLS.Gateway.DNSNames...)
}

// ensureGatewayTLS provisions the gateway's downstream (client-facing)
// TLS server certificate via cert-manager, recording the result in
// instance.Status.GatewayTLS once ready. Mirrors ensureEngineTLS's shape
// and failure-isolation reasoning; must run before ensureGatewayResources
// in Reconcile since buildEnvoyConfigYAML and effectiveGatewayPodTemplate
// both read Status.GatewayTLS.
func (r *FireboltInstanceReconciler) ensureGatewayTLS(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	tls := instance.Spec.TLS
	if tls == nil || tls.Gateway == nil || !tls.Gateway.Enabled {
		instance.Status.GatewayTLS = nil
		setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionTrue,
			"Disabled", "spec.tls.gateway is unset or disabled")
		return nil
	}

	// Defense-in-depth against a bypassed validating webhook, mirroring
	// ensureEngineTLS's re-check of ValidateTLS.
	if errs := computev1alpha1.ValidateTLS(instance); len(errs) > 0 {
		err := errs.ToAggregate()
		setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionFalse,
			"TLSSpecInvalid", err.Error())
		return err
	}

	ready, err := r.ensureGatewayTLSCertificate(ctx, instance)
	if err != nil {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionFalse,
			"CertificateEnsureFailed", err.Error())
		return err
	}
	if !ready {
		reason, msg := "CertificatePending", "waiting for cert-manager to issue the gateway TLS certificate"
		if tls.Gateway.SecretRef != nil {
			reason = "SecretPending"
			msg = "waiting for the referenced gateway TLS secret to exist and carry tls.crt and tls.key"
		}
		setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionFalse, reason, msg)
		return nil
	}

	setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionTrue,
		"Ready", "gateway TLS certificate is provisioned")
	return nil
}

// gatewayTLSSecretReady reports whether secret carries what the gateway's
// downstream listener needs to terminate TLS: tls.crt and tls.key.
//
// Deliberately NOT the same three-key check as engineTLSSecretReady: that
// check additionally requires ca.crt because the gateway uses the engine
// TLS Secret to AUTHENTICATE A PEER (validating engine certificates when
// re-encrypting upstream). Here the gateway only PRESENTS this
// certificate to inbound clients; it never validates anything against it.
// Requiring ca.crt here would wedge every gateway TLS rollout in
// CertificatePending forever on any issuer that does not populate ca.crt
// (e.g. some ACME configurations) — a false failure with no corresponding
// hazard to guard against.
func gatewayTLSSecretReady(secret *corev1.Secret) bool {
	return len(secret.Data[corev1.TLSCertKey]) > 0 &&
		len(secret.Data[corev1.TLSPrivateKeyKey]) > 0
}

// ensureGatewayTLSCertificate applies the desired gateway-TLS Certificate
// and, once cert-manager has issued it, records the resulting Secret in
// instance.Status.GatewayTLS. Returns ready=true only once
// gatewayTLSSecretReady is satisfied.
//
// Not covered by the envtest suite for the same reason as
// ensureEngineTLSCertificate: envtest has no cert-manager CRDs installed.
// buildGatewayTLSCertificate and gatewayTLSSecretReady carry the
// unit-tested surface for this function's logic.
func (r *FireboltInstanceReconciler) ensureGatewayTLSCertificate(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	var secretName string
	if ref := instance.Spec.TLS.Gateway.SecretRef; ref != nil {
		// Bring-your-own-Secret: operator-consumed, not operator-owned. The
		// gatewayTLSSecretReady check below still applies (tls.crt + tls.key).
		secretName = ref.Name
	} else {
		desired := buildGatewayTLSCertificate(instance)
		desired.TypeMeta = metav1.TypeMeta{APIVersion: certmanagerv1.SchemeGroupVersion.String(), Kind: "Certificate"}

		if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
			return false, err
		}
		if err := applySSA(ctx, r.Client, desired); err != nil {
			return false, fmt.Errorf("applying gateway TLS certificate: %w", err)
		}
		secretName = desired.Spec.SecretName
	}

	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: secretName}, &secret)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting gateway TLS secret %s/%s: %w", instance.Namespace, secretName, err)
	}
	if !gatewayTLSSecretReady(&secret) {
		return false, nil
	}

	createdAt := metav1.Now()
	if instance.Status.GatewayTLS != nil {
		createdAt = instance.Status.GatewayTLS.CreatedAt
	}
	instance.Status.GatewayTLS = &computev1alpha1.GatewayTLSStatus{
		SecretName: secretName,
		CreatedAt:  createdAt,
	}
	return true, nil
}

// buildGatewayTLSCertificate returns the desired cert-manager Certificate
// used to provision the gateway's downstream TLS server certificate. Kept
// as a pure function so its shape is unit-testable without envtest (see
// ensureGatewayTLSCertificate's doc comment).
//
// DNSNames comes from gatewayTLSDNSNames: the in-cluster Service names
// plus any user-supplied external names. No CommonName is set, mirroring
// buildEngineTLSCertificate.
func buildGatewayTLSCertificate(instance *computev1alpha1.FireboltInstance) *certmanagerv1.Certificate {
	policy := instance.Spec.TLS.Gateway.CertManager
	name := gatewayTLSCertificateName(instance.Name)
	labels := instanceLabels(instance.Name, "gateway-tls")

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
			SecretName: name,
			SecretTemplate: &certmanagerv1.CertificateSecretTemplate{
				Labels: labels,
			},
			DNSNames:  gatewayTLSDNSNames(instance),
			Usages:    []certmanagerv1.KeyUsage{certmanagerv1.UsageServerAuth},
			IssuerRef: cmmeta.IssuerReference{Name: policy.IssuerRef.Name, Kind: issuerKind},
			Duration:  &metav1.Duration{Duration: gatewayTLSCertDuration},
			PrivateKey: &certmanagerv1.CertificatePrivateKey{
				RotationPolicy: certmanagerv1.RotationPolicyNever,
				Encoding:       certmanagerv1.PKCS8,
				Algorithm:      algorithm,
				Size:           int(policy.Size),
			},
		},
	}
}
