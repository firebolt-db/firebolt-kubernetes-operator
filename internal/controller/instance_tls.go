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
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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

	// FB-896 #4: the anchor certificate is provisioned, but the gateway→engine hop
	// stays plaintext until every engine has rolled onto a TLS-serving generation
	// (allOnTLS, mirrored into Reencrypting above). EngineTLSReady feeds the
	// Instance Ready roll-up, so report True only once the fleet has actually
	// converged — otherwise the Instance would advertise Ready over a plaintext
	// upstream. This does NOT deadlock the enable ramp: the engine roll that
	// produces convergence is unblocked by the provisioned fact
	// (Status.EngineTLS != nil) in resolveInstanceInfo, not by this condition.
	// With no engines the fleet is vacuously converged (engineFleetTLSState
	// returns allOnTLS=true), so a brand-new instance still reports Ready.
	if !allOnTLS {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionFalse,
			"Converging", "engine TLS certificate provisioned; waiting for the engine fleet to roll onto TLS")
		return nil
	}

	setInstanceCondition(instance, computev1alpha1.InstanceConditionEngineTLSReady, metav1.ConditionTrue,
		"Ready", "engine TLS certificate is provisioned and the fleet is re-encrypting")
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

	alg, size := resolvedCertManagerAlgSize(*policy)
	algorithm := certmanagerv1.RSAKeyAlgorithm
	if alg == "ECDSA" {
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
				Size:           int(size),
			},
		},
	}
}

// engineCABundleSecretName is the operator-owned Secret the gateway mounts as
// its engine trusted_ca (FB-896 #4). See SuffixEngineCABundle.
func engineCABundleSecretName(instanceName string) string {
	return instanceName + SuffixEngineCABundle
}

// AnnotationEngineTrustCAs records, on the engine CA bundle Secret, the sorted
// SHA-256 fingerprint set the bundle physically contains. It is the source of
// truth ensureEngineCABundle reports on a preserve pass (when it declines to
// rewrite the bundle), so the caller can never publish a Status.RolledEngineTrustCAs
// that claims a CA the bundle does not actually carry. Deterministic (sorted,
// comma-joined), so it changes only when the trusted CA set changes — preserving
// the "byte-identical output across reconciles ⇒ no spurious gateway rollout"
// property dedupCABlobs already gives the ca.crt bytes.
const AnnotationEngineTrustCAs = "firebolt.io/engine-trust-cas"

// ensureEngineCABundle assembles the gateway's engine trust bundle: the union
// of every distinct CA certificate currently signing a live engine
// generation's TLS Secret. It writes the concatenation to the operator-owned
// Secret engineCABundleSecretName(instance) under ca.crt, which the gateway pod
// mounts instead of a single anchor ca.crt, and returns the SHA-256 fingerprints
// (hex) of the CAs — the set the caller publishes to Status.RolledEngineTrustCAs
// once the gateway has rolled it out, so the engine cutover gate can confirm a
// generation's CA is trusted.
//
// The returned set is ALWAYS the fingerprint set the bundle Secret physically
// contains after this call: the freshly-written set on a write, or the preserved
// set (read back from AnnotationEngineTrustCAs) on a preserve. It is never the
// possibly-stale Status.RolledEngineTrustCAs — publishing that could claim a CA
// the bundle no longer carries (over-claim), letting a later generation signed by
// a since-pruned CA pass the cutover gate while the gateway trusts only the
// reduced set, which then fails the upstream handshake.
//
// Why a bundle: the gateway verifies engines against a CA, but the CA material
// behind an issuer can rotate even though the issuer *name* is immutable. A
// per-generation cert minted after such a rotation is signed by the NEW CA while
// an older, still-live generation carries the OLD one; trusting only one would
// make the gateway reject either the old or the new generation, so an ordinary
// later rollout could make an engine unreachable. Trusting the union lets both
// verify during the overlap. Because the bundle is reassembled from *live*
// generations every reconcile, a CA drops out on its own once the last
// generation using it retires (self-pruning) — no separate convergence
// bookkeeping.
//
// The long-lived anchor (Status.EngineTLS.SecretName) is deliberately excluded
// from the union (FB-896 #3): with rotationPolicy:Never and a ~100yr lifetime its
// ca.crt is pinned to the CA that signed it, so seeding it unconditionally would
// keep a retired CA trusted indefinitely, contradicting the self-pruning above.
// It is used only as a fallback when no live per-generation CA is discoverable in
// a given pass, so the bundle is never emptied out from under a re-encrypting fleet.
//
// Bound, not eliminated: a new CA is only discoverable once a generation cert
// signed by it exists, so the bundle expands and the gateway rolls only after
// that — new-CA certs are rejected for one gateway roll. Coordinating that with
// engine retirement is handled separately (see enginesTrustBundleConverged /
// the engine cutover gate).
//
// ca.crt is a public certificate, so deduping it raises no
// weak-sensitive-data-hashing concern (unlike private key material). A live
// generation's Secret that is transiently NotFound or keyless is not silently
// dropped: it marks the read incomplete. The write-guard is anchored on the CA
// set the bundle PHYSICALLY serves, recorded in AnnotationEngineTrustCAs — an
// incomplete read still WRITES a pure addition (assembled set ⊇ served set) so a
// new CA from one engine is not blocked by an unrelated engine's transient gap,
// but it never prunes a served CA. A legacy bundle without the annotation has an
// unknown served set, so it is rewritten only by a complete read (which stamps
// the annotation and makes all later reads safe); an incomplete read preserves it
// untouched. An empty or reduced trusted_ca would fail every gateway pod's
// readiness or break a still-serving generation's handshake. Runs only while
// engine upstream TLS is engaged (engineUpstreamTLSReady); otherwise the gateway
// mounts no engine CA and there is nothing to maintain.
func (r *FireboltInstanceReconciler) ensureEngineCABundle(ctx context.Context, instance *computev1alpha1.FireboltInstance) ([]string, error) {
	if !engineUpstreamTLSReady(instance) {
		return nil, nil
	}

	// Every live generation's per-engine TLS Secret across the fleet — the CAs
	// signing the certs engines actually serve. The anchor is deliberately NOT
	// seeded here (FB-896 #3): it is rotationPolicy:Never with a ~100yr lifetime,
	// so its ca.crt is pinned to whatever CA signed it at creation, and keeping it
	// would leave a retired CA in the bundle forever after its generations drain —
	// defeating the self-pruning below. It serves only as a fallback when no live
	// per-generation CA is discoverable this pass.
	var names []string
	var engines computev1alpha1.FireboltEngineList
	if err := r.List(ctx, &engines, client.InNamespace(instance.Namespace)); err != nil {
		return nil, fmt.Errorf("listing engines for CA bundle: %w", err)
	}
	for i := range engines.Items {
		e := &engines.Items[i]
		if e.Spec.InstanceRef != instance.Name {
			continue
		}
		for _, gen := range liveEngineGenerations(e) {
			names = append(names, genResourceName(e.Name, gen, SuffixEngineTLS))
		}
	}

	cas, complete, err := r.collectCACerts(ctx, instance.Namespace, names)
	if err != nil {
		return nil, err
	}
	if len(cas) == 0 {
		// Fallback: no live per-generation CA readable this pass (e.g. a transient
		// gap between generations). Keep the anchor so the gateway's trust bundle is
		// never emptied out from under a fleet that is still re-encrypting upstream.
		// The fallback read's own completeness is intentionally discarded: the
		// live-generation read's `complete` above governs the write/preserve decision
		// below (the anchor is only a floor, never a reason to prune).
		cas, _, err = r.collectCACerts(ctx, instance.Namespace, []string{instance.Status.EngineTLS.SecretName})
		if err != nil {
			return nil, err
		}
	}

	blobs := dedupCABlobs(cas)
	newFPs := caFingerprintsSorted(blobs)

	// servedFPs is the CA set the bundle Secret PHYSICALLY carries right now, read
	// from its annotation — the write-guard's anchor. It must be the physical set,
	// NOT Status.RolledEngineTrustCAs: that Status field is cleared on the legacy
	// preserve path below, and anchoring on it would make a later incomplete read's
	// subset check vacuously true and prune a still-live CA.
	servedFPs, servedKnown, bundlePresent, err := r.servedEngineTrustCAs(ctx, instance)
	if err != nil {
		return nil, err
	}

	// Gate the write so a transiently-incomplete read can never drop a physically
	// served CA, yet genuine additions still land:
	//   - complete read: authoritative full observation → write (legit
	//     shrink-on-retire proceeds; retired generations are absent from names, not
	//     "missing"). This also stamps the annotation, healing a legacy bundle.
	//   - no bundle yet: nothing to drop → write.
	//   - annotated bundle + incomplete + newFPs ⊇ servedFPs: a pure addition
	//     relative to what is physically served → safe → write (a new CA from one
	//     engine lands even while an unrelated gen's Secret is transiently missing).
	//   - legacy bundle present but NOT annotated + incomplete: the served set is
	//     UNKNOWN, so a write cannot be proven non-pruning → PRESERVE.
	write := len(blobs) > 0 && (complete || !bundlePresent || (servedKnown && engineTrustCAsSubset(servedFPs, newFPs)))
	if !write {
		// Preserve: report the bundle's physical set so a published
		// Status.RolledEngineTrustCAs never claims a CA the bundle does not carry.
		if servedKnown {
			return servedFPs, nil
		}
		// No verifiable served set — either no bundle at all, or a legacy bundle
		// without AnnotationEngineTrustCAs whose "\n"-joined ca.crt cannot be split
		// back into per-CA fingerprints, so its true set is unknowable. Publish
		// nothing (nil) rather than re-confirm a possibly-stale Status, which could
		// over-claim a CA the physical bundle no longer carries; a later complete
		// read authors the annotation and republishes. In practice this operator
		// writes the bundle and its annotation together, so an unannotated bundle
		// only arises from a pre-release build. Clearing Status here cannot re-arm a
		// prune: the write-guard above routes a legacy bundle on an incomplete read
		// to PRESERVE regardless of Status, so no pruning write follows.
		return nil, nil
	}

	desired := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        engineCABundleSecretName(instance.Name),
			Namespace:   instance.Namespace,
			Labels:      instanceLabels(instance.Name, "engine-ca-bundle"),
			Annotations: map[string]string{AnnotationEngineTrustCAs: strings.Join(newFPs, ",")},
		},
		Data: map[string][]byte{engineTLSCASecretKey: caBundleBytes(blobs)},
	}
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return nil, err
	}
	if err := applySSA(ctx, r.Client, desired); err != nil {
		return nil, fmt.Errorf("applying engine CA bundle secret: %w", err)
	}

	return newFPs, nil
}

// collectCACerts reads the ca.crt from each named Secret in namespace. complete
// reports whether EVERY requested name was read with a non-empty ca.crt: a name
// that is NotFound or carries no ca.crt is skipped AND clears complete, so the
// caller can tell a genuine set (all names read) apart from a transiently partial
// one and refuse to publish a pruned bundle. A hard Get error (other than
// NotFound) is surfaced instead. With no names, complete is vacuously true.
func (r *FireboltInstanceReconciler) collectCACerts(ctx context.Context, namespace string, names []string) (cas [][]byte, complete bool, err error) {
	complete = true
	for _, name := range names {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
			if errors.IsNotFound(err) {
				complete = false
				continue
			}
			return nil, false, fmt.Errorf("reading CA secret %s/%s for engine trust bundle: %w", namespace, name, err)
		}
		if ca := secret.Data[engineTLSCASecretKey]; len(ca) > 0 {
			cas = append(cas, ca)
		} else {
			complete = false
		}
	}
	return cas, complete, nil
}

// servedEngineTrustCAs reports the CA set the existing engine CA bundle Secret
// PHYSICALLY carries, in three distinguishable states so ensureEngineCABundle can
// anchor its write-guard on physical truth rather than the mutable Status:
//
//   - bundle absent (or empty ca.crt): known=false, bundlePresent=false, fps=nil.
//   - bundle present WITH AnnotationEngineTrustCAs: known=true, bundlePresent=true,
//     fps=the recorded set.
//   - bundle present WITHOUT the annotation (a legacy/pre-upgrade bundle): the
//     served set is UNKNOWN, so known=false, bundlePresent=true, fps=nil.
//
// The set is read from the annotation, never parsed from the "\n"-joined ca.crt —
// that concatenation cannot be split back into per-CA blobs (a blob may be a
// multi-line chain). A legacy bundle is therefore unreadable as a set until a
// complete read rewrites it and stamps the annotation.
func (r *FireboltInstanceReconciler) servedEngineTrustCAs(ctx context.Context, instance *computev1alpha1.FireboltInstance) (fps []string, known, bundlePresent bool, err error) {
	name := engineCABundleSecretName(instance.Name)
	var s corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: name}, &s); err != nil {
		if errors.IsNotFound(err) {
			return nil, false, false, nil
		}
		return nil, false, false, fmt.Errorf("reading existing engine CA bundle %s/%s: %w", instance.Namespace, name, err)
	}
	if len(s.Data[engineTLSCASecretKey]) == 0 {
		return nil, false, false, nil
	}
	ann := s.Annotations[AnnotationEngineTrustCAs]
	if ann == "" {
		return nil, false, true, nil
	}
	return strings.Split(ann, ","), true, true, nil
}

// caFingerprintsSorted returns the deterministic, sorted SHA-256 fingerprint set
// of the deduped CA blobs — the value stamped into AnnotationEngineTrustCAs and
// reported to the caller on a write.
func caFingerprintsSorted(blobs []string) []string {
	fps := make([]string, len(blobs))
	for i, b := range blobs {
		fps[i] = caFingerprint(b)
	}
	sort.Strings(fps)
	return fps
}

// engineTrustCAsSubset reports whether every fingerprint in a is present in b.
func engineTrustCAsSubset(a, b []string) bool {
	set := make(map[string]struct{}, len(b))
	for _, fp := range b {
		set[fp] = struct{}{}
	}
	for _, fp := range a {
		if _, ok := set[fp]; !ok {
			return false
		}
	}
	return true
}

// liveEngineGenerations returns the distinct, nonzero generation numbers an
// engine currently has resources for: the latest created, the active, and any
// draining generation. A CA signing any of these must stay in the gateway
// trust bundle until the generation retires.
func liveEngineGenerations(e *computev1alpha1.FireboltEngine) []int {
	seen := map[int]bool{}
	var out []int
	add := func(g int) {
		if g > 0 && !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	add(e.Status.CurrentGeneration)
	add(e.Status.ActiveGeneration)
	if e.Status.DrainingGeneration != nil {
		add(*e.Status.DrainingGeneration)
	}
	return out
}

// dedupCABlobs trims each ca.crt input, drops empties and duplicates, and
// returns the distinct blobs in a deterministic (sorted) order. Whole-blob
// dedup, not per-certificate: each ca.crt is one issuer's CA (chain), so
// identical CAs across the anchor and every generation collapse to one entry in
// the common no-rotation case, while a rotated CA yields a distinct blob that
// coexists. Sorted output means an unchanged CA set yields byte-identical
// results across reconciles — so the bundle Secret's ResourceVersion (folded
// into the gateway config hash) and the published fingerprint set only change
// when the trusted CA set actually changes, avoiding spurious gateway rollouts.
func dedupCABlobs(inputs [][]byte) []string {
	seen := map[string]struct{}{}
	var blobs []string
	for _, in := range inputs {
		b := strings.TrimSpace(string(in))
		if b == "" {
			continue
		}
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = struct{}{}
		blobs = append(blobs, b)
	}
	sort.Strings(blobs)
	return blobs
}

// caBundleBytes joins deduped CA blobs into a single PEM bundle. Envoy's
// trusted_ca accepts concatenated PEMs.
func caBundleBytes(blobs []string) []byte {
	return []byte(strings.Join(blobs, "\n"))
}

// caFingerprint is the hex SHA-256 of a trimmed CA blob — the stable, public
// identity published in Status.RolledEngineTrustCAs and matched by the engine
// cutover gate. Computed over the same trimmed bytes on both sides
// (dedupCABlobs here, the per-generation ca.crt in the engine controller) so
// the same CA always yields the same fingerprint. Never hashes key material.
func caFingerprint(blob string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(blob)))
	return hex.EncodeToString(sum[:])
}

// signingKeyPublicKeyFingerprint returns the hex SHA-256 of the DER
// SubjectPublicKeyInfo parsed from a signing key's tls.crt. It fingerprints the
// PUBLIC KEY, not the whole certificate, so the value changes only when the
// private key is actually replaced: under rotationPolicy:Never a cert-only
// reissuance (an issuer-capped lifetime, or a manual `cmctl renew`) rewrites
// tls.crt — new serial/notAfter, bumped Certificate.Status.Revision — while
// reusing the same key, leaving the public key and this fingerprint unchanged.
// Public material only; the private key (tls.key) is never read or hashed, so
// this raises no weak-sensitive-data-hashing concern (same rationale as
// caFingerprint). Returns an error when tls.crt is absent or unparseable.
func signingKeyPublicKeyFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", fmt.Errorf("signing certificate: no PEM block found in %d bytes", len(certPEM))
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parsing signing certificate: %w", err)
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return "", fmt.Errorf("marshaling signing public key: %w", err)
	}
	sum := sha256.Sum256(spki)
	return hex.EncodeToString(sum[:]), nil
}

// mergeCACerts assembles the deduped, deterministic PEM trust bundle from raw
// ca.crt inputs. Thin composition of dedupCABlobs + caBundleBytes.
func mergeCACerts(inputs [][]byte) []byte {
	return caBundleBytes(dedupCABlobs(inputs))
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

	ready, staging, err := r.ensureGatewayTLSCertificate(ctx, instance)
	if err != nil {
		setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionFalse,
			"CertificateEnsureFailed", err.Error())
		return err
	}
	if staging {
		// FB-896 #1: cert material is ready, but the gateway is being tightened
		// (plaintext→TLS or one-way→mTLS) and is intentionally serving fail-closed
		// until the previous, looser pods are fully replaced. Report not-ready with
		// a distinct reason rather than the generic pending message.
		setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionFalse,
			"StagingFailClosed", "tightening gateway TLS posture: serving fail-closed until the previous listener's pods are fully replaced")
		return nil
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

	// FB-896 #5: the certificate is ready and Status.GatewayTLS now records the
	// desired secure posture, but ensureGatewayResources has not yet rolled that
	// secure config out this reconcile — the gateway may still be serving the
	// prior fail-closed (or looser) pods, whose metrics-port readiness keeps
	// GatewayReady>0. Withhold GatewayTLSReady until the gateway is actually
	// serving the secure config, so the Instance Ready roll-up cannot advertise
	// Ready while the client port still rejects. This also handles an invalid BYO
	// certificate that never loads: its secure pods never become Ready, so
	// gatewayServingCurrentConfig stays false and the Instance stays not-ready
	// instead of reporting Ready forever with a dead client port.
	//
	// Scope this to the transition OUT of a not-ready state only: once
	// GatewayTLSReady is True the secure config has served at least once, and a
	// later zero-downtime renewal roll (secure→secure, maxUnavailable=0) keeps old
	// secure pods serving throughout — dropping back to SecureRolloutPending then
	// would flap the Instance out of Ready for no hazard. A tightening transition
	// resets the condition to False (StagingFailClosed) above, so a genuine
	// re-tighten still re-gates here; a post-Ready gateway outage is caught by the
	// separate GatewayReady component of the roll-up.
	if !apimeta.IsStatusConditionTrue(instance.Status.Conditions, computev1alpha1.InstanceConditionGatewayTLSReady) {
		serving, err := r.gatewayServingCurrentConfig(ctx, instance)
		if err != nil {
			setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionFalse,
				"CertificateEnsureFailed", err.Error())
			return err
		}
		if !serving {
			setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionFalse,
				"SecureRolloutPending", "gateway TLS certificate provisioned; waiting for the secure listener to finish rolling out")
			return nil
		}
	}

	setInstanceCondition(instance, computev1alpha1.InstanceConditionGatewayTLSReady, metav1.ConditionTrue,
		"Ready", "gateway TLS certificate is provisioned and serving")
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
// instance.Status.GatewayTLS. Returns ready=true once gatewayTLSSecretReady is
// satisfied AND — on a tightening posture transition — the fail-closed rollout
// has completed and Status.GatewayTLS has been populated with the desired secure
// posture. ready=true means "the status now describes the secure config"; it is
// NOT the same as "the secure config is serving" — the caller (ensureGatewayTLS)
// gates GatewayTLSReady on gatewayServingCurrentConfig separately (FB-896 #5),
// because this function runs before ensureGatewayResources rolls the secure
// config out.
//
// staging=true reports the FB-896 #1 window: the desired client-facing posture
// is stricter than what the gateway is currently serving (plaintext→TLS,
// one-way→mTLS, or a client-CA replacement — FB-896 #2). The tightening decision
// is made and fail-closed enforced BEFORE the cert/secret readiness gates, so a
// tighten that coincides with a server-cert reissuance still stops the old,
// looser listener rather than fail-open while the new cert is issued.
// Populating Status.GatewayTLS immediately would render the secure
// listener; the gateway would then roll, and because the client-facing Service
// keeps one selector, old looser pods stay endpoints during the roll — accepting
// the very clients the tighter posture is meant to reject (fail-open). Instead we
// hold Status.GatewayTLS nil (→ gatewayDownstreamTLSPending →
// buildFailClosedEnvoyConfigYAML omits the listener, and gatewayRollingUpdateStrategy
// drains old pods before new ones start) until gatewayServingCurrentConfig confirms
// the fail-closed config is fully serving, then serve secure. A brief reject-all
// window during a deliberate tighten is the accepted trade for never being fail-open.
//
// Not covered by the envtest suite for the same reason as
// ensureEngineTLSCertificate: envtest has no cert-manager CRDs installed.
// buildGatewayTLSCertificate and gatewayTLSSecretReady carry the
// unit-tested surface for this function's logic.
func (r *FireboltInstanceReconciler) ensureGatewayTLSCertificate(ctx context.Context, instance *computev1alpha1.FireboltInstance) (ready, staging bool, err error) {
	var secretName, certName string
	certManagerPath := false
	if ref := instance.Spec.TLS.Gateway.SecretRef; ref != nil {
		// Bring-your-own-Secret: operator-consumed, not operator-owned. The
		// gatewayTLSSecretReady check below still applies (tls.crt + tls.key).
		secretName = ref.Name
	} else {
		desired := buildGatewayTLSCertificate(instance)
		desired.TypeMeta = metav1.TypeMeta{APIVersion: certmanagerv1.SchemeGroupVersion.String(), Kind: "Certificate"}

		if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
			return false, false, err
		}
		// Apply unconditionally, up front: a tightening transition must keep
		// driving the (re)issuance even while it stages fail-closed below, so the
		// new cert is ready by the time the fail-closed rollout completes.
		if err := applySSA(ctx, r.Client, desired); err != nil {
			return false, false, fmt.Errorf("applying gateway TLS certificate: %w", err)
		}
		secretName = desired.Spec.SecretName
		certName = desired.Name
		certManagerPath = true
	}

	// Mutual TLS: the client-CA Secret the Envoy pod will mount (and verify
	// client certs against) must already exist and carry ca.crt. Resolved here,
	// ahead of the cert/secret readiness gates, so the tightening decision below
	// is made — and fail-closed enforced — even while the server cert is
	// mid-reissue (FB-896 #1).
	var desiredClientCAFP string
	if ref := instance.Spec.TLS.Gateway.ClientCASecretRef; ref != nil {
		var caSecret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: ref.Name}, &caSecret); err != nil {
			if !errors.IsNotFound(err) {
				return false, false, fmt.Errorf("getting gateway client-CA secret %s/%s: %w", instance.Namespace, ref.Name, err)
			}
			caSecret = corev1.Secret{}
		}
		ca := caSecret.Data[engineTLSCASecretKey]
		if len(ca) == 0 {
			// Fail closed: enabling mutual TLS means the operator intends to
			// REJECT clients without a valid certificate. Until the client-CA
			// Secret exists and carries ca.crt we must NOT leave a stale non-nil
			// Status.GatewayTLS standing — gatewayDownstreamTLSReady keys off it,
			// and a stale value keeps the previous one-way (or old-CA) Envoy pods
			// serving, still accepting the very clients the new policy is meant to
			// reject. Clearing it flips gatewayDownstreamTLSPending on, so the
			// fail-closed listener-omission path (and the aggressive rollout that
			// drains old pods first — gatewayRollingUpdateStrategy) takes over.
			instance.Status.GatewayTLS = nil
			return false, false, nil
		}
		// FB-896 #2: fingerprint the client CA (a public cert — safe to hash) so a
		// replacement CA-A→CA-B, which keeps the mode MutualTLS but retires trust
		// in CA-A, is recognized as a tightening transition below.
		desiredClientCAFP = caFingerprint(string(ca))
	}

	// FB-896 #1/#2: decide whether the desired posture is a *tightening* of what
	// the gateway currently serves, using the CURRENT (pre-mutation) status, and
	// capture the prior CreatedAt before any status mutation.
	//
	// A client-CA replacement (CA-A→CA-B) keeps the posture ordinal at MutualTLS
	// on both sides, so gatewayPostureTightening does not see it; compare the
	// recorded served fingerprint to catch it as a tightening too (#2). A blank
	// recorded fingerprint (a status written before this field existed) is treated
	// as "unknown, matches" — it is simply recorded on this pass rather than
	// forcing a one-time fail-closed restage of every already-serving mTLS gateway
	// on operator upgrade; genuine swaps are caught once the fingerprint is stored.
	clientCASwapped := gatewayDesiredMode(instance) == computev1alpha1.GatewayTLSModeMutual &&
		gatewayServedMode(instance) == computev1alpha1.GatewayTLSModeMutual &&
		instance.Status.GatewayTLS != nil &&
		instance.Status.GatewayTLS.ClientCAFingerprint != "" &&
		instance.Status.GatewayTLS.ClientCAFingerprint != desiredClientCAFP
	tightening := gatewayPostureTightening(instance) || clientCASwapped

	createdAt := metav1.Now()
	if instance.Status.GatewayTLS != nil {
		createdAt = instance.Status.GatewayTLS.CreatedAt
	}

	// A tightening transition forces fail-closed BEFORE the cert/secret readiness
	// gates: clear the status so the secure listener is withheld and — even if the
	// server cert is mid-reissue — the old, looser listener stops accepting the
	// clients the tighter posture is meant to reject. Stage until the gateway is
	// actually serving that fail-closed config: gatewayServingCurrentConfig (with
	// the status now nil) confirms both that the running pod template IS the
	// fail-closed one and that its rollout is complete. Loosening/steady
	// transitions skip this.
	if tightening {
		instance.Status.GatewayTLS = nil
		rolled, err := r.gatewayServingCurrentConfig(ctx, instance)
		if err != nil {
			return false, false, err
		}
		if !rolled {
			return false, true, nil
		}
	}

	// Cert-manager path only: require the Certificate to be Ready for its current
	// generation (see certificateReadyForCurrentGeneration) so a failed re-issuance
	// after an issuer/DNS-SAN change cannot keep the stale certificate reported
	// ready. Scoped to this branch because a BYO SecretRef has no Certificate.
	// When NOT tightening this early-return leaves Status.GatewayTLS untouched, so
	// the still-valid old cert keeps serving while the condition reports the
	// re-issuance is pending — deliberately the opposite of the fail-closed
	// client-CA handling above. On a tightening reissuance the status has already
	// been cleared (fail-closed), so returning here holds that closed posture.
	if certManagerPath {
		var cert certmanagerv1.Certificate
		if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: certName}, &cert); err != nil {
			if errors.IsNotFound(err) {
				return false, false, nil
			}
			return false, false, fmt.Errorf("getting gateway TLS certificate %s/%s: %w", instance.Namespace, certName, err)
		}
		if !certificateReadyForCurrentGeneration(&cert) {
			return false, false, nil
		}
	}

	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: secretName}, &secret); err != nil {
		if errors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("getting gateway TLS secret %s/%s: %w", instance.Namespace, secretName, err)
	}
	if !gatewayTLSSecretReady(&secret) {
		return false, false, nil
	}

	instance.Status.GatewayTLS = &computev1alpha1.GatewayTLSStatus{
		SecretName:          secretName,
		CreatedAt:           createdAt,
		Mode:                gatewayDesiredMode(instance),
		ClientCAFingerprint: desiredClientCAFP,
	}
	return true, false, nil
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

	alg, size := resolvedCertManagerAlgSize(*policy)
	algorithm := certmanagerv1.RSAKeyAlgorithm
	if alg == "ECDSA" {
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
				Size:           int(size),
			},
		},
	}
}
