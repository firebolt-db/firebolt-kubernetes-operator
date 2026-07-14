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
	_ context.Context, _, newInst *FireboltInstance,
) (admission.Warnings, error) {
	// spec.id immutability is enforced by CEL on the CRD itself
	// (XValidation rule="oldSelf == '' || self == oldSelf"), so it works
	// even when webhooks are disabled. The empty->value transition is
	// explicitly allowed so the controller fallback can generate and
	// persist an ID when the defaulting webhook is not active.
	return nil, validateSpec(newInst).ToAggregate()
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
	errs = append(errs, validateTLSListener(tls.Engine, field.NewPath("spec", "tls", "engine"))...)
	errs = append(errs, validateTLSListener(tls.Gateway, field.NewPath("spec", "tls", "gateway"))...)
	return errs
}

// validateTLSListener requires a cert-manager provisioning policy with a
// named issuer whenever a TLS listener is enabled: the operator has no
// bring-your-own-Secret alternative (see TLSListenerSpec's doc comment),
// so an enabled listener with no CertManager block, or one with an empty
// issuer name, can never actually be provisioned.
func validateTLSListener(listener *TLSListenerSpec, base *field.Path) field.ErrorList {
	if listener == nil || !listener.Enabled {
		return nil
	}
	if listener.CertManager == nil {
		return field.ErrorList{field.Required(base.Child("certManager"),
			"required when enabled is true: the operator provisions every certificate via cert-manager")}
	}
	if listener.CertManager.IssuerRef.Name == "" {
		return field.ErrorList{field.Required(base.Child("certManager", "issuerRef", "name"), "required")}
	}
	return nil
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

	for i, p := range oidc.Providers {
		providerPath := base.Child("providers").Index(i)
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
		alg = "RS256" // matches the CRD default
	}
	keyAlg := local.SigningKeys.CertManager.Algorithm
	if keyAlg == "" {
		keyAlg = "RSA" // matches the CRD default
	}

	wantsRSA := strings.HasPrefix(alg, "RS")
	switch {
	case wantsRSA && keyAlg != "RSA":
		return field.Invalid(base.Child("signingAlgorithm"), alg,
			fmt.Sprintf("requires signingKeys.certManager.algorithm=RSA, got %q", keyAlg))
	case !wantsRSA && keyAlg != "ECDSA":
		return field.Invalid(base.Child("signingAlgorithm"), alg,
			fmt.Sprintf("requires signingKeys.certManager.algorithm=ECDSA, got %q", keyAlg))
	default:
		return nil
	}
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
