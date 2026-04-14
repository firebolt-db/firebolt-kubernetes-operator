/*
Copyright 2025.

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
	"fmt"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	computev1alpha1 "github.com/firebolt-analytics/core-operator/api/v1alpha1"
)

const (
	testEngineName = "test-engine"
	testNamespace  = "default"
)

func testSpec() *computev1alpha1.FireboltEngineSpec {
	return &computev1alpha1.FireboltEngineSpec{
		Replicas: 3,
		Image: computev1alpha1.ImageSpec{
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

func stableStatus(activeGen int) *computev1alpha1.FireboltEngineStatus {
	return &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseStable,
		CurrentGeneration: activeGen,
		ActiveGeneration:  activeGen,
	}
}

func makeSTS(engineName string, gen int, replicas int32, image string) *appsv1.StatefulSet { //nolint:unparam
	spec := testSpec()
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
					NodeSelector: spec.NodeSelector,
					Tolerations:  spec.Tolerations,
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

func makeClusterSvc(engineName string, gen int) *corev1.Service { //nolint:unparam
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

func TestComputeEngineReconcile_S1_InitialCreation(t *testing.T) {
	spec := testSpec()
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:            computev1alpha1.PhaseStable,
		ActiveGeneration: -1,
	}
	current := EngineState{ClusterServiceTargetGen: -1}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected phase Creating, got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 0 {
		t.Errorf("expected generation 0, got %d", result.Status.CurrentGeneration)
	}
	if result.EnsureStatefulSet == nil {
		t.Fatal("expected StatefulSet to be created")
	}
	if result.EnsureConfigMap == nil {
		t.Fatal("expected ConfigMap to be created")
	}
	if result.EnsureHeadlessSvc == nil {
		t.Fatal("expected headless service to be created")
	}
	if !result.Requeue {
		t.Error("expected Requeue=true")
	}
}

// --- S2: Blue-green upgrade ---

func TestComputeEngineReconcile_S2_SpecChange(t *testing.T) {
	spec := testSpec()
	spec.Image.Tag = "v2.0"
	status := stableStatus(0)
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(testSpec(), testEngineName, testNamespace, 0),
		CurrentPodsReady:        true,
		CurrentPodCount:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected phase Creating, got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 1 {
		t.Errorf("expected generation 1, got %d", result.Status.CurrentGeneration)
	}
	if result.EnsureStatefulSet == nil {
		t.Fatal("expected new gen StatefulSet")
	}
	expectedImage := fmt.Sprintf("%s:%s", spec.Image.Repository, spec.Image.Tag)
	if result.EnsureStatefulSet.Spec.Template.Spec.Containers[0].Image != expectedImage {
		t.Errorf("expected image %s, got %s", expectedImage, result.EnsureStatefulSet.Spec.Template.Spec.Containers[0].Image)
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
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 1),
		CurrentPodsReady:        true,
		CurrentPodCount:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

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
		CurrentPodCount:         1,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

	if result.Status.Phase != computev1alpha1.PhaseCleaning {
		t.Errorf("expected phase Cleaning, got %s", result.Status.Phase)
	}
}

func TestComputeEngineReconcile_S3_DrainingRecreateStrategy(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	spec.Rollout = computev1alpha1.RolloutRecreate
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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

	if result.Status.Phase != computev1alpha1.PhaseCleaning {
		t.Errorf("expected phase Cleaning (recreate skips drain), got %s", result.Status.Phase)
	}
}

func TestComputeEngineReconcile_S3_DrainingSkippedWhenDisabled(t *testing.T) {
	drainingGen := 0
	spec := testSpec()
	disabled := false
	spec.DrainCheckEnabled = &disabled
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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

	if result.Status.Phase != computev1alpha1.PhaseCleaning {
		t.Errorf("expected phase Cleaning (drain check disabled), got %s", result.Status.Phase)
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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2)

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
	status := stableStatus(0)
	current := EngineState{
		CurrentSTS:              nil,
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0),
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected new transition (Creating), got %s", result.Status.Phase)
	}
	if result.Status.CurrentGeneration != 1 {
		t.Errorf("expected new generation 1, got %d", result.Status.CurrentGeneration)
	}
}

func TestComputeEngineReconcile_S5_ConfigMapDrift(t *testing.T) {
	spec := testSpec()
	status := stableStatus(0)
	driftedCM := buildConfigMap(spec, testEngineName, testNamespace, 0)
	driftedCM.Data["config.json"] = `{"nodes": []}`
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        driftedCM,
		CurrentPodsReady:        true,
		CurrentPodCount:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

	if result.Status.Phase != computev1alpha1.PhaseStable {
		t.Errorf("expected to stay Stable (in-place fix), got %s", result.Status.Phase)
	}
	if result.EnsureConfigMap == nil {
		t.Error("expected ConfigMap to be updated in-place")
	}
}

func TestComputeEngineReconcile_S5_ClusterSvcSelectorDrift(t *testing.T) {
	spec := testSpec()
	status := stableStatus(0)
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0),
		CurrentPodsReady:        true,
		CurrentPodCount:         3,
		ClusterService:          makeClusterSvc(testEngineName, 99),
		ClusterServiceTargetGen: 99,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

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
	status := stableStatus(0)
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0),
		CurrentPodsReady:        true,
		CurrentPodCount:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

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

// --- S8: Orphan cleanup ---

func TestComputeEngineReconcile_S8_OrphanCleanup(t *testing.T) {
	spec := testSpec()
	status := stableStatus(2)
	orphanSTS := makeSTS(testEngineName, 0, 3, "firebolt/core:old")
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 2, 3, "firebolt/core:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 2),
		CurrentPodsReady:        true,
		CurrentPodCount:         3,
		ClusterService:          makeClusterSvc(testEngineName, 2),
		ClusterServiceTargetGen: 2,
		OrphanedResources:       []client.Object{orphanSTS},
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

	found := false
	for _, obj := range result.DeleteResources {
		if obj.GetName() == orphanSTS.Name {
			found = true
		}
	}
	if !found {
		t.Error("expected orphaned STS to be in DeleteResources")
	}
}

// --- Idempotency (I1) ---

func TestComputeEngineReconcile_Idempotency(t *testing.T) {
	spec := testSpec()
	status := stableStatus(0)
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 0, 3, "firebolt/core:v1.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0),
		CurrentPodsReady:        true,
		CurrentPodCount:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	d1 := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)
	d2 := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3)

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
	current := EngineState{
		CurrentSTS:              makeSTS(testEngineName, 1, 3, "firebolt/core:v2.0"),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentPodsReady:        false,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3)

	if result.Status.Phase != computev1alpha1.PhaseCreating {
		t.Errorf("expected to stay in Creating, got %s", result.Status.Phase)
	}
	if result.EnsureStatefulSet == nil {
		t.Fatal("expected STS to be updated with new spec")
	}
	expectedImage := "firebolt/core:v3.0"
	if result.EnsureStatefulSet.Spec.Template.Spec.Containers[0].Image != expectedImage {
		t.Errorf("expected image %s, got %s", expectedImage, result.EnsureStatefulSet.Spec.Template.Spec.Containers[0].Image)
	}
	if result.Status.CurrentGeneration != 1 {
		t.Errorf("should not increment generation during creating, got %d", result.Status.CurrentGeneration)
	}
}
