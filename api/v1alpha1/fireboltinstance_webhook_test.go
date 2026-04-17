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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
						ComponentSpec: ComponentSpec{
							Replicas: tc.replicas,
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
						ComponentSpec: ComponentSpec{
							Labels:      tc.metadataLabels,
							Annotations: tc.metadataAnns,
						},
					},
					Gateway: GatewaySpec{
						ComponentSpec: ComponentSpec{
							Labels:      tc.gatewayLabels,
							Annotations: tc.gatewayAnns,
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
