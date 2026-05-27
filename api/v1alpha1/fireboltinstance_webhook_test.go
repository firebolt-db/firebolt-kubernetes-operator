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
	"k8s.io/apimachinery/pkg/api/resource"
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
