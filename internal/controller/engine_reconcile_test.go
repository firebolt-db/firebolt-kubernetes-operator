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
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	testEngineName       = "test-engine"
	testNamespace        = "default"
	testMetadataEndpoint = "test-instance-metadata.default.svc.cluster.local:7000"
	testInstanceID       = "test-instance-id"
)

func testInstanceInfo() InstanceInfo {
	return InstanceInfo{
		MetadataEndpoint: testMetadataEndpoint,
		InstanceID:       testInstanceID,
	}
}

func testSpec() *computev1alpha1.FireboltEngineSpec {
	return &computev1alpha1.FireboltEngineSpec{
		InstanceRef: "test-instance",
		Replicas:    3,
		Image: &computev1alpha1.ImageSpec{
			Repository: "firebolt/engine",
			Tag:        "v1.0",
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("2"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
		},
		// Pre-default-flip the storage suite assumed empty Storage
		// meant "PVC with operator defaults". The default now resolves
		// to emptyDir; pin testSpec to the PVC backend explicitly so
		// the existing engine_reconcile_test.go fixtures (makeSTS, the
		// outer-loop reconciler tests, etc.) keep exercising the PVC
		// code path they were written for. Tests that specifically want
		// emptyDir or hostPath override spec.Storage themselves.
		Storage: computev1alpha1.EngineStorageSpec{
			PersistentVolumeClaim: &computev1alpha1.EnginePersistentVolumeClaimSpec{},
		},
		Rollout: computev1alpha1.RolloutGraceful,
	}
}

func stableStatus() *computev1alpha1.FireboltEngineStatus {
	return &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseStable,
		CurrentGeneration: 0,
		ActiveGeneration:  0,
	}
}

func makeSTS(engineName string, gen int, replicas int32, image string) *appsv1.StatefulSet {
	spec := testSpec()
	defaultTGPS := int64(DefaultTerminationGracePeriodSeconds)
	pvc := resolvePersistentVolumeClaimDefaults(spec.Storage.PersistentVolumeClaim)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      genResourceName(engineName, gen, ""),
			Namespace: testNamespace,
			Labels: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: DataVolumeName},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: pvc.AccessModes,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: pvc.Size},
					},
					StorageClassName: pvc.StorageClassName,
				},
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelEngine:     engineName,
						LabelGeneration: strconv.Itoa(gen),
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            enginePodServiceAccountName(spec),
					NodeSelector:                  spec.NodeSelector,
					Tolerations:                   spec.Tolerations,
					Affinity:                      spec.Affinity,
					TerminationGracePeriodSeconds: &defaultTGPS,
					SecurityContext:               getEnginePodSecurityContext(spec),
					Containers: []corev1.Container{
						{
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources:       engineContainerResources(spec),
							SecurityContext: getEngineContainerSecurityContext(spec),
						},
					},
				},
			},
		},
	}
}

// makeEmptyDirSTS is the emptyDir sibling of makeSTS: it produces an STS
// fixture whose data volume is a pod-template emptyDir Volume named
// DataVolumeName instead of a VolumeClaimTemplate. Mirrors makeSTS's other
// fields exactly so stsMatchesSpec sees only the data-volume shape change.
// Used by reconciler tests parameterised over storageBackendCases.
func makeEmptyDirSTS(engineName string, gen int, replicas int32, image string) *appsv1.StatefulSet {
	spec := testSpec()
	spec.Storage = computev1alpha1.EngineStorageSpec{}
	defaultTGPS := int64(DefaultTerminationGracePeriodSeconds)
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      genResourceName(engineName, gen, ""),
			Namespace: testNamespace,
			Labels: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &replicas,
			// No VolumeClaimTemplates for the emptyDir backend.
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelEngine:     engineName,
						LabelGeneration: strconv.Itoa(gen),
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName:            enginePodServiceAccountName(spec),
					NodeSelector:                  spec.NodeSelector,
					Tolerations:                   spec.Tolerations,
					Affinity:                      spec.Affinity,
					TerminationGracePeriodSeconds: &defaultTGPS,
					SecurityContext:               getEnginePodSecurityContext(spec),
					Containers: []corev1.Container{
						{
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources:       engineContainerResources(spec),
							SecurityContext: getEngineContainerSecurityContext(spec),
						},
					},
					Volumes: []corev1.Volume{
						{
							Name:         DataVolumeName,
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
				},
			},
		},
	}
}

// storageBackendCase parameterizes a reconciler test across the engine
// storage backends. applySpec mutates testSpec()'s Storage in place to
// select the backend; seedSTS builds the matching seed StatefulSet that
// stsMatchesSpec must see as up-to-date; assertSTS asserts the
// data-volume shape on a freshly built StatefulSet (used on the create
// path where buildStatefulSet's output is the result).
//
// Currently scoped to pvc + emptyDir. hostPath isn't included because no
// reconciler test currently load-bears on it; engine_storage_test.go
// covers the hostPath rendering / matching code path directly.
type storageBackendCase struct {
	name      string
	applySpec func(s *computev1alpha1.FireboltEngineSpec)
	seedSTS   func(engineName string, gen int, replicas int32, image string) *appsv1.StatefulSet
	assertSTS func(t *testing.T, sts *appsv1.StatefulSet)
}

var storageBackendCases = []storageBackendCase{
	{
		name: "pvc",
		applySpec: func(s *computev1alpha1.FireboltEngineSpec) {
			s.Storage = computev1alpha1.EngineStorageSpec{
				PersistentVolumeClaim: &computev1alpha1.EnginePersistentVolumeClaimSpec{},
			}
		},
		seedSTS: makeSTS,
		assertSTS: func(t *testing.T, sts *appsv1.StatefulSet) {
			t.Helper()
			if got := len(sts.Spec.VolumeClaimTemplates); got != 1 {
				t.Fatalf("VolumeClaimTemplates: got %d, want 1 (PVC backend)", got)
			}
			if v := findDataPodVolume(sts); v != nil {
				t.Errorf("expected no pod-template data Volume for PVC backend, got %+v", v)
			}
		},
	},
	{
		name: "emptyDir",
		applySpec: func(s *computev1alpha1.FireboltEngineSpec) {
			s.Storage = computev1alpha1.EngineStorageSpec{} // empty → emptyDir default
		},
		seedSTS: makeEmptyDirSTS,
		assertSTS: func(t *testing.T, sts *appsv1.StatefulSet) {
			t.Helper()
			if got := len(sts.Spec.VolumeClaimTemplates); got != 0 {
				t.Errorf("VolumeClaimTemplates: got %d, want 0 (emptyDir backend)", got)
			}
			v := findDataPodVolume(sts)
			if v == nil {
				t.Fatal("expected pod-template data Volume for emptyDir backend, got nil")
			}
			if v.EmptyDir == nil {
				t.Fatalf("expected pod-template data Volume to be EmptyDir-backed, got %+v", v)
			}
		},
	},
}

func makeClusterSvc(engineName string, gen int) *corev1.Service { //nolint:unparam // engineName is always testEngineName in tests but kept as param for readability
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      engineName + SuffixService,
			Namespace: testNamespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				LabelEngine:     engineName,
				LabelGeneration: strconv.Itoa(gen),
			},
		},
	}
}

// --- S1: Initial creation ---
//
// The controller's top-level Reconcile initializes Phase=Creating and
// ActiveGeneration=-1 on first sight of an engine, then requeues. So the
// first invocation of computeEngineReconcile that actually sees real state
// always lands in computeCreating with CurrentGeneration=0 and no existing
// resources. This test mirrors that entry condition.

func TestComputeEngineReconcile_S1_InitialCreation(t *testing.T) {
	for _, sc := range storageBackendCases {
		t.Run(sc.name, func(t *testing.T) {
			spec := testSpec()
			sc.applySpec(spec)
			status := &computev1alpha1.FireboltEngineStatus{
				Phase:             computev1alpha1.PhaseCreating,
				CurrentGeneration: 0,
				ActiveGeneration:  -1,
			}
			current := EngineState{ClusterServiceTargetGen: -1}

			result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

			if result.Status.Phase != computev1alpha1.PhaseCreating {
				t.Errorf("expected phase Creating, got %s", result.Status.Phase)
			}
			if result.Status.CurrentGeneration != 0 {
				t.Errorf("expected generation 0, got %d", result.Status.CurrentGeneration)
			}
			if result.EnsureStatefulSet == nil {
				t.Fatal("expected StatefulSet to be built on first Creating visit")
			}
			sc.assertSTS(t, result.EnsureStatefulSet)
			if result.EnsureConfigMap == nil {
				t.Error("expected ConfigMap to be built on first Creating visit")
			}
			if result.EnsureHeadlessSvc == nil {
				t.Error("expected headless Service to be built on first Creating visit")
			}
			if result.EnsureClusterSvc == nil {
				t.Error("expected cluster Service to be built on first Creating visit")
			}
		})
	}
}

// TestComputeStable_PanicsOnNegativeActiveGeneration documents the
// ActiveGeneration>=0 invariant of computeStable: reaching it with a
// negative generation indicates a bug in the state machine (the controller
// top-level is supposed to route Phase="" through Creating first).
func TestComputeStable_PanicsOnNegativeActiveGeneration(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when computeStable is entered with ActiveGeneration=-1")
		}
	}()

	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:            computev1alpha1.PhaseStable,
		ActiveGeneration: -1,
	}
	current := EngineState{ClusterServiceTargetGen: -1}
	_ = computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())
}

// --- S2: Blue-green upgrade ---

func TestComputeEngineReconcile_S2_SpecChange(t *testing.T) {
	for _, sc := range storageBackendCases {
		t.Run(sc.name, func(t *testing.T) {
			spec := testSpec()
			sc.applySpec(spec)
			spec.Image.Tag = "v2.0"
			// Build the seeded STS from a spec on the same backend so
			// the storage shape is consistent — the gen bump under
			// test must come from the image change, not from a
			// backend mismatch the harness accidentally introduced.
			seedSpec := testSpec()
			sc.applySpec(seedSpec)
			status := stableStatus()
			current := EngineState{
				CurrentSTS:              sc.seedSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
				CurrentHeadlessSvc:      &corev1.Service{},
				CurrentConfigMap:        buildConfigMap(seedSpec, testEngineName, testNamespace, 0, testInstanceInfo()),
				CurrentPodsReady:        true,
				CurrentPodTotal:         3,
				CurrentPodReady:         3,
				ClusterService:          makeClusterSvc(testEngineName, 0),
				ClusterServiceTargetGen: 0,
			}

			result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

			if result.Status.Phase != computev1alpha1.PhaseCreating {
				t.Errorf("expected phase Creating, got %s", result.Status.Phase)
			}
			if result.Status.CurrentGeneration != 1 {
				t.Errorf("expected generation 1, got %d", result.Status.CurrentGeneration)
			}
			if result.EnsureStatefulSet != nil {
				t.Error("expected no resource creation (intent-first: deferred to computeCreating)")
			}
			if !result.Requeue {
				t.Error("expected Requeue=true")
			}
		})
	}
}

// --- S3: Transition phases ---

func TestComputeEngineReconcile_S3_CreatingToSwitching(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 1, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 1, testInstanceInfo()),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseSwitching {
		t.Errorf("expected phase Switching, got %s", result.Status.Phase)
	}
	if !result.Requeue {
		t.Error("expected Requeue=true")
	}
}

func TestComputeEngineReconcile_S3_CreatingNotReady(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 1, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentPodsReady:        false,
		CurrentPodTotal:         1,
		CurrentPodReady:         0,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected phase Creating (waiting), got %s", result.Status.Phase)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Errorf("expected RequeueAfter 5s, got %v", result.RequeueAfter)
	}
}

func TestComputeEngineReconcile_S3_SwitchingUpdateSelector(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseSwitching,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	current := EngineState{
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.EnsureClusterSvc == nil {
		t.Fatal("expected cluster service selector update")
	}
	if result.EnsureClusterSvc.Spec.Selector[LabelGeneration] != "1" {
		t.Errorf("expected selector gen 1, got %s", result.EnsureClusterSvc.Spec.Selector[LabelGeneration])
	}
}

func TestComputeEngineReconcile_S3_SwitchingToDraining(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseSwitching,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	current := EngineState{
		ClusterService:          makeClusterSvc(testEngineName, 1),
		ClusterServiceTargetGen: 1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseDraining {
		t.Errorf("expected phase Draining, got %s", result.Status.Phase)
	}
	if result.Status.ActiveGeneration != 1 {
		t.Errorf("expected active gen 1, got %d", result.Status.ActiveGeneration)
	}
	if result.Status.DrainingGeneration == nil || *result.Status.DrainingGeneration != 0 {
		t.Error("expected draining gen 0")
	}
}

func TestComputeEngineReconcile_S3_SwitchingToStable_InitialDeploy(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseSwitching,
		CurrentGeneration: 0,
		ActiveGeneration:  -1,
	}
	current := EngineState{
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected phase Stable, got %s", result.Status.Phase)
	}
	if result.Status.ActiveGeneration != 0 {
		t.Errorf("expected active gen 0, got %d", result.Status.ActiveGeneration)
	}
}

func TestComputeEngineReconcile_S3_DrainingWait(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseDraining,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: &drainingGen,
	}
	current := EngineState{
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		DrainingPodsDrained: false,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseDraining {
		t.Errorf("expected phase Draining, got %s", result.Status.Phase)
	}
	if result.RequeueAfter != DefaultDrainCheckInterval {
		t.Errorf("expected RequeueAfter %v, got %v", DefaultDrainCheckInterval, result.RequeueAfter)
	}
}

func TestComputeEngineReconcile_S3_DrainingComplete(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseDraining,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: &drainingGen,
	}
	current := EngineState{
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		DrainingPodsDrained: true,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCleaning {
		t.Errorf("expected phase Cleaning, got %s", result.Status.Phase)
	}
}

func TestComputeEngineReconcile_S3_DrainingNilDrainingGeneration(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseDraining,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: nil,
	}
	current := EngineState{}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected phase Stable (nil draining gen recovery), got %s", result.Status.Phase)
	}
	if !result.Requeue {
		t.Error("expected Requeue=true")
	}
}

func TestComputeEngineReconcile_S3_DrainingCustomInterval(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	interval := metav1.Duration{Duration: 15 * time.Second}
	spec.DrainCheckInterval = &interval
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseDraining,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: &drainingGen,
	}
	current := EngineState{
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		DrainingPodsDrained: false,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.RequeueAfter != 15*time.Second {
		t.Errorf("expected RequeueAfter 15s (custom interval), got %v", result.RequeueAfter)
	}
}

func TestComputeEngineReconcile_S3_CleaningDeletesOldResources(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseCleaning,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: &drainingGen,
	}
	oldSTS := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
	oldSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: genResourceName(testEngineName, 0, SuffixHL)}}
	oldCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: genResourceName(testEngineName, 0, SuffixConfig)}}
	current := EngineState{
		DrainingSTS:         oldSTS,
		DrainingHeadlessSvc: oldSvc,
		DrainingConfigMap:   oldCM,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected phase Stable, got %s", result.Status.Phase)
	}
	if result.Status.DrainingGeneration != nil {
		t.Error("expected DrainingGeneration to be nil")
	}
	if len(result.DeleteResources) != 3 {
		t.Errorf("expected 3 resources to delete, got %d", len(result.DeleteResources))
	}
}

// --- S5: Self-healing / drift correction ---

func TestComputeEngineReconcile_S5_STSMissing(t *testing.T) {
	spec := testSpec()
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              nil,
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo()),
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected new transition (Creating), got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 1 {
		t.Errorf("expected new generation 1, got %d", result.Status.CurrentGeneration)
	}
	if result.EnsureStatefulSet != nil {
		t.Error("expected no resource creation (intent-first: deferred to computeCreating)")
	}
}

func TestComputeEngineReconcile_S5_MetadataOverrideDrift(t *testing.T) {
	spec := testSpec()
	override := "new-metadata.default.svc.cluster.local:7000"
	spec.MetadataEndpointOverride = &override
	status := stableStatus()
	// The existing STS was built without an override annotation.
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo()),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected Creating (new generation for metadata override drift), got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 1 {
		t.Errorf("expected generation bumped to 1, got %d", result.Status.CurrentGeneration)
	}
}

func TestComputeEngineReconcile_S5_ClusterSvcSelectorDrift(t *testing.T) {
	spec := testSpec()
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo()),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 99),
		ClusterServiceTargetGen: 99,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected to stay Stable (in-place fix), got %s", result.Status.Phase)
	}
	if result.EnsureClusterSvc == nil {
		t.Fatal("expected cluster service selector fix")
	}
	if result.EnsureClusterSvc.Spec.Selector[LabelGeneration] != "0" {
		t.Errorf("expected selector gen 0, got %s", result.EnsureClusterSvc.Spec.Selector[LabelGeneration])
	}
}

// --- S7: No-op stable ---

func TestComputeEngineReconcile_S7_NoOp(t *testing.T) {
	for _, sc := range storageBackendCases {
		t.Run(sc.name, func(t *testing.T) {
			spec := testSpec()
			sc.applySpec(spec)
			status := stableStatus()
			current := EngineState{
				CurrentSTS:              sc.seedSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
				CurrentHeadlessSvc:      &corev1.Service{},
				CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo()),
				CurrentPodsReady:        true,
				CurrentPodTotal:         3,
				CurrentPodReady:         3,
				ClusterService:          makeClusterSvc(testEngineName, 0),
				ClusterServiceTargetGen: 0,
			}

			result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

			if result.Status.Phase != computev1alpha1.PhaseStable {
				t.Errorf("expected Stable, got %s", result.Status.Phase)
			}
			if result.EnsureStatefulSet != nil {
				t.Error("expected no STS mutation")
			}
			if result.EnsureHeadlessSvc != nil {
				t.Error("expected no headless svc mutation")
			}
			if result.Requeue {
				t.Error("expected no immediate requeue")
			}
			if result.RequeueAfter != 30*time.Second {
				t.Errorf("expected RequeueAfter 30s, got %v", result.RequeueAfter)
			}
			if result.Status.ObservedGeneration != 1 {
				t.Errorf("expected ObservedGeneration 1, got %d", result.Status.ObservedGeneration)
			}
		})
	}
}

// --- Idempotency (I1) ---

func TestComputeEngineReconcile_Idempotency(t *testing.T) {
	spec := testSpec()
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo()),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	d1 := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())
	d2 := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if d1.Status.Phase != d2.Status.Phase {
		t.Errorf("idempotency violated: phases differ %s vs %s", d1.Status.Phase, d2.Status.Phase)
	}
	if d1.Requeue != d2.Requeue || d1.RequeueAfter != d2.RequeueAfter {
		t.Error("idempotency violated: requeue settings differ")
	}
	if (d1.EnsureStatefulSet == nil) != (d2.EnsureStatefulSet == nil) {
		t.Error("idempotency violated: STS ensure differs")
	}
}

// --- Operation Completion (OC1): no new gen during draining/cleaning ---

func TestComputeEngineReconcile_OC1_NoNewGenDuringDraining(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	spec.Image.Tag = "v3.0"
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseDraining,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: &drainingGen,
	}
	current := EngineState{
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		DrainingPodsDrained: false,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo())

	if result.Status.CurrentGeneration != 1 {
		t.Errorf("OC1 violated: generation changed to %d during draining", result.Status.CurrentGeneration)
	}
	if result.EnsureStatefulSet != nil {
		t.Error("OC1 violated: new StatefulSet created during draining")
	}
}

// --- Spec change during creating (absorb into current gen) ---

func TestComputeEngineReconcile_SpecChangeDuringCreating(t *testing.T) {
	spec := testSpec()
	spec.Image.Tag = "v3.0"
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	sts := makeSTS(testEngineName, 1, 3, "firebolt/engine:v2.0")
	hlSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-engine-g1-hl", Namespace: testNamespace}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-engine-g1-config", Namespace: testNamespace}}
	current := EngineState{
		CurrentSTS:              sts,
		CurrentHeadlessSvc:      hlSvc,
		CurrentConfigMap:        cm,
		CurrentPodsReady:        false,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected to stay in Creating, got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 2 {
		t.Errorf("expected generation to be bumped to 2, got %d", result.Status.CurrentGeneration)
	}
	if len(result.DeleteResources) != 3 {
		t.Errorf("expected 3 resources to delete (STS, headless svc, configmap), got %d", len(result.DeleteResources))
	}
	if !result.Requeue {
		t.Error("expected Requeue to be true")
	}
}

// --- Nil cluster service (buildClusterService paths) ---

func TestComputeEngineReconcile_CreatingNilClusterService(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 0,
		ActiveGeneration:  -1,
	}
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentPodsReady:        false,
		CurrentPodTotal:         1,
		CurrentPodReady:         0,
		ClusterService:          nil,
		ClusterServiceTargetGen: -1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.EnsureClusterSvc == nil {
		t.Fatal("expected cluster service to be created from scratch")
	}
	if result.EnsureClusterSvc.Spec.Selector[LabelGeneration] != "0" {
		t.Errorf("expected selector gen 0, got %s", result.EnsureClusterSvc.Spec.Selector[LabelGeneration])
	}
}

func TestComputeEngineReconcile_SwitchingNilClusterService(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseSwitching,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	current := EngineState{
		ClusterService:          nil,
		ClusterServiceTargetGen: -1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.EnsureClusterSvc == nil {
		t.Fatal("expected cluster service to be created from scratch")
	}
	if result.EnsureClusterSvc.Spec.Selector[LabelGeneration] != "1" {
		t.Errorf("expected selector gen 1, got %s", result.EnsureClusterSvc.Spec.Selector[LabelGeneration])
	}
}

func TestComputeEngineReconcile_StableNilClusterService(t *testing.T) {
	spec := testSpec()
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo()),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          nil,
		ClusterServiceTargetGen: -1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.EnsureClusterSvc == nil {
		t.Fatal("expected cluster service to be created")
	}
	if result.EnsureClusterSvc.Spec.Selector[LabelGeneration] != "0" {
		t.Errorf("expected selector gen 0, got %s", result.EnsureClusterSvc.Spec.Selector[LabelGeneration])
	}
}

// --- Headless service missing (without STS missing) ---

// If the headless Service for the active generation is deleted (manual
// kubectl, runaway script, stale GC logic, etc.), computeStable must
// re-materialize it in place — not bump to a new generation. The Service
// is name/selector-deterministic from (engine, gen) and its resurrection
// immediately restores intra-pod DNS without disrupting running pods.
func TestComputeEngineReconcile_S5_HeadlessSvcMissing(t *testing.T) {
	spec := testSpec()
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      nil,
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo()),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected phase Stable (in-place re-ensure), got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 0 {
		t.Errorf("expected unchanged generation 0, got %d", result.Status.CurrentGeneration)
	}
	if result.EnsureHeadlessSvc == nil {
		t.Fatal("expected headless Service to be re-ensured in place")
	}
	wantName := genResourceName(testEngineName, 0, SuffixHL)
	if result.EnsureHeadlessSvc.Name != wantName {
		t.Errorf("expected headless svc name %q, got %q", wantName, result.EnsureHeadlessSvc.Name)
	}
	if result.EnsureStatefulSet != nil {
		t.Error("expected no StatefulSet rebuild on HL-svc recovery")
	}
	if !result.Requeue {
		t.Error("expected Requeue=true after re-ensure")
	}
}

// --- ConfigMap missing (without STS missing) ---

// If the ConfigMap for the currently-active generation is accidentally
// deleted (manual kubectl, backup tool, etc.), computeStable must
// re-materialize it in place at the current generation. Engine pods only
// read the ConfigMap at startup, so re-creating it has no effect on
// running pods but unblocks any future pod restart that would otherwise
// get stuck Pending on the projected-volume mount. No new generation and
// no full rollout are needed — the rebuilt content is byte-identical to
// what the original generation was created with.
func TestComputeEngineReconcile_S5_ConfigMapMissing(t *testing.T) {
	spec := testSpec()
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        nil,
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected phase Stable (in-place re-ensure), got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 0 {
		t.Errorf("expected unchanged generation 0, got %d", result.Status.CurrentGeneration)
	}
	if result.EnsureConfigMap == nil {
		t.Fatal("expected ConfigMap to be re-ensured in place")
	}
	wantName := genResourceName(testEngineName, 0, SuffixConfig)
	if result.EnsureConfigMap.Name != wantName {
		t.Errorf("expected ConfigMap name %q, got %q", wantName, result.EnsureConfigMap.Name)
	}
	// Re-materialized content must equal the canonical generator output so
	// that ensureConfigMap's content-based update path treats a subsequent
	// reconcile (CM now present) as a no-op rather than a drift-driven write.
	want := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo())
	if result.EnsureConfigMap.Data[ConfigFileName] != want.Data[ConfigFileName] {
		t.Error("rebuilt ConfigMap content diverged from buildConfigMap output")
	}
	if result.EnsureStatefulSet != nil {
		t.Error("expected no StatefulSet rebuild on ConfigMap recovery")
	}
	if !result.Requeue {
		t.Error("expected Requeue=true after re-ensure")
	}
}

// --- Cleaning nil DrainingGeneration guard ---

func TestComputeEngineReconcile_CleaningNilDrainingGeneration(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseCleaning,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: nil,
	}
	current := EngineState{}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected phase Stable (nil draining gen recovery), got %s", result.Status.Phase)
	}
	if len(result.DeleteResources) != 0 {
		t.Errorf("expected no deletes, got %d", len(result.DeleteResources))
	}
}

// --- Spec change during creating: pods ready but STS stale ---

func TestComputeEngineReconcile_CreatingPodsReadyButSTSStale(t *testing.T) {
	spec := testSpec()
	spec.Image.Tag = "v3.0"
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	sts := makeSTS(testEngineName, 1, 3, "firebolt/engine:v2.0")
	hlSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "test-engine-g1-hl", Namespace: testNamespace}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "test-engine-g1-config", Namespace: testNamespace}}
	current := EngineState{
		CurrentSTS:              sts,
		CurrentHeadlessSvc:      hlSvc,
		CurrentConfigMap:        cm,
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected to stay Creating, got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 2 {
		t.Errorf("expected generation bumped to 2, got %d", result.Status.CurrentGeneration)
	}
	if len(result.DeleteResources) != 3 {
		t.Errorf("expected 3 resources to delete, got %d", len(result.DeleteResources))
	}
	if !result.Requeue {
		t.Error("expected Requeue to be true")
	}
}

// --- stsMatchesSpec coverage ---

func TestStsMatchesSpec(t *testing.T) {
	spec := testSpec()

	mutate := func(fn func(*appsv1.StatefulSet)) *appsv1.StatefulSet {
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		fn(sts)
		return sts
	}

	tests := []struct {
		name  string
		sts   *appsv1.StatefulSet
		match bool
	}{
		{"matching", makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"), true},
		{"replica mismatch", makeSTS(testEngineName, 0, 5, "firebolt/engine:v1.0"), false},
		{"image mismatch", makeSTS(testEngineName, 0, 3, "firebolt/engine:v2.0"), false},
		{"pull policy mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
		}), false},
		{"resource mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("4")
		}), false},
		{"node selector mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.NodeSelector = map[string]string{"zone": "us-east-1a"}
		}), false},
		{"toleration mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Tolerations = []corev1.Toleration{{Key: "special", Operator: corev1.TolerationOpExists}}
		}), false},
		{"service account mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.ServiceAccountName = "custom-sa"
		}), false},
		{"termination grace period mismatch", mutate(func(s *appsv1.StatefulSet) {
			other := int64(30)
			s.Spec.Template.Spec.TerminationGracePeriodSeconds = &other
		}), false},
		{"nil termination grace period", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.TerminationGracePeriodSeconds = nil
		}), false},
		{"nil replicas", mutate(func(s *appsv1.StatefulSet) { s.Spec.Replicas = nil }), false},
		{"no containers", mutate(func(s *appsv1.StatefulSet) { s.Spec.Template.Spec.Containers = nil }), false},
		{"affinity mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Affinity = &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      "pool",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"engine"},
							}},
						}},
					},
				},
			}
		}), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stsMatchesSpec(tt.sts, spec)
			if got != tt.match {
				t.Errorf("stsMatchesSpec() = %v, want %v", got, tt.match)
			}
		})
	}

	t.Run("no resources matches empty container resources", func(t *testing.T) {
		customSpec := testSpec()
		customSpec.Resources = corev1.ResourceRequirements{}
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
		if !stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want true for omitted resources")
		}
	})

	t.Run("requests only matches", func(t *testing.T) {
		customSpec := testSpec()
		customSpec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.Containers[0].Resources = engineContainerResources(customSpec)
		if !stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want true for requests-only resources")
		}
	})

	t.Run("limits only matches", func(t *testing.T) {
		customSpec := testSpec()
		customSpec.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		}
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.Containers[0].Resources = engineContainerResources(customSpec)
		if !stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want true for limits-only resources")
		}
	})

	t.Run("partial requests and limits match", func(t *testing.T) {
		customSpec := testSpec()
		customSpec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("750m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		}
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.Containers[0].Resources = engineContainerResources(customSpec)
		if !stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want true for partial resource requirements")
		}
	})

	t.Run("limit drift triggers mismatch", func(t *testing.T) {
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("16Gi")
		if stsMatchesSpec(sts, testSpec()) {
			t.Fatal("stsMatchesSpec() want false when a resource limit drifts")
		}
	})

	t.Run("removed resources trigger mismatch", func(t *testing.T) {
		customSpec := testSpec()
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
		if stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want false when resources are removed")
		}
	})

	t.Run("equivalent quantities match", func(t *testing.T) {
		customSpec := testSpec()
		customSpec.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
		}
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1024Mi"),
			},
		}
		if !stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want true for semantically equivalent quantities")
		}
	})

	t.Run("explicit serviceAccountName matches STS", func(t *testing.T) {
		customSpec := testSpec()
		sa := "custom-sa"
		customSpec.ServiceAccountName = &sa
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		sts.Spec.Template.Spec.ServiceAccountName = sa
		if !stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want true for matching serviceAccountName")
		}
	})

	t.Run("pod security context drift triggers mismatch", func(t *testing.T) {
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		// Drop fsGroup to simulate an STS built before the default existed:
		// the spec resolves to fsGroup=3473, so the comparison must fail.
		sts.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{}
		if stsMatchesSpec(sts, testSpec()) {
			t.Fatal("stsMatchesSpec() want false when pod SecurityContext drifts")
		}
	})

	t.Run("container security context drift triggers mismatch", func(t *testing.T) {
		customSpec := testSpec()
		customSpec.SecurityContext = &corev1.SecurityContext{
			ReadOnlyRootFilesystem: boolPtr(true),
		}
		// STS still has the old (nil) container SC, so a spec change must
		// not be silently absorbed by an in-place update.
		sts := makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0")
		if stsMatchesSpec(sts, customSpec) {
			t.Fatal("stsMatchesSpec() want false when spec.securityContext is added")
		}
	})

	t.Run("user-supplied podSecurityContext fields pass through and override fsGroup", func(t *testing.T) {
		customSpec := testSpec()
		userFSGroup := int64(9999)
		runAsGroup := int64(2000)
		customSpec.PodSecurityContext = &corev1.PodSecurityContext{
			FSGroup:    &userFSGroup,
			RunAsGroup: &runAsGroup,
		}
		got := getEnginePodSecurityContext(customSpec)
		if got.FSGroup == nil || *got.FSGroup != userFSGroup {
			t.Fatalf("expected user FSGroup=%d to win over operator default, got %+v", userFSGroup, got.FSGroup)
		}
		if got.RunAsGroup == nil || *got.RunAsGroup != runAsGroup {
			t.Fatalf("expected RunAsGroup pass-through, got %+v", got.RunAsGroup)
		}
	})

	t.Run("operator stamps default fsGroup 3473 when spec leaves it unset", func(t *testing.T) {
		got := getEnginePodSecurityContext(testSpec())
		if got == nil || got.FSGroup == nil || *got.FSGroup != DefaultEngineFSGroup {
			t.Fatalf("expected default FSGroup=%d, got %+v", DefaultEngineFSGroup, got)
		}
	})

	t.Run("operator stamps default FSGroupChangePolicy=OnRootMismatch when spec leaves it unset", func(t *testing.T) {
		got := getEnginePodSecurityContext(testSpec())
		if got == nil || got.FSGroupChangePolicy == nil || *got.FSGroupChangePolicy != corev1.FSGroupChangeOnRootMismatch {
			t.Fatalf("expected default FSGroupChangePolicy=OnRootMismatch, got %+v", got.FSGroupChangePolicy)
		}
	})

	t.Run("user-supplied FSGroupChangePolicy is preserved", func(t *testing.T) {
		customSpec := testSpec()
		always := corev1.FSGroupChangeAlways
		customSpec.PodSecurityContext = &corev1.PodSecurityContext{
			FSGroupChangePolicy: &always,
		}
		got := getEnginePodSecurityContext(customSpec)
		if got.FSGroupChangePolicy == nil || *got.FSGroupChangePolicy != corev1.FSGroupChangeAlways {
			t.Fatalf("expected user FSGroupChangePolicy=Always to win, got %+v", got.FSGroupChangePolicy)
		}
	})

	t.Run("operator preserves user fields without clobbering them", func(t *testing.T) {
		customSpec := testSpec()
		customSpec.PodSecurityContext = &corev1.PodSecurityContext{
			Sysctls: []corev1.Sysctl{{Name: "net.core.somaxconn", Value: "1024"}},
		}
		got := getEnginePodSecurityContext(customSpec)
		if len(got.Sysctls) != 1 || got.Sysctls[0].Name != "net.core.somaxconn" {
			t.Fatalf("expected user Sysctls pass-through, got %+v", got.Sysctls)
		}
		if got.FSGroup == nil || *got.FSGroup != DefaultEngineFSGroup {
			t.Fatalf("expected default FSGroup to fill in when user omitted it, got %+v", got.FSGroup)
		}
	})

	t.Run("getEngineContainerSecurityContext returns nil when unset", func(t *testing.T) {
		if got := getEngineContainerSecurityContext(testSpec()); got != nil {
			t.Fatalf("expected nil container SecurityContext when unset, got %+v", got)
		}
	})
}

func TestStsSpecEqual(t *testing.T) {
	base := func() *appsv1.StatefulSet { return makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0") }

	t.Run("identical StatefulSets are equal", func(t *testing.T) {
		if !stsSpecEqual(base(), base()) {
			t.Fatal("stsSpecEqual() want true for identical StatefulSets")
		}
	})

	t.Run("affinity mismatch is detected", func(t *testing.T) {
		a := base()
		b := base()
		b.Spec.Template.Spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "pool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"engine"},
						}},
					}},
				},
			},
		}
		if stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want false when Affinity differs")
		}
	})

	t.Run("nil vs non-nil affinity is detected", func(t *testing.T) {
		a := base()
		b := base()
		b.Spec.Template.Spec.Affinity = &corev1.Affinity{}
		if stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want false when one Affinity is nil and the other is non-nil")
		}
	})

	t.Run("matching affinity is equal", func(t *testing.T) {
		affinity := &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "pool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"engine"},
						}},
					}},
				},
			},
		}
		a := base()
		b := base()
		a.Spec.Template.Spec.Affinity = affinity
		b.Spec.Template.Spec.Affinity = affinity.DeepCopy()
		if !stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want true for matching Affinity")
		}
	})

	t.Run("pod labels mismatch is detected", func(t *testing.T) {
		a := base()
		b := base()
		b.Spec.Template.Labels = map[string]string{
			LabelEngine:     testEngineName,
			LabelGeneration: "0",
			"custom":        "value",
		}
		if stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want false when pod labels differ")
		}
	})

	t.Run("matching pod labels are equal", func(t *testing.T) {
		a := base()
		b := base()
		labels := map[string]string{
			LabelEngine:     testEngineName,
			LabelGeneration: "0",
			"app":           "packdb",
			"team":          "control-plane",
		}
		a.Spec.Template.Labels = labels
		b.Spec.Template.Labels = map[string]string{
			LabelEngine:     testEngineName,
			LabelGeneration: "0",
			"app":           "packdb",
			"team":          "control-plane",
		}
		if !stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want true for matching pod labels")
		}
	})

	t.Run("pod annotations mismatch is detected", func(t *testing.T) {
		a := base()
		b := base()
		b.Spec.Template.Annotations = map[string]string{
			"prometheus.io/scrape": "true",
		}
		if stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want false when pod template annotations differ")
		}
	})

	t.Run("nil and empty pod annotations are equal", func(t *testing.T) {
		a := base()
		b := base()
		a.Spec.Template.Annotations = nil
		b.Spec.Template.Annotations = map[string]string{}
		if !stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want true when one side is nil and the other is an empty map")
		}
	})

	t.Run("matching pod annotations are equal", func(t *testing.T) {
		a := base()
		b := base()
		a.Spec.Template.Annotations = map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   "9090",
		}
		b.Spec.Template.Annotations = map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   "9090",
		}
		if !stsSpecEqual(a, b) {
			t.Fatal("stsSpecEqual() want true for matching pod annotations")
		}
	})
}

func TestBuildStatefulSet_Affinity(t *testing.T) {
	t.Run("nodeAffinity propagates to pod template", func(t *testing.T) {
		spec := testSpec()
		spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "pool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"engine"},
						}},
					}},
				},
			},
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		got := sts.Spec.Template.Spec.Affinity
		if got == nil || got.NodeAffinity == nil {
			t.Fatal("expected NodeAffinity to be propagated to pod template")
		}
		terms := got.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		if len(terms) != 1 || terms[0].MatchExpressions[0].Key != "pool" {
			t.Fatalf("unexpected NodeAffinity content: %+v", got.NodeAffinity)
		}
	})

	t.Run("podAntiAffinity propagates to pod template", func(t *testing.T) {
		spec := testSpec()
		spec.Affinity = &corev1.Affinity{
			PodAntiAffinity: &corev1.PodAntiAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					TopologyKey: "kubernetes.io/hostname",
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "firebolt-engine"},
					},
				}},
			},
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		got := sts.Spec.Template.Spec.Affinity
		if got == nil || got.PodAntiAffinity == nil {
			t.Fatal("expected PodAntiAffinity to be propagated to pod template")
		}
		terms := got.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if len(terms) != 1 || terms[0].TopologyKey != "kubernetes.io/hostname" {
			t.Fatalf("unexpected PodAntiAffinity content: %+v", got.PodAntiAffinity)
		}
	})

	t.Run("nil affinity produces nil pod template affinity", func(t *testing.T) {
		spec := testSpec()
		spec.Affinity = nil
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		if sts.Spec.Template.Spec.Affinity != nil {
			t.Fatalf("expected nil Affinity in pod template, got %+v", sts.Spec.Template.Spec.Affinity)
		}
	})

	t.Run("affinity drift detected by stsMatchesSpec", func(t *testing.T) {
		spec := testSpec()
		spec.Affinity = &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
					TopologyKey: "topology.kubernetes.io/zone",
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"firebolt.io/engine": testEngineName},
					},
				}},
			},
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		// The STS matches spec because we built it from spec.
		if !stsMatchesSpec(sts, spec) {
			t.Fatal("stsMatchesSpec() want true for matching affinity")
		}
		// Remove affinity from spec — now the STS is ahead of spec and must be
		// detected as drifted.
		specNoAffinity := testSpec()
		if stsMatchesSpec(sts, specNoAffinity) {
			t.Fatal("stsMatchesSpec() want false when spec affinity removed but STS still has it")
		}
	})

	t.Run("adding affinity to spec is detected as drift", func(t *testing.T) {
		// STS built without affinity.
		sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0)
		// Spec now requires nodeAffinity — STS must be re-rolled.
		spec := testSpec()
		spec.Affinity = &corev1.Affinity{
			NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{{
						MatchExpressions: []corev1.NodeSelectorRequirement{{
							Key:      "pool",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"engine"},
						}},
					}},
				},
			},
		}
		if stsMatchesSpec(sts, spec) {
			t.Fatal("stsMatchesSpec() want false when affinity added to spec but STS has none")
		}
	})
}

func TestBuildStatefulSet_PodLabels(t *testing.T) {
	t.Run("base labels always present", func(t *testing.T) {
		spec := testSpec()
		spec.PodLabels = nil
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 2)
		labels := sts.Spec.Template.Labels
		if labels[LabelEngine] != testEngineName {
			t.Errorf("expected %s label = %q, got %q", LabelEngine, testEngineName, labels[LabelEngine])
		}
		if labels[LabelGeneration] != "2" {
			t.Errorf("expected %s label = %q, got %q", LabelGeneration, "2", labels[LabelGeneration])
		}
	})

	t.Run("user labels merged with base labels", func(t *testing.T) {
		spec := testSpec()
		spec.PodLabels = map[string]string{
			"app":         "packdb",
			"environment": "prod",
			"team":        "control-plane",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 1)
		labels := sts.Spec.Template.Labels
		// Base labels must be present.
		if labels[LabelEngine] != testEngineName {
			t.Errorf("expected %s label = %q, got %q", LabelEngine, testEngineName, labels[LabelEngine])
		}
		if labels[LabelGeneration] != "1" {
			t.Errorf("expected %s label = %q, got %q", LabelGeneration, "1", labels[LabelGeneration])
		}
		// User labels must be present.
		if labels["app"] != "packdb" {
			t.Errorf("expected user label app = packdb, got %q", labels["app"])
		}
		if labels["environment"] != "prod" {
			t.Errorf("expected user label environment = prod, got %q", labels["environment"])
		}
		if labels["team"] != "control-plane" {
			t.Errorf("expected user label team = control-plane, got %q", labels["team"])
		}
	})

	t.Run("reserved labels cannot be overridden", func(t *testing.T) {
		spec := testSpec()
		spec.PodLabels = map[string]string{
			LabelEngine:     "user-engine-override",
			LabelGeneration: "999",
			"custom":        "value",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 5)
		labels := sts.Spec.Template.Labels
		// Reserved labels must be operator-owned.
		if labels[LabelEngine] != testEngineName {
			t.Errorf("expected %s label = %q (not user override), got %q", LabelEngine, testEngineName, labels[LabelEngine])
		}
		if labels[LabelGeneration] != "5" {
			t.Errorf("expected %s label = %q (not user override), got %q", LabelGeneration, "5", labels[LabelGeneration])
		}
		// Non-reserved user labels are still applied.
		if labels["custom"] != "value" {
			t.Errorf("expected user label custom = value, got %q", labels["custom"])
		}
	})

	t.Run("pod labels drift detected by stsMatchesSpec", func(t *testing.T) {
		spec := testSpec()
		spec.PodLabels = map[string]string{
			"version": "1.0",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		if !stsMatchesSpec(sts, spec) {
			t.Fatal("stsMatchesSpec() want true for matching pod labels")
		}
		// Remove label from spec — STS is now drifted.
		specNoLabels := testSpec()
		if stsMatchesSpec(sts, specNoLabels) {
			t.Fatal("stsMatchesSpec() want false when spec pod labels removed but STS still has them")
		}
	})

	t.Run("adding pod labels to spec is detected as drift", func(t *testing.T) {
		// STS built without extra pod labels.
		sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0)
		// Spec now has extra labels — STS must be re-rolled.
		spec := testSpec()
		spec.PodLabels = map[string]string{
			"new-label": "new-value",
		}
		if stsMatchesSpec(sts, spec) {
			t.Fatal("stsMatchesSpec() want false when pod labels added to spec but STS has none")
		}
	})

	t.Run("changing pod label value is detected as drift", func(t *testing.T) {
		spec := testSpec()
		spec.PodLabels = map[string]string{
			"version": "1.0",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		// Change the label value.
		specChanged := testSpec()
		specChanged.PodLabels = map[string]string{
			"version": "2.0",
		}
		if stsMatchesSpec(sts, specChanged) {
			t.Fatal("stsMatchesSpec() want false when pod label value changed")
		}
	})
}

func TestBuildStatefulSet_PodAnnotations(t *testing.T) {
	t.Run("nil pod annotations produce no pod template annotations", func(t *testing.T) {
		spec := testSpec()
		spec.PodAnnotations = nil
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		// We intentionally render no annotations on the pod template
		// when the spec contributes none, so existing StatefulSets
		// stay byte-identical across upgrades that introduce this
		// field.
		if got := sts.Spec.Template.Annotations; len(got) != 0 {
			t.Fatalf("expected no pod template annotations when spec.PodAnnotations is nil, got %v", got)
		}
	})

	t.Run("user annotations propagate to pod template", func(t *testing.T) {
		spec := testSpec()
		spec.PodAnnotations = map[string]string{
			"prometheus.io/scrape":        "true",
			"prometheus.io/port":          "9090",
			"firebolt.dev/cost-center":    "analytics",
			"karpenter.sh/do-not-disrupt": "true",
			"kubernetes.io/description":   "engine pod",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 1)
		got := sts.Spec.Template.Annotations
		for k, want := range spec.PodAnnotations {
			if got[k] != want {
				t.Errorf("expected annotation %q = %q, got %q", k, want, got[k])
			}
		}
	})

	t.Run("operator-managed StatefulSet annotations are not affected by PodAnnotations", func(t *testing.T) {
		// AnnotationMetadataOverride lives on the StatefulSet's own
		// ObjectMeta, not on the pod template. User pod annotations
		// must never leak there, and operator-managed STS
		// annotations must keep their values regardless of user
		// pod-annotation input that happens to use the same key.
		override := "metadata.example.com:7000"
		spec := testSpec()
		spec.MetadataEndpointOverride = &override
		spec.PodAnnotations = map[string]string{
			AnnotationMetadataOverride: "user-override-should-not-take-effect",
			"unrelated":                "value",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)

		if got := sts.Annotations[AnnotationMetadataOverride]; got != override {
			t.Errorf("expected STS-level %s = %q (operator-owned), got %q",
				AnnotationMetadataOverride, override, got)
		}
		if _, ok := sts.Annotations["unrelated"]; ok {
			t.Error("user pod annotation must not leak onto the StatefulSet's own ObjectMeta")
		}
		// User annotations land on the pod template.
		if got := sts.Spec.Template.Annotations["unrelated"]; got != "value" {
			t.Errorf("expected pod-template annotation unrelated = %q, got %q", "value", got)
		}
	})

	t.Run("pod annotations drift detected by stsMatchesSpec", func(t *testing.T) {
		spec := testSpec()
		spec.PodAnnotations = map[string]string{
			"prometheus.io/scrape": "true",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		if !stsMatchesSpec(sts, spec) {
			t.Fatal("stsMatchesSpec() want true for matching pod annotations")
		}
		specNoAnnotations := testSpec()
		if stsMatchesSpec(sts, specNoAnnotations) {
			t.Fatal("stsMatchesSpec() want false when spec pod annotations removed but STS still has them")
		}
	})

	t.Run("adding pod annotations to spec is detected as drift", func(t *testing.T) {
		sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0)
		spec := testSpec()
		spec.PodAnnotations = map[string]string{
			"new-annotation": "new-value",
		}
		if stsMatchesSpec(sts, spec) {
			t.Fatal("stsMatchesSpec() want false when pod annotations added to spec but STS has none")
		}
	})

	t.Run("changing pod annotation value is detected as drift", func(t *testing.T) {
		spec := testSpec()
		spec.PodAnnotations = map[string]string{
			"version": "1.0",
		}
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		specChanged := testSpec()
		specChanged.PodAnnotations = map[string]string{
			"version": "2.0",
		}
		if stsMatchesSpec(sts, specChanged) {
			t.Fatal("stsMatchesSpec() want false when pod annotation value changed")
		}
	})

	t.Run("empty map and nil are equal for stsMatchesSpec", func(t *testing.T) {
		// Round-tripping through the API server can normalise an
		// empty annotation map to nil (json:omitempty). Treat them
		// as identical so we don't churn StatefulSets on no-op
		// changes.
		spec := testSpec()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
		sts.Spec.Template.Annotations = map[string]string{}
		if !stsMatchesSpec(sts, spec) {
			t.Fatal("stsMatchesSpec() want true when pod template annotations is empty map and spec.PodAnnotations is nil")
		}
	})
}

func TestEnginePodAnnotations(t *testing.T) {
	t.Run("nil input returns nil", func(t *testing.T) {
		got := enginePodAnnotations(&computev1alpha1.FireboltEngineSpec{})
		if got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})

	t.Run("returns a copy, not the spec reference", func(t *testing.T) {
		// Mutating the returned map must not bleed into the spec.
		// buildStatefulSet might otherwise share state across
		// generations if future code grows the map in place.
		spec := &computev1alpha1.FireboltEngineSpec{
			PodAnnotations: map[string]string{"k": "v"},
		}
		got := enginePodAnnotations(spec)
		got["mutated"] = "yes"
		if _, ok := spec.PodAnnotations["mutated"]; ok {
			t.Fatal("enginePodAnnotations must return a copy, not the underlying spec map")
		}
	})
}

func TestAnnotationsEqual(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"nil and empty are equal", nil, map[string]string{}, true},
		{"empty and nil are equal", map[string]string{}, nil, true},
		{"both empty", map[string]string{}, map[string]string{}, true},
		{"identical maps", map[string]string{"a": "1", "b": "2"}, map[string]string{"a": "1", "b": "2"}, true},
		{"different value", map[string]string{"a": "1"}, map[string]string{"a": "2"}, false},
		{"different key", map[string]string{"a": "1"}, map[string]string{"b": "1"}, false},
		{"different length", map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := annotationsEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("annotationsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestBuildStatefulSet_UsesEngineResources(t *testing.T) {
	spec := testSpec()
	spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("750m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("3Gi"),
		},
	}

	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
	got := sts.Spec.Template.Spec.Containers[0].Resources
	if !resourceRequirementsEqual(got, spec.Resources) {
		t.Fatalf("buildStatefulSet resources = %#v, want %#v", got, spec.Resources)
	}
}

// --- Scale to zero / Stopped phase ---

// TestTerminalPhase verifies the single-point selector: replicas==0 ->
// Stopped, replicas>0 -> Stable. Every terminal-phase write in the state
// machine funnels through this helper.
func TestTerminalPhase(t *testing.T) {
	tests := []struct {
		name     string
		replicas int32
		want     computev1alpha1.EnginePhase
	}{
		{"replicas=0 => Stopped", 0, computev1alpha1.PhaseStopped},
		{"replicas=1 => Stable", 1, computev1alpha1.PhaseStable},
		{"replicas=5 => Stable", 5, computev1alpha1.PhaseStable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec()
			spec.Replicas = tt.replicas
			if got := terminalPhase(spec); got != tt.want {
				t.Errorf("terminalPhase(replicas=%d) = %s, want %s", tt.replicas, got, tt.want)
			}
		})
	}
}

// TestComputeEngineReconcile_Stop_StableToCreating verifies that scaling
// a running engine down to 0 replicas goes through the normal blue-green
// drift path: computeStable sees spec.Replicas change, bumps the
// generation, and transitions to Creating. The zero-replica drop does not
// take a shortcut around the state machine.
func TestComputeEngineReconcile_Stop_StableToCreating(t *testing.T) {
	spec := testSpec()
	spec.Replicas = 0
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(testSpec(), testEngineName, testNamespace, 0, testInstanceInfo()),
		CurrentPodsReady:        true,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected phase Creating (drift to replicas=0), got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 1 {
		t.Errorf("expected generation 1, got %d", result.Status.CurrentGeneration)
	}
}

// TestComputeEngineReconcile_Stop_ComputeStableChoosesStoppedWhenReplicasZero
// verifies that when the live STS already matches spec.Replicas==0
// (i.e., the blue-green transition to zero has settled), the terminal
// write picks Stopped rather than Stable.
func TestComputeEngineReconcile_Stop_ComputeStableChoosesStoppedWhenReplicasZero(t *testing.T) {
	spec := testSpec()
	spec.Replicas = 0
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseStable,
		CurrentGeneration: 1,
		ActiveGeneration:  1,
	}
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 1, 0, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 1, testInstanceInfo()),
		CurrentPodsReady:        true,
		ClusterService:          makeClusterSvc(testEngineName, 1),
		ClusterServiceTargetGen: 1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStopped {
		t.Errorf("expected phase Stopped, got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 1 {
		t.Errorf("expected no generation bump, got %d", result.Status.CurrentGeneration)
	}
}

// TestComputeEngineReconcile_Stop_SwitchingToStoppedInitialDeploy verifies
// that a zero-replica initial deploy (no old generation to drain) lands
// directly in Stopped after the selector flip.
func TestComputeEngineReconcile_Stop_SwitchingToStoppedInitialDeploy(t *testing.T) {
	spec := testSpec()
	spec.Replicas = 0
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseSwitching,
		CurrentGeneration: 0,
		ActiveGeneration:  -1,
	}
	current := EngineState{
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStopped {
		t.Errorf("expected phase Stopped, got %s", result.Status.Phase)
	}
	if result.Status.ActiveGeneration != 0 {
		t.Errorf("expected active gen 0, got %d", result.Status.ActiveGeneration)
	}
}

// TestComputeEngineReconcile_Stop_CleaningToStopped verifies that the
// cleaning phase's terminal write picks Stopped when spec.Replicas==0.
// This is the path taken when a running engine is scaled down to 0 and
// the old generation has finished draining.
func TestComputeEngineReconcile_Stop_CleaningToStopped(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	spec.Replicas = 0
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseCleaning,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: &drainingGen,
	}
	current := EngineState{
		DrainingSTS: makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseStopped {
		t.Errorf("expected phase Stopped, got %s", result.Status.Phase)
	}
	if result.Status.DrainingGeneration != nil {
		t.Error("expected DrainingGeneration cleared")
	}
	if len(result.DeleteResources) == 0 {
		t.Error("expected draining STS to be queued for deletion")
	}
}

// TestComputeEngineReconcile_Stop_StoppedToCreating verifies that a
// stopped engine resumes via a new blue-green generation when
// spec.Replicas goes back to non-zero. The state machine routes Stopped
// through computeStable, which detects the spec drift and bumps the
// generation just like from Stable.
func TestComputeEngineReconcile_Stop_StoppedToCreating(t *testing.T) {
	spec := testSpec()
	spec.Replicas = 3
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseStopped,
		CurrentGeneration: 1,
		ActiveGeneration:  1,
	}
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 1, 0, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 1, testInstanceInfo()),
		CurrentPodsReady:        true,
		ClusterService:          makeClusterSvc(testEngineName, 1),
		ClusterServiceTargetGen: 1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo())

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected phase Creating (resume from stopped), got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 2 {
		t.Errorf("expected generation 2, got %d", result.Status.CurrentGeneration)
	}
}

// --- buildConfigMap: customEngineConfig root-merge semantics ---

// renderConfig builds a ConfigMap with the given customEngineConfig and
// returns the parsed config.yaml document. Returns the top-level map.
func renderConfig(t *testing.T, custom string) map[string]interface{} {
	t.Helper()
	spec := testSpec()
	if custom != "" {
		spec.CustomEngineConfig = &apiextensionsv1.JSON{Raw: []byte(custom)}
	}
	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo())
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	return root
}

// nestedMap returns root[section] as a map, failing the test if absent or wrong type.
func nestedMap(t *testing.T, root map[string]interface{}, section string) map[string]interface{} {
	t.Helper()
	m, ok := root[section].(map[string]interface{})
	if !ok {
		t.Fatalf("rendered config.yaml has no `%s` object: %v", section, root[section])
	}
	return m
}

func TestBuildConfigMap_NoCustomConfig_DefaultsApplied(t *testing.T) {
	root := renderConfig(t, "")

	if root["schema_version"] != EngineConfigSchemaVersion {
		t.Errorf("schema_version = %v, want %q", root["schema_version"], EngineConfigSchemaVersion)
	}

	instance := nestedMap(t, root, "instance")
	if instance["id"] != testInstanceID {
		t.Errorf("instance.id = %v, want %v", instance["id"], testInstanceID)
	}
	if instance["type"] != "multi_engine" {
		t.Errorf("instance.type = %v, want multi_engine", instance["type"])
	}
	multi := nestedMap(t, instance, "multi_engine")
	if multi["metadata_endpoint"] != testMetadataEndpoint {
		t.Errorf("instance.multi_engine.metadata_endpoint = %v, want %v",
			multi["metadata_endpoint"], testMetadataEndpoint)
	}

	engine := nestedMap(t, root, "engine")
	if engine["id"] != testEngineName {
		t.Errorf("engine.id = %v, want %v", engine["id"], testEngineName)
	}
	nodes, ok := engine["nodes"].([]interface{})
	if !ok {
		t.Fatalf("engine.nodes = %v, want array", engine["nodes"])
	}
	if len(nodes) != int(testSpec().Replicas) {
		t.Errorf("engine.nodes length = %d, want %d", len(nodes), testSpec().Replicas)
	}

	logging := nestedMap(t, root, "logging")
	if logging["format"] != "json" {
		t.Errorf("logging.format = %v, want json", logging["format"])
	}
}

func TestBuildConfigMap_NestedSectionOverridesUserDefaults(t *testing.T) {
	custom := `{"logging": {"format": "text", "level": "debug"}}`
	root := renderConfig(t, custom)
	logging := nestedMap(t, root, "logging")
	if logging["format"] != "text" {
		t.Errorf("logging.format = %v, want text", logging["format"])
	}
	if logging["level"] != "debug" {
		t.Errorf("logging.level = %v, want debug", logging["level"])
	}

	// Operator-controlled sections should be untouched.
	instance := nestedMap(t, root, "instance")
	if instance["id"] != testInstanceID {
		t.Errorf("instance.id was clobbered: got %v", instance["id"])
	}
}

func TestBuildConfigMap_RootKeysAddedAsSiblings(t *testing.T) {
	custom := `{"some_root_key": "value", "auth": {"mode": "disabled"}}`
	root := renderConfig(t, custom)
	if root["some_root_key"] != "value" {
		t.Errorf("root.some_root_key = %v, want value", root["some_root_key"])
	}
	auth := nestedMap(t, root, "auth")
	if auth["mode"] != "disabled" {
		t.Errorf("root.auth.mode = %v, want disabled", auth["mode"])
	}
	instance := nestedMap(t, root, "instance")
	if instance["id"] != testInstanceID {
		t.Errorf("instance.id was clobbered: got %v", instance["id"])
	}
}

func TestBuildConfigMap_UserOverridesNonAuthoritativeFields(t *testing.T) {
	// Sibling keys inside `engine` and `instance` (outside the operator-
	// owned paths) must round-trip while the operator-set siblings remain
	// intact. This covers the partial-extension case the full-section-
	// override and root-sibling tests miss. The `instance.multi_engine`
	// subtree is entirely operator-owned and tested separately.
	custom := `{
		"engine": {"extra_setting": 7},
		"instance": {"display_name": "prod"}
	}`
	root := renderConfig(t, custom)

	engine := nestedMap(t, root, "engine")
	if engine["extra_setting"] != float64(7) {
		t.Errorf("engine.extra_setting = %v, want 7", engine["extra_setting"])
	}
	if engine["id"] != testEngineName {
		t.Errorf("engine.id was clobbered: %v", engine["id"])
	}

	instance := nestedMap(t, root, "instance")
	if instance["display_name"] != "prod" {
		t.Errorf("instance.display_name = %v, want prod", instance["display_name"])
	}
	if instance["id"] != testInstanceID {
		t.Errorf("instance.id was clobbered: %v", instance["id"])
	}
	multi := nestedMap(t, instance, "multi_engine")
	if multi["metadata_endpoint"] != testMetadataEndpoint {
		t.Errorf("instance.multi_engine.metadata_endpoint was clobbered: %v", multi["metadata_endpoint"])
	}
}

func TestBuildConfigMap_ProtectedPathsStripped(t *testing.T) {
	custom := `{
		"schema_version": "99.0",
		"instance": {
			"id": "evil",
			"type": "single_engine",
			"multi_engine": {"metadata_endpoint": "evil:0"}
		},
		"engine": {
			"id": "evil",
			"nodes": [{"host": "evil"}],
			"termination_grace_period": "99999s"
		}
	}`
	root := renderConfig(t, custom)
	if root["schema_version"] != EngineConfigSchemaVersion {
		t.Errorf("schema_version = %v, want %q (operator-authoritative)",
			root["schema_version"], EngineConfigSchemaVersion)
	}
	instance := nestedMap(t, root, "instance")
	if instance["id"] != testInstanceID {
		t.Errorf("instance.id = %v, want %v (operator-authoritative)", instance["id"], testInstanceID)
	}
	if instance["type"] != "multi_engine" {
		t.Errorf("instance.type = %v, want multi_engine", instance["type"])
	}
	multi := nestedMap(t, instance, "multi_engine")
	if multi["metadata_endpoint"] != testMetadataEndpoint {
		t.Errorf("instance.multi_engine.metadata_endpoint = %v, want %v (operator-authoritative)",
			multi["metadata_endpoint"], testMetadataEndpoint)
	}

	engine := nestedMap(t, root, "engine")
	if engine["id"] != testEngineName {
		t.Errorf("engine.id = %v, want %v", engine["id"], testEngineName)
	}
	nodes, ok := engine["nodes"].([]interface{})
	if !ok {
		t.Fatalf("engine.nodes = %v, want array", engine["nodes"])
	}
	if len(nodes) != int(testSpec().Replicas) {
		t.Errorf("engine.nodes length = %d, want %d (user input must not replace)",
			len(nodes), testSpec().Replicas)
	}
	first, ok := nodes[0].(map[string]interface{})
	if !ok || first["host"] == "evil" {
		t.Errorf("engine.nodes[0] was overridden by user input: %v", nodes[0])
	}
	if engine["termination_grace_period"] == "99999s" {
		t.Error("engine.termination_grace_period was overridden by user input")
	}
}

func TestBuildConfigMap_NonMapSectionsDropped(t *testing.T) {
	// When user supplies a scalar (or any non-object) for an operator-
	// managed section, the whole key must be stripped: a deep merge would
	// otherwise replace the operator-built map wholesale with the scalar,
	// losing every authoritative key.
	cases := []string{
		`{"instance": "evil"}`,
		`{"instance": 42}`,
		`{"instance": ["evil"]}`,
		`{"engine": null}`,
		`{"engine": "evil"}`,
	}
	for _, custom := range cases {
		t.Run(custom, func(t *testing.T) {
			root := renderConfig(t, custom)
			instance := nestedMap(t, root, "instance")
			if instance["id"] != testInstanceID {
				t.Errorf("operator-authoritative instance.id lost: got %v", instance["id"])
			}
			engine := nestedMap(t, root, "engine")
			if engine["id"] != testEngineName {
				t.Errorf("operator-authoritative engine.id lost: got %v", engine["id"])
			}
		})
	}
}

func TestBuildConfigMap_InvalidJSONIgnored(t *testing.T) {
	// CRD admission would normally reject this, but the controller defends
	// against an apiserver bug by skipping the merge silently.
	spec := testSpec()
	spec.CustomEngineConfig = &apiextensionsv1.JSON{Raw: []byte(`not valid json`)}
	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo())
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	instance := root["instance"].(map[string]interface{})
	if instance["id"] != testInstanceID {
		t.Errorf("invalid customEngineConfig should be ignored, but defaults were touched: %v", instance)
	}
}

// TestBuildStatefulSet_PartialImageOverride pins the contract end to
// end: a spec.Image that sets only repository or only tag must produce a
// pod container image that combines the user-supplied half with the
// operator default for the other half, and stsMatchesSpec must accept the
// resulting STS without triggering a new blue-green generation.
func TestBuildStatefulSet_PartialImageOverride(t *testing.T) {
	tests := []struct {
		name      string
		image     *computev1alpha1.ImageSpec
		wantImage string
	}{
		{
			name:      "nil spec uses default reference",
			image:     nil,
			wantImage: DefaultEngineImage,
		},
		{
			name:      "repository-only override keeps default tag",
			image:     &computev1alpha1.ImageSpec{Repository: "mirror.example.com/engine"},
			wantImage: "mirror.example.com/engine:" + DefaultEngineTag,
		},
		{
			name:      "tag-only override keeps default repository",
			image:     &computev1alpha1.ImageSpec{Tag: "v9.9.9"},
			wantImage: DefaultEngineRepository + ":v9.9.9",
		},
		{
			name: "both fields override completely",
			image: &computev1alpha1.ImageSpec{
				Repository: "mirror.example.com/engine",
				Tag:        "v9.9.9",
			},
			wantImage: "mirror.example.com/engine:v9.9.9",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := testSpec()
			spec.Image = tc.image

			sts := buildStatefulSet(spec, testEngineName, testNamespace, 0)
			got := sts.Spec.Template.Spec.Containers[0].Image
			if got != tc.wantImage {
				t.Errorf("container image = %q, want %q", got, tc.wantImage)
			}

			// stsMatchesSpec must accept the just-built STS so a partial
			// override does not perpetually trigger a new blue-green
			// generation.
			if !stsMatchesSpec(sts, spec) {
				t.Error("stsMatchesSpec returned false for a freshly built STS")
			}
		})
	}
}
