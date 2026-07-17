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
		// Deferred teardown: while the engine fleet is still draining off TLS
		// (some engine still serves it), keep Status.EngineTLS populated with
		// Reencrypting=true so the gateway retains the trust anchor and keeps
		// re-encrypting until every engine has rolled back to plaintext. Engines
		// roll based on spec (Enabled=false), not on Status, so keeping this
		// populated does not stall the drain — it only prevents the gateway from
		// dropping to plaintext while an engine still speaks TLS. See
		// engineFleetTLSState and engineUpstreamTLSReady.
		if instance.Status.EngineTLS != nil {
			_, anyOnTLS, err := r.engineFleetTLSState(ctx, instance)
			if err != nil {
				setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse,
					"DrainCheckFailed", err.Error())
				return err
			}
			if anyOnTLS {
				instance.Status.EngineTLS.Reencrypting = true
				setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse,
					"Draining", "engine TLS disabled; retaining the trust anchor while the engine fleet rolls back to plaintext")
				return nil
			}
		}
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

	// The trust anchor is provisioned; tell the gateway to re-encrypt only once
	// every engine has actually rolled onto a TLS-serving generation. Switching
	// earlier would break upstream handshakes against engines still selecting a
	// plaintext generation. See engineFleetTLSState / engineUpstreamTLSReady.
	allOnTLS, _, err := r.engineFleetTLSState(ctx, instance)
	if err != nil {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse,
			"ConvergenceCheckFailed", err.Error())
		return err
	}
	instance.Status.EngineTLS.Reencrypting = allOnTLS

	setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionTrue,
		"Ready", "engine TLS certificate is provisioned")
	return nil
}

// engineFleetTLSState inspects every FireboltEngine bound to this Instance (by
// spec.instanceRef) and reports the fleet's observed TLS-serving state via each
// engine's Status.ObservedEngineTLSHash (populated by computeStable):
//
//   - allOnTLS: there is at least one engine and every one has rolled onto the
//     current TLS-serving hash (derived from Status.EngineTLS.SecretName).
//     Meaningful only while Status.EngineTLS is populated.
//   - anyOnTLS: at least one engine still reports a non-empty engine-TLS hash,
//     i.e. is still serving TLS and has not yet drained to plaintext.
//
// With no engines the fleet is vacuously converged and nothing is serving TLS
// (true, false): a brand-new Instance may flip the gateway straight to TLS, and
// a disable with no engines tears the anchor down at once. Mirrors
// enginesConvergedOn's list-and-filter on spec.instanceRef (engines never carry
// a LabelInstance label).
func (r *FireboltInstanceReconciler) engineFleetTLSState(ctx context.Context, instance *computev1alpha1.FireboltInstance) (allOnTLS, anyOnTLS bool, err error) {
	var list computev1alpha1.FireboltEngineList
	if err := r.List(ctx, &list, client.InNamespace(instance.Namespace)); err != nil {
		return false, false, fmt.Errorf("listing engines for instance %s: %w", instance.Name, err)
	}
	expectedOn := ""
	if instance.Status.EngineTLS != nil {
		// Build the SAME ResolvedEngineTLSInfo the engine controller hashes
		// (resolveInstanceInfo, engine_controller.go), including the
		// cert-manager policy, so tlsHash matches on both sides — tlsHash now
		// folds the serving-cert alg/size, so omitting CertManager here would
		// make the gate never converge. Spec.TLS.Engine is non-nil in the
		// steady/enable state that allOnTLS is consumed in; during a disable
		// drain it may be nil, but only anyOnTLS (independent of expectedOn) is
		// read then, so falling back to no policy is safe.
		info := &ResolvedEngineTLSInfo{SecretName: instance.Status.EngineTLS.SecretName}
		if instance.Spec.TLS != nil && instance.Spec.TLS.Engine != nil {
			info.CertManager = instance.Spec.TLS.Engine.CertManager
		}
		expectedOn = tlsHash(info)
	}
	allOnTLS = expectedOn != ""
	sawEngine := false
	for i := range list.Items {
		if list.Items[i].Spec.InstanceRef != instance.Name {
			continue
		}
		sawEngine = true
		observed := list.Items[i].Status.ObservedEngineTLSHash
		if observed != "" {
			anyOnTLS = true
		}
		if observed != expectedOn {
			allOnTLS = false
		}
	}
	if !sawEngine {
		return true, false, nil
	}
	return allOnTLS, anyOnTLS, nil
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

// certificateReadyForCurrentGeneration reports whether a cert-manager
// Certificate is Ready AND that Ready status was observed for the
// Certificate's CURRENT generation. Checking only the Secret's key material
// (engineTLSSecretReady/gatewayTLSSecretReady) is not enough: after an
// issuer or DNS-SAN change, a re-issuance that then FAILS leaves the previous
// Secret in place, so a Secret-only check would keep reporting the listener
// Ready while cert-manager still serves the stale old certificate. Requiring
// the Ready condition's ObservedGeneration to match metadata.generation means
// a Ready=True left over from a prior successful issuance no longer counts
// once the desired spec has moved on. cert-manager tracks ObservedGeneration
// per condition (there is no top-level Status.ObservedGeneration), so it is
// read off the Ready condition itself.
func certificateReadyForCurrentGeneration(cert *certmanagerv1.Certificate) bool {
	for _, c := range cert.Status.Conditions {
		if c.Type == certmanagerv1.CertificateConditionReady {
			return c.Status == cmmeta.ConditionTrue && c.ObservedGeneration == cert.Generation
		}
	}
	return false
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

		// Cert-manager path only: require the Certificate to be Ready for its
		// current generation before trusting the Secret, so a failed
		// re-issuance cannot keep the previous certificate reported ready. A
		// BYO SecretRef has no Certificate object, so this check is scoped to
		// this branch. Returning not-ready here leaves Status.EngineTLS
		// untouched, so the still-valid old cert keeps serving while the
		// condition reports the re-issuance is pending.
		var cert certmanagerv1.Certificate
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: desired.Name}, &cert); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("getting engine TLS certificate %s/%s: %w", instance.Namespace, desired.Name, err)
		}
		if !certificateReadyForCurrentGeneration(&cert) {
			return false, nil
		}
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

		// Cert-manager path only: require the Certificate to be Ready for its
		// current generation (see certificateReadyForCurrentGeneration) so a
		// failed re-issuance after an issuer/DNS-SAN change cannot keep the
		// stale certificate reported ready. Scoped to this branch because a BYO
		// SecretRef has no Certificate. Returning not-ready leaves
		// Status.GatewayTLS untouched, so the still-valid old cert keeps serving
		// while the condition reports the re-issuance is pending — deliberately
		// the opposite of the fail-closed client-CA handling below, which is an
		// authorization control rather than a still-valid server certificate.
		var cert certmanagerv1.Certificate
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: desired.Name}, &cert); err != nil {
			if errors.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("getting gateway TLS certificate %s/%s: %w", instance.Namespace, desired.Name, err)
		}
		if !certificateReadyForCurrentGeneration(&cert) {
			return false, nil
		}
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

	// Mutual TLS: the client-CA Secret the Envoy pod will mount (and verify
	// client certs against) must already exist and carry ca.crt. Gating
	// Status.GatewayTLS on it keeps a single source of truth — once the
	// status is populated, buildListenerDownstreamTLSTransportSocket's
	// validation_context and the client-CA volume mount both have real files
	// to reference, and the fail-closed window covers the wait.
	if ref := instance.Spec.TLS.Gateway.ClientCASecretRef; ref != nil {
		//nolint:nilerr // a missing/incomplete client-CA Secret is a soft "not ready yet" (pending), not a hard error — mirrors the IsNotFound branch above.
		if _, err := checkSecretKeyPresent(ctx, r.Client, instance.Namespace, ref.Name, engineTLSCASecretKey, "gateway client-CA secret"); err != nil {
			// Fail closed: enabling mutual TLS means the operator intends to
			// REJECT clients without a valid certificate. Until the client-CA
			// Secret exists and carries ca.crt we must NOT leave a stale non-nil
			// Status.GatewayTLS standing — gatewayDownstreamTLSReady keys off it,
			// and a stale value keeps the previous one-way (or old-CA) Envoy pods
			// serving (maxUnavailable=0), still accepting the very clients the new
			// policy is meant to reject. Clearing it flips gatewayDownstreamTLSPending
			// on, so the fail-closed listener-omission path takes over instead.
			instance.Status.GatewayTLS = nil
			return false, nil
		}
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
