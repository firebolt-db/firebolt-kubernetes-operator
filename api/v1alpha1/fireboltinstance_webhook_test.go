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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
)

func TestDefaulter_GeneratesULID(t *testing.T) {
	d := &FireboltInstanceDefaulter{}
	inst := &FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec:       FireboltInstanceSpec{},
	}

	if err := d.Default(context.Background(), inst); err != nil {
		t.Fatalf("Default: unexpected error: %v", err)
	}

	if inst.Spec.ID == "" {
		t.Fatal("Default: expected spec.id to be set, got empty string")
	}
	if len(inst.Spec.ID) != 26 {
		t.Errorf("Default: expected 26-char ULID, got %d chars: %q", len(inst.Spec.ID), inst.Spec.ID)
	}
}

func TestDefaulter_PreservesExistingID(t *testing.T) {
	d := &FireboltInstanceDefaulter{}
	inst := &FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: FireboltInstanceSpec{
			ID: "my-custom-id",
		},
	}

	if err := d.Default(context.Background(), inst); err != nil {
		t.Fatalf("Default: unexpected error: %v", err)
	}

	if inst.Spec.ID != "my-custom-id" {
		t.Errorf("Default: expected spec.id to remain %q, got %q", "my-custom-id", inst.Spec.ID)
	}
}

// spec.id immutability is enforced by CEL
// (XValidation rule="oldSelf == '' || self == oldSelf") on the CRD, not by
// the webhook. No webhook-level test is needed; the rule explicitly allows
// the one-time empty->value transition used by the controller fallback
// when the mutating webhook is disabled.

func TestValidateUpdate_AllowsSameID(t *testing.T) {
	v := &FireboltInstanceCustomValidator{}
	oldInst := &FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: FireboltInstanceSpec{
			ID: "same-id",
		},
	}
	newInst := &FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: FireboltInstanceSpec{
			ID: "same-id",
		},
	}

	_, err := v.ValidateUpdate(context.Background(), oldInst, newInst)
	if err != nil {
		t.Errorf("ValidateUpdate: unexpected error: %v", err)
	}
}

func TestValidateMetadataReplicas(t *testing.T) {
	tests := []struct {
		name      string
		replicas  *int32
		wantError bool
	}{
		{
			name:      "replicas=1 is valid",
			replicas:  ptr.To(int32(1)),
			wantError: false,
		},
		{
			name:      "replicas=2 is rejected",
			replicas:  ptr.To(int32(2)),
			wantError: true,
		},
		{
			name:      "replicas=0 is rejected",
			replicas:  ptr.To(int32(0)),
			wantError: true,
		},
		{
			name:      "replicas=nil is allowed (controller defaults to 1)",
			replicas:  nil,
			wantError: false,
		},
	}

	v := &FireboltInstanceCustomValidator{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: FireboltInstanceSpec{
					Metadata: MetadataSpec{
						Replicas: tc.replicas,
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), inst)
			if tc.wantError && err == nil {
				t.Error("ValidateCreate: expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("ValidateCreate: unexpected error: %v", err)
			}

			_, err = v.ValidateUpdate(context.Background(), inst, inst)
			if tc.wantError && err == nil {
				t.Error("ValidateUpdate: expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("ValidateUpdate: unexpected error: %v", err)
			}
		})
	}
}

func TestValidateReservedKeys(t *testing.T) {
	tests := []struct {
		name           string
		metadataLabels map[string]string
		metadataAnns   map[string]string
		gatewayLabels  map[string]string
		gatewayAnns    map[string]string
		wantError      bool
	}{
		{
			name:      "no reserved keys is valid",
			wantError: false,
		},
		{
			name:           "user keys on metadata are valid",
			metadataLabels: map[string]string{"team": "data"},
			metadataAnns:   map[string]string{"owner": "sre"},
			wantError:      false,
		},
		{
			name:           "reserved key in metadata labels is rejected",
			metadataLabels: map[string]string{"firebolt.io/config-hash": "fake"},
			wantError:      true,
		},
		{
			name:         "reserved key in metadata annotations is rejected",
			metadataAnns: map[string]string{"firebolt.io/config-hash": "fake"},
			wantError:    true,
		},
		{
			name:          "reserved key in gateway labels is rejected",
			gatewayLabels: map[string]string{"firebolt.io/generation": "5"},
			wantError:     true,
		},
		{
			name:        "reserved key in gateway annotations is rejected",
			gatewayAnns: map[string]string{"firebolt.io/generation": "5"},
			wantError:   true,
		},
		{
			name: "mixed user + reserved keys still fail",
			metadataLabels: map[string]string{
				"team":                    "data",
				"firebolt.io/managed-by":  "other",
				"firebolt.io/config-hash": "fake",
			},
			wantError: true,
		},
	}

	v := &FireboltInstanceCustomValidator{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: FireboltInstanceSpec{
					Metadata: MetadataSpec{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels:      tc.metadataLabels,
								Annotations: tc.metadataAnns,
							},
						},
					},
					Gateway: GatewaySpec{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels:      tc.gatewayLabels,
								Annotations: tc.gatewayAnns,
							},
						},
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), inst)
			if tc.wantError && err == nil {
				t.Error("ValidateCreate: expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("ValidateCreate: unexpected error: %v", err)
			}

			_, err = v.ValidateUpdate(context.Background(), inst, inst)
			if tc.wantError && err == nil {
				t.Error("ValidateUpdate: expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("ValidateUpdate: unexpected error: %v", err)
			}
		})
	}
}

func TestValidateExternalPostgres(t *testing.T) {
	tests := []struct {
		name      string
		postgres  *PostgresSpec
		wantError bool
	}{
		{
			name:      "nil postgres (internal) is valid",
			postgres:  nil,
			wantError: false,
		},
		{
			name: "external postgres with secret name is valid",
			postgres: &PostgresSpec{
				Host:     "pg.example.com",
				Database: "firebolt",
				CredentialsSecretRef: corev1.LocalObjectReference{
					Name: "pg-creds",
				},
			},
			wantError: false,
		},
		{
			name: "external postgres with empty secret name is rejected",
			postgres: &PostgresSpec{
				Host:     "pg.example.com",
				Database: "firebolt",
			},
			wantError: true,
		},
	}

	v := &FireboltInstanceCustomValidator{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: FireboltInstanceSpec{
					Metadata: MetadataSpec{
						Postgres: tc.postgres,
					},
				},
			}

			_, err := v.ValidateCreate(context.Background(), inst)
			if tc.wantError && err == nil {
				t.Error("ValidateCreate: expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("ValidateCreate: unexpected error: %v", err)
			}
		})
	}
}

// validAdminSpec returns an AdminSpec that satisfies validateLocalAuth on
// its own, so individual test cases below only need to override the one
// field under test.
func validAdminSpec() AdminSpec {
	return AdminSpec{
		Password: corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "admin-creds"},
			Key:                  "password",
		},
	}
}

// validSigningKeys returns a SigningKeyPolicy with an RSA cert-manager
// key, matching the default (empty-string) SigningAlgorithm of RS256.
func validSigningKeys() *SigningKeyPolicy {
	return &SigningKeyPolicy{
		CertManager: CertManagerSpec{
			IssuerRef: CertManagerIssuerRef{Name: "internal-ca"},
		},
	}
}

func TestValidateAuth(t *testing.T) {
	tests := []struct {
		name      string
		auth      *AuthSpec
		wantError bool
	}{
		{
			name:      "nil auth is valid",
			auth:      nil,
			wantError: false,
		},
		{
			name:      "disabled with nothing else set is valid",
			auth:      &AuthSpec{Enabled: false},
			wantError: false,
		},
		{
			name: "disabled with local set is rejected",
			auth: &AuthSpec{
				Enabled: false,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
			},
			wantError: true,
		},
		{
			name: "disabled with oidc set is rejected",
			auth: &AuthSpec{
				Enabled: false,
				OIDC: &OIDCAuthSpec{Providers: []OIDCProviderSpec{
					{Name: "okta", DiscoveryURL: "https://okta.example.com/.well-known/openid-configuration", UsernameMapping: "{{ email }}"},
				}},
			},
			wantError: true,
		},
		{
			name: "disabled with preferredAuthorizationServer set is rejected",
			auth: &AuthSpec{
				Enabled:                      false,
				PreferredAuthorizationServer: "_local",
			},
			wantError: true,
		},
		{
			name:      "enabled with no local is rejected (admin required)",
			auth:      &AuthSpec{Enabled: true},
			wantError: true,
		},
		{
			name: "enabled with local but no admin password name is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:       AdminSpec{Password: corev1.SecretKeySelector{Key: "password"}},
					SigningKeys: validSigningKeys(),
				},
			},
			wantError: true,
		},
		{
			name: "enabled with local but no admin password key is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:       AdminSpec{Password: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "admin-creds"}}},
					SigningKeys: validSigningKeys(),
				},
			},
			wantError: true,
		},
		{
			name: "enabled with local but no signingKeys is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec()},
			},
			wantError: true,
		},
		{
			name: "enabled with fully populated local is valid",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
			},
			wantError: false,
		},
		{
			name: "RS256 (default) with default (RSA) cert-manager key is valid",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
			},
			wantError: false,
		},
		{
			name: "RS256 with ECDSA cert-manager key is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:            validAdminSpec(),
					SigningAlgorithm: "RS256",
					SigningKeys: &SigningKeyPolicy{
						CertManager: CertManagerSpec{
							IssuerRef: CertManagerIssuerRef{Name: "internal-ca"},
							Algorithm: "ECDSA",
						},
					},
				},
			},
			wantError: true,
		},
		{
			name: "ES256 with RSA cert-manager key is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:            validAdminSpec(),
					SigningAlgorithm: "ES256",
					SigningKeys:      validSigningKeys(), // defaults to RSA
				},
			},
			wantError: true,
		},
		{
			name: "ES256 with ECDSA cert-manager key is valid",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:            validAdminSpec(),
					SigningAlgorithm: "ES256",
					SigningKeys: &SigningKeyPolicy{
						CertManager: CertManagerSpec{
							IssuerRef: CertManagerIssuerRef{Name: "internal-ca"},
							Algorithm: "ECDSA",
						},
					},
				},
			},
			wantError: false,
		},
		{
			name: "preferredAuthorizationServer=_local is valid",
			auth: &AuthSpec{
				Enabled:                      true,
				PreferredAuthorizationServer: "_local",
				Local:                        &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
			},
			wantError: false,
		},
		{
			name: "preferredAuthorizationServer matching a configured oidc provider is valid",
			auth: &AuthSpec{
				Enabled:                      true,
				PreferredAuthorizationServer: "okta",
				Local:                        &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
				OIDC: &OIDCAuthSpec{Providers: []OIDCProviderSpec{
					{Name: "okta", DiscoveryURL: "https://okta.example.com/.well-known/openid-configuration", UsernameMapping: "{{ email }}"},
				}},
			},
			wantError: false,
		},
		{
			name: "preferredAuthorizationServer naming an unconfigured server is rejected",
			auth: &AuthSpec{
				Enabled:                      true,
				PreferredAuthorizationServer: "no-such-provider",
				Local:                        &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
			},
			wantError: true,
		},
		{
			name: "local jwt durations in packdb's Go-style+days grammar are valid",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:              validAdminSpec(),
					SigningKeys:        validSigningKeys(),
					TokenExpiry:        "1h",
					MaxTokenAge:        "1d",
					ClockSkewTolerance: "30s",
				},
			},
			wantError: false,
		},
		{
			name: "unparseable local jwt duration is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:       validAdminSpec(),
					SigningKeys: validSigningKeys(),
					TokenExpiry: "1hr", // not a valid Go/packdb duration unit
				},
			},
			wantError: true,
		},
		{
			name: "oidc.jwt duration fields validated the same way as local's",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
				OIDC: &OIDCAuthSpec{
					JWT: &OIDCJWTSpec{ClockSkewTolerance: "not-a-duration"},
					Providers: []OIDCProviderSpec{
						{Name: "okta", DiscoveryURL: "https://okta.example.com/.well-known/openid-configuration", UsernameMapping: "{{ email }}"},
					},
				},
			},
			wantError: true,
		},
		{
			name: "oidc provider jwks.cacheTTL and discovery.refreshInterval accept packdb's days unit",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
				OIDC: &OIDCAuthSpec{Providers: []OIDCProviderSpec{
					{
						Name: "okta", DiscoveryURL: "https://okta.example.com/.well-known/openid-configuration", UsernameMapping: "{{ email }}",
						JWKS:      &OIDCJWKSSpec{CacheTTL: "1d"},
						Discovery: &OIDCDiscoverySpec{RefreshInterval: "1d"},
					},
				}},
			},
			wantError: false,
		},
		{
			name: "oidc provider discovery.refreshInterval of zero is rejected " +
				"(packdb's AuthConfig::Validate rejects non-positive refresh_interval)",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
				OIDC: &OIDCAuthSpec{Providers: []OIDCProviderSpec{
					{
						Name: "okta", DiscoveryURL: "https://okta.example.com/.well-known/openid-configuration", UsernameMapping: "{{ email }}",
						Discovery: &OIDCDiscoverySpec{RefreshInterval: "0s"},
					},
				}},
			},
			wantError: true,
		},
		{
			name: "oidc provider discovery.refreshInterval negative is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
				OIDC: &OIDCAuthSpec{Providers: []OIDCProviderSpec{
					{
						Name: "okta", DiscoveryURL: "https://okta.example.com/.well-known/openid-configuration", UsernameMapping: "{{ email }}",
						Discovery: &OIDCDiscoverySpec{RefreshInterval: "-1h"},
					},
				}},
			},
			wantError: true,
		},
		{
			name: "rotationInterval without retainDuration is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin: validAdminSpec(),
					SigningKeys: &SigningKeyPolicy{
						CertManager:      validSigningKeys().CertManager,
						RotationInterval: &metav1.Duration{Duration: 30 * 24 * time.Hour},
					},
				},
			},
			wantError: true,
		},
		{
			name: "retainDuration without rotationInterval is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin: validAdminSpec(),
					SigningKeys: &SigningKeyPolicy{
						CertManager:    validSigningKeys().CertManager,
						RetainDuration: &metav1.Duration{Duration: 24 * time.Hour},
					},
				},
			},
			wantError: true,
		},
		{
			name: "retainDuration shorter than the default 1-day maxTokenAge is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin: validAdminSpec(),
					SigningKeys: &SigningKeyPolicy{
						CertManager:      validSigningKeys().CertManager,
						RotationInterval: &metav1.Duration{Duration: 30 * 24 * time.Hour},
						RetainDuration:   &metav1.Duration{Duration: time.Hour},
					},
				},
			},
			wantError: true,
		},
		{
			name: "retainDuration shorter than an explicit maxTokenAge is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:       validAdminSpec(),
					MaxTokenAge: "2d",
					SigningKeys: &SigningKeyPolicy{
						CertManager:      validSigningKeys().CertManager,
						RotationInterval: &metav1.Duration{Duration: 30 * 24 * time.Hour},
						RetainDuration:   &metav1.Duration{Duration: 24 * time.Hour}, // < 2d
					},
				},
			},
			wantError: true,
		},
		{
			name: "rotationInterval with retainDuration at least maxTokenAge is valid",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin: validAdminSpec(),
					SigningKeys: &SigningKeyPolicy{
						CertManager:      validSigningKeys().CertManager,
						RotationInterval: &metav1.Duration{Duration: 30 * 24 * time.Hour},
						RetainDuration:   &metav1.Duration{Duration: 24 * time.Hour},
					},
				},
			},
			wantError: false,
		},
		{
			name: "negative local maxTokenAge is rejected (a negative floor would defeat the rotation retain-duration check)",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:       validAdminSpec(),
					SigningKeys: validSigningKeys(),
					MaxTokenAge: "-1d",
				},
			},
			wantError: true,
		},
		{
			name: "zero local tokenExpiry is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local: &LocalAuthSpec{
					Admin:       validAdminSpec(),
					SigningKeys: validSigningKeys(),
					TokenExpiry: "0s",
				},
			},
			wantError: true,
		},
		{
			name: "negative oidc.jwt maxTokenAge is rejected",
			auth: &AuthSpec{
				Enabled: true,
				Local:   &LocalAuthSpec{Admin: validAdminSpec(), SigningKeys: validSigningKeys()},
				OIDC: &OIDCAuthSpec{
					JWT: &OIDCJWTSpec{MaxTokenAge: "-1h"},
					Providers: []OIDCProviderSpec{
						{Name: "okta", DiscoveryURL: "https://okta.example.com/.well-known/openid-configuration", UsernameMapping: "{{ email }}"},
					},
				},
			},
			wantError: true,
		},
	}

	v := &FireboltInstanceCustomValidator{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: FireboltInstanceSpec{
					Auth: tc.auth,
				},
			}

			_, err := v.ValidateCreate(context.Background(), inst)
			if tc.wantError && err == nil {
				t.Error("ValidateCreate: expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("ValidateCreate: unexpected error: %v", err)
			}
		})
	}
}

func TestValidateTLS(t *testing.T) {
	tests := []struct {
		name      string
		tls       *TLSSpec
		wantError bool
	}{
		{
			name:      "nil tls is valid",
			tls:       nil,
			wantError: false,
		},
		{
			name:      "engine disabled with nothing else set is valid",
			tls:       &TLSSpec{Engine: &TLSListenerSpec{Enabled: false}},
			wantError: false,
		},
		{
			name:      "engine enabled with no certManager is rejected",
			tls:       &TLSSpec{Engine: &TLSListenerSpec{Enabled: true}},
			wantError: true,
		},
		{
			name: "engine enabled with certManager but no issuer name is rejected",
			tls: &TLSSpec{Engine: &TLSListenerSpec{
				Enabled:     true,
				CertManager: &CertManagerSpec{},
			}},
			wantError: true,
		},
		{
			name: "engine enabled with issuer name is valid",
			tls: &TLSSpec{Engine: &TLSListenerSpec{
				Enabled:     true,
				CertManager: &CertManagerSpec{IssuerRef: CertManagerIssuerRef{Name: "internal-ca"}},
			}},
			wantError: false,
		},
		{
			name:      "gateway disabled with nothing else set is valid",
			tls:       &TLSSpec{Gateway: &TLSListenerSpec{Enabled: false}},
			wantError: false,
		},
		{
			name:      "gateway enabled with no certManager is rejected",
			tls:       &TLSSpec{Gateway: &TLSListenerSpec{Enabled: true}},
			wantError: true,
		},
		{
			name: "gateway enabled with certManager but no issuer name is rejected",
			tls: &TLSSpec{Gateway: &TLSListenerSpec{
				Enabled:     true,
				CertManager: &CertManagerSpec{},
			}},
			wantError: true,
		},
		{
			name: "gateway enabled with issuer name is valid even without explicit dnsNames",
			tls: &TLSSpec{Gateway: &TLSListenerSpec{
				Enabled:     true,
				CertManager: &CertManagerSpec{IssuerRef: CertManagerIssuerRef{Name: "internal-ca"}},
			}},
			wantError: false,
		},
	}

	v := &FireboltInstanceCustomValidator{}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
				Spec: FireboltInstanceSpec{
					TLS: tc.tls,
				},
			}

			_, err := v.ValidateCreate(context.Background(), inst)
			if tc.wantError && err == nil {
				t.Error("ValidateCreate: expected error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Errorf("ValidateCreate: unexpected error: %v", err)
			}
		})
	}
}

// TestParsePackdbDuration pins down the grammar against packdb's actual
// duration parser (src/Common/Configuration/Unit/Duration.h): Go's
// time.ParseDuration grammar plus a "d" (days) unit that Go's standard
// library doesn't support, since every duration default in packdb's own
// schema (application-config.schema.json) uses "d" for values of a day or
// more (e.g. instance.auth.local.jwt.max_token_age default "1d").
func TestParsePackdbDuration(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{name: "plain Go duration passes through", in: "30s", want: 30 * time.Second},
		{name: "hours", in: "1h", want: time.Hour},
		{name: "Go multi-component still works", in: "1h30m", want: time.Hour + 30*time.Minute},
		{name: "single day", in: "1d", want: 24 * time.Hour},
		{name: "multiple days", in: "7d", want: 7 * 24 * time.Hour},
		{name: "fractional days", in: "0.5d", want: 12 * time.Hour},
		{name: "negative days", in: "-1d", want: -24 * time.Hour},
		{name: "empty string is invalid", in: "", wantErr: true},
		{name: "bare number with no unit is invalid", in: "5", wantErr: true},
		{name: "bogus unit is invalid", in: "1hr", wantErr: true},
		{name: "bare d with no number is invalid", in: "d", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePackdbDuration(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parsePackdbDuration(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePackdbDuration(%q): unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsePackdbDuration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	v := &FireboltInstanceCustomValidator{}
	inst := &FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	_, err := v.ValidateDelete(context.Background(), inst)
	if err != nil {
		t.Errorf("ValidateDelete: unexpected error: %v", err)
	}
}

// instanceWithComponentTemplates returns a minimal FireboltInstance
// whose gateway and metadata templates are non-nil; tests mutate
// inst.Spec.Gateway.Template or .Metadata.Template before validating.
func instanceWithComponentTemplates() *FireboltInstance {
	return &FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: FireboltInstanceSpec{
			Metadata: MetadataSpec{Template: &corev1.PodTemplateSpec{}},
			Gateway:  GatewaySpec{Template: &corev1.PodTemplateSpec{}},
		},
	}
}

// TestFireboltInstanceValidator_GatewayRejectsOwnedFields runs every
// rejection rule on the gateway template through the validator.
// Asserts each mutation surfaces a field.Path that includes the
// offending coordinate; bundles every case in one table so a new
// owned-field check lands here without sprawling files.
func TestFireboltInstanceValidator_GatewayRejectsOwnedFields(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*FireboltInstance)
		wantField string
	}{
		{
			name: "pod terminationGracePeriodSeconds",
			mutate: func(inst *FireboltInstance) {
				v := int64(30)
				inst.Spec.Gateway.Template.Spec.TerminationGracePeriodSeconds = &v
			},
			wantField: "spec.gateway.template.spec.terminationGracePeriodSeconds",
		},
		{
			name: "pod subdomain",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Subdomain = "headless"
			},
			wantField: "spec.gateway.template.spec.subdomain",
		},
		{
			name: "envoy container command",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name:    GatewayContainerName,
					Command: []string{"/bin/sh"},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].command",
		},
		{
			name: "envoy container args",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name: GatewayContainerName,
					Args: []string{"-x"},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].args",
		},
		{
			name: "envoy container ports",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name:  GatewayContainerName,
					Ports: []corev1.ContainerPort{{ContainerPort: 1234}},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].ports",
		},
		{
			name: "envoy container readinessProbe",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name:           GatewayContainerName,
					ReadinessProbe: &corev1.Probe{},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].readinessProbe",
		},
		{
			name: "envoy container lifecycle (preStop is operator-owned)",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name:      GatewayContainerName,
					Lifecycle: &corev1.Lifecycle{},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].lifecycle",
		},
		{
			name: "envoy container securityContext is operator-owned",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name:            GatewayContainerName,
					SecurityContext: &corev1.SecurityContext{},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].securityContext",
		},
		{
			name: "envoy container env is operator-owned",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name: GatewayContainerName,
					Env:  []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].env",
		},
		{
			name: "envoy container volumeMounts is operator-owned",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{{
					Name: GatewayContainerName,
					VolumeMounts: []corev1.VolumeMount{
						{Name: "my-vol", MountPath: "/data"},
					},
				}}
			},
			wantField: "spec.gateway.template.spec.containers[0].volumeMounts",
		},
		{
			name: "second envoy container is rejected as duplicate",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Spec.Containers = []corev1.Container{
					{Name: GatewayContainerName},
					{Name: GatewayContainerName},
				}
			},
			wantField: "spec.gateway.template.spec.containers[1].name",
		},
		{
			name: "reserved firebolt.io label on gateway template",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Gateway.Template.Labels = map[string]string{
					"firebolt.io/config-hash": "abc",
				}
			},
			wantField: "spec.gateway.template.metadata.labels",
		},
	}

	v := &FireboltInstanceCustomValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := instanceWithComponentTemplates()
			tc.mutate(inst)
			_, err := v.ValidateCreate(context.Background(), inst)
			if err == nil {
				t.Fatalf("ValidateCreate: expected error containing %q, got nil", tc.wantField)
			}
			if !contains(err.Error(), tc.wantField) {
				t.Errorf("ValidateCreate: error %q does not mention field path %q", err.Error(), tc.wantField)
			}
		})
	}
}

// TestFireboltInstanceValidator_MetadataRejectsOwnedFields mirrors
// the gateway table for metadata: command, ports, probes, securityContext,
// env (including the operator-injected POSTGRES_*_FILE keys),
// volumeMounts, and the duplicate-primary check.
func TestFireboltInstanceValidator_MetadataRejectsOwnedFields(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*FireboltInstance)
		wantField string
	}{
		{
			name: "pod hostname",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Metadata.Template.Spec.Hostname = "pensieve-0"
			},
			wantField: "spec.metadata.template.spec.hostname",
		},
		{
			name: "metadata container command",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Metadata.Template.Spec.Containers = []corev1.Container{{
					Name:    MetadataContainerName,
					Command: []string{"/bin/sh"},
				}}
			},
			wantField: "spec.metadata.template.spec.containers[0].command",
		},
		{
			name: "metadata container env is operator-owned",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Metadata.Template.Spec.Containers = []corev1.Container{{
					Name: MetadataContainerName,
					Env: []corev1.EnvVar{
						{Name: MetadataPostgresUsernameEnvKey, Value: "/etc/pg/u"},
					},
				}}
			},
			wantField: "spec.metadata.template.spec.containers[0].env",
		},
		{
			name: "metadata container volumeMounts is operator-owned",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Metadata.Template.Spec.Containers = []corev1.Container{{
					Name: MetadataContainerName,
					VolumeMounts: []corev1.VolumeMount{
						{Name: "extra", MountPath: "/data"},
					},
				}}
			},
			wantField: "spec.metadata.template.spec.containers[0].volumeMounts",
		},
		{
			name: "second metadata container is rejected as duplicate",
			mutate: func(inst *FireboltInstance) {
				inst.Spec.Metadata.Template.Spec.Containers = []corev1.Container{
					{Name: MetadataContainerName},
					{Name: MetadataContainerName},
				}
			},
			wantField: "spec.metadata.template.spec.containers[1].name",
		},
	}

	v := &FireboltInstanceCustomValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := instanceWithComponentTemplates()
			tc.mutate(inst)
			_, err := v.ValidateCreate(context.Background(), inst)
			if err == nil {
				t.Fatalf("ValidateCreate: expected error containing %q, got nil", tc.wantField)
			}
			if !contains(err.Error(), tc.wantField) {
				t.Errorf("ValidateCreate: error %q does not mention field path %q", err.Error(), tc.wantField)
			}
		})
	}
}

// TestFireboltInstanceValidator_AllowsUserPermittedFields pins down
// the positive surface: scheduling fields, the user-allowed primary
// container fields (image, imagePullPolicy, resources), pod-level
// volumes / imagePullSecrets / podSecurityContext / serviceAccountName,
// sidecars and additional init containers all pass validation.
func TestFireboltInstanceValidator_AllowsUserPermittedFields(t *testing.T) {
	v := &FireboltInstanceCustomValidator{}
	inst := instanceWithComponentTemplates()
	inst.Spec.Gateway.Template.Spec = corev1.PodSpec{
		ServiceAccountName: "user-sa",
		NodeSelector:       map[string]string{"pool": "system"},
		Tolerations:        []corev1.Toleration{{Key: "pool", Operator: corev1.TolerationOpEqual, Value: "system"}},
		Affinity:           &corev1.Affinity{},
		TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
			{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone"},
		},
		PriorityClassName: "critical",
		SecurityContext:   &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true)},
		ImagePullSecrets:  []corev1.LocalObjectReference{{Name: "ghcr-creds"}},
		Volumes: []corev1.Volume{
			{Name: "my-extra-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
		Containers: []corev1.Container{
			{
				Name:  GatewayContainerName,
				Image: "envoyproxy/envoy:custom",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resourceMustParse("100m")},
				},
			},
			{
				Name:  "log-shipper",
				Image: "fluent/fluent-bit:latest",
			},
		},
		InitContainers: []corev1.Container{
			{Name: "config-validator", Image: "busybox"},
		},
	}
	inst.Spec.Metadata.Template.Spec = corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:            MetadataContainerName,
				Image:           "ghcr.io/firebolt-db/metadata:custom",
				ImagePullPolicy: corev1.PullAlways,
			},
		},
	}
	if _, err := v.ValidateCreate(context.Background(), inst); err != nil {
		t.Fatalf("ValidateCreate: unexpected error on user-permitted template: %v", err)
	}
}

// contains is a tiny substring helper kept local so tests don't pull
// strings.Contains imports for one call site.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func resourceMustParse(s string) resource.Quantity {
	return resource.MustParse(s)
}

// TestValidateSigningKeyRotation_NonPositiveMaxTokenAgeUsesDefaultFloor is a
// direct regression for the negative-duration finding: even if a non-positive
// MaxTokenAge reached validateSigningKeyRotation (it is now rejected upstream
// by validatePositiveDurationField, but this cross-check must not depend on
// that), the retain-duration floor must fall back to packdbDefaultMaxTokenAge
// rather than adopt the bad value — otherwise a "-1d" floor would approve a
// retainDuration far shorter than a real token's lifetime.
func TestValidateSigningKeyRotation_NonPositiveMaxTokenAgeUsesDefaultFloor(t *testing.T) {
	base := field.NewPath("spec", "auth", "local", "signingKeys")
	for _, mta := range []string{"-1d", "0s"} {
		local := &LocalAuthSpec{
			Admin:       validAdminSpec(),
			MaxTokenAge: mta,
			SigningKeys: &SigningKeyPolicy{
				CertManager:      validSigningKeys().CertManager,
				RotationInterval: &metav1.Duration{Duration: 30 * 24 * time.Hour},
				// 1h < the 24h default floor: must be rejected because the bad
				// MaxTokenAge is ignored, not treated as the (negative/zero) floor.
				RetainDuration: &metav1.Duration{Duration: time.Hour},
			},
		}
		if errs := validateSigningKeyRotation(local, base); len(errs) == 0 {
			t.Errorf("maxTokenAge=%q: expected retainDuration rejected against the default floor, got no error", mta)
		}
	}
}
