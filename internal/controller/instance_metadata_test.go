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

func TestBuildMetadataDeploymentPodSecurityContext(t *testing.T) {
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigXML(mkMetadataInstance()))

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
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigXML(mkMetadataInstance()))

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
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigXML(mkMetadataInstance()))
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
	dep := buildMetadataDeployment(mkMetadataInstance(), buildMetadataConfigXML(mkMetadataInstance()))
	esl := dep.Spec.Template.Spec.EnableServiceLinks
	if esl == nil || *esl {
		t.Errorf("EnableServiceLinks: got %+v, want *false", esl)
	}
}
