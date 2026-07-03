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
	"k8s.io/apimachinery/pkg/util/intstr"
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
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: computev1alpha1.EngineContainerName,
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
				}},
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

// setSpecTemplateContainer mutates spec.Template (creating it as needed)
// so the engine container carries the provided customization. Tests
// that customize engine-container fields (resources, securityContext,
// env, ...) reach the spec.template shape through this helper.
func setSpecTemplateContainer(spec *computev1alpha1.FireboltEngineSpec, mutate func(*corev1.Container)) {
	if spec.Template == nil {
		spec.Template = &corev1.PodTemplateSpec{}
	}
	idx := -1
	for i := range spec.Template.Spec.Containers {
		if spec.Template.Spec.Containers[i].Name == computev1alpha1.EngineContainerName {
			idx = i
			break
		}
	}
	if idx < 0 {
		spec.Template.Spec.Containers = append(spec.Template.Spec.Containers,
			corev1.Container{Name: computev1alpha1.EngineContainerName})
		idx = len(spec.Template.Spec.Containers) - 1
	}
	mutate(&spec.Template.Spec.Containers[idx])
}

// setSpecTemplatePod mutates spec.Template.Spec (creating Template as
// needed). The previous flat fields (NodeSelector, Tolerations,
// Affinity, ServiceAccountName, etc.) now live on the pod-template
// pod spec; this helper centralizes the dispatch so the existing
// per-field tests do not need to duplicate the lazy-init dance.
func setSpecTemplatePod(spec *computev1alpha1.FireboltEngineSpec, mutate func(*corev1.PodSpec)) {
	if spec.Template == nil {
		spec.Template = &corev1.PodTemplateSpec{}
	}
	mutate(&spec.Template.Spec)
}

// setSpecTemplateMeta mutates spec.Template.ObjectMeta (creating
// Template as needed). Used by tests that previously set
// spec.PodLabels / spec.PodAnnotations.
func setSpecTemplateMeta(spec *computev1alpha1.FireboltEngineSpec, mutate func(*metav1.ObjectMeta)) {
	if spec.Template == nil {
		spec.Template = &corev1.PodTemplateSpec{}
	}
	mutate(&spec.Template.ObjectMeta)
}

func stableStatus() *computev1alpha1.FireboltEngineStatus {
	return &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseStable,
		CurrentGeneration: 0,
		ActiveGeneration:  0,
	}
}

// makeSTS returns the seed StatefulSet used by reconciler tests that
// need a "stable, in-sync with testSpec" fixture. It delegates to the
// real buildStatefulSet so every field stsMatchesSpec inspects is
// stamped consistently; a hand-rolled STS would silently fall out of
// sync whenever stsMatchesSpec grows a new comparison.
func makeSTS(engineName string, gen int, replicas int32) *appsv1.StatefulSet {
	spec := testSpec()
	sts := buildStatefulSet(spec, engineName, testNamespace, gen, InstanceInfo{}, nil)
	sts.Spec.Replicas = &replicas
	return sts
}

// makeEmptyDirSTS is the emptyDir sibling of makeSTS: produces an STS
// whose data volume is a pod-template emptyDir Volume rather than a
// VolumeClaimTemplate. Mirrors makeSTS's delegation to buildStatefulSet.
func makeEmptyDirSTS(engineName string, gen int, replicas int32) *appsv1.StatefulSet {
	spec := testSpec()
	spec.Storage = computev1alpha1.EngineStorageSpec{}
	sts := buildStatefulSet(spec, engineName, testNamespace, gen, InstanceInfo{}, nil)
	sts.Spec.Replicas = &replicas
	return sts
}

func TestBuildStatefulSetUISidecar(t *testing.T) {
	findContainer := func(sts *appsv1.StatefulSet, name string) *corev1.Container {
		for i := range sts.Spec.Template.Spec.Containers {
			if sts.Spec.Template.Spec.Containers[i].Name == name {
				return &sts.Spec.Template.Spec.Containers[i]
			}
		}
		return nil
	}
	countVolumes := func(sts *appsv1.StatefulSet, name string) int {
		var n int
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Name == name {
				n++
			}
		}
		return n
	}

	t.Run("absent by default", func(t *testing.T) {
		sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if findContainer(sts, computev1alpha1.EngineWebContainerName) != nil {
			t.Errorf("did not expect a %q container by default", computev1alpha1.EngineWebContainerName)
		}
		if countVolumes(sts, EngineWebWritableVolumeName) != 0 {
			t.Errorf("did not expect the %q volume by default", EngineWebWritableVolumeName)
		}
	})

	t.Run("injected when enabled", func(t *testing.T) {
		spec := testSpec()
		spec.UISidecar = ptr(true)
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)

		c := findContainer(sts, computev1alpha1.EngineWebContainerName)
		if c == nil {
			t.Fatalf("expected a %q container when uiSidecar=true", computev1alpha1.EngineWebContainerName)
		}
		if c.Image != DefaultEngineWebImage {
			t.Errorf("engine-web image = %q, want %q", c.Image, DefaultEngineWebImage)
		}
		if len(c.Ports) != 1 || c.Ports[0].ContainerPort != EngineWebPort || c.Ports[0].Name != EngineWebPortName {
			t.Errorf("engine-web ports = %+v, want one %q port %d", c.Ports, EngineWebPortName, EngineWebPort)
		}
		// The sidecar image is the mutable :latest alias, so the default pull
		// policy must be Always or nodes would pin the first :latest they cached.
		if c.ImagePullPolicy != corev1.PullAlways {
			t.Errorf("engine-web pull policy = %q, want %q by default", c.ImagePullPolicy, corev1.PullAlways)
		}
		if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
			t.Error("engine-web should run with a read-only root filesystem")
		}
		// The readiness probe is what keeps a crash-looping sidecar from
		// opening a transient all-ready window the promotion gate can catch.
		if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet == nil {
			t.Fatal("engine-web must carry an HTTP readiness probe")
		}
		if got := c.ReadinessProbe.HTTPGet.Port.IntValue(); got != int(EngineWebPort) {
			t.Errorf("engine-web readiness probe port = %d, want %d", got, EngineWebPort)
		}
		if countVolumes(sts, EngineWebWritableVolumeName) != 1 {
			t.Errorf("expected exactly one %q volume when uiSidecar=true", EngineWebWritableVolumeName)
		}
		// The engine container must remain present and first.
		if sts.Spec.Template.Spec.Containers[0].Name != computev1alpha1.EngineContainerName {
			t.Errorf("engine container should stay first, got %q", sts.Spec.Template.Spec.Containers[0].Name)
		}
	})

	t.Run("explicit engine pull policy overrides the Always default", func(t *testing.T) {
		spec := testSpec()
		spec.UISidecar = ptr(true)
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			for i := range p.Containers {
				if p.Containers[i].Name == computev1alpha1.EngineContainerName {
					p.Containers[i].ImagePullPolicy = corev1.PullIfNotPresent
				}
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		c := findContainer(sts, computev1alpha1.EngineWebContainerName)
		if c == nil {
			t.Fatalf("expected a %q container when uiSidecar=true", computev1alpha1.EngineWebContainerName)
		}
		if c.ImagePullPolicy != corev1.PullIfNotPresent {
			t.Errorf("engine-web pull policy = %q, want the engine's explicit %q", c.ImagePullPolicy, corev1.PullIfNotPresent)
		}
	})

	t.Run("no drift on its own output", func(t *testing.T) {
		// A freshly built UI-sidecar STS must match its own spec, or every
		// reconcile would roll a new blue-green generation. This load-bears on
		// buildStatefulSet and stsMatchesSpec sharing effectiveSidecarsWithUI
		// and on applyContainerAPIServerDefaults stamping.
		spec := testSpec()
		spec.UISidecar = ptr(true)
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec = false for a freshly built UI-sidecar STS (drift)")
		}
	})

	t.Run("no drift against an API-server-defaulted read-back", func(t *testing.T) {
		// Simulate what the API server stamps onto the stored StatefulSet
		// (probe successThreshold/timeout/period, HTTPGet scheme, ...): the
		// read-back must still compare equal to the freshly rendered spec,
		// or every reconcile would roll a new generation.
		spec := testSpec()
		spec.UISidecar = ptr(true)
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		for i := range sts.Spec.Template.Spec.Containers {
			c := &sts.Spec.Template.Spec.Containers[i]
			if c.ReadinessProbe != nil {
				c.ReadinessProbe.SuccessThreshold = 1
				if c.ReadinessProbe.HTTPGet != nil {
					c.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTP
				}
			}
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec = false after API-server probe defaulting (phantom drift)")
		}
	})

	t.Run("toggling is detected as drift", func(t *testing.T) {
		off := testSpec()
		on := testSpec()
		on.UISidecar = ptr(true)
		if stsMatchesSpec(buildStatefulSet(off, testEngineName, testNamespace, 0, InstanceInfo{}, nil), on, InstanceInfo{}, nil) {
			t.Error("enabling uiSidecar should be detected as drift")
		}
		if stsMatchesSpec(buildStatefulSet(on, testEngineName, testNamespace, 0, InstanceInfo{}, nil), off, InstanceInfo{}, nil) {
			t.Error("disabling uiSidecar should be detected as drift")
		}
	})

	t.Run("reserved writable volume is never duplicated", func(t *testing.T) {
		// nginx-writable-dir is operator-owned (operatorOwnedPodVolumeNames),
		// so a user/class volume of that name is dropped at render and only the
		// operator's emptyDir survives. (A user container named engine-web is
		// rejected by the webhook, so it can never reach buildStatefulSet — see
		// the webhook tests.)
		spec := testSpec()
		spec.UISidecar = ptr(true)
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{
				Name: EngineWebWritableVolumeName,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/shadow"},
				},
			})
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)

		if got := countVolumes(sts, EngineWebWritableVolumeName); got != 1 {
			t.Fatalf("expected exactly one %q volume, got %d", EngineWebWritableVolumeName, got)
		}
		for _, v := range sts.Spec.Template.Spec.Volumes {
			if v.Name == EngineWebWritableVolumeName && v.EmptyDir == nil {
				t.Error("the reserved engine-web volume should be the operator's emptyDir, not a user volume")
			}
		}
	})
}

// TestContainersEqualAfterDefaults_ProbeDefaulting pins the probe branch of
// applyContainerAPIServerDefaults: a user-supplied sidecar whose probe omits
// API-server-defaulted fields (successThreshold, timeouts, HTTPGet scheme)
// must compare equal to its stored read-back, or every reconcile would roll
// a new blue-green generation. A genuine probe difference must still drift.
func TestContainersEqualAfterDefaults_ProbeDefaulting(t *testing.T) {
	desired := corev1.Container{
		Name:  "metrics",
		Image: "metrics:v1",
		ReadinessProbe: &corev1.Probe{
			PeriodSeconds: 5,
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt(8080)},
			},
		},
	}

	stored := *desired.DeepCopy()
	// What the API server stamps at create time (SetDefaults_Probe +
	// SetDefaults_HTTPGetAction) on the fields the user left empty.
	stored.ReadinessProbe.TimeoutSeconds = 1
	stored.ReadinessProbe.SuccessThreshold = 1
	stored.ReadinessProbe.FailureThreshold = 3
	stored.ReadinessProbe.HTTPGet.Path = "/"
	stored.ReadinessProbe.HTTPGet.Scheme = corev1.URISchemeHTTP

	if !containersEqualAfterDefaults([]corev1.Container{stored}, []corev1.Container{desired}) {
		t.Error("API-server probe defaulting reads back as drift (phantom drift)")
	}

	changed := *desired.DeepCopy()
	changed.ReadinessProbe.PeriodSeconds = 30
	if containersEqualAfterDefaults([]corev1.Container{stored}, []corev1.Container{changed}) {
		t.Error("a real probe change must be detected as drift")
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
	seedSTS   func(engineName string, gen int, replicas int32) *appsv1.StatefulSet
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

			result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
	_ = computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)
}

// --- S2: Blue-green upgrade ---

func TestComputeEngineReconcile_S2_SpecChange(t *testing.T) {
	for _, sc := range storageBackendCases {
		t.Run(sc.name, func(t *testing.T) {
			spec := testSpec()
			sc.applySpec(spec)
			// Trigger pod-template drift via a spec field that lives on
			// FireboltEngineSpec. The image is no longer engine-owned, so
			// we use ServiceAccountName (also drift-affecting via
			// stsMatchesSpec) — now nested under spec.template.spec.
			setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.ServiceAccountName = "sa-v2" })
			// Build the seeded STS from a spec on the same backend so
			// the storage shape is consistent — the gen bump under
			// test must come from the spec change, not from a
			// backend mismatch the harness accidentally introduced.
			seedSpec := testSpec()
			sc.applySpec(seedSpec)
			status := stableStatus()
			current := EngineState{
				CurrentSTS:              sc.seedSTS(testEngineName, 0, 3),
				CurrentHeadlessSvc:      &corev1.Service{},
				CurrentConfigMap:        buildConfigMap(seedSpec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
				CurrentPodsReady:        true,
				CurrentPodTotal:         3,
				CurrentPodReady:         3,
				ClusterService:          makeClusterSvc(testEngineName, 0),
				ClusterServiceTargetGen: 0,
			}

			result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 1, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 1, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 1, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentPodsReady:        false,
		CurrentPodTotal:         1,
		CurrentPodReady:         0,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
		DrainingSTS:         makeSTS(testEngineName, 0, 3),
		DrainingPodsDrained: false,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		DrainingSTS:         makeSTS(testEngineName, 0, 3),
		DrainingPodsDrained: true,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		DrainingSTS:         makeSTS(testEngineName, 0, 3),
		DrainingPodsDrained: false,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
	oldSTS := makeSTS(testEngineName, 0, 3)
	oldSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: genResourceName(testEngineName, 0, SuffixHL)}}
	oldCM := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: genResourceName(testEngineName, 0, SuffixConfig)}}
	current := EngineState{
		DrainingSTS:         oldSTS,
		DrainingHeadlessSvc: oldSvc,
		DrainingConfigMap:   oldCM,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 99),
		ClusterServiceTargetGen: 99,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
				CurrentSTS:              sc.seedSTS(testEngineName, 0, 3),
				CurrentHeadlessSvc:      &corev1.Service{},
				CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
				CurrentPodsReady:        true,
				CurrentPodTotal:         3,
				CurrentPodReady:         3,
				ClusterService:          makeClusterSvc(testEngineName, 0),
				ClusterServiceTargetGen: 0,
			}

			result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	d1 := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)
	d2 := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
	setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.ServiceAccountName = "sa-v3" })
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:              computev1alpha1.PhaseDraining,
		CurrentGeneration:  1,
		ActiveGeneration:   1,
		DrainingGeneration: &drainingGen,
	}
	current := EngineState{
		DrainingSTS:         makeSTS(testEngineName, 0, 3),
		DrainingPodsDrained: false,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo(), nil)

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
	setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.ServiceAccountName = "sa-v3" })
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	sts := makeSTS(testEngineName, 1, 3)
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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentPodsReady:        false,
		CurrentPodTotal:         1,
		CurrentPodReady:         0,
		ClusterService:          nil,
		ClusterServiceTargetGen: -1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          nil,
		ClusterServiceTargetGen: -1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      nil,
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        nil,
		CurrentPodsReady:        true,
		CurrentPodTotal:         3,
		CurrentPodReady:         3,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
	want := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil)
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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
	setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.ServiceAccountName = "sa-v3" })
	status := &computev1alpha1.FireboltEngineStatus{
		Phase:             computev1alpha1.PhaseCreating,
		CurrentGeneration: 1,
		ActiveGeneration:  0,
	}
	sts := makeSTS(testEngineName, 1, 3)
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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo(), nil)

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
		sts := makeSTS(testEngineName, 0, 3)
		fn(sts)
		return sts
	}

	tests := []struct {
		name  string
		sts   *appsv1.StatefulSet
		match bool
	}{
		{"matching", makeSTS(testEngineName, 0, 3), true},
		{"replica mismatch", makeSTS(testEngineName, 0, 5), false},
		{"image mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.Containers[0].Image = "other/engine:tag"
		}), false},
		{"pull policy mismatch", mutate(func(s *appsv1.StatefulSet) {
			// PullNever differs from the effective default under both build
			// variants (dev resolves to Always, latest to IfNotPresent).
			s.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullNever
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
		{"init container mismatch", mutate(func(s *appsv1.StatefulSet) {
			s.Spec.Template.Spec.InitContainers = []corev1.Container{{
				Name:  "prep-disk",
				Image: "busybox:1.36",
			}}
		}), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stsMatchesSpec(tt.sts, spec, InstanceInfo{}, nil)
			if got != tt.match {
				t.Errorf("stsMatchesSpec() = %v, want %v", got, tt.match)
			}
		})
	}

	t.Run("no resources matches empty container resources", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplateContainer(customSpec, func(c *corev1.Container) {
			c.Resources = corev1.ResourceRequirements{}
		})
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
		if !stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for omitted resources")
		}
	})

	t.Run("requests only matches", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplateContainer(customSpec, func(c *corev1.Container) {
			c.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}
		})
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.Containers[0].Resources = engineContainerResources(customSpec)
		if !stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for requests-only resources")
		}
	})

	t.Run("limits only matches", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplateContainer(customSpec, func(c *corev1.Container) {
			c.Resources = corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			}
		})
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.Containers[0].Resources = engineContainerResources(customSpec)
		if !stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for limits-only resources")
		}
	})

	t.Run("partial requests and limits match", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplateContainer(customSpec, func(c *corev1.Container) {
			c.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("750m"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				},
			}
		})
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.Containers[0].Resources = engineContainerResources(customSpec)
		if !stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for partial resource requirements")
		}
	})

	t.Run("limit drift triggers mismatch", func(t *testing.T) {
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("16Gi")
		if stsMatchesSpec(sts, testSpec(), InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when a resource limit drifts")
		}
	})

	t.Run("removed resources trigger mismatch", func(t *testing.T) {
		customSpec := testSpec()
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
		if stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when resources are removed")
		}
	})

	t.Run("equivalent quantities match", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplateContainer(customSpec, func(c *corev1.Container) {
			c.Resources = corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			}
		})
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1024Mi"),
			},
		}
		if !stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for semantically equivalent quantities")
		}
	})

	t.Run("explicit serviceAccountName matches STS", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplatePod(customSpec, func(p *corev1.PodSpec) { p.ServiceAccountName = "custom-sa" })
		sts := makeSTS(testEngineName, 0, 3)
		sts.Spec.Template.Spec.ServiceAccountName = "custom-sa"
		if !stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for matching serviceAccountName")
		}
	})

	t.Run("pod security context drift triggers mismatch", func(t *testing.T) {
		sts := makeSTS(testEngineName, 0, 3)
		// Drop fsGroup to simulate an STS built before the default existed:
		// the spec resolves to fsGroup=3473, so the comparison must fail.
		sts.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{}
		if stsMatchesSpec(sts, testSpec(), InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when pod SecurityContext drifts")
		}
	})

	t.Run("container security context drift triggers mismatch", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplateContainer(customSpec, func(c *corev1.Container) {
			c.SecurityContext = &corev1.SecurityContext{
				AllowPrivilegeEscalation: boolPtr(true),
			}
		})
		// STS still has the old (nil) container SC, so a spec change must
		// not be silently absorbed by an in-place update.
		sts := makeSTS(testEngineName, 0, 3)
		if stsMatchesSpec(sts, customSpec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when spec.securityContext is added")
		}
	})

	t.Run("default engine container security context has read-only root filesystem", func(t *testing.T) {
		sc := defaultEngineContainerSecurityContext()
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Fatal("defaultEngineContainerSecurityContext() must set ReadOnlyRootFilesystem=true")
		}
	})

	t.Run("user-supplied podSecurityContext fields pass through and override fsGroup", func(t *testing.T) {
		customSpec := testSpec()
		userFSGroup := int64(9999)
		runAsGroup := int64(2000)
		setSpecTemplatePod(customSpec, func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				FSGroup:    &userFSGroup,
				RunAsGroup: &runAsGroup,
			}
		})
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
		setSpecTemplatePod(customSpec, func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				FSGroupChangePolicy: &always,
			}
		})
		got := getEnginePodSecurityContext(customSpec)
		if got.FSGroupChangePolicy == nil || *got.FSGroupChangePolicy != corev1.FSGroupChangeAlways {
			t.Fatalf("expected user FSGroupChangePolicy=Always to win, got %+v", got.FSGroupChangePolicy)
		}
	})

	t.Run("operator preserves user fields without clobbering them", func(t *testing.T) {
		customSpec := testSpec()
		setSpecTemplatePod(customSpec, func(p *corev1.PodSpec) {
			p.SecurityContext = &corev1.PodSecurityContext{
				Sysctls: []corev1.Sysctl{{Name: "net.core.somaxconn", Value: "1024"}},
			}
		})
		got := getEnginePodSecurityContext(customSpec)
		if len(got.Sysctls) != 1 || got.Sysctls[0].Name != "net.core.somaxconn" {
			t.Fatalf("expected user Sysctls pass-through, got %+v", got.Sysctls)
		}
		if got.FSGroup == nil || *got.FSGroup != DefaultEngineFSGroup {
			t.Fatalf("expected default FSGroup to fill in when user omitted it, got %+v", got.FSGroup)
		}
	})

	t.Run("getEngineContainerSecurityContext stamps the hardened default when unset", func(t *testing.T) {
		got := getEngineContainerSecurityContext(testSpec())
		if got == nil {
			t.Fatal("expected default container SecurityContext when unset, got nil")
		}
		if got.RunAsNonRoot == nil || !*got.RunAsNonRoot {
			t.Errorf("RunAsNonRoot = %v, want true (firebolt-instance-helm parity)", got.RunAsNonRoot)
		}
		if got.RunAsUser == nil || *got.RunAsUser != DefaultEngineWebD {
			t.Errorf("RunAsUser = %v, want %d", got.RunAsUser, DefaultEngineWebD)
		}
		if got.RunAsGroup == nil || *got.RunAsGroup != DefaultEngineGID {
			t.Errorf("RunAsGroup = %v, want %d", got.RunAsGroup, DefaultEngineGID)
		}
		if got.AllowPrivilegeEscalation == nil || *got.AllowPrivilegeEscalation {
			t.Errorf("AllowPrivilegeEscalation = %v, want false", got.AllowPrivilegeEscalation)
		}
		if got.Capabilities == nil || len(got.Capabilities.Drop) != 1 || got.Capabilities.Drop[0] != "ALL" {
			t.Errorf("Capabilities.Drop = %v, want [ALL]", got.Capabilities)
		}
	})

	t.Run("getEngineContainerSecurityContext passes spec value through unchanged", func(t *testing.T) {
		spec := testSpec()
		runAsUser := int64(2222)
		setSpecTemplateContainer(spec, func(c *corev1.Container) {
			c.SecurityContext = &corev1.SecurityContext{
				RunAsUser: &runAsUser,
			}
		})
		got := getEngineContainerSecurityContext(spec)
		if got == nil || got.RunAsUser == nil || *got.RunAsUser != 2222 {
			t.Errorf("RunAsUser = %v, want 2222 (spec wins wholesale)", got)
		}
		if got.Capabilities != nil {
			t.Errorf("Capabilities = %v, want nil (whole-struct ownership; defaults must not leak when spec is set)", got.Capabilities)
		}
	})
}

func TestBuildStatefulSet_Affinity(t *testing.T) {
	t.Run("nodeAffinity propagates to pod template", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.Affinity = &corev1.Affinity{
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
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
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
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.Affinity = &corev1.Affinity{
				PodAntiAffinity: &corev1.PodAntiAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
						TopologyKey: "kubernetes.io/hostname",
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "firebolt-engine"},
						},
					}},
				},
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
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
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.Affinity = nil })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if sts.Spec.Template.Spec.Affinity != nil {
			t.Fatalf("expected nil Affinity in pod template, got %+v", sts.Spec.Template.Spec.Affinity)
		}
	})

	t.Run("affinity drift detected by stsMatchesSpec", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.Affinity = &corev1.Affinity{
				PodAffinity: &corev1.PodAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
						TopologyKey: "topology.kubernetes.io/zone",
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"firebolt.io/engine": testEngineName},
						},
					}},
				},
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		// The STS matches spec because we built it from spec.
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for matching affinity")
		}
		// Remove affinity from spec — now the STS is ahead of spec and must be
		// detected as drifted.
		specNoAffinity := testSpec()
		if stsMatchesSpec(sts, specNoAffinity, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when spec affinity removed but STS still has it")
		}
	})

	t.Run("adding affinity to spec is detected as drift", func(t *testing.T) {
		// STS built without affinity.
		sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		// Spec now requires nodeAffinity — STS must be re-rolled.
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.Affinity = &corev1.Affinity{
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
		})
		if stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when affinity added to spec but STS has none")
		}
	})

	t.Run("rendered affinity is not aliased to spec", func(t *testing.T) {
		// Regression guard: effectiveAffinity must deep-copy like the
		// other effective* helpers. Mutating the rendered STS's
		// Affinity must not bleed back into the spec.
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.Affinity = &corev1.Affinity{
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
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)

		// Mutate the rendered STS's Affinity in place — must not reach
		// the spec's Affinity.
		sts.Spec.Template.Spec.Affinity.NodeAffinity.
			RequiredDuringSchedulingIgnoredDuringExecution.
			NodeSelectorTerms[0].MatchExpressions[0].Values[0] = "mutated"

		specVal := spec.Template.Spec.Affinity.NodeAffinity.
			RequiredDuringSchedulingIgnoredDuringExecution.
			NodeSelectorTerms[0].MatchExpressions[0].Values[0]
		if specVal != "engine" {
			t.Fatalf("aliasing: spec.Affinity was mutated via rendered STS, got %q want \"engine\"", specVal)
		}
	})
}

// TestBuildStatefulSet_HonorsExtraPodSpecFields covers the long tail
// of PodSpec fields the operator passes through from spec.template
// beyond the core carriers. Each
// sub-test sets one field on the engine template and asserts both:
// (a) the value lands on the rendered StatefulSet's pod template,
// (b) stsMatchesSpec sees it (no false drift) and detects removal
// (false-positive drift guard skipped — covered by the existing
// "drift detected" tests for the other carriers).
func TestBuildStatefulSet_HonorsExtraPodSpecFields(t *testing.T) {
	t.Run("topologySpreadConstraints", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
				MaxSkew:           1,
				TopologyKey:       "topology.kubernetes.io/zone",
				WhenUnsatisfiable: corev1.DoNotSchedule,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{LabelEngine: testEngineName}},
			}}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if len(sts.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
			t.Fatalf("topologySpreadConstraints not propagated: %+v", sts.Spec.Template.Spec.TopologySpreadConstraints)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching topologySpreadConstraints")
		}
	})

	t.Run("priorityClassName", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.PriorityClassName = "critical" })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if sts.Spec.Template.Spec.PriorityClassName != "critical" {
			t.Fatalf("PriorityClassName = %q, want critical", sts.Spec.Template.Spec.PriorityClassName)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching priorityClassName")
		}
		if stsMatchesSpec(sts, testSpec(), InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: failed to detect drift after priorityClassName removed from spec")
		}
	})

	t.Run("runtimeClassName", func(t *testing.T) {
		spec := testSpec()
		rc := "gvisor"
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.RuntimeClassName = &rc })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if sts.Spec.Template.Spec.RuntimeClassName == nil || *sts.Spec.Template.Spec.RuntimeClassName != "gvisor" {
			t.Fatalf("RuntimeClassName = %+v, want gvisor", sts.Spec.Template.Spec.RuntimeClassName)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching runtimeClassName")
		}
	})

	t.Run("dnsPolicy + dnsConfig", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.DNSPolicy = corev1.DNSClusterFirstWithHostNet
			p.DNSConfig = &corev1.PodDNSConfig{
				Nameservers: []string{"1.1.1.1"},
				Searches:    []string{"svc.cluster.local"},
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if sts.Spec.Template.Spec.DNSPolicy != corev1.DNSClusterFirstWithHostNet {
			t.Errorf("DNSPolicy = %q, want ClusterFirstWithHostNet", sts.Spec.Template.Spec.DNSPolicy)
		}
		if sts.Spec.Template.Spec.DNSConfig == nil || len(sts.Spec.Template.Spec.DNSConfig.Nameservers) != 1 {
			t.Errorf("DNSConfig = %+v, want nameservers", sts.Spec.Template.Spec.DNSConfig)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching dnsPolicy/dnsConfig")
		}
	})

	t.Run("schedulerName", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.SchedulerName = "custom-scheduler" })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if sts.Spec.Template.Spec.SchedulerName != "custom-scheduler" {
			t.Fatalf("SchedulerName = %q, want custom-scheduler", sts.Spec.Template.Spec.SchedulerName)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching schedulerName")
		}
	})

	t.Run("preemptionPolicy", func(t *testing.T) {
		spec := testSpec()
		p := corev1.PreemptNever
		setSpecTemplatePod(spec, func(s *corev1.PodSpec) { s.PreemptionPolicy = &p })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if sts.Spec.Template.Spec.PreemptionPolicy == nil || *sts.Spec.Template.Spec.PreemptionPolicy != corev1.PreemptNever {
			t.Fatalf("PreemptionPolicy = %+v, want Never", sts.Spec.Template.Spec.PreemptionPolicy)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching preemptionPolicy")
		}
	})

	t.Run("readinessGates", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: "example.com/Ready"}}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if len(sts.Spec.Template.Spec.ReadinessGates) != 1 {
			t.Fatalf("ReadinessGates = %+v, want 1", sts.Spec.Template.Spec.ReadinessGates)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching readinessGates")
		}
	})

	t.Run("hostAliases", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.HostAliases = []corev1.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"db.local"}}}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if len(sts.Spec.Template.Spec.HostAliases) != 1 || sts.Spec.Template.Spec.HostAliases[0].IP != "10.0.0.1" {
			t.Fatalf("HostAliases = %+v, want one entry", sts.Spec.Template.Spec.HostAliases)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching hostAliases")
		}
	})

	t.Run("pod OS", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.OS = &corev1.PodOS{Name: corev1.Linux} })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if sts.Spec.Template.Spec.OS == nil || sts.Spec.Template.Spec.OS.Name != corev1.Linux {
			t.Fatalf("OS = %+v, want linux", sts.Spec.Template.Spec.OS)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching OS")
		}
	})

	t.Run("overhead", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.Overhead = corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if got := sts.Spec.Template.Spec.Overhead[corev1.ResourceMemory]; got.String() != "128Mi" {
			t.Fatalf("Overhead[memory] = %s, want 128Mi", got.String())
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching overhead")
		}
		// Semantic equivalence: 128Mi == 131072Ki via Quantity.Cmp.
		sts.Spec.Template.Spec.Overhead = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("131072Ki")}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on semantically equivalent overhead quantity")
		}
	})

	t.Run("resourceClaims", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
			p.ResourceClaims = []corev1.PodResourceClaim{{Name: "gpu"}}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if len(sts.Spec.Template.Spec.ResourceClaims) != 1 || sts.Spec.Template.Spec.ResourceClaims[0].Name != "gpu" {
			t.Fatalf("ResourceClaims = %+v, want one entry", sts.Spec.Template.Spec.ResourceClaims)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching resourceClaims")
		}
	})
}

// TestStsMatchesSpec_TolerantOfPodSpecAPIServerDefaults pins the
// false-drift fix for DNSPolicy and SchedulerName: with neither set
// on spec.template, buildStatefulSet renders "" but the apiserver
// fills them with "ClusterFirst" / "default-scheduler" on read-back.
// Without the tolerant comparators, stsMatchesSpec would return false
// on its own output and the reconciler would roll a fresh blue-green
// generation on every loop forever (observed in CI as engine-g0 →
// engine-g41 STSes piling up before the test timeout).
func TestStsMatchesSpec_TolerantOfPodSpecAPIServerDefaults(t *testing.T) {
	spec := testSpec()
	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
	// Simulate what the apiserver stamps on every pod spec at create
	// time when the fields are left empty.
	sts.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	sts.Spec.Template.Spec.SchedulerName = corev1.DefaultSchedulerName
	if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
		t.Fatal("stsMatchesSpec: false drift on apiserver-defaulted DNSPolicy / SchedulerName — would loop the reconciler rolling fresh generations forever")
	}
}

// TestBuildStatefulSet_HonorsExtraEngineContainerFields covers the
// pass-through engine-container fields under
// spec.template.spec.containers[engine]. Each sub-test sets one field
// and asserts both that the rendered container carries it and that
// stsMatchesSpec sees the match (no false drift) and detects removal.
func TestBuildStatefulSet_HonorsExtraEngineContainerFields(t *testing.T) {
	t.Run("workingDir", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateContainer(spec, func(c *corev1.Container) { c.WorkingDir = "/tmp/firebolt" })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if got := sts.Spec.Template.Spec.Containers[0].WorkingDir; got != "/tmp/firebolt" {
			t.Fatalf("WorkingDir = %q, want /tmp/firebolt", got)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching workingDir")
		}
		if stsMatchesSpec(sts, testSpec(), InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: failed to detect drift when workingDir removed from spec")
		}
	})

	t.Run("terminationMessagePath", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateContainer(spec, func(c *corev1.Container) { c.TerminationMessagePath = "/var/log/term" })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if got := sts.Spec.Template.Spec.Containers[0].TerminationMessagePath; got != "/var/log/term" {
			t.Fatalf("TerminationMessagePath = %q, want /var/log/term", got)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching terminationMessagePath")
		}
	})

	t.Run("terminationMessagePath defaults tolerated", func(t *testing.T) {
		// Engine spec doesn't set the field. STS read-back has the
		// apiserver-defaulted /dev/termination-log. stsMatchesSpec must
		// not flag drift.
		spec := testSpec()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		sts.Spec.Template.Spec.Containers[0].TerminationMessagePath = "/dev/termination-log"
		sts.Spec.Template.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on apiserver-defaulted termination message fields")
		}
	})

	t.Run("terminationMessagePolicy", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateContainer(spec, func(c *corev1.Container) {
			c.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if got := sts.Spec.Template.Spec.Containers[0].TerminationMessagePolicy; got != corev1.TerminationMessageFallbackToLogsOnError {
			t.Fatalf("TerminationMessagePolicy = %q, want FallbackToLogsOnError", got)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching terminationMessagePolicy")
		}
	})

	t.Run("volumeDevices", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateContainer(spec, func(c *corev1.Container) {
			c.VolumeDevices = []corev1.VolumeDevice{{Name: "raw-disk", DevicePath: "/dev/xvdf"}}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if got := sts.Spec.Template.Spec.Containers[0].VolumeDevices; len(got) != 1 || got[0].Name != "raw-disk" {
			t.Fatalf("VolumeDevices = %+v, want one entry", got)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching volumeDevices")
		}
	})

	t.Run("resizePolicy", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateContainer(spec, func(c *corev1.Container) {
			c.ResizePolicy = []corev1.ContainerResizePolicy{{
				ResourceName:  corev1.ResourceCPU,
				RestartPolicy: corev1.NotRequired,
			}}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if got := sts.Spec.Template.Spec.Containers[0].ResizePolicy; len(got) != 1 || got[0].ResourceName != corev1.ResourceCPU {
			t.Fatalf("ResizePolicy = %+v, want one CPU entry", got)
		}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Error("stsMatchesSpec: false drift on matching resizePolicy")
		}
	})
}

func TestBuildStatefulSet_PodLabels(t *testing.T) {
	t.Run("base labels always present", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) { m.Labels = nil })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 2, InstanceInfo{}, nil)
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
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Labels = map[string]string{
				"app":         "packdb",
				"environment": "prod",
				"team":        "control-plane",
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 1, InstanceInfo{}, nil)
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
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Labels = map[string]string{
				LabelEngine:     "user-engine-override",
				LabelGeneration: "999",
				"custom":        "value",
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 5, InstanceInfo{}, nil)
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
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Labels = map[string]string{"version": "1.0"}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for matching pod labels")
		}
		// Remove label from spec — STS is now drifted.
		specNoLabels := testSpec()
		if stsMatchesSpec(sts, specNoLabels, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when spec pod labels removed but STS still has them")
		}
	})

	t.Run("adding pod labels to spec is detected as drift", func(t *testing.T) {
		// STS built without extra pod labels.
		sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		// Spec now has extra labels — STS must be re-rolled.
		spec := testSpec()
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Labels = map[string]string{"new-label": "new-value"}
		})
		if stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when pod labels added to spec but STS has none")
		}
	})

	t.Run("changing pod label value is detected as drift", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Labels = map[string]string{"version": "1.0"}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		// Change the label value.
		specChanged := testSpec()
		setSpecTemplateMeta(specChanged, func(m *metav1.ObjectMeta) {
			m.Labels = map[string]string{"version": "2.0"}
		})
		if stsMatchesSpec(sts, specChanged, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when pod label value changed")
		}
	})
}

func TestBuildStatefulSet_PodAnnotations(t *testing.T) {
	t.Run("nil pod annotations produce no pod template annotations", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) { m.Annotations = nil })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
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
		userAnnotations := map[string]string{
			"prometheus.io/scrape":        "true",
			"prometheus.io/port":          "9090",
			"firebolt.dev/cost-center":    "analytics",
			"karpenter.sh/do-not-disrupt": "true",
			"kubernetes.io/description":   "engine pod",
		}
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) { m.Annotations = userAnnotations })
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 1, InstanceInfo{}, nil)
		got := sts.Spec.Template.Annotations
		for k, want := range userAnnotations {
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
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Annotations = map[string]string{
				AnnotationMetadataOverride: "user-override-should-not-take-effect",
				"unrelated":                "value",
			}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)

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
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Annotations = map[string]string{"prometheus.io/scrape": "true"}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true for matching pod annotations")
		}
		specNoAnnotations := testSpec()
		if stsMatchesSpec(sts, specNoAnnotations, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when spec pod annotations removed but STS still has them")
		}
	})

	t.Run("adding pod annotations to spec is detected as drift", func(t *testing.T) {
		sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		spec := testSpec()
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Annotations = map[string]string{"new-annotation": "new-value"}
		})
		if stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when pod annotations added to spec but STS has none")
		}
	})

	t.Run("changing pod annotation value is detected as drift", func(t *testing.T) {
		spec := testSpec()
		setSpecTemplateMeta(spec, func(m *metav1.ObjectMeta) {
			m.Annotations = map[string]string{"version": "1.0"}
		})
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		specChanged := testSpec()
		setSpecTemplateMeta(specChanged, func(m *metav1.ObjectMeta) {
			m.Annotations = map[string]string{"version": "2.0"}
		})
		if stsMatchesSpec(sts, specChanged, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want false when pod annotation value changed")
		}
	})

	t.Run("empty map and nil are equal for stsMatchesSpec", func(t *testing.T) {
		// Round-tripping through the API server can normalise an
		// empty annotation map to nil (json:omitempty). Treat them
		// as identical so we don't churn StatefulSets on no-op
		// changes.
		spec := testSpec()
		sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		sts.Spec.Template.Annotations = map[string]string{}
		if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true when pod template annotations is empty map and spec.template.metadata.annotations is nil")
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
			Template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"k": "v"},
				},
			},
		}
		got := enginePodAnnotations(spec)
		got["mutated"] = "yes"
		if _, ok := spec.Template.Annotations["mutated"]; ok {
			t.Fatal("enginePodAnnotations must return a copy, not the underlying template annotations map")
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
	want := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("750m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("3Gi"),
		},
	}
	setSpecTemplateContainer(spec, func(c *corev1.Container) { c.Resources = want })

	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
	got := sts.Spec.Template.Spec.Containers[0].Resources
	if !resourceRequirementsEqual(got, want) {
		t.Fatalf("buildStatefulSet resources = %#v, want %#v", got, want)
	}
}

// Engine pods must not get the kubelet's legacy Docker-link env block —
// the FIREBOLT_* prefix is owned by the engine's own config schema and a
// future Service named `firebolt-*` could shadow a real config key.
// DNS is the only service-discovery channel the engine needs.
func TestBuildStatefulSet_DisablesServiceLinks(t *testing.T) {
	sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, InstanceInfo{}, nil)
	esl := sts.Spec.Template.Spec.EnableServiceLinks
	if esl == nil || *esl {
		t.Errorf("EnableServiceLinks: got %+v, want *false", esl)
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
		CurrentSTS:              makeSTS(testEngineName, 0, 3),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(testSpec(), testEngineName, testNamespace, 0, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		ClusterService:          makeClusterSvc(testEngineName, 0),
		ClusterServiceTargetGen: 0,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 1, 0),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 1, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		ClusterService:          makeClusterSvc(testEngineName, 1),
		ClusterServiceTargetGen: 1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo(), nil)

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

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 1, testInstanceInfo(), nil)

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
		DrainingSTS: makeSTS(testEngineName, 0, 3),
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 2, testInstanceInfo(), nil)

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
		CurrentSTS:              makeSTS(testEngineName, 1, 0),
		CurrentHeadlessSvc:      &corev1.Service{},
		CurrentConfigMap:        buildConfigMap(spec, testEngineName, testNamespace, 1, testInstanceInfo(), nil),
		CurrentPodsReady:        true,
		ClusterService:          makeClusterSvc(testEngineName, 1),
		ClusterServiceTargetGen: 1,
	}

	result := computeEngineReconcile(spec, status, current, testEngineName, testNamespace, 3, testInstanceInfo(), nil)

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
	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil)
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
	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), nil)
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	instance := root["instance"].(map[string]interface{})
	if instance["id"] != testInstanceID {
		t.Errorf("invalid customEngineConfig should be ignored, but defaults were touched: %v", instance)
	}
}

// TestBuildStatefulSet_DefaultImage pins the operator-default image as the
// only image source for the engine container. Per-engine image overrides
// were removed when FireboltEngineClass-based template merging became the single
// source of truth for the pod template's image. constants_test.go covers
// the resolveImageRef partial-override semantics that still apply to
// instance components (gateway, metadata) where the ImageSpec field
// remains.
func TestBuildStatefulSet_DefaultImage(t *testing.T) {
	spec := testSpec()
	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
	got := sts.Spec.Template.Spec.Containers[0].Image
	want := resolveImageRef(nil, DefaultEngineRepository, DefaultEngineTag)
	if got != want {
		t.Errorf("container image = %q, want %q (operator default)", got, want)
	}
	if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
		t.Error("stsMatchesSpec returned false for a freshly built STS")
	}
}

func TestBuildStatefulSet_InitContainers(t *testing.T) {
	spec := testSpec()
	setSpecTemplatePod(spec, func(p *corev1.PodSpec) {
		p.InitContainers = []corev1.Container{{
			Name:    "prep-disk",
			Image:   "busybox:1.36",
			Command: []string{"sh", "-c"},
			Args:    []string{"chown -R 3473:3473 /var/lib/firebolt"},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: boolPtrInt64(0),
			},
			VolumeMounts: []corev1.VolumeMount{{
				Name:      DataVolumeName,
				MountPath: DataMountPath,
			}},
		}}
	})

	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
	got := sts.Spec.Template.Spec.InitContainers
	if len(got) != 1 || got[0].Name != "prep-disk" {
		t.Fatalf("InitContainers = %+v, want prep-disk", got)
	}
	if got[0].VolumeMounts[0].MountPath != DataMountPath {
		t.Fatalf("volume mount path = %q, want %q", got[0].VolumeMounts[0].MountPath, DataMountPath)
	}
	if !stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
		t.Fatal("stsMatchesSpec() want true for matching init containers")
	}

	t.Run("API server defaults on read-back", func(t *testing.T) {
		specWithInit := testSpec()
		setSpecTemplatePod(specWithInit, func(p *corev1.PodSpec) {
			p.InitContainers = []corev1.Container{{
				Name:  "prep-disk",
				Image: "busybox:1.36",
			}}
		})
		stsWithInit := buildStatefulSet(specWithInit, testEngineName, testNamespace, 0, InstanceInfo{}, nil)
		// Simulate what the API server stamps on every container after create.
		stsWithInit.Spec.Template.Spec.InitContainers[0].ImagePullPolicy = corev1.PullIfNotPresent
		stsWithInit.Spec.Template.Spec.InitContainers[0].TerminationMessagePath = "/dev/termination-log"
		stsWithInit.Spec.Template.Spec.InitContainers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
		if !stsMatchesSpec(stsWithInit, specWithInit, InstanceInfo{}, nil) {
			t.Fatal("stsMatchesSpec() want true when init containers differ only by API defaults")
		}
	})

	setSpecTemplatePod(spec, func(p *corev1.PodSpec) { p.InitContainers = nil })
	if stsMatchesSpec(sts, spec, InstanceInfo{}, nil) {
		t.Fatal("stsMatchesSpec() want false when init containers removed from spec")
	}
}

func boolPtrInt64(v int64) *int64 {
	return &v
}

func TestEngineShutdownWaitSeconds(t *testing.T) {
	// Specific values, including the boundary that previously produced a
	// non-monotonic dip (TGPS=5 -> 4s but TGPS=6 -> 1s under the old logic).
	cases := map[int64]int64{
		1:   1,
		2:   1,
		5:   1,
		6:   1,
		7:   2,
		10:  5,
		60:  55,
		120: 115,
	}
	for tgps, want := range cases {
		if got := engineShutdownWaitSeconds(tgps); got != want {
			t.Errorf("engineShutdownWaitSeconds(%d) = %d, want %d", tgps, got, want)
		}
	}

	// Across a sweep the budget must stay within [1, gracePeriod] and be
	// monotonic non-decreasing, so raising the grace period never shrinks it.
	prev := int64(0)
	for tgps := int64(1); tgps <= 600; tgps++ {
		got := engineShutdownWaitSeconds(tgps)
		if got < 1 {
			t.Errorf("engineShutdownWaitSeconds(%d) = %d, want >= 1", tgps, got)
		}
		if got > tgps {
			t.Errorf("engineShutdownWaitSeconds(%d) = %d, exceeds grace period", tgps, got)
		}
		if got < prev {
			t.Errorf("engineShutdownWaitSeconds non-monotonic: tgps=%d gave %d after %d", tgps, got, prev)
		}
		prev = got
	}
}
