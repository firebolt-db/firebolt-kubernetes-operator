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

// signingKeyID formats a signing key's ID (JWT "kid") from its generation
// number. Every key an Instance ever mints — the first and every
// subsequent rotation — goes through this one function, so the naming
// scheme can never drift between the bootstrap path and the rotation
// path.
func signingKeyID(generation int) string {
	return fmt.Sprintf("signing-%d", generation)
}

// resolvedSigningKeyAlgSize returns the cert-manager key algorithm and size a
// signing key would be issued with under the given policy, applying the same
// empty→default resolution the CRD markers do (ECDSA / P-384). Used both when
// stamping a freshly minted key's SigningKeyStatus.{Algorithm,Size} and when
// the rotation state machine checks whether the active key's issued
// properties still match the current policy — resolving both sides keeps a
// spec that merely spells out the default value explicitly from reading as a
// change.
func resolvedSigningKeyAlgSize(cm computev1alpha1.CertManagerSpec) (string, int32) {
	alg := cm.Algorithm
	if alg == "" {
		alg = "ECDSA"
	}
	size := cm.Size
	if size == 0 {
		size = 384
	}
	return alg, size
}

// AuthSigningKeyID is the ID (JWT "kid") of the very first signing key
// any Instance ever mints — generation 1. Exported as a stable named
// constant because operatorauthority.go's reserved-engine-volume-name
// documentation and tests reference it directly; every later generation
// is minted via signingKeyID instead.
var AuthSigningKeyID = signingKeyID(1)

// authSigningCertDuration is set far beyond any practical operator or
// cluster lifetime so cert-manager does not renew a signing Certificate
// on its own schedule. packdb's SigningKeyManager reads signing keys only
// at process startup, so an uncoordinated renewal would make one engine
// sign tokens with a key its peers can't yet validate — see
// buildSigningCertificate's doc comment. When SigningKeyPolicy's
// RotationInterval is set, the operator performs its own coordinated
// rotation (stepSigningKeyRotation) instead of relying on cert-manager's
// renewal schedule.
const authSigningCertDuration = 100 * 365 * 24 * time.Hour

// authSigningCertCommonName is the fixed X.509 subject used for every
// signing Certificate. Deliberately NOT derived from the instance name or
// namespace: RFC 5280's ub-common-name caps CommonName at 64 characters
// and cert-manager warns that exceeding it produces an invalid CSR,
// which realistic "<instance>-auth-signing.<namespace>" pairs can do.
// The value carries no meaning beyond satisfying "a valid Certificate
// requires at least one of CommonName, LiteralSubject, DNSName, or URI."
const authSigningCertCommonName = "firebolt-jwt-signing"

// signingCertificateName returns the cert-manager Certificate name for
// one signing key. Generation 1 keeps the exact pre-rotation name
// (instanceName + SuffixAuthSigning, no "-<kid>" suffix): every Instance
// that predates rotation support — and every Instance that never sets
// RotationInterval — has exactly this one Certificate, and upgrading the
// operator must never force it to be recreated under a new name (which
// would mint a brand-new keypair every engine would then need to pick
// up). Only additional keys an actual rotation mints get the "-<kid>"
// suffix.
func signingCertificateName(instanceName, kid string) string {
	if kid == AuthSigningKeyID {
		return instanceName + SuffixAuthSigning
	}
	return instanceName + SuffixAuthSigning + "-" + kid
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
		// Preserve the monotonic signing-key generation counter across a
		// disable so a later re-enable mints a FRESH generation (N+1) with a
		// brand-new Certificate/Secret name, instead of resetting to 0 and
		// re-bootstrapping generation 1. cert-manager (rotationPolicy:Never)
		// reuses the existing private key and kid behind a Secret name that
		// already exists, so a reset would silently resurrect a retired key —
		// making previously-issued tokens valid again. The SigningKeys list is
		// cleared (nothing is rendered while disabled); the orphaned
		// Certificates/Secrets are harmless while the counter only advances and
		// are swept when the Instance is deleted (reconcileDelete). Keeping a
		// non-nil Status.Auth here is safe: the only Status.Auth == nil readers
		// are existingSigningKeys / authStatusGeneration, both of which treat an
		// empty SigningKeys list exactly like "no keys provisioned yet".
		if gen := authStatusGeneration(instance); gen > 0 {
			instance.Status.Auth = &computev1alpha1.AuthStatus{SigningKeyGeneration: gen}
		} else {
			instance.Status.Auth = nil
		}
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

	ready, err := r.ensureSigningKeys(ctx, instance)
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
	_, err := checkSecretKeyPresent(ctx, r.Client, instance.Namespace, ref.Name, ref.Key, "admin password secret")
	return err
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
func checkSecretKeyPresent(ctx context.Context, cli client.Client, namespace, name, key, label string) (string, error) {
	var secret corev1.Secret
	err := cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret)
	if errors.IsNotFound(err) {
		return "", fmt.Errorf("%s %s/%s not found", label, namespace, name)
	}
	if err != nil {
		return "", fmt.Errorf("getting %s %s/%s: %w", label, namespace, name, err)
	}
	if len(secret.Data[key]) == 0 {
		return "", fmt.Errorf("%s %s/%s is missing or empty for key %q", label, namespace, name, key)
	}
	// ResourceVersion lets callers fold an in-place data rotation (same name,
	// new bytes) into a rollout hash without ever hashing the secret bytes
	// themselves — see ResolvedAuthInfo.AdminSecretVersion.
	return secret.ResourceVersion, nil
}

// existingSigningKeys returns instance.Status.Auth.SigningKeys, or nil if
// auth has never been provisioned for this Instance yet.
func existingSigningKeys(instance *computev1alpha1.FireboltInstance) []computev1alpha1.SigningKeyStatus {
	if instance.Status.Auth == nil {
		return nil
	}
	return instance.Status.Auth.SigningKeys
}

// authStatusGeneration returns the SigningKeyGeneration counter recorded
// in instance.Status.Auth, or 0 if auth has never been provisioned yet —
// so the very first key minted by a fresh Instance is generation 1,
// matching AuthSigningKeyID.
func authStatusGeneration(instance *computev1alpha1.FireboltInstance) int {
	if instance.Status.Auth == nil {
		return 0
	}
	return instance.Status.Auth.SigningKeyGeneration
}

// normalizeSigningKeyPhases backfills Phase=="" to Active in place. Only
// reachable for an Instance that provisioned its one signing key before
// SigningKeyPhase existed — such an Instance always has exactly one key,
// so blindly treating every unset Phase as Active is safe.
func normalizeSigningKeyPhases(keys []computev1alpha1.SigningKeyStatus) {
	for i := range keys {
		if keys[i].Phase == "" {
			keys[i].Phase = computev1alpha1.SigningKeyActive
		}
	}
}

// activeSigningKey returns the one key currently used to sign new tokens,
// or nil if none is provisioned yet (a brand-new Instance).
func activeSigningKey(keys []computev1alpha1.SigningKeyStatus) *computev1alpha1.SigningKeyStatus {
	for i := range keys {
		if keys[i].Phase == computev1alpha1.SigningKeyActive {
			return &keys[i]
		}
	}
	return nil
}

// otherSigningKey returns the single non-Active key, if any — the one key
// a rotation in flight can ever have outstanding at once, since
// stepSigningKeyRotation only ever starts a new rotation once the
// previous one has fully finished (mint, promote, retain, then remove,
// strictly in sequence).
func otherSigningKey(keys []computev1alpha1.SigningKeyStatus) *computev1alpha1.SigningKeyStatus {
	for i := range keys {
		if keys[i].Phase != computev1alpha1.SigningKeyActive {
			return &keys[i]
		}
	}
	return nil
}

// signingKeysForRender returns the subset of keys engines should render
// into signing_keys[] and mount, Active key first: the Active key plus,
// only while a rotation is in flight, the single other non-Removing key
// (freshly minted and pending promotion, or freshly demoted and still
// within RetainDuration). A Phase=Removing key has already been confirmed
// unnecessary for validation by every engine and is deliberately excluded
// — see SigningKeyPhase's doc comment. Applied once, at the boundary
// where FireboltInstance.Status.Auth.SigningKeys becomes engine-facing
// InstanceInfo (resolveInstanceInfo in engine_controller.go), so every
// downstream consumer (renderSigningKeys, buildAuthVolumes,
// buildAuthVolumeMounts, authHash) sees an already-filtered,
// already-ordered list and needs no rotation-awareness of its own.
func signingKeysForRender(keys []computev1alpha1.SigningKeyStatus) []computev1alpha1.SigningKeyStatus {
	out := make([]computev1alpha1.SigningKeyStatus, 0, 2)
	if active := activeSigningKey(keys); active != nil {
		out = append(out, *active)
	}
	if other := otherSigningKey(keys); other != nil && other.Phase != computev1alpha1.SigningKeyRemoving {
		out = append(out, *other)
	}
	return out
}

// ensureSigningKeys applies the desired Certificate for every signing key
// currently tracked in instance.Status.Auth.SigningKeys, advances at most
// one step of an in-flight rotation (see AuthStatus's doc comment for the
// full state machine), and reports ready=true once the Active key's
// Secret carries a private key. A rotation step never blocks readiness:
// an Instance is "auth ready" as soon as it has a usable Active key,
// regardless of whether a rotation is also in progress.
//
// This function is deliberately not covered by the envtest suite: the
// envtest CRD set (config/crd/bases) does not include cert-manager's
// CRDs, so applying a Certificate would fail with "no matches for kind"
// against the fake apiserver. Coverage for the apply-and-issue path lives
// in e2e (test/testhelpers/utils.go already installs cert-manager for
// that suite); buildSigningCertificate carries the unit-tested surface
// for this function's Certificate-shape logic, and the pure
// state-transition helpers below (promoteSigningKey and friends) carry
// the unit-tested surface for the rotation state machine itself.
func (r *FireboltInstanceReconciler) ensureSigningKeys(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	keys := existingSigningKeys(instance)
	normalizeSigningKeyPhases(keys)

	active := activeSigningKey(keys)
	if active == nil {
		return r.bootstrapSigningKey(ctx, instance, keys)
	}

	activeReady, err := r.applySigningCertificate(ctx, instance, active)
	if err != nil {
		return false, err
	}

	if other := otherSigningKey(keys); other != nil && other.Phase != computev1alpha1.SigningKeyRemoving {
		if _, err := r.applySigningCertificate(ctx, instance, other); err != nil {
			return false, err
		}
	}

	instance.Status.Auth = &computev1alpha1.AuthStatus{
		SigningKeys:          keys,
		SigningKeyGeneration: authStatusGeneration(instance),
	}
	if !activeReady {
		return false, nil
	}

	if err := r.stepSigningKeyRotation(ctx, instance); err != nil {
		return false, err
	}
	return true, nil
}

// bootstrapSigningKey mints the very first signing key for an Instance
// that has none yet. Unlike a rotation-minted key, this one gates the
// entire AuthReady condition (see ensureAuth), so there is no "add before
// it's ready" concern to guard against here the way mintNextSigningKey
// must for a mid-rotation key.
func (r *FireboltInstanceReconciler) bootstrapSigningKey(ctx context.Context, instance *computev1alpha1.FireboltInstance, keys []computev1alpha1.SigningKeyStatus) (bool, error) {
	gen := authStatusGeneration(instance) + 1
	alg, size := resolvedSigningKeyAlgSize(instance.Spec.Auth.Local.SigningKeys.CertManager)
	key := computev1alpha1.SigningKeyStatus{ID: signingKeyID(gen), Phase: computev1alpha1.SigningKeyActive, Algorithm: alg, Size: size}
	ready, err := r.applySigningCertificate(ctx, instance, &key)
	if err != nil {
		return false, err
	}
	instance.Status.Auth = &computev1alpha1.AuthStatus{
		SigningKeys:          append(keys, key),
		SigningKeyGeneration: gen,
	}
	return ready, nil
}

// applySigningCertificate applies the desired cert-manager Certificate
// for one signing key and reports whether its Secret is ready (carries a
// non-empty private key), stamping SecretName and, the first time it
// becomes ready, CreatedAt onto key in place. Because key is always a
// pointer into instance.Status.Auth.SigningKeys' existing entries (never
// a fresh copy) once that key has been observed ready before,
// CreatedAt naturally survives unchanged across reconciles with no
// separate lookup needed.
//
// GC note: the Certificate is cleaned up via this OwnerReference and
// Kubernetes' built-in garbage collector, NOT via reconcileDelete's
// manual label-based sweep — that sweep only covers core/apps/rbac kinds
// envtest can always resolve, and unconditionally Listing a cert-manager
// kind there would break every deletion test on a cluster (like envtest)
// that has no cert-manager CRDs installed. The target Secret cert-manager
// creates IS covered by that sweep, because it carries LabelInstance via
// SecretTemplate.Labels and corev1.Secret is always resolvable. A
// rotation-driven removal (deleteSigningKey) cannot rely on either of
// these — it runs on a live Instance, not at Instance-delete time — so it
// deletes the Certificate and Secret explicitly instead.
func (r *FireboltInstanceReconciler) applySigningCertificate(ctx context.Context, instance *computev1alpha1.FireboltInstance, key *computev1alpha1.SigningKeyStatus) (bool, error) {
	desired := buildSigningCertificate(instance, key.ID)
	desired.TypeMeta = metav1.TypeMeta{APIVersion: certmanagerv1.SchemeGroupVersion.String(), Kind: "Certificate"}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return false, err
	}
	if err := applySSA(ctx, r.Client, desired); err != nil {
		return false, fmt.Errorf("applying signing certificate %s: %w", desired.Name, err)
	}

	key.SecretName = desired.Spec.SecretName
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: key.SecretName}, &secret)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("getting signing key secret %s/%s: %w", instance.Namespace, key.SecretName, err)
	}
	if len(secret.Data[corev1.TLSPrivateKeyKey]) == 0 {
		// cert-manager writes tls.crt/tls.key/ca.crt atomically once
		// issuance completes, so an existing Secret with no private key
		// means issuance is still in flight — not an error, just not
		// ready yet.
		return false, nil
	}
	if key.CreatedAt.IsZero() {
		key.CreatedAt = metav1.Now()
	}
	return true, nil
}

// buildSigningCertificate returns the desired cert-manager Certificate
// used to provision one JWT signing keypair for this Instance. Kept as a
// pure function so its shape is unit-testable without envtest (see
// ensureSigningKeys' doc comment for why envtest itself cannot exercise
// the apply).
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
func buildSigningCertificate(instance *computev1alpha1.FireboltInstance, kid string) *certmanagerv1.Certificate {
	policy := instance.Spec.Auth.Local.SigningKeys.CertManager
	name := signingCertificateName(instance.Name, kid)
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

// stepSigningKeyRotation advances instance.Status.Auth.SigningKeys by at
// most one step of the rotation state machine described on AuthStatus's
// doc comment, or does nothing if no step is currently due. Only one step
// per call: the controller's normal 30s requeue loop drives the next step
// forward on a later reconcile, which is what keeps each individual step
// idempotent and safe to interrupt (crash, restart, conflict-retry) at
// any point without needing its own bespoke RequeueAfter scheduling.
//
// Called only once ensureSigningKeys has confirmed the Active key's own
// Certificate is applied — active and other (if any) are always non-nil
// pointers into the same instance.Status.Auth.SigningKeys slice, so
// mutating through them updates the slice this function returns via that
// Status field directly.
func (r *FireboltInstanceReconciler) stepSigningKeyRotation(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	keys := instance.Status.Auth.SigningKeys
	active := activeSigningKey(keys)
	other := otherSigningKey(keys)
	policy := instance.Spec.Auth.Local.SigningKeys

	switch {
	case other == nil:
		// Backfill Algorithm/Size onto a key minted before they were recorded,
		// so the scheduler and status reflect its real policy. A legitimate
		// algorithm/size drift never reaches here: both signingAlgorithm and
		// the signing-key size are immutable after first set — enforced at the
		// API server by CEL transition rules on LocalAuthSpec.SigningAlgorithm
		// and on SigningKeyPolicy.CertManager (see fireboltinstance_types.go),
		// and again in the validating webhook (validateImmutableSigningKey) for
		// a clearer message. packdb's single global signing_algorithm cannot
		// represent two curves at once (the closed config schema has no per-key
		// algorithm field), so an in-place change is unsupportable — it is
		// rejected at admission rather than migrated by minting a new key, which
		// would only have produced an invalid mixed-curve keyset. A legacy key
		// (minted before Algorithm/Size existed) simply adopts its baseline.
		if active.Algorithm == "" || active.Size == 0 {
			wantAlg, wantSize := resolvedSigningKeyAlgSize(policy.CertManager)
			active.Algorithm, active.Size = wantAlg, wantSize
		}
		if policy.RotationInterval == nil {
			return nil
		}
		if metav1.Now().Time.Before(active.CreatedAt.Add(policy.RotationInterval.Duration)) {
			return nil
		}
		return r.mintNextSigningKey(ctx, instance)

	case other.Phase == computev1alpha1.SigningKeyValidationOnly && other.DemotedAt == nil:
		// A freshly minted key waiting to be promoted: safe only once
		// every engine has rolled onto a signing_keys[] that includes it
		// — promoting any earlier would let a rolled engine sign tokens a
		// not-yet-rolled engine cannot yet validate.
		converged, err := r.enginesConvergedOn(ctx, instance, keys)
		if err != nil || !converged {
			return err
		}
		promoteSigningKey(active, other)
		return nil

	case other.Phase == computev1alpha1.SigningKeyValidationOnly && other.RetireEligibleAt == nil:
		// other.DemotedAt != nil: demoted, but not yet confirmed that
		// every engine has actually rolled onto the promoted config.
		// Every engine keeps signing with this key until it does, so the
		// retain window must not start counting yet — see
		// RetireEligibleAt's doc comment for why anchoring at DemotedAt
		// instead would reopen a validation gap.
		converged, err := r.enginesConvergedOn(ctx, instance, keys)
		if err != nil || !converged {
			return err
		}
		now := metav1.Now()
		other.RetireEligibleAt = &now
		return nil

	case other.Phase == computev1alpha1.SigningKeyValidationOnly:
		// Demoted and confirmed no engine can still be signing with it:
		// now it's just waiting out RetainDuration before it can be
		// dropped from render.
		if policy.RetainDuration == nil {
			// Cannot happen when RotationInterval is set (ValidateAuth
			// requires RetainDuration alongside it, and a demoted key
			// only exists because a rotation ran under that policy), but
			// if the user edits RotationInterval away mid-rotation,
			// leaving a demoted key ValidationOnly forever is a safe,
			// fail-static outcome — it just keeps validating old tokens
			// indefinitely — rather than a guess at what duration to use.
			return nil
		}
		if metav1.Now().Time.Before(other.RetireEligibleAt.Add(policy.RetainDuration.Duration)) {
			return nil
		}
		other.Phase = computev1alpha1.SigningKeyRemoving
		return nil

	case other.Phase == computev1alpha1.SigningKeyRemoving:
		// Already dropped from render (signingKeysForRender excludes
		// Removing). Safe to delete its Certificate/Secret and forget it
		// entirely once every engine has confirmed the removal rolled out
		// too — otherwise a slow engine could still be relying on it to
		// validate a token signed before it was demoted.
		converged, err := r.enginesConvergedOn(ctx, instance, keys)
		if err != nil || !converged {
			return err
		}
		return r.deleteSigningKey(ctx, instance, other)
	}
	return nil
}

// mintNextSigningKey starts (or continues) provisioning the next signing
// key once a rotation is due. The key is intentionally NOT appended to
// instance.Status.Auth.SigningKeys — and therefore not yet rendered or
// mounted anywhere, via signingKeysForRender — until its Certificate's
// Secret is confirmed ready: engines gate their own reconcile on every
// auth Secret in Status.Auth.SigningKeys actually existing
// (resolveInstanceInfo, engine_controller.go), so appending an unready
// key here would start failing every engine's reconcile in this Instance
// for no benefit. While not yet ready, this function reapplies the same
// Certificate (idempotent) and rechecks readiness every reconcile until
// it succeeds, at which point the key is appended exactly once.
func (r *FireboltInstanceReconciler) mintNextSigningKey(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	gen := instance.Status.Auth.SigningKeyGeneration + 1
	alg, size := resolvedSigningKeyAlgSize(instance.Spec.Auth.Local.SigningKeys.CertManager)
	key := computev1alpha1.SigningKeyStatus{ID: signingKeyID(gen), Phase: computev1alpha1.SigningKeyValidationOnly, Algorithm: alg, Size: size}
	ready, err := r.applySigningCertificate(ctx, instance, &key)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	instance.Status.Auth.SigningKeys = append(instance.Status.Auth.SigningKeys, key)
	instance.Status.Auth.SigningKeyGeneration = gen
	return nil
}

// promoteSigningKey flips other to Active and demotes the previous Active
// key to ValidationOnly with DemotedAt=now. RetireEligibleAt is
// deliberately left unset here — engines still sign with the demoted key
// until they roll onto this promotion, so stepSigningKeyRotation
// separately confirms that rollout (via enginesConvergedOn) before
// stamping RetireEligibleAt and letting the retain window start counting.
func promoteSigningKey(active, other *computev1alpha1.SigningKeyStatus) {
	now := metav1.Now()
	active.Phase = computev1alpha1.SigningKeyValidationOnly
	active.DemotedAt = &now
	other.Phase = computev1alpha1.SigningKeyActive
}

// deleteSigningKey deletes a Removing key's Certificate and Secret and
// drops its entry from instance.Status.Auth.SigningKeys — the last step
// of a rotation, run only once enginesConvergedOn has confirmed every
// engine no longer needs this key to validate anything. Both deletes are
// explicit (rather than relying on a cert-manager owner-reference cascade
// from Secret to Certificate, which is off by default) and individually
// not-found-tolerant, so a reconcile that deleted one and then crashed or
// conflict-retried before updating Status resumes cleanly.
func (r *FireboltInstanceReconciler) deleteSigningKey(ctx context.Context, instance *computev1alpha1.FireboltInstance, key *computev1alpha1.SigningKeyStatus) error {
	certName := signingCertificateName(instance.Name, key.ID)
	cert := &certmanagerv1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, cert); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting signing certificate %s: %w", certName, err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.SecretName, Namespace: instance.Namespace}}
	if err := r.Delete(ctx, secret); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting signing key secret %s: %w", key.SecretName, err)
	}
	instance.Status.Auth.SigningKeys = removeSigningKey(instance.Status.Auth.SigningKeys, key.ID)
	return nil
}

// removeSigningKey returns keys with the entry matching id dropped.
func removeSigningKey(keys []computev1alpha1.SigningKeyStatus, id string) []computev1alpha1.SigningKeyStatus {
	out := make([]computev1alpha1.SigningKeyStatus, 0, len(keys))
	for _, k := range keys {
		if k.ID != id {
			out = append(out, k)
		}
	}
	return out
}

// enginesConvergedOn reports whether every FireboltEngine belonging to
// instance has an ObservedAuthHash matching the hash that rendering keys
// (filtered/ordered through signingKeysForRender, exactly as
// resolveInstanceInfo would) produces — i.e. whether it is safe to
// advance whatever step is gated on that rendered state. An Instance with
// no engines yet is vacuously converged: there is nothing that could be
// left behind on a stale key.
//
// Engines bind to their instance via spec.instanceRef, not a label — the
// operator never stamps LabelInstance onto the engine CR (only onto the
// instance's own child resources). We therefore list every engine in the
// namespace and filter on spec.instanceRef, exactly as the instance→engine
// watch mapper (instanceToEngines) does. A label selector here would match
// zero engines and make every rotation gate vacuously "converged", letting
// promote/retire advance before engines have actually observed the new hash.
func (r *FireboltInstanceReconciler) enginesConvergedOn(ctx context.Context, instance *computev1alpha1.FireboltInstance, keys []computev1alpha1.SigningKeyStatus) (bool, error) {
	var list computev1alpha1.FireboltEngineList
	if err := r.List(ctx, &list, client.InNamespace(instance.Namespace)); err != nil {
		return false, fmt.Errorf("listing engines for instance %s: %w", instance.Name, err)
	}
	// Gather this Instance's engines first. With none there is nothing to
	// compare against, so short-circuit as vacuously converged before reading
	// the admin Secret — both an optimization and what keeps a bootstrap-time
	// or engine-less reconcile from failing on an admin Secret it never needs.
	var ours []*computev1alpha1.FireboltEngine
	for i := range list.Items {
		if list.Items[i].Spec.InstanceRef == instance.Name {
			ours = append(ours, &list.Items[i])
		}
	}
	if len(ours) == 0 {
		return true, nil
	}

	// Must read the admin Secret's ResourceVersion and fold it into the
	// expected hash exactly as the engine reconciler does (resolveInstanceInfo
	// → ResolvedAuthInfo.AdminSecretVersion): otherwise, after an in-place
	// password rotation, this side would compute a hash the engines can never
	// match and the rotation gate would deadlock. Rotation only runs with
	// local signing keys configured, so admin is always set here; guard nil
	// defensively regardless.
	var adminRV string
	if instance.Spec.Auth != nil && instance.Spec.Auth.Local != nil {
		admin := instance.Spec.Auth.Local.Admin.Password
		rv, err := checkSecretKeyPresent(ctx, r.Client, instance.Namespace, admin.Name, admin.Key, "admin password secret")
		if err != nil {
			return false, err
		}
		adminRV = rv
	}
	expected := authHash(&ResolvedAuthInfo{
		Spec:               instance.Spec.Auth,
		SigningKeys:        signingKeysForRender(keys),
		AdminSecretVersion: adminRV,
	})
	for _, e := range ours {
		if e.Status.ObservedAuthHash != expected {
			return false, nil
		}
	}
	return true, nil
}
