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
	"encoding/xml"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func TestBuildMetadataConfigXMLSchema(t *testing.T) {
	tests := []struct {
		name     string
		postgres *computev1alpha1.PostgresSpec
		want     string
	}{
		{
			name:     "internal postgres uses default schema",
			postgres: nil,
			want:     "<schema>public</schema>",
		},
		{
			name: "external postgres without schema falls back to public",
			postgres: &computev1alpha1.PostgresSpec{
				Host:                 "pg.example.com",
				Database:             "fb",
				CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
			},
			want: "<schema>public</schema>",
		},
		{
			name: "external postgres with custom schema is honored",
			postgres: &computev1alpha1.PostgresSpec{
				Host:                 "pg.example.com",
				Database:             "fb",
				Schema:               "firebolt_metadata",
				CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
			},
			want: "<schema>firebolt_metadata</schema>",
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
			got := buildMetadataConfigXML(inst)
			if !strings.Contains(got, tc.want) {
				t.Errorf("expected %q in rendered config; got:\n%s", tc.want, got)
			}
		})
	}
}

// TestBuildMetadataConfigXML_EscapesUserFields locks in the FB-1163 fix:
// every user-controlled string interpolated into the pensieve config
// template must be XML-escaped. Without escaping, a malicious operator
// could inject extra XML elements (e.g. a second <host>) and redirect
// the metadata service to an attacker-controlled PostgreSQL.
//
// This test pretends the CRD admission Pattern (which now also rejects
// these strings at admission time) is bypassed — controller-internal
// code must remain safe even if a future change widens the pattern.
func TestBuildMetadataConfigXML_EscapesUserFields(t *testing.T) {
	inst := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"},
		Spec: computev1alpha1.FireboltInstanceSpec{
			ID: "acc<1>",
			Metadata: computev1alpha1.MetadataSpec{
				Postgres: &computev1alpha1.PostgresSpec{
					Host:                 `evil</host><port>9999</port><host>attacker.example`,
					Database:             `db&name`,
					Schema:               `s"chema'`,
					CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
				},
			},
		},
	}

	got := buildMetadataConfigXML(inst)

	if strings.Contains(got, "</host><port>9999</port><host>") {
		t.Errorf("host injection not escaped — attacker-controlled XML appears literally:\n%s", got)
	}
	if strings.Contains(got, "<host>attacker.example</host>") {
		t.Errorf("injected attacker host element appears literally:\n%s", got)
	}
	wantSubstrings := []string{
		"acc&lt;1&gt;",
		"evil&lt;/host&gt;&lt;port&gt;9999&lt;/port&gt;&lt;host&gt;attacker.example",
		"db&amp;name",
		"s&#34;chema&#39;",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("expected escaped substring %q in rendered config; got:\n%s", s, got)
		}
	}

	// The rendered document must still be a single well-formed XML
	// document with exactly the expected element structure: a single
	// pensieve_lite/metadata_storage/postgresql/host element, not two.
	type postgresql struct {
		Host     []string `xml:"host"`
		Database []string `xml:"database"`
		Schema   []string `xml:"schema"`
	}
	type metadataStorage struct {
		Postgres postgresql `xml:"postgresql"`
	}
	type pensieveLite struct {
		DefaultAccountID string          `xml:"default_account_id"`
		Storage          metadataStorage `xml:"metadata_storage"`
	}
	type config struct {
		XMLName xml.Name     `xml:"config"`
		Lite    pensieveLite `xml:"pensieve_lite"`
	}
	var doc config
	if err := xml.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("rendered config must be well-formed XML: %v\n%s", err, got)
	}
	if got, want := len(doc.Lite.Storage.Postgres.Host), 1; got != want {
		t.Errorf("postgresql host elements: got %d, want %d (injected element leaked)", got, want)
	}
	if got, want := doc.Lite.Storage.Postgres.Host[0], inst.Spec.Metadata.Postgres.Host; got != want {
		t.Errorf("host element text round-trip: got %q, want %q", got, want)
	}
	if got, want := doc.Lite.Storage.Postgres.Database[0], inst.Spec.Metadata.Postgres.Database; got != want {
		t.Errorf("database element text round-trip: got %q, want %q", got, want)
	}
	if got, want := doc.Lite.Storage.Postgres.Schema[0], inst.Spec.Metadata.Postgres.Schema; got != want {
		t.Errorf("schema element text round-trip: got %q, want %q", got, want)
	}
	if got, want := doc.Lite.DefaultAccountID, inst.Spec.ID; got != want {
		t.Errorf("default_account_id round-trip: got %q, want %q", got, want)
	}
}
