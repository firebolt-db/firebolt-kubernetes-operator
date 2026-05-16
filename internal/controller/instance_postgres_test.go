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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// FB-1164: lock in the SecurityContext hardening on the internal
// PostgreSQL pod template. The official postgres:16-alpine image runs
// fine as the built-in non-root postgres user (UID 70) with a read-only
// root filesystem, provided /var/run/postgresql and /tmp are backed by
// writable emptyDir mounts. These tests are the regression guard against
// any of those fields silently disappearing.

func mkPostgresInstance() *computev1alpha1.FireboltInstance {
	return &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
	}
}

func TestBuildPostgresStatefulSetPodSecurityContext(t *testing.T) {
	sts := buildPostgresStatefulSet(mkPostgresInstance())

	psc := sts.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatal("expected a pod-level SecurityContext to be set")
	}

	if psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot {
		t.Errorf("RunAsNonRoot: got %+v, want *true", psc.RunAsNonRoot)
	}
	for name, ptr := range map[string]*int64{
		"RunAsUser":  psc.RunAsUser,
		"RunAsGroup": psc.RunAsGroup,
		"FSGroup":    psc.FSGroup,
	} {
		if ptr == nil {
			t.Errorf("%s: nil; want *%d", name, PostgresUID)
			continue
		}
		if *ptr != PostgresUID {
			t.Errorf("%s: got %d, want %d", name, *ptr, PostgresUID)
		}
	}

	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("SeccompProfile: got %+v, want type=%s", psc.SeccompProfile, corev1.SeccompProfileTypeRuntimeDefault)
	}
}

func TestBuildPostgresStatefulSetContainerSecurityContext(t *testing.T) {
	sts := buildPostgresStatefulSet(mkPostgresInstance())

	if got, want := len(sts.Spec.Template.Spec.Containers), 1; got != want {
		t.Fatalf("containers: got %d, want %d", got, want)
	}
	c := sts.Spec.Template.Spec.Containers[0]
	csc := c.SecurityContext
	if csc == nil {
		t.Fatal("expected a container-level SecurityContext to be set")
	}

	if csc.RunAsNonRoot == nil || !*csc.RunAsNonRoot {
		t.Errorf("RunAsNonRoot: got %+v, want *true", csc.RunAsNonRoot)
	}
	if csc.RunAsUser == nil || *csc.RunAsUser != PostgresUID {
		t.Errorf("RunAsUser: got %v, want *%d", csc.RunAsUser, PostgresUID)
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

// Read-only-rootfs pods need writable emptyDir mounts at the two paths
// the postgres entrypoint touches outside of the data directory:
// /var/run/postgresql (unix socket and postmaster.pid) and /tmp (initdb
// scratch). If either disappears the pod CrashLoopBackOffs at startup.
func TestBuildPostgresStatefulSetWritablePathVolumes(t *testing.T) {
	sts := buildPostgresStatefulSet(mkPostgresInstance())
	pod := sts.Spec.Template.Spec

	wantVolumes := map[string]bool{
		"run-postgresql": false,
		"tmp":            false,
	}
	for _, v := range pod.Volumes {
		if _, ok := wantVolumes[v.Name]; !ok {
			continue
		}
		if v.EmptyDir == nil {
			t.Errorf("volume %q: not an emptyDir (got %+v)", v.Name, v.VolumeSource)
		}
		wantVolumes[v.Name] = true
	}
	for name, found := range wantVolumes {
		if !found {
			t.Errorf("missing pod volume %q (required to back the read-only root fs)", name)
		}
	}

	if len(pod.Containers) == 0 {
		t.Fatal("no containers in pod spec")
	}
	wantMounts := map[string]string{
		"run-postgresql": "/var/run/postgresql",
		"tmp":            "/tmp",
		"data":           "/var/lib/postgresql/data",
	}
	gotMounts := map[string]string{}
	for _, m := range pod.Containers[0].VolumeMounts {
		gotMounts[m.Name] = m.MountPath
	}
	for name, wantPath := range wantMounts {
		gotPath, ok := gotMounts[name]
		if !ok {
			t.Errorf("missing container volumeMount %q -> %s", name, wantPath)
			continue
		}
		if gotPath != wantPath {
			t.Errorf("volumeMount %q: got mountPath %q, want %q", name, gotPath, wantPath)
		}
	}
}

// kubelet's FSGroup chgrp does not propagate ownership to a subPath
// mount, so initdb's `chmod 0700 $PGDATA` fails with EPERM on a non-root
// pod. We work around that by mounting the PVC root and setting PGDATA
// to a sub-directory the postgres process creates itself. Both halves
// of the workaround are load-bearing; this test fails loudly if either
// disappears or drifts.
func TestBuildPostgresStatefulSetPGDataLayout(t *testing.T) {
	sts := buildPostgresStatefulSet(mkPostgresInstance())
	if got, want := len(sts.Spec.Template.Spec.Containers), 1; got != want {
		t.Fatalf("containers: got %d, want %d", got, want)
	}
	c := sts.Spec.Template.Spec.Containers[0]

	var dataMount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "data" {
			dataMount = &c.VolumeMounts[i]
			break
		}
	}
	if dataMount == nil {
		t.Fatal("no data volumeMount on container")
	}
	if dataMount.SubPath != "" {
		t.Errorf("data volumeMount.subPath: got %q, want \"\" (FSGroup must reach the mount root for non-root postgres to chmod $PGDATA)", dataMount.SubPath)
	}
	if dataMount.MountPath != "/var/lib/postgresql/data" {
		t.Errorf("data volumeMount.mountPath: got %q, want /var/lib/postgresql/data", dataMount.MountPath)
	}

	var pgdata string
	var pgdataPresent bool
	for _, e := range c.Env {
		if e.Name == "PGDATA" {
			pgdata = e.Value
			pgdataPresent = true
			break
		}
	}
	if !pgdataPresent {
		t.Fatal("PGDATA env var is missing; postgres entrypoint would default it to the mount point")
	}
	if pgdata != "/var/lib/postgresql/data/pgdata" {
		t.Errorf("PGDATA: got %q, want /var/lib/postgresql/data/pgdata (sub-directory of the data mount)", pgdata)
	}
}
