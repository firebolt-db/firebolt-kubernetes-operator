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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func TestBuildMetadataConfigYAMLSchema(t *testing.T) {
	tests := []struct {
		name     string
		postgres *computev1alpha1.PostgresSpec
		want     string
	}{
		{
			name:     "internal postgres uses default schema",
			postgres: nil,
			want:     `schema: "public"`,
		},
		{
			name: "external postgres without schema falls back to public",
			postgres: &computev1alpha1.PostgresSpec{
				Host:                 "pg.example.com",
				Database:             "fb",
				CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
			},
			want: `schema: "public"`,
		},
		{
			name: "external postgres with custom schema is honored",
			postgres: &computev1alpha1.PostgresSpec{
				Host:                 "pg.example.com",
				Database:             "fb",
				Schema:               "firebolt_metadata",
				CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
			},
			want: `schema: "firebolt_metadata"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst := &computev1alpha1.FireboltInstance{
				ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"},
				Spec: computev1alpha1.FireboltInstanceSpec{
					ID: "acc-1",
					Metadata: computev1alpha1.MetadataSpec{
						Postgres: tc.postgres,
					},
				},
			}
			got := buildMetadataConfigYAML(inst)
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected %q in rendered config; got:\n%s", tc.want, got)
			}
		})
	}
}

func TestMetadataConfigHashInternalPostgresUsesConfigOnly(t *testing.T) {
	instance := mkMetadataInstance()
	configYAML := buildMetadataConfigYAML(instance)

	// Internal PostgreSQL must use only the config hash and never read a Secret.
	reconciler := &FireboltInstanceReconciler{}
	got, err := reconciler.metadataConfigHash(context.Background(), instance, configYAML)
	if err != nil {
		t.Fatalf("metadataConfigHash: %v", err)
	}
	if want := contentHash(configYAML); got != want {
		t.Fatalf("internal metadata config hash = %q, want config-only hash %q", got, want)
	}
}

func TestMetadataConfigHashExternalPostgresWithoutTLSIncludesCredentialsSecretVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := mkMetadataInstance()
	instance.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host: "postgres.example.com", Database: "metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "external-creds"},
	}
	configYAML := buildMetadataConfigYAML(instance)
	hashForSecret := func(secret *corev1.Secret) string {
		t.Helper()
		reconciler := &FireboltInstanceReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		}
		hash, err := reconciler.metadataConfigHash(context.Background(), instance, configYAML)
		if err != nil {
			t.Fatalf("metadataConfigHash: %v", err)
		}
		return hash
	}

	secret := func(resourceVersion, username, password string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "external-creds",
				Namespace:       instance.Namespace,
				ResourceVersion: resourceVersion,
			},
			Data: map[string][]byte{
				"username": []byte(username),
				"password": []byte(password),
				"database": []byte("ignored"),
			},
		}
	}
	base := secret("10", "metadata-user", "password-one")
	baseHash := hashForSecret(base)

	sameCredentials := secret("11", "metadata-user", "password-one")
	sameCredentials.Annotations = map[string]string{"synchronizer.example/version": "2"}
	if got := hashForSecret(sameCredentials); got == baseHash {
		t.Fatalf("Secret resource-version change did not change metadata config hash: %q", got)
	}
	differentCredentialsSameVersion := secret("10", "metadata-user-2", "password-two")
	if got := hashForSecret(differentCredentialsSameVersion); got != baseHash {
		t.Fatalf("credential bytes entered metadata config hash: got %q, want %q", got, baseHash)
	}
	for _, credential := range []string{"metadata-user", "password-one"} {
		if strings.Contains(baseHash, credential) || baseHash == contentHash(credential) {
			t.Fatalf("metadata config hash exposes an individual credential: %q", baseHash)
		}
	}
}

func TestEnsureMetadataDeploymentAppliesComputedConfigHash(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := mkMetadataInstance()
	instance.UID = types.UID("instance-uid")
	instance.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host: "postgres.example.com", Database: "metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "external-creds"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-creds", Namespace: instance.Namespace},
		Data: map[string][]byte{
			"username": []byte("metadata-user"),
			"password": []byte("metadata-password"),
		},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance, secret).Build()
	reconciler := &FireboltInstanceReconciler{Client: client, Scheme: scheme}
	configYAML := buildMetadataConfigYAML(instance)
	want, err := reconciler.metadataConfigHash(context.Background(), instance, configYAML)
	if err != nil {
		t.Fatalf("metadataConfigHash: %v", err)
	}

	if err := reconciler.ensureMetadataDeployment(context.Background(), instance, configYAML); err != nil {
		t.Fatalf("ensureMetadataDeployment: %v", err)
	}
	applied := &appsv1.Deployment{}
	if err := client.Get(context.Background(), types.NamespacedName{
		Namespace: instance.Namespace,
		Name:      instance.Name + SuffixMetadataService,
	}, applied); err != nil {
		t.Fatalf("get applied metadata Deployment: %v", err)
	}
	if got := applied.Spec.Template.Annotations[AnnotationConfigHash]; got != want {
		t.Fatalf("applied metadata config hash = %q, want %q", got, want)
	}
	if got := len(applied.Spec.Template.Annotations); got != 1 {
		t.Fatalf("applied metadata Deployment has %d annotations, want one aggregate hash", got)
	}
}

func TestMetadataConfigHashExternalPostgresTLSIncludesCASecretVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := mkMetadataInstance()
	instance.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host:                 "postgres.example.com",
		Database:             "metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "external-creds"},
		TLS: &computev1alpha1.PostgresTLSSpec{
			Mode: computev1alpha1.PostgresTLSModeVerifyFull,
			CASecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "external-ca"},
				Key:                  "root.pem",
			},
		},
	}
	configYAML := buildMetadataConfigYAML(instance)
	hashForVersions := func(credentialsVersion, caVersion string) string {
		t.Helper()
		credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: "external-creds", Namespace: instance.Namespace, ResourceVersion: credentialsVersion,
		}}
		ca := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "external-ca", Namespace: instance.Namespace, ResourceVersion: caVersion,
			},
			Data: map[string][]byte{"root.pem": []byte("test CA")},
		}
		reconciler := &FireboltInstanceReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(credentials, ca).Build(),
		}
		hash, err := reconciler.metadataConfigHash(context.Background(), instance, configYAML)
		if err != nil {
			t.Fatalf("metadataConfigHash: %v", err)
		}
		return hash
	}

	baseline := hashForVersions("10", "20")
	if rotatedCredentials := hashForVersions("11", "20"); baseline == rotatedCredentials {
		t.Fatalf("metadata config hash did not change across credentials Secret rotation: %q", baseline)
	}
	if rotatedCA := hashForVersions("10", "21"); baseline == rotatedCA {
		t.Fatalf("metadata config hash did not change across CA Secret rotation: %q", baseline)
	}
}

func TestCheckExternalPostgresSecretPreflightsTLSCAKey(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := mkMetadataInstance()
	instance.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host:                 "postgres.example.com",
		Database:             "metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "external-creds"},
		TLS: &computev1alpha1.PostgresTLSSpec{
			Mode: computev1alpha1.PostgresTLSModeVerifyFull,
			CASecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "external-ca"},
				Key:                  "root.pem",
			},
		},
	}
	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-creds", Namespace: instance.Namespace},
	}

	tests := []struct {
		name      string
		ca        *corev1.Secret
		wantError string
	}{
		{
			name: "selected key is present",
			ca: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "external-ca", Namespace: instance.Namespace},
				Data:       map[string][]byte{"root.pem": []byte("test CA"), "unused.pem": []byte("unused")},
			},
		},
		{
			name:      "CA Secret is missing",
			wantError: "external postgres CA Secret ns-1/external-ca not found",
		},
		{
			name: "selected key is missing",
			ca: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "external-ca", Namespace: instance.Namespace},
				Data:       map[string][]byte{"another.pem": []byte("test CA")},
			},
			wantError: `missing or empty for key "root.pem"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			objects := []runtime.Object{credentials.DeepCopy()}
			if tc.ca != nil {
				objects = append(objects, tc.ca)
			}
			reconciler := &FireboltInstanceReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build(),
			}
			err := reconciler.checkExternalPostgresSecret(context.Background(), instance)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("checkExternalPostgresSecret: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("checkExternalPostgresSecret error = %v, want substring %q", err, tc.wantError)
			}
		})
	}

	t.Run("controller rejects unsupported TLS mode", func(t *testing.T) {
		invalid := instance.DeepCopy()
		invalid.Spec.Metadata.Postgres.TLS.Mode = "require"
		reconciler := &FireboltInstanceReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
		}
		err := reconciler.checkExternalPostgresSecret(context.Background(), invalid)
		if err == nil || !strings.Contains(err.Error(), "tls.mode") {
			t.Fatalf("checkExternalPostgresSecret error = %v, want tls.mode validation error", err)
		}
	})
}

func TestMetadataPostgresSecretsMapOnlyExternalReferences(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	referencing := mkMetadataInstance()
	referencing.Name = "references-secret"
	referencing.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host: "postgres.example.com", Database: "metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "external-creds"},
		TLS: &computev1alpha1.PostgresTLSSpec{
			CASecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "external-ca"},
				Key:                  "root.pem",
			},
		},
	}
	other := mkMetadataInstance()
	other.Name = "other-instance"
	other.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host: "postgres.example.com", Database: "other",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "other-creds"},
	}
	internal := mkMetadataInstance()
	internal.Name = "internal-instance"
	reconciler := &FireboltInstanceReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(referencing, other, internal).
			WithIndex(
				&computev1alpha1.FireboltInstance{},
				externalPostgresSecretIndexField,
				externalPostgresSecretIndexValues,
			).
			Build(),
	}

	tests := []struct {
		name        string
		secretName  string
		wantRequest string
	}{
		{
			name:        "referenced credentials Secret",
			secretName:  "external-creds",
			wantRequest: referencing.Name,
		},
		{
			name:        "referenced CA Secret",
			secretName:  "external-ca",
			wantRequest: referencing.Name,
		},
		{
			name:       "unrelated Secret",
			secretName: "unrelated",
		},
		{
			name:       "operator-generated internal credentials Secret",
			secretName: pgCredentialsSecretName(internal.Name),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requests := reconciler.mapMetadataPostgresSecretToInstances(context.Background(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: tc.secretName, Namespace: referencing.Namespace},
			})
			if tc.wantRequest == "" {
				if len(requests) != 0 {
					t.Fatalf("mapped requests = %#v, want none", requests)
				}
				return
			}
			if len(requests) != 1 || requests[0].Name != tc.wantRequest ||
				requests[0].Namespace != referencing.Namespace {
				t.Fatalf("mapped requests = %#v, want only %s/%s",
					requests, referencing.Namespace, tc.wantRequest)
			}
		})
	}
}

// TestBuildMetadataConfigYAML_EscapesUserFields locks in YAML quoting:
// every user-controlled string interpolated into the pensieve config
// template must be rendered as a quoted YAML scalar. Without quoting, a
// malicious operator could inject extra YAML keys (e.g. a second host) and
// redirect the metadata service to an attacker-controlled PostgreSQL.
//
// This test pretends the CRD admission Pattern (which also rejects
// these strings at admission time) is bypassed — controller-internal
// code must remain safe even if a future change widens the pattern.
func TestBuildMetadataConfigYAML_EscapesUserFields(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: "acc<1>",
			Metadata: computev1alpha1.MetadataSpec{
				Postgres: &computev1alpha1.PostgresSpec{
					Host:                 "evil\ninjected: pwned\nhost: attacker.example",
					Database:             `db: "name"`,
					Schema:               `s'chema"`,
					CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
				},
			},
		},
	}

	got := buildMetadataConfigYAML(inst)

	// The rendered document must parse as YAML with a map root and round-trip
	// each user field to its exact value. If any field were interpolated
	// unquoted, the newline / colon payloads above would either break the parse
	// or surface as an injected sibling key — both caught here.
	var doc struct {
		PensieveLite struct {
			DefaultAccountID string `json:"default_account_id"`
			MetadataStorage  struct {
				PostgreSQL struct {
					Host     string `json:"host"`
					Database string `json:"database"`
					Schema   string `json:"schema"`
				} `json:"postgresql"`
			} `json:"metadata_storage"`
		} `json:"pensieve_lite"`
	}
	if err := yaml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("rendered config must be well-formed YAML: %v\n%s", err, got)
	}
	pg := doc.PensieveLite.MetadataStorage.PostgreSQL
	if pg.Host != inst.Spec.Metadata.Postgres.Host {
		t.Errorf("host round-trip: got %q, want %q (injection leaked)", pg.Host, inst.Spec.Metadata.Postgres.Host)
	}
	if pg.Database != inst.Spec.Metadata.Postgres.Database {
		t.Errorf("database round-trip: got %q, want %q", pg.Database, inst.Spec.Metadata.Postgres.Database)
	}
	if pg.Schema != inst.Spec.Metadata.Postgres.Schema {
		t.Errorf("schema round-trip: got %q, want %q", pg.Schema, inst.Spec.Metadata.Postgres.Schema)
	}
	if doc.PensieveLite.DefaultAccountID != inst.Spec.ID {
		t.Errorf("default_account_id round-trip: got %q, want %q", doc.PensieveLite.DefaultAccountID, inst.Spec.ID)
	}

	// Defense-in-depth: the injected top-level key from the host payload must
	// not appear as a real map key (it is contained inside the quoted scalar).
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(got), &raw); err != nil {
		t.Fatalf("rendered config must parse into a map: %v\n%s", err, got)
	}
	if _, leaked := raw["injected"]; leaked {
		t.Errorf("injected top-level key leaked into the config:\n%s", got)
	}
}

// TestBuildMetadataConfigYAML_GarbageCollectionKeys pins the GC key names to the
// dialect the dedicated-pensieve server actually reads
// (pensieve_lite.metadata_storage.garbage_collection.{enabled,time_horizon_sec,interval_ms}).
// An earlier template rendered interval_seconds/max_age_seconds, key names no server
// version has ever read, so GC silently ran on the server defaults (60s interval, 1h
// horizon) instead of the values below.
func TestBuildMetadataConfigYAML_GarbageCollectionKeys(t *testing.T) {
	got := buildMetadataConfigYAML(mkMetadataInstance())
	for _, want := range []string{
		"interval_ms: 3600000",
		"time_horizon_sec: 86400",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in rendered config; got:\n%s", want, got)
		}
	}
	for _, stale := range []string{"interval_seconds", "max_age_seconds"} {
		if strings.Contains(got, stale) {
			t.Errorf("stale GC key %q must not be rendered (the server never read it); got:\n%s", stale, got)
		}
	}
}

func TestBuildMetadataConfigYAML_MetadataNG(t *testing.T) {
	inst := mkMetadataInstance()
	inst.Spec.MetadataNG = true

	got := buildMetadataConfigYAML(inst)

	for _, want := range []string{
		`host: "inst-metadata-pg.ns-1.svc.cluster.local"`,
		`port: 5432`,
		`database: "firebolt_metadata"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in metadata-ng config; got:\n%s", want, got)
		}
	}

	// Keys the metadata-ng service ignores and warns about when present.
	for _, legacyOnly := range []string{
		"default_account_id",
		"schema",
		"server_threads",
		"log_level",
		"keepalive",
		"connect_timeout_sec",
		"garbage_collection",
	} {
		if strings.Contains(got, legacyOnly) {
			t.Errorf("legacy-only key %q must not be rendered for metadata-ng; got:\n%s", legacyOnly, got)
		}
	}

	var root map[string]any
	if err := yaml.Unmarshal([]byte(got), &root); err != nil {
		t.Fatalf("metadata-ng config must be valid YAML: %v\n%s", err, got)
	}
	if _, ok := root["pensieve_lite"]; !ok {
		t.Fatalf("metadata-ng config missing pensieve_lite root: %v", root)
	}

	// A custom external schema must not leak into the metadata-ng document.
	external := mkMetadataInstance()
	external.Spec.MetadataNG = true
	external.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host:                 "pg.example.com",
		Database:             "fb",
		Schema:               "firebolt_metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
	}
	got = buildMetadataConfigYAML(external)
	if strings.Contains(got, "schema") || strings.Contains(got, "firebolt_metadata") {
		t.Errorf("external schema must not be rendered for metadata-ng; got:\n%s", got)
	}
	if !strings.Contains(got, `host: "pg.example.com"`) || !strings.Contains(got, `database: "fb"`) {
		t.Errorf("external endpoint must still be rendered for metadata-ng; got:\n%s", got)
	}
}

func TestMetadataNGPreservesUserImageAndRollsConfig(t *testing.T) {
	legacy := mkMetadataInstance()
	legacy.Spec.Metadata.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  computev1alpha1.MetadataContainerName,
			Image: "registry.example/metadata:user-selected",
		}},
	}}
	ng := legacy.DeepCopy()
	ng.Spec.MetadataNG = true

	legacyConfig := buildMetadataConfigYAML(legacy)
	ngConfig := buildMetadataConfigYAML(ng)
	legacyDep := buildMetadataDeployment(legacy, legacyConfig)
	ngDep := buildMetadataDeployment(ng, ngConfig)

	if got, want := legacyDep.Spec.Template.Spec.Containers[0].Image, "registry.example/metadata:user-selected"; got != want {
		t.Errorf("legacy image = %q, want %q", got, want)
	}
	if got, want := ngDep.Spec.Template.Spec.Containers[0].Image, "registry.example/metadata:user-selected"; got != want {
		t.Errorf("metadata-ng image = %q, want unchanged user image %q", got, want)
	}
	if legacyConfig == ngConfig {
		t.Fatal("metadata-ng config must differ from legacy config")
	}
	legacyHash := legacyDep.Spec.Template.Annotations[AnnotationConfigHash]
	ngHash := ngDep.Spec.Template.Annotations[AnnotationConfigHash]
	if legacyHash == ngHash {
		t.Fatalf("config hashes must differ so toggling metadataNG rolls the Deployment: %q", legacyHash)
	}
	if got := *legacyDep.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser; got != MetadataUID {
		t.Errorf("legacy RunAsUser = %d, want %d", got, MetadataUID)
	}
	if got := *ngDep.Spec.Template.Spec.Containers[0].SecurityContext.RunAsUser; got != MetadataNGUID {
		t.Errorf("metadata-ng RunAsUser = %d, want %d", got, MetadataNGUID)
	}
	if got := *ngDep.Spec.Template.Spec.SecurityContext.RunAsGroup; got != MetadataNGUID {
		t.Errorf("metadata-ng pod RunAsGroup = %d, want %d", got, MetadataNGUID)
	}
}

// The metadata (pensieve) pod has the same security posture as the
// internal PostgreSQL and Envoy gateway pods: built-in non-root user,
// read-only rootfs, all capabilities dropped, RuntimeDefault seccomp, and
// no auto-mounted service account token. These tests are the regression
// guard against any of those fields silently disappearing.

func mkMetadataInstance() *computev1alpha1.FireboltInstance {
	return &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: "acc-1",
		},
	}
}

func TestBuildMetadataDeploymentPostgresTLSIsOptIn(t *testing.T) {
	external := mkMetadataInstance()
	external.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host:                 "postgres.example.com",
		Database:             "metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "external-creds"},
	}
	for _, tc := range []struct {
		name     string
		instance *computev1alpha1.FireboltInstance
	}{
		{name: "internal PostgreSQL", instance: mkMetadataInstance()},
		{name: "external PostgreSQL without TLS", instance: external},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := buildMetadataDeployment(tc.instance, buildMetadataConfigYAML(tc.instance))
			pod := dep.Spec.Template.Spec
			container := pod.Containers[0]

			for _, env := range container.Env {
				if env.Name == computev1alpha1.MetadataPostgresSSLModeEnvKey ||
					env.Name == computev1alpha1.MetadataPostgresSSLRootCertEnvKey {
					t.Errorf("rendered unexpected environment variable %q", env.Name)
				}
			}
			for _, mount := range container.VolumeMounts {
				if mount.Name == computev1alpha1.MetadataPostgresCAVolumeName {
					t.Errorf("rendered unexpected CA mount: %+v", mount)
				}
			}
			for _, volume := range pod.Volumes {
				if volume.Name == computev1alpha1.MetadataPostgresCAVolumeName {
					t.Errorf("rendered unexpected CA volume: %+v", volume)
				}
			}
		})
	}
}

func TestBuildMetadataDeploymentPostgresTLSUsesSelectedCAKey(t *testing.T) {
	instance := mkMetadataInstance()
	instance.Spec.Metadata.Postgres = &computev1alpha1.PostgresSpec{
		Host:                 "postgres.example.com",
		Database:             "metadata",
		CredentialsSecretRef: corev1.LocalObjectReference{Name: "external-creds"},
		TLS: &computev1alpha1.PostgresTLSSpec{
			Mode: computev1alpha1.PostgresTLSModeVerifyFull,
			CASecretRef: corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "postgres-ca"},
				Key:                  "provider-root.pem",
			},
		},
	}
	dep := buildMetadataDeployment(instance, buildMetadataConfigYAML(instance))
	pod := dep.Spec.Template.Spec
	container := pod.Containers[0]

	env := make(map[string]string, len(container.Env))
	for _, item := range container.Env {
		env[item.Name] = item.Value
	}
	if got := env[computev1alpha1.MetadataPostgresSSLModeEnvKey]; got != "verify-full" {
		t.Errorf("PGSSLMODE = %q, want verify-full", got)
	}
	wantRootCert := metadataPostgresCAMount + "/" + metadataPostgresCAFileName
	if got := env[computev1alpha1.MetadataPostgresSSLRootCertEnvKey]; got != wantRootCert {
		t.Errorf("PGSSLROOTCERT = %q, want %q", got, wantRootCert)
	}

	var caMount *corev1.VolumeMount
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == computev1alpha1.MetadataPostgresCAVolumeName {
			caMount = &container.VolumeMounts[i]
		}
	}
	if caMount == nil {
		t.Fatal("metadata container is missing the PostgreSQL CA mount")
	}
	if !caMount.ReadOnly || caMount.MountPath != metadataPostgresCAMount {
		t.Errorf("PostgreSQL CA mount = %+v, want read-only at %q", caMount, metadataPostgresCAMount)
	}

	var caVolume *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == computev1alpha1.MetadataPostgresCAVolumeName {
			caVolume = &pod.Volumes[i]
		}
	}
	if caVolume == nil || caVolume.Secret == nil {
		t.Fatalf("PostgreSQL CA Secret volume missing: %+v", caVolume)
	}
	if caVolume.Secret.SecretName != "postgres-ca" {
		t.Errorf("CA Secret name = %q, want postgres-ca", caVolume.Secret.SecretName)
	}
	if len(caVolume.Secret.Items) != 1 {
		t.Fatalf("CA Secret projects %d keys, want exactly one", len(caVolume.Secret.Items))
	}
	if got := caVolume.Secret.Items[0]; got.Key != "provider-root.pem" || got.Path != metadataPostgresCAFileName {
		t.Errorf("CA Secret projection = %+v, want provider-root.pem -> %s", got, metadataPostgresCAFileName)
	}
}

func TestBuildMetadataDeploymentPodSecurityContext(t *testing.T) {
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigYAML(mkMetadataInstance()))

	psc := dep.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("expected a pod-level SecurityContext to be set")
	}
	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Errorf("RunAsNonRoot: got %+v, want *true", psc.RunAsNonRoot)
	}
	for name, ptr := range map[string]*int64{
		"RunAsUser":  psc.RunAsUser,
		"RunAsGroup": psc.RunAsGroup,
	} {
		if ptr == nil {
			t.Errorf("%s: nil; want *%d", name, MetadataUID)
			continue
		}
		if *ptr != MetadataUID {
			t.Errorf("%s: got %d, want %d", name, *ptr, MetadataUID)
		}
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile: got %+v, want type=%s", psc.SeccompProfile, corev1.SeccompProfileTypeRuntimeDefault)
	}

	if amt := dep.Spec.Template.Spec.AutomountServiceAccountToken; amt == nil || *amt {
		t.Errorf("AutomountServiceAccountToken: got %+v, want *false (pensieve does not call the Kubernetes API)", amt)
	}
}

func TestBuildMetadataDeploymentContainerSecurityContext(t *testing.T) {
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigYAML(mkMetadataInstance()))

	if got, want := len(dep.Spec.Template.Spec.Containers), 1; got != want {
		t.Fatalf("containers: got %d, want %d", got, want)
	}
	c := dep.Spec.Template.Spec.Containers[0]
	csc := c.SecurityContext
	if csc == nil {
		t.Fatal("expected a container-level SecurityContext to be set")
	}

	if csc.RunAsNonRoot == nil || !*csc.RunAsNonRoot {
		t.Errorf("RunAsNonRoot: got %+v, want *true", csc.RunAsNonRoot)
	}
	if csc.RunAsUser == nil || *csc.RunAsUser != MetadataUID {
		t.Errorf("RunAsUser: got %v, want *%d", csc.RunAsUser, MetadataUID)
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Errorf("ReadOnlyRootFilesystem: got %+v, want *true", csc.ReadOnlyRootFilesystem)
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Errorf("AllowPrivilegeEscalation: got %+v, want *false", csc.AllowPrivilegeEscalation)
	}
	if csc.Capabilities == nil {
		t.Fatal("Capabilities: nil; want Drop=[ALL]")
	}
	if got, want := len(csc.Capabilities.Drop), 1; got != want {
		t.Fatalf("Capabilities.Drop: got %d entries, want %d", got, want)
	}
	if csc.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Errorf("Capabilities.Drop[0]: got %q, want %q", csc.Capabilities.Drop[0], "ALL")
	}
	if len(csc.Capabilities.Add) != 0 {
		t.Errorf("Capabilities.Add: got %v, want empty", csc.Capabilities.Add)
	}
}

// Read-only-rootfs pods need a writable emptyDir backing /tmp. The
// pensieve binary has not been audited for filesystem writes, so /tmp
// is backed defensively; without an emptyDir mount there, any runtime
// write under /tmp would fail on a read-only fs.
func TestBuildMetadataDeploymentWritableTmpVolume(t *testing.T) {
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigYAML(mkMetadataInstance()))
	pod := dep.Spec.Template.Spec

	var tmp *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == "tmp" {
			tmp = &pod.Volumes[i]
			break
		}
	}
	if tmp == nil {
		t.Fatal(`expected a "tmp" volume on the pod`)
	}
	if tmp.EmptyDir == nil {
		t.Errorf(`"tmp" volume must be an EmptyDir, got %+v`, tmp.VolumeSource)
	}

	var mounted bool
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == "tmp" && m.MountPath == "/tmp" {
			mounted = true
			break
		}
	}
	if !mounted {
		t.Errorf(`container missing /tmp mount of the "tmp" emptyDir; mounts: %+v`, pod.Containers[0].VolumeMounts)
	}
}

// Pensieve is a Quarkus app that maps env vars to MicroProfile config keys
// by lowercasing and dot-separating, so a kubelet-injected service-link var
// could shadow a real config key (cf. floci's `FLOCI_PORT` collision). The
// floci incident motivated turning the legacy service-link env block off
// across every operator-managed PodSpec; this test is the lock-in.
func TestBuildMetadataDeploymentDisablesServiceLinks(t *testing.T) {
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigYAML(mkMetadataInstance()))
	esl := dep.Spec.Template.Spec.EnableServiceLinks
	if esl == nil || *esl {
		t.Errorf("EnableServiceLinks: got %+v, want *false", esl)
	}
}
