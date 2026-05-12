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
	"encoding/json"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	testEngineName       = "test-engine"
	testNamespace        = "default"
	testMetadataEndpoint = "test-instance-metadata.default.svc.cluster.local:7000"
	testAccountID        = "test-account-id"
)

func testInstanceInfo() InstanceInfo {
	return InstanceInfo{
		MetadataEndpoint: testMetadataEndpoint,
		AccountID:        testAccountID,
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

func makeSTS(engineName string, gen int, replicas int32, image string) *appsv1.StatefulSet { //nolint:unparam // engineName is always testEngineName in tests but kept as param for readability
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
				Spec: corev1.PodSpec{
					ServiceAccountName:            enginePodServiceAccountName(spec),
					NodeSelector:                  spec.NodeSelector,
					Tolerations:                   spec.Tolerations,
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
	spec := testSpec()
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
		t.Error("expected StatefulSet to be built on first Creating visit")
	}
	if result.EnsureConfigMap == nil {
		t.Error("expected ConfigMap to be built on first Creating visit")
	}
	if result.EnsureHeadlessSvc == nil {
		t.Error("expected headless Service to be built on first Creating visit")
	}
	if result.EnsureClusterSvc == nil {
		t.Error("expected cluster Service to be built on first Creating visit")
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
	spec := testSpec()
	spec.Image.Tag = "v2.0"
	status := stableStatus()
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/engine:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(testSpec(), testEngineName, testNamespace, 0, testInstanceInfo()),
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
	if result.EnsureConfigMap.Data["config.json"] != want.Data["config.json"] {
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
// returns the parsed config.json document plus the inner config block.
func renderConfig(t *testing.T, custom string) (root, cfg map[string]interface{}) {
	t.Helper()
	spec := testSpec()
	if custom != "" {
		spec.CustomEngineConfig = &apiextensionsv1.JSON{Raw: []byte(custom)}
	}
	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo())
	if err := json.Unmarshal([]byte(cm.Data["config.json"]), &root); err != nil {
		t.Fatalf("rendered config.json is not valid JSON: %v", err)
	}
	cfg, ok := root["config"].(map[string]interface{})
	if !ok {
		t.Fatalf("rendered config.json has no `config` object: %v", root)
	}
	return root, cfg
}

func TestBuildConfigMap_NoCustomConfig_DefaultsApplied(t *testing.T) {
	_, cfg := renderConfig(t, "")
	wants := map[string]interface{}{
		"account_name":              "default-account",
		"organization_name":         "default-org",
		"cluster_id":                "default-cluster",
		"multi_engine_mode_enabled": true,
		"logger_formatting":         "json",
		"account_id":                testAccountID,
		"engine_id":                 testEngineName,
		"engine_name":               testEngineName,
		"multi_engine_endpoint":     testMetadataEndpoint,
	}
	for k, want := range wants {
		if got := cfg[k]; got != want {
			t.Errorf("config[%q] = %v, want %v", k, got, want)
		}
	}
}

func TestBuildConfigMap_NestedConfigOverridesUserDefaults(t *testing.T) {
	custom := `{"config": {"account_name": "acme", "logger_formatting": "text", "extra": "x"}}`
	_, cfg := renderConfig(t, custom)
	if cfg["account_name"] != "acme" {
		t.Errorf("account_name = %v, want acme", cfg["account_name"])
	}
	if cfg["logger_formatting"] != "text" {
		t.Errorf("logger_formatting = %v, want text", cfg["logger_formatting"])
	}
	if cfg["extra"] != "x" {
		t.Errorf("config.extra = %v, want x", cfg["extra"])
	}
	if cfg["organization_name"] != "default-org" {
		t.Errorf("organization_name was clobbered: got %v, want default-org", cfg["organization_name"])
	}
}

func TestBuildConfigMap_RootKeysAddedAsSiblings(t *testing.T) {
	custom := `{"some_root_key": "value", "obj": {"a": 1}}`
	root, cfg := renderConfig(t, custom)
	if root["some_root_key"] != "value" {
		t.Errorf("root.some_root_key = %v, want value", root["some_root_key"])
	}
	obj, ok := root["obj"].(map[string]interface{})
	if !ok || obj["a"] != float64(1) {
		t.Errorf("root.obj = %v, want {a: 1}", root["obj"])
	}
	if cfg["account_name"] != "default-account" {
		t.Errorf("config.account_name was clobbered: got %v", cfg["account_name"])
	}
}

func TestBuildConfigMap_ProtectedConfigPathsStripped(t *testing.T) {
	custom := `{"config": {
		"account_id": "evil",
		"engine_id": "evil",
		"engine_name": "evil",
		"multi_engine_endpoint": "evil",
		"multi_engine_mode_enabled": false,
		"shutdown_wait_unfinished": 99999
	}}`
	_, cfg := renderConfig(t, custom)
	if cfg["account_id"] != testAccountID {
		t.Errorf("account_id = %v, want %v (operator-authoritative)", cfg["account_id"], testAccountID)
	}
	if cfg["engine_id"] != testEngineName {
		t.Errorf("engine_id = %v, want %v", cfg["engine_id"], testEngineName)
	}
	if cfg["engine_name"] != testEngineName {
		t.Errorf("engine_name = %v, want %v", cfg["engine_name"], testEngineName)
	}
	if cfg["multi_engine_endpoint"] != testMetadataEndpoint {
		t.Errorf("multi_engine_endpoint = %v, want %v", cfg["multi_engine_endpoint"], testMetadataEndpoint)
	}
	if v, ok := cfg["multi_engine_mode_enabled"].(bool); !ok || !v {
		t.Errorf("multi_engine_mode_enabled = %v, want true (operator-authoritative)", cfg["multi_engine_mode_enabled"])
	}
	if cfg["shutdown_wait_unfinished"] == float64(99999) {
		t.Error("shutdown_wait_unfinished was overridden by user input")
	}
}

func TestBuildConfigMap_RootNodesProtected(t *testing.T) {
	custom := `{"nodes": [{"host": "evil"}]}`
	root, _ := renderConfig(t, custom)
	nodes, ok := root["nodes"].([]interface{})
	if !ok {
		t.Fatalf("nodes = %v, want array", root["nodes"])
	}
	if len(nodes) != int(testSpec().Replicas) {
		t.Errorf("nodes length = %d, want %d (user input must not replace)", len(nodes), testSpec().Replicas)
	}
	first, ok := nodes[0].(map[string]interface{})
	if !ok || first["host"] == "evil" {
		t.Errorf("nodes[0] was overridden by user input: %v", nodes[0])
	}
}

func TestBuildConfigMap_NonMapConfigDropped(t *testing.T) {
	// When user supplies a scalar (or any non-object) for `config`, the
	// whole key must be stripped: a deep merge would otherwise replace
	// the operator-built config block wholesale with the scalar, losing
	// every authoritative key.
	cases := []string{
		`{"config": "evil"}`,
		`{"config": 42}`,
		`{"config": ["evil"]}`,
		`{"config": null}`,
	}
	for _, custom := range cases {
		t.Run(custom, func(t *testing.T) {
			root, cfg := renderConfig(t, custom)
			if _, ok := root["config"].(map[string]interface{}); !ok {
				t.Fatalf("rendered `config` is not an object: %v", root["config"])
			}
			if cfg["account_id"] != testAccountID {
				t.Errorf("operator-authoritative account_id lost: got %v", cfg["account_id"])
			}
			if cfg["engine_id"] != testEngineName {
				t.Errorf("operator-authoritative engine_id lost: got %v", cfg["engine_id"])
			}
			if cfg["account_name"] != "default-account" {
				t.Errorf("operator default account_name lost: got %v", cfg["account_name"])
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
	if err := json.Unmarshal([]byte(cm.Data["config.json"]), &root); err != nil {
		t.Fatalf("rendered config.json is not valid JSON: %v", err)
	}
	cfg := root["config"].(map[string]interface{})
	if cfg["account_name"] != "default-account" {
		t.Errorf("invalid customEngineConfig should be ignored, but defaults were touched: %v", cfg)
	}
}
