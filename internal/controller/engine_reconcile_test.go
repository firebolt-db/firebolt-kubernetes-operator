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
			Repository: "firebolt/core",
			Tag:        "v1.0",
		},
		Resources: computev1alpha1.ResourceRequirements{
			CPU:    resource.MustParse("2"),
			Memory: resource.MustParse("8Gi"),
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
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeSelector:                  spec.NodeSelector,
					Tolerations:                   spec.Tolerations,
					TerminationGracePeriodSeconds: &defaultTGPS,
					Containers: []corev1.Container{
						{
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    spec.Resources.CPU,
									corev1.ResourceMemory: spec.Resources.Memory,
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    spec.Resources.CPU,
									corev1.ResourceMemory: spec.Resources.Memory,
								},
							},
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 1, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 1, 3, "firebolt/core:v1.0"),
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
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
	oldSTS := makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0")
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		DrainingSTS:         makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
	sts := makeSTS(testEngineName, 1, 3, "firebolt/core:v2.0")
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
	sts := makeSTS(testEngineName, 1, 3, "firebolt/core:v2.0")
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
		sts := makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0")
		fn(sts)
		return sts
	}

	tests := []struct {
		name  string
		sts   *appsv1.StatefulSet
		match bool
	}{
		{"matching", makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"), true},
		{"replica mismatch", makeSTS(testEngineName, 0, 5, "firebolt/core:v1.0"), false},
		{"image mismatch", makeSTS(testEngineName, 0, 3, "firebolt/core:v2.0"), false},
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 1, 0, "firebolt/core:v1.0"),
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
		DrainingSTS: makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
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
		CurrentSTS:              makeSTS(testEngineName, 1, 0, "firebolt/core:v1.0"),
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
