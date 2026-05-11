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
