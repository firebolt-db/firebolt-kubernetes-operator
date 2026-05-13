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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestBuildStatefulSet_DataVolumeBackends guards FB-1085: the engine pod's
// /firebolt-core/volume mount can be backed by the default emptyDir, an
// opt-in per-pod PVC, an explicit emptyDir with knobs, or a hostPath. The
// PVC backend emits a single VolumeClaimTemplate (and no pod-level data
// Volume); the other backends suppress the VCT and instead add a pod-level
// Volume named DataVolumeName.
func TestBuildStatefulSet_DataVolumeBackends(t *testing.T) {
	gigi := resource.MustParse("1Gi")
	dirType := corev1.HostPathDirectory

	cases := []struct {
		name           string
		storage        computev1alpha1.EngineStorageSpec
		wantVCT        bool
		wantPodVolType string // "emptyDir", "hostPath", or "" (none)
		check          func(t *testing.T, v *corev1.Volume)
	}{
		{
			name:           "default-resolves-to-emptyDir (empty EngineStorageSpec)",
			storage:        computev1alpha1.EngineStorageSpec{},
			wantVCT:        false,
			wantPodVolType: "emptyDir",
			check: func(t *testing.T, v *corev1.Volume) {
				t.Helper()
				if v.EmptyDir.Medium != "" {
					t.Errorf("EmptyDir.Medium = %q, want \"\" (default)", v.EmptyDir.Medium)
				}
				if v.EmptyDir.SizeLimit != nil {
					t.Errorf("EmptyDir.SizeLimit = %v, want nil (default)", v.EmptyDir.SizeLimit)
				}
			},
		},
		{
			name: "explicit persistentVolumeClaim{} opts into PVC backend",
			storage: computev1alpha1.EngineStorageSpec{
				PersistentVolumeClaim: &computev1alpha1.EnginePersistentVolumeClaimSpec{},
			},
			wantVCT:        true,
			wantPodVolType: "",
		},
		{
			name: "emptyDir with default medium",
			storage: computev1alpha1.EngineStorageSpec{
				EmptyDir: &computev1alpha1.EngineEmptyDirSpec{},
			},
			wantVCT:        false,
			wantPodVolType: "emptyDir",
			check: func(t *testing.T, v *corev1.Volume) {
				t.Helper()
				if v.EmptyDir.Medium != "" {
					t.Errorf("EmptyDir.Medium = %q, want \"\"", v.EmptyDir.Medium)
				}
				if v.EmptyDir.SizeLimit != nil {
					t.Errorf("EmptyDir.SizeLimit = %v, want nil", v.EmptyDir.SizeLimit)
				}
			},
		},
		{
			name: "emptyDir on Memory medium with sizeLimit",
			storage: computev1alpha1.EngineStorageSpec{
				EmptyDir: &computev1alpha1.EngineEmptyDirSpec{
					Medium:    corev1.StorageMediumMemory,
					SizeLimit: &gigi,
				},
			},
			wantVCT:        false,
			wantPodVolType: "emptyDir",
			check: func(t *testing.T, v *corev1.Volume) {
				t.Helper()
				if v.EmptyDir.Medium != corev1.StorageMediumMemory {
					t.Errorf("EmptyDir.Medium = %q, want Memory", v.EmptyDir.Medium)
				}
				if v.EmptyDir.SizeLimit == nil || !v.EmptyDir.SizeLimit.Equal(gigi) {
					t.Errorf("EmptyDir.SizeLimit = %v, want 1Gi", v.EmptyDir.SizeLimit)
				}
			},
		},
		{
			name: "hostPath with explicit type",
			storage: computev1alpha1.EngineStorageSpec{
				HostPath: &computev1alpha1.EngineHostPathSpec{
					Path: "/mnt/nvme/firebolt",
					Type: &dirType,
				},
			},
			wantVCT:        false,
			wantPodVolType: "hostPath",
			check: func(t *testing.T, v *corev1.Volume) {
				t.Helper()
				if v.HostPath.Path != "/mnt/nvme/firebolt" {
					t.Errorf("HostPath.Path = %q, want /mnt/nvme/firebolt", v.HostPath.Path)
				}
				if v.HostPath.Type == nil || *v.HostPath.Type != corev1.HostPathDirectory {
					t.Errorf("HostPath.Type = %v, want Directory", v.HostPath.Type)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			spec.Storage = tc.storage

			sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)

			if tc.wantVCT {
				if len(sts.Spec.VolumeClaimTemplates) != 1 {
					t.Fatalf("VolumeClaimTemplates: got %d, want 1", len(sts.Spec.VolumeClaimTemplates))
				}
				if sts.Spec.VolumeClaimTemplates[0].Name != DataVolumeName {
					t.Errorf("VCT name = %q, want %q", sts.Spec.VolumeClaimTemplates[0].Name, DataVolumeName)
				}
			} else if len(sts.Spec.VolumeClaimTemplates) != 0 {
				t.Errorf("VolumeClaimTemplates: got %d, want 0 (non-PVC backend)", len(sts.Spec.VolumeClaimTemplates))
			}

			dataVol := findDataPodVolume(sts)
			switch tc.wantPodVolType {
			case "":
				if dataVol != nil {
					t.Errorf("pod-template data Volume should be nil for PVC backend, got %+v", dataVol)
				}
			case "emptyDir":
				if dataVol == nil || dataVol.EmptyDir == nil {
					t.Fatalf("expected pod-template emptyDir Volume named %q, got %+v", DataVolumeName, dataVol)
				}
			case "hostPath":
				if dataVol == nil || dataVol.HostPath == nil {
					t.Fatalf("expected pod-template hostPath Volume named %q, got %+v", DataVolumeName, dataVol)
				}
			default:
				t.Fatalf("test setup: unknown wantPodVolType %q", tc.wantPodVolType)
			}

			if tc.check != nil {
				tc.check(t, dataVol)
			}
		})
	}
}

// TestStorageMatchesSpec_BackendSwitchBumpsGeneration guards that the
// resolver correctly identifies each backend's STS as matching its own
// spec and mismatching any other backend's spec. A mismatch is what
// pushes the reconciler into a new blue-green generation, which is the
// only safe way to swap the data-volume source (VolumeClaimTemplates are
// immutable on a StatefulSet and the pod-template Volume differs across
// backends). An empty EngineStorageSpec{} now resolves to BackendEmptyDir
// (see resolveStorageBackend) — PVC specs must set PersistentVolumeClaim
// explicitly.
func TestStorageMatchesSpec_BackendSwitchBumpsGeneration(t *testing.T) {
	pvcSpec := testSpec() // testSpec() pins Storage to PVC opt-in; keep as-is.
	emptyDirSpec := testSpec()
	emptyDirSpec.Storage = computev1alpha1.EngineStorageSpec{} // empty → default emptyDir
	hostPathSpec := testSpec()
	hostPathSpec.Storage = computev1alpha1.EngineStorageSpec{
		HostPath: &computev1alpha1.EngineHostPathSpec{Path: "/mnt/nvme/firebolt"},
	}

	pvcSts := buildStatefulSet(pvcSpec, testEngineName, testNamespace, 0)
	emptyDirSts := buildStatefulSet(emptyDirSpec, testEngineName, testNamespace, 0)
	hostPathSts := buildStatefulSet(hostPathSpec, testEngineName, testNamespace, 0)

	cases := []struct {
		name string
		sts  *appsv1.StatefulSet
		spec *computev1alpha1.FireboltEngineSpec
		want bool
	}{
		{"PVC sts vs PVC spec", pvcSts, pvcSpec, true},
		{"EmptyDir sts vs EmptyDir spec (both default)", emptyDirSts, emptyDirSpec, true},
		{"HostPath sts vs HostPath spec", hostPathSts, hostPathSpec, true},
		{"PVC sts vs EmptyDir spec", pvcSts, emptyDirSpec, false},
		{"EmptyDir sts vs PVC spec", emptyDirSts, pvcSpec, false},
		{"EmptyDir sts vs HostPath spec", emptyDirSts, hostPathSpec, false},
		{"HostPath sts vs PVC spec", hostPathSts, pvcSpec, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storageMatchesSpec(tc.sts, tc.spec)
			if got != tc.want {
				t.Errorf("storageMatchesSpec() = %v, want %v", got, tc.want)
			}
		})
	}
}
