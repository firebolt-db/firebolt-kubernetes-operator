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

package v1alpha1

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// localAuthServerName is the authorization server name packdb reserves
// for its embedded ("_local") server. Mirrors packdb's
// DB::Auth::kLocalAuthServerName (AuthConfig.h) — kept in sync manually
// since the operator does not vendor packdb's C++ sources.
const localAuthServerName = "_local"

// FireboltInstanceDefaulter defaults FireboltInstance resources.
type FireboltInstanceDefaulter struct{}

// FireboltInstanceCustomValidator validates FireboltInstance resources.
type FireboltInstanceCustomValidator struct{}

var (
	_ admission.Defaulter[*FireboltInstance] = &FireboltInstanceDefaulter{}
	_ admission.Validator[*FireboltInstance] = &FireboltInstanceCustomValidator{}
)

// SetupFireboltInstanceWebhookWithManager registers the defaulting and
// validating webhooks with the manager.
func SetupFireboltInstanceWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &FireboltInstance{}).
		WithDefaulter(&FireboltInstanceDefaulter{}).
		WithValidator(&FireboltInstanceCustomValidator{}).
		Complete()
}

// Default sets default values for a FireboltInstance. If spec.id is empty, a
// new ULID is generated so every instance has a stable unique identifier.
func (d *FireboltInstanceDefaulter) Default(_ context.Context, inst *FireboltInstance) error {
	if inst.Spec.ID == "" {
		inst.Spec.ID = ulid.MustNew(ulid.Now(), rand.Reader).String()
	}
	return nil
}

// ValidateCreate validates a FireboltInstance on creation.
func (v *FireboltInstanceCustomValidator) ValidateCreate(_ context.Context, inst *FireboltInstance) (admission.Warnings, error) {
	return nil, validateSpec(inst).ToAggregate()
}

// ValidateUpdate validates a FireboltInstance on update.
func (v *FireboltInstanceCustomValidator) ValidateUpdate(
	_ context.Context, oldInst, newInst *FireboltInstance,
) (admission.Warnings, error) {
	// spec.id immutability is enforced by CEL on the CRD itself
	// (XValidation rule="oldSelf == '' || self == oldSelf"), so it works
	// even when webhooks are disabled. The empty->value transition is
	// explicitly allowed so the controller fallback can generate and
	// persist an ID when the defaulting webhook is not active.
	//
	// Signing algorithm/size immutability is primarily enforced at the API
	// server by CEL transition rules (see fireboltinstance_types.go:
	// LocalAuthSpec.SigningAlgorithm and SigningKeyPolicy.CertManager), so it
	// holds even if this webhook is bypassed — critical because a bypassed
	// change would permanently wedge engines (renderAuthConfig reads
	// signing_algorithm from spec, and there is no in-place migration). The
	// checks below re-enforce those with a clearer message AND cover the engine
	// TLS issuerRef, which is NOT CEL-enforced: its key/curve fields live in the
	// CertManagerSpec struct shared with the gateway-TLS listener, so a
	// struct-level CEL rule would wrongly freeze the gateway issuer too;
	// comparing the specific engine path here scopes it exactly.
	errs := validateSpec(newInst)
	errs = append(errs, validateImmutableFields(oldInst, newInst)...)
	return nil, errs.ToAggregate()
}

// validateImmutableFields rejects in-place changes to key/curve properties that
// cannot be migrated on a live Instance. Each rule fires only while the owning
// feature stays continuously enabled across the update: disabling a feature and
// re-enabling it starts from fresh key material (no overlap with the old key),
// so a change made across that gap is safe and is permitted — only an in-place
// edit while the feature is continuously enabled is rejected.
func validateImmutableFields(oldInst, newInst *FireboltInstance) field.ErrorList {
	var errs field.ErrorList
	errs = append(errs, validateImmutableSigningKey(oldInst, newInst)...)
	errs = append(errs, validateImmutableEngineTLSIssuer(oldInst, newInst)...)
	return errs
}

// authLocalEnabled reports whether the local auth server (which owns the JWT
// signing key) is enabled and configured on inst.
func authLocalEnabled(inst *FireboltInstance) bool {
	return inst.Spec.Auth != nil && inst.Spec.Auth.Enabled && inst.Spec.Auth.Local != nil
}

// validateImmutableSigningKey freezes the JWT signing algorithm and the signing
// key size while auth stays enabled. packdb exposes a single global
// signing_algorithm and derives each key's curve from it (the config schema has
// no per-key algorithm field), so it can never serve two curves at once —
// changing either in place would roll engines onto a signing_algorithm their
// mounted key no longer matches, producing an invalid JWKS. There is no safe
// in-place migration; the change must go through a disable/re-enable (fresh
// key) or a new Instance.
func validateImmutableSigningKey(oldInst, newInst *FireboltInstance) field.ErrorList {
	if !authLocalEnabled(oldInst) || !authLocalEnabled(newInst) {
		return nil
	}
	oldLocal, newLocal := oldInst.Spec.Auth.Local, newInst.Spec.Auth.Local
	base := field.NewPath("spec", "auth", "local")
	var errs field.ErrorList
	if oldLocal.SigningAlgorithm != newLocal.SigningAlgorithm {
		errs = append(errs, field.Invalid(base.Child("signingAlgorithm"), newLocal.SigningAlgorithm,
			"is immutable while auth is enabled: packdb uses one global signing algorithm and "+
				"cannot serve two key curves at once, so changing it in place would break token "+
				"validation. Disable auth (or recreate the Instance) to change it."))
	}
	if oldLocal.SigningKeys != nil && newLocal.SigningKeys != nil &&
		oldLocal.SigningKeys.CertManager.Size != newLocal.SigningKeys.CertManager.Size {
		errs = append(errs, field.Invalid(base.Child("signingKeys", "certManager", "size"),
			newLocal.SigningKeys.CertManager.Size,
			"is immutable while auth is enabled (same reason as signingAlgorithm): disable auth "+
				"or recreate the Instance to change the signing key size."))
	}
	return errs
}

// engineTLSCertManaged reports whether engine TLS is enabled and provisioned via
// cert-manager on inst (bring-your-own is rejected for the engine listener).
func engineTLSCertManaged(inst *FireboltInstance) bool {
	return inst.Spec.TLS != nil && inst.Spec.TLS.Engine != nil && inst.Spec.TLS.Engine.Enabled &&
		inst.Spec.TLS.Engine.CertManager != nil
}

// validateImmutableEngineTLSIssuer freezes the engine TLS issuer while engine
// TLS stays enabled. Reissuing per-generation engine certificates under a new
// CA while the gateway still trusts the old CA anchor (Status.EngineTLS) would
// fail every upstream handshake mid-roll. Key algorithm/size may still change
// (tlsHash folds them, reissuing per generation) — only the issuer is frozen.
func validateImmutableEngineTLSIssuer(oldInst, newInst *FireboltInstance) field.ErrorList {
	if !engineTLSCertManaged(oldInst) || !engineTLSCertManaged(newInst) {
		return nil
	}
	oldRef := oldInst.Spec.TLS.Engine.CertManager.IssuerRef
	newRef := newInst.Spec.TLS.Engine.CertManager.IssuerRef
	if oldRef.Name == newRef.Name && oldRef.Kind == newRef.Kind {
		return nil
	}
	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "tls", "engine", "certManager", "issuerRef"), newRef,
		"is immutable while engine TLS is enabled: reissuing engine certificates under a new CA "+
			"while the gateway still trusts the old CA anchor would fail every upstream handshake. "+
			"Disable engine TLS or recreate the Instance to change the issuer.")}
}

// ValidateDelete validates a FireboltInstance on deletion.
func (v *FireboltInstanceCustomValidator) ValidateDelete(_ context.Context, _ *FireboltInstance) (admission.Warnings, error) {
	return nil, nil
}

// validateSpec runs every spec-level validation check and collects the
// results. Individual checks return *field.Error (not a plain error) so
// they can be appended directly into a field.ErrorList; wrapping them as
// field.InternalError would surface to users as a 500-style internal
// error instead of a validation failure.
func validateSpec(inst *FireboltInstance) field.ErrorList {
	var errs field.ErrorList

	if err := validateMetadataReplicas(inst); err != nil {
		errs = append(errs, err)
	}

	// Per-component pod-template validation. Each ruleset rejects user
	// input on fields the operator owns end-to-end (commands, ports,
	// probes, reserved env keys, reserved volume mount names) and on
	// universally operator-owned pod-level fields (subdomain, hostname,
	// terminationGracePeriodSeconds, restartPolicy, activeDeadlineSeconds).
	// See MetadataPodTemplateRules / GatewayPodTemplateRules in
	// operatorauthority.go for the authoritative allowlists.
	errs = append(errs, ValidatePodTemplate(
		inst.Spec.Metadata.Template,
		field.NewPath("spec", "metadata", "template"),
		MetadataPodTemplateRules,
	)...)
	errs = append(errs, ValidatePodTemplate(
		inst.Spec.Gateway.Template,
		field.NewPath("spec", "gateway", "template"),
		GatewayPodTemplateRules,
	)...)

	if err := validateExternalPostgres(inst); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, ValidateAuth(inst)...)
	errs = append(errs, ValidateTLS(inst)...)

	return errs
}

// ValidateTLS mirrors ValidateAuth's exported/defense-in-depth shape, but
// for spec.tls: re-run by the instance controller (see ensureEngineTLS,
// ensureGatewayTLS) in case the webhook is disabled.
func ValidateTLS(inst *FireboltInstance) field.ErrorList {
	tls := inst.Spec.TLS
	if tls == nil {
		return nil
	}
	var errs field.ErrorList
	enginePath := field.NewPath("spec", "tls", "engine")
	errs = append(errs, validateTLSListener(tls.Engine, enginePath)...)
	errs = append(errs, validateTLSListener(tls.Gateway, field.NewPath("spec", "tls", "gateway"))...)

	// Mutual TLS (client-certificate verification) is honored only on the
	// gateway's client-facing listener; the engine-side verify-client path
	// is not wired yet, so reject it on the engine rather than silently
	// ignore it.
	if tls.Engine != nil && tls.Engine.ClientCASecretRef != nil {
		errs = append(errs, field.Forbidden(enginePath.Child("clientCASecretRef"),
			"mutual TLS is only supported on spec.tls.gateway"))
	}

	// Bring-your-own Secret is not viable for the engine listener: the operator
	// issues per-generation certificates whose SANs cover the engine pods'
	// blue-green hostnames (…svc.cluster.local), which packdb verifies at
	// startup, and a static user Secret cannot cover the unbounded per-generation
	// hostname set. Engine TLS must therefore use cert-manager (certManager).
	// secretRef remains supported on spec.tls.gateway.
	if tls.Engine != nil && tls.Engine.SecretRef != nil {
		errs = append(errs, field.Forbidden(enginePath.Child("secretRef"),
			"bring-your-own Secret is not supported for the engine listener "+
				"(per-generation certificate SANs must cover the engine pod hostnames): use certManager"))
	}
	return errs
}

// validateTLSListener requires exactly one certificate source whenever a
// TLS listener is enabled: CertManager (the operator provisions a
// cert-manager Certificate) or SecretRef (a user-supplied Secret — see
// TLSListenerSpec's doc comment). A cert-manager source additionally needs
// a named issuer and a valid algorithm/size; a SecretRef needs a name.
func validateTLSListener(listener *TLSListenerSpec, base *field.Path) field.ErrorList {
	if listener == nil || !listener.Enabled {
		return nil
	}

	var errs field.ErrorList

	hasCertManager := listener.CertManager != nil
	hasSecretRef := listener.SecretRef != nil
	switch {
	case hasCertManager && hasSecretRef:
		errs = append(errs, field.Forbidden(base.Child("secretRef"),
			"must not be set together with certManager: provide exactly one certificate source"))
	case !hasCertManager && !hasSecretRef:
		errs = append(errs, field.Required(base.Child("certManager"),
			"required when enabled is true: provide certManager (operator provisions via cert-manager) or secretRef (bring your own Secret)"))
	case hasSecretRef:
		if listener.SecretRef.Name == "" {
			errs = append(errs, field.Required(base.Child("secretRef", "name"), "required"))
		}
	default: // certManager only
		if listener.CertManager.IssuerRef.Name == "" {
			errs = append(errs, field.Required(base.Child("certManager", "issuerRef", "name"), "required"))
		}
		if err := validateCertManagerKey(listener.CertManager, base.Child("certManager")); err != nil {
			errs = append(errs, err)
		}
	}

	// Mutual TLS: the client-CA Secret needs a name (its ca.crt presence is
	// checked at reconcile, not admission). Whether mTLS is honored for this
	// particular listener is enforced by the caller (ValidateTLS).
	if listener.ClientCASecretRef != nil && listener.ClientCASecretRef.Name == "" {
		errs = append(errs, field.Required(base.Child("clientCASecretRef", "name"), "required"))
	}

	return errs
}

// ValidateAuth mirrors packdb's instance.auth validation rules
// (AuthConfig::Validate) so misconfiguration is rejected at admission
// time instead of crashing every engine at startup against packdb's
// closed (additionalProperties: false) config schema. See AuthSpec's doc
// comment for why auth is validated once per Instance rather than per
// Engine.
//
// Exported (unlike this file's other validate* helpers) so the instance
// controller can re-run it at reconcile time as defense-in-depth against
// a bypassed webhook — the same pattern ValidatePodTemplate follows for
// spec.gateway.template / spec.metadata.template. Without this,
// controller code that assumes "Enabled implies Local != nil" (a
// webhook-enforced invariant) would panic on a CR that reached the
// cluster without admission.
func ValidateAuth(inst *FireboltInstance) field.ErrorList {
	auth := inst.Spec.Auth
	if auth == nil {
		return nil
	}
	base := field.NewPath("spec", "auth")

	if !auth.Enabled {
		return validateAuthDisabled(auth, base)
	}
	return validateAuthEnabled(auth, base)
}

// validateAuthDisabled enforces packdb's rule that instance.auth.admin
// and instance.auth.oidc — and, by extension, our Local wrapper around
// admin — must be absent when instance.auth.enabled is false, along with
// preferred_authorization_server.
func validateAuthDisabled(auth *AuthSpec, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	if auth.Local != nil {
		errs = append(errs, field.Forbidden(base.Child("local"),
			"must not be set when spec.auth.enabled is false"))
	}
	if auth.OIDC != nil {
		errs = append(errs, field.Forbidden(base.Child("oidc"),
			"must not be set when spec.auth.enabled is false"))
	}
	if auth.PreferredAuthorizationServer != "" {
		errs = append(errs, field.Forbidden(base.Child("preferredAuthorizationServer"),
			"must not be set when spec.auth.enabled is false"))
	}
	return errs
}

// validateAuthEnabled enforces packdb's rules for instance.auth.enabled=true:
// an admin block with an explicit password Secret reference (the operator
// never generates one) and a signing-key provisioning policy are both
// required, and the configured JWT signing algorithm must match the
// cert-manager key algorithm used to provision the signing keypair.
//
// OIDC provider shape (non-empty providers, names not starting with "_",
// https discovery URLs) is enforced structurally by the CRD schema
// (kubebuilder MinItems/Pattern markers on OIDCAuthSpec/OIDCProviderSpec).
// Provider duration fields (JWKS cache TTL, discovery refresh interval) are
// NOT structurally constrained by the CRD schema — a malformed or
// non-positive value passes admission today and only fails at packdb's own
// config load (AuthConfig::Validate rejects a non-positive
// discovery.refresh_interval; the config parser rejects an unparseable
// duration string for any of them) — so those are validated here.
func validateAuthEnabled(auth *AuthSpec, base *field.Path) field.ErrorList {
	var errs field.ErrorList

	if auth.Local == nil {
		errs = append(errs, field.Required(base.Child("local"),
			"required when spec.auth.enabled is true: packdb requires an admin account whenever auth is enabled"))
	} else {
		errs = append(errs, validateLocalAuth(auth.Local, base.Child("local"))...)
	}

	errs = append(errs, validatePreferredAuthorizationServer(auth, base.Child("preferredAuthorizationServer"))...)

	if auth.OIDC != nil {
		errs = append(errs, validateOIDCAuth(auth.OIDC, base.Child("oidc"))...)
	}

	return errs
}

// validateLocalAuth checks the admin-password reference and signing-key
// policy that Enabled=true requires, the chosen JWT signing algorithm's
// compatibility with the signing key's cert-manager algorithm, and the
// format of the embedded server's optional JWT duration fields.
func validateLocalAuth(local *LocalAuthSpec, base *field.Path) field.ErrorList {
	var errs field.ErrorList

	if local.Admin.Password.Name == "" {
		errs = append(errs, field.Required(base.Child("admin", "password", "name"),
			"required when spec.auth.enabled is true: the operator does not generate an admin password"))
	}
	if local.Admin.Password.Key == "" {
		errs = append(errs, field.Required(base.Child("admin", "password", "key"),
			"required: the Secret key holding the admin password"))
	}

	if local.SigningKeys == nil {
		errs = append(errs, field.Required(base.Child("signingKeys"),
			"required when spec.auth.enabled is true: every engine in a multi-engine Instance must share "+
				"identical signing keys, which packdb's own dev-autogen fallback cannot guarantee"))
	} else {
		if err := validateSigningAlgorithmCompatibility(local, base); err != nil {
			errs = append(errs, err)
		}
		if err := validateCertManagerKey(&local.SigningKeys.CertManager, base.Child("signingKeys", "certManager")); err != nil {
			errs = append(errs, err)
		}
		errs = append(errs, validateSigningKeyRotation(local, base.Child("signingKeys"))...)
	}

	// tokenExpiry and maxTokenAge must be positive, not merely parseable: a
	// zero/negative token lifetime is meaningless to packdb, and a negative
	// maxTokenAge would additionally corrupt validateSigningKeyRotation's
	// retain-duration floor (a "-1d" would lower it below any real token's
	// lifetime, defeating the rotation-safety check).
	if err := validatePositiveDurationField(base.Child("tokenExpiry"), local.TokenExpiry); err != nil {
		errs = append(errs, err)
	}
	if err := validatePositiveDurationField(base.Child("maxTokenAge"), local.MaxTokenAge); err != nil {
		errs = append(errs, err)
	}
	// clockSkewTolerance stays parseable-only: zero (no tolerance) is a
	// legitimate value, and it does not feed the rotation floor.
	if err := validateDurationField(base.Child("clockSkewTolerance"), local.ClockSkewTolerance); err != nil {
		errs = append(errs, err)
	}

	return errs
}

// validateOIDCAuth validates the duration-shaped fields on every configured
// OIDC provider that the CRD schema leaves as plain, unconstrained strings.
// Structural shape (non-empty providers, https discovery URL, name not
// starting with "_", required name/discoveryURL/usernameMapping) is already
// enforced by kubebuilder markers on OIDCAuthSpec/OIDCProviderSpec; this
// covers what those markers cannot express.
func validateOIDCAuth(oidc *OIDCAuthSpec, base *field.Path) field.ErrorList {
	var errs field.ErrorList

	if oidc.JWT != nil {
		jwtPath := base.Child("jwt")
		if err := validateDurationField(jwtPath.Child("clockSkewTolerance"), oidc.JWT.ClockSkewTolerance); err != nil {
			errs = append(errs, err)
		}
		if err := validatePositiveDurationField(jwtPath.Child("maxTokenAge"), oidc.JWT.MaxTokenAge); err != nil {
			errs = append(errs, err)
		}
	}

	seen := make(map[string]struct{}, len(oidc.Providers))
	for i, p := range oidc.Providers {
		providerPath := base.Child("providers").Index(i)
		// packdb's IssuerRegistry::registerRemote throws when a second provider
		// with the same name is registered, which prevents every affected engine
		// from starting. The CRD cannot express uniqueness across list items, and
		// validatePreferredAuthorizationServer already assumes names are unique,
		// so reject a duplicate here.
		if _, dup := seen[p.Name]; dup {
			errs = append(errs, field.Duplicate(providerPath.Child("name"), p.Name))
		}
		seen[p.Name] = struct{}{}
		if p.JWKS != nil {
			if err := validateDurationField(providerPath.Child("jwks", "cacheTTL"), p.JWKS.CacheTTL); err != nil {
				errs = append(errs, err)
			}
		}
		if p.Discovery != nil {
			// Positive, not just parseable: packdb's AuthConfig::Validate
			// rejects a zero/negative discovery.refresh_interval outright
			// (the discovery-document refresh task would never run,
			// permanently leaving the provider unresolved) — the one
			// duration field packdb validates for more than parseability.
			if err := validatePositiveDurationField(
				providerPath.Child("discovery", "refreshInterval"), p.Discovery.RefreshInterval,
			); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errs
}

// parsePackdbDuration parses a duration string using the grammar packdb's
// config loader accepts for every instance.auth duration field (TokenExpiry,
// MaxTokenAge, ClockSkewTolerance, CacheTTL, RefreshInterval): Go's
// time.ParseDuration grammar plus a "d" (days) unit, which Go's standard
// library does not support (packdb
// src/Common/Configuration/Unit/Duration.h: "Parser implementation for
// Go-style duration strings"). Multi-component strings mixing "d" with
// other units (e.g. "1d12h") are not supported here — every default and
// realistic auth duration value packdb ships is a single component (e.g.
// "1d", "30s", "1h"). A value that fails to parse here is guaranteed to
// fail packdb's own config load the same way, crashing every engine at
// startup; this validates it at admission time instead.
func parsePackdbDuration(s string) (time.Duration, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	days, ok := strings.CutSuffix(s, "d")
	if !ok || days == "" {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	n, err := strconv.ParseFloat(days, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	return time.Duration(n * float64(24*time.Hour)), nil
}

// validateDurationField rejects a non-empty value that parsePackdbDuration
// cannot parse. Empty is always valid — every one of these CRD fields is
// optional, and packdb applies its own built-in default when absent.
func validateDurationField(path *field.Path, value string) *field.Error {
	if value == "" {
		return nil
	}
	if _, err := parsePackdbDuration(value); err != nil {
		return field.Invalid(path, value, `must be a valid duration string (e.g. "30s", "1h", "1d")`)
	}
	return nil
}

// validatePositiveDurationField is validateDurationField plus a positivity
// check (the value must parse AND be > 0). Used for the duration fields where
// a zero/negative value is either meaningless or unsafe:
// oidc.providers[].discovery.refreshInterval (packdb itself rejects it
// non-positive — the discovery-refresh task would never run), and the
// token-lifetime fields tokenExpiry / maxTokenAge (a non-positive value is
// nonsensical to packdb and, for maxTokenAge, would corrupt
// validateSigningKeyRotation's retain-duration floor).
func validatePositiveDurationField(path *field.Path, value string) *field.Error {
	if value == "" {
		return nil
	}
	d, err := parsePackdbDuration(value)
	if err != nil {
		return field.Invalid(path, value, `must be a valid duration string (e.g. "30s", "1h", "1d")`)
	}
	if d <= 0 {
		return field.Invalid(path, value, "must be positive")
	}
	return nil
}

// validateSigningAlgorithmCompatibility rejects a JWT signing algorithm
// that doesn't match the cert-manager key algorithm used to provision the
// signing keypair — packdb would otherwise fail to load an RSA-family
// signing_algorithm against an ECDSA private key (or vice versa) at
// engine startup, which is a much later and harder-to-diagnose failure
// than an admission rejection.
func validateSigningAlgorithmCompatibility(local *LocalAuthSpec, base *field.Path) *field.Error {
	alg := local.SigningAlgorithm
	if alg == "" {
		alg = "ES384" // matches the CRD default
	}
	keyAlg := local.SigningKeys.CertManager.Algorithm
	if keyAlg == "" {
		keyAlg = "ECDSA" // matches the CRD default
	}

	wantsRSA := strings.HasPrefix(alg, "RS")
	switch {
	case wantsRSA && keyAlg != "RSA":
		return field.Invalid(base.Child("signingAlgorithm"), alg,
			fmt.Sprintf("requires signingKeys.certManager.algorithm=RSA, got %q", keyAlg))
	case !wantsRSA && keyAlg != "ECDSA":
		return field.Invalid(base.Child("signingAlgorithm"), alg,
			fmt.Sprintf("requires signingKeys.certManager.algorithm=ECDSA, got %q", keyAlg))
	}

	// For ECDSA (ES*) the JWT algorithm pins the exact curve: JOSE requires
	// ES256↔P-256, ES384↔P-384, ES512↔P-521. packdb derives the JWK curve and
	// coordinate size from the JWT algorithm rather than from the actual key,
	// so a mismatched curve (e.g. ES256 with the default P-384 key) produces an
	// invalid JWKS or an engine startup failure. RSA (RS*) imposes no such
	// pairing — the algorithms differ only in digest size, any 2048..8192
	// modulus is valid — so validateCertManagerKey's range check suffices there.
	if !wantsRSA {
		size := local.SigningKeys.CertManager.Size
		if size == 0 {
			size = 384 // matches the CRD default
		}
		wantSize := map[string]int32{"ES256": 256, "ES384": 384, "ES512": 521}[alg]
		if wantSize != 0 && size != wantSize {
			return field.Invalid(base.Child("signingKeys", "certManager", "size"), size,
				fmt.Sprintf("must be %d (the P-%d curve) to match signingAlgorithm %q", wantSize, wantSize, alg))
		}
	}
	return nil
}

// validateCertManagerKey rejects an algorithm/size combination cert-manager
// would reject when it mints the Certificate: an RSA modulus outside
// 2048..8192 bits, or an ECDSA size that is not one of the P-256/384/521 curve
// sizes. The CRD applies the algorithm and size defaults independently, so this
// also guards a partial override — e.g. algorithm=RSA left with the ECDSA-shaped
// default size (384), or size=2048 left with the ECDSA algorithm default — that
// would otherwise pass admission and fail much later at cert-manager. Empty
// fields are treated as the CRD defaults so the controller's defense-in-depth
// re-run behaves identically on an object that bypassed defaulting.
func validateCertManagerKey(cm *CertManagerSpec, base *field.Path) *field.Error {
	if cm == nil {
		return nil
	}
	alg := cm.Algorithm
	if alg == "" {
		alg = "ECDSA" // matches the CRD default
	}
	size := cm.Size
	if size == 0 {
		size = 384 // matches the CRD default
	}
	sizePath := base.Child("size")
	switch alg {
	case "RSA":
		if size < 2048 || size > 8192 {
			return field.Invalid(sizePath, size, "RSA key size must be between 2048 and 8192 bits")
		}
	case "ECDSA":
		if size != 256 && size != 384 && size != 521 {
			return field.Invalid(sizePath, size, "ECDSA key size must be one of 256, 384, or 521")
		}
	}
	return nil
}

// packdbDefaultMaxTokenAge is packdb's own built-in default for
// instance.auth.local.max_token_age (LocalAuthSpec.MaxTokenAge's doc
// comment), used by validateSigningKeyRotation as the floor for
// RetainDuration when MaxTokenAge is left unset.
const packdbDefaultMaxTokenAge = 24 * time.Hour

// validateSigningKeyRotation enforces that SigningKeyPolicy's
// RotationInterval and RetainDuration are always set together (base is
// already scoped to "signingKeys" by the caller), and that RetainDuration
// leaves at least MaxTokenAge of margin: a token signed in the instant a
// key is demoted remains valid until it ages out, so pruning that key
// any sooner would leave a still-valid token nothing can validate it
// against.
func validateSigningKeyRotation(local *LocalAuthSpec, base *field.Path) field.ErrorList {
	policy := local.SigningKeys
	rotation := policy.RotationInterval
	retain := policy.RetainDuration

	switch {
	case rotation == nil && retain == nil:
		return nil
	case rotation != nil && retain == nil:
		return field.ErrorList{field.Required(base.Child("retainDuration"),
			"required when signingKeys.rotationInterval is set")}
	case rotation == nil && retain != nil:
		return field.ErrorList{field.Forbidden(base.Child("retainDuration"),
			"must not be set when signingKeys.rotationInterval is unset")}
	}

	// A zero or negative interval makes stepSigningKeyRotation treat the active
	// key as perpetually overdue, so the controller mints a new key on every
	// reconcile — perpetual churn and repeated fleet rollouts. (metav1.Duration
	// cannot be range-checked by a kubebuilder marker, hence this Go guard.)
	if rotation.Duration <= 0 {
		return field.ErrorList{field.Invalid(base.Child("rotationInterval"), rotation.Duration.String(),
			"must be positive")}
	}

	maxTokenAge := packdbDefaultMaxTokenAge
	if local.MaxTokenAge != "" {
		// A MaxTokenAge that is unparseable OR non-positive is already
		// reported separately by validatePositiveDurationField; ignore it
		// here (falling back to the packdb default floor) rather than let a
		// bad value — most dangerously a negative duration — lower the
		// retain-duration floor below a real token's lifetime.
		if d, err := parsePackdbDuration(local.MaxTokenAge); err == nil && d > 0 {
			maxTokenAge = d
		}
	}
	if retain.Duration < maxTokenAge {
		return field.ErrorList{field.Invalid(base.Child("retainDuration"), retain.Duration.String(),
			fmt.Sprintf("must be at least maxTokenAge (%s): a token signed the instant a key is demoted "+
				"remains valid until it ages out, so the operator must not prune that key any sooner — "+
				"add extra margin on top of this minimum for a full engine fleet rollout to complete", maxTokenAge))}
	}
	return nil
}

// validatePreferredAuthorizationServer mirrors packdb's rule that a set
// preferred_authorization_server must name a configured authorization
// server: either the embedded "_local" server or one of the configured
// OIDC providers.
func validatePreferredAuthorizationServer(auth *AuthSpec, path *field.Path) field.ErrorList {
	preferred := auth.PreferredAuthorizationServer
	if preferred == "" || preferred == localAuthServerName {
		return nil
	}
	if auth.OIDC != nil {
		for _, p := range auth.OIDC.Providers {
			if p.Name == preferred {
				return nil
			}
		}
	}
	return field.ErrorList{field.Invalid(path, preferred,
		fmt.Sprintf("must be %q or the name of a configured spec.auth.oidc.providers[] entry", localAuthServerName))}
}

// validateExternalPostgres enforces that any user configuring an external
// PostgreSQL also provides a non-empty Secret reference for credentials.
// Without this check the metadata Deployment is still scheduled; kubelet
// then fails to mount a Secret volume with an empty name and the pod sits
// in ContainerCreating with only a kubelet event explaining why, which is
// invisible from the FireboltInstance CR. Catching it at admission time
// keeps the error close to the offending apply.
func validateExternalPostgres(inst *FireboltInstance) *field.Error {
	pg := inst.Spec.Metadata.Postgres
	if pg == nil {
		return nil
	}
	if pg.CredentialsSecretRef.Name == "" {
		return field.Required(
			field.NewPath("spec", "metadata", "postgres", "credentialsSecretRef", "name"),
			"must be set when spec.metadata.postgres is configured",
		)
	}
	return nil
}

func validateMetadataReplicas(inst *FireboltInstance) *field.Error {
	r := inst.Spec.Metadata.Replicas
	if r != nil && *r != 1 {
		return field.Invalid(
			field.NewPath("spec", "metadata", "replicas"),
			*r,
			"metadata replicas must be 1; multi-replica metadata is not currently supported",
		)
	}
	return nil
}
