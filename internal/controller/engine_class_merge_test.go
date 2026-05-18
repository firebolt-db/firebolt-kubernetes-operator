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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// classWith returns an EngineClass whose spec.template carries the given
// pod-template metadata and spec values. The helper is intentionally
// permissive: callers compose minimal fixtures and pass them to
// newEngineClassInfo, which is the public entrypoint exercised by these
// tests.
func classWith(meta *metav1.ObjectMeta, spec *corev1.PodSpec) *computev1alpha1.EngineClass {
	tmplMeta := metav1.ObjectMeta{}
	if meta != nil {
		tmplMeta = *meta
	}
	tmplSpec := corev1.PodSpec{}
	if spec != nil {
		tmplSpec = *spec
	}
	return &computev1alpha1.EngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-optimized"},
		Spec: computev1alpha1.EngineClassSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: tmplMeta,
				Spec:       tmplSpec,
			},
		},
	}
}

func TestEffectiveServiceAccountName(t *testing.T) {
	classWithSA := newEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "class-sa"}))
	specWithSA := testSpec()
	sa := "engine-sa"
	specWithSA.ServiceAccountName = &sa

	tests := []struct {
		name      string
		spec      *computev1alpha1.FireboltEngineSpec
		classInfo *EngineClassInfo
		want      string
	}{
		{"engine wins over class", specWithSA, classWithSA, "engine-sa"},
		{"class fills in when engine empty", testSpec(), classWithSA, "class-sa"},
		{"empty when neither side sets it", testSpec(), nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveServiceAccountName(tc.spec, tc.classInfo)
			if got != tc.want {
				t.Errorf("effectiveServiceAccountName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveNodeSelector_MergesKeys(t *testing.T) {
	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{
		NodeSelector: map[string]string{"pool": "engine", "zone": "a"},
	}))
	spec := testSpec()
	spec.NodeSelector = map[string]string{"zone": "b", "gpu": "true"} // engine overrides "zone"

	got := effectiveNodeSelector(spec, classInfo)
	want := map[string]string{
		"pool": "engine",
		"zone": "b", // engine wins
		"gpu":  "true",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestEffectiveTolerations_ConcatenatesClassThenEngine(t *testing.T) {
	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{
		Tolerations: []corev1.Toleration{{Key: "from-class", Operator: corev1.TolerationOpExists}},
	}))
	spec := testSpec()
	spec.Tolerations = []corev1.Toleration{{Key: "from-engine", Operator: corev1.TolerationOpExists}}

	got := effectiveTolerations(spec, classInfo)
	if len(got) != 2 {
		t.Fatalf("got %d tolerations, want 2", len(got))
	}
	if got[0].Key != "from-class" {
		t.Errorf("got[0].Key = %q, want from-class", got[0].Key)
	}
	if got[1].Key != "from-engine" {
		t.Errorf("got[1].Key = %q, want from-engine", got[1].Key)
	}
}

func TestEffectivePodAnnotations_EngineWinsOnConflict(t *testing.T) {
	// kube2iam (legacy) is the IAM binding mechanism that uses a pod
	// annotation. IRSA's eks.amazonaws.com/role-arn lives on the
	// ServiceAccount, not the pod, so this fixture would teach the
	// wrong pattern.
	classInfo := newEngineClassInfo(classWith(
		&metav1.ObjectMeta{Annotations: map[string]string{
			"shared":                 "class",
			"iam.amazonaws.com/role": "arn:class",
		}},
		&corev1.PodSpec{},
	))
	spec := testSpec()
	spec.PodAnnotations = map[string]string{
		"shared":               "engine",
		"prometheus.io/scrape": "true",
	}

	got := effectivePodAnnotations(spec, classInfo)
	if got["shared"] != "engine" {
		t.Errorf("got[shared] = %q, want engine (engine wins on conflict)", got["shared"])
	}
	if got["iam.amazonaws.com/role"] != "arn:class" {
		t.Errorf("got[iam.amazonaws.com/role] = %q, want arn:class (class contributes when engine doesn't)", got["iam.amazonaws.com/role"])
	}
	if got["prometheus.io/scrape"] != "true" {
		t.Errorf("got[prometheus scrape] = %q, want true", got["prometheus.io/scrape"])
	}
}

func TestBuildStatefulSet_MergesClassTemplate(t *testing.T) {
	classInfo := newEngineClassInfo(classWith(
		&metav1.ObjectMeta{
			Labels: map[string]string{"team": "data"},
			// kube2iam-style pod annotation; IRSA's role-arn lives on
			// the ServiceAccount, not on the pod template.
			Annotations: map[string]string{"iam.amazonaws.com/role": "arn:aws:iam::1:role/x"},
		},
		&corev1.PodSpec{
			ServiceAccountName: "irsa-sa",
			NodeSelector:       map[string]string{"pool": "engine"},
			Containers: []corev1.Container{
				{Name: computev1alpha1.EngineContainerName, Image: "my/engine:v2"},
				{Name: "log-shipper", Image: "fluent/fluent-bit"},
			},
			InitContainers: []corev1.Container{
				{Name: "warmup", Image: "busybox", Command: []string{"sh", "-c", "echo warm"}},
			},
		},
	))
	spec := testSpec()

	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, classInfo)

	pod := sts.Spec.Template.Spec
	if pod.ServiceAccountName != "irsa-sa" {
		t.Errorf("ServiceAccountName = %q, want irsa-sa (from class)", pod.ServiceAccountName)
	}
	if pod.NodeSelector["pool"] != "engine" {
		t.Errorf("NodeSelector[pool] = %q, want engine (from class)", pod.NodeSelector["pool"])
	}
	if got := sts.Spec.Template.Annotations["iam.amazonaws.com/role"]; got != "arn:aws:iam::1:role/x" {
		t.Errorf("pod annotation iam.amazonaws.com/role = %q, want class value", got)
	}
	if got := sts.Spec.Template.Labels["team"]; got != "data" {
		t.Errorf("pod label team = %q, want data (from class)", got)
	}
	if len(pod.Containers) != 2 {
		t.Fatalf("expected 2 containers (engine + sidecar), got %d", len(pod.Containers))
	}
	if pod.Containers[0].Name != computev1alpha1.EngineContainerName {
		t.Errorf("container[0].Name = %q, want %q", pod.Containers[0].Name, computev1alpha1.EngineContainerName)
	}
	if pod.Containers[0].Image != "my/engine:v2" {
		t.Errorf("engine container image = %q, want my/engine:v2 (from class)", pod.Containers[0].Image)
	}
	if pod.Containers[1].Name != "log-shipper" {
		t.Errorf("container[1].Name = %q, want log-shipper", pod.Containers[1].Name)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != "warmup" {
		t.Errorf("InitContainers = %+v, want [warmup]", pod.InitContainers)
	}
	if sts.Annotations[AnnotationEngineClassHash] == "" {
		t.Error("STS annotation AnnotationEngineClassHash missing; class change drift won't be detected")
	}

	// stsMatchesSpec must accept its own buildStatefulSet output, otherwise
	// every reconcile would roll a fresh generation.
	if !stsMatchesSpec(sts, spec, classInfo) {
		t.Error("stsMatchesSpec returned false for a freshly built STS with classInfo")
	}
}

func TestStsMatchesSpec_ClassHashDrift(t *testing.T) {
	specA := testSpec()
	classA := newEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "sa-a"}))
	sts := buildStatefulSet(specA, testEngineName, testNamespace, 0, classA)

	// Class spec edited (different SA) → class hash changes. stsMatchesSpec
	// must report drift even though the engine spec is identical.
	classB := newEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "sa-b"}))
	if stsMatchesSpec(sts, specA, classB) {
		t.Error("stsMatchesSpec returned true after class edit; AnnotationEngineClassHash drift not detected")
	}
}

func TestStsMatchesSpec_ClassRemovedDrift(t *testing.T) {
	spec := testSpec()
	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "sa-x"}))
	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, classInfo)

	// engineClassRef cleared. The annotation is still on the STS but the
	// expected hash is "" — must surface as drift.
	if stsMatchesSpec(sts, spec, nil) {
		t.Error("stsMatchesSpec returned true after engineClassRef cleared; drift not detected")
	}
}

// TestEffectiveEngineResources_SpecWinsElseClass covers the precedence
// rule: a non-zero engine spec replaces the class wholesale; an empty
// spec lets the class fill in. Whole-struct ownership matters here —
// partial spec overrides must not silently inherit unrelated keys from
// the class.
func TestEffectiveEngineResources_SpecWinsElseClass(t *testing.T) {
	classRes := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
	}
	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: computev1alpha1.EngineContainerName, Resources: classRes},
		},
	}))

	t.Run("class fills in when spec empty", func(t *testing.T) {
		spec := testSpec()
		spec.Resources = corev1.ResourceRequirements{}
		got := effectiveEngineResources(spec, classInfo)
		if got.Requests.Cpu().String() != "4" {
			t.Errorf("CPU = %s, want 4 (from class)", got.Requests.Cpu().String())
		}
	})

	t.Run("spec wins wholesale when set", func(t *testing.T) {
		spec := testSpec() // testSpec carries 2cpu/8Gi in both requests and limits.
		got := effectiveEngineResources(spec, classInfo)
		if got.Requests.Cpu().String() != "2" {
			t.Errorf("CPU = %s, want 2 (engine spec wins entirely)", got.Requests.Cpu().String())
		}
		// Class set only Requests — spec sets both. Spec's Limits must come through.
		if got.Limits.Memory().String() != "8Gi" {
			t.Errorf("Memory limit = %s, want 8Gi (from spec, not inherited from class)", got.Limits.Memory().String())
		}
	})

	t.Run("zero when neither side sets it", func(t *testing.T) {
		spec := testSpec()
		spec.Resources = corev1.ResourceRequirements{}
		got := effectiveEngineResources(spec, nil)
		if len(got.Requests) != 0 || len(got.Limits) != 0 {
			t.Errorf("ResourceRequirements = %+v, want zero", got)
		}
	})
}

// TestBuildStatefulSet_MergesClassEngineContainerFields covers the
// class-only engine-container fields the validator accepts: env (non-
// reserved), envFrom, volumeMounts (beyond the operator-owned ones),
// and Lifecycle. The operator-injected env vars stay first; class env
// is appended; operator volumeMounts stay first and the class mount
// appears after.
func TestBuildStatefulSet_MergesClassEngineContainerFields(t *testing.T) {
	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name: computev1alpha1.EngineContainerName,
				Env: []corev1.EnvVar{
					{Name: "DATABASE_URL", Value: "postgres://shared"},
				},
				EnvFrom: []corev1.EnvFromSource{
					{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "shared-config"}}},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "shared-cache", MountPath: "/var/cache/shared"},
				},
				Lifecycle: &corev1.Lifecycle{
					PreStop: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "echo bye"}},
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{Name: "shared-cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}))

	spec := testSpec()
	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, classInfo)
	engine := sts.Spec.Template.Spec.Containers[0]

	// Operator env vars must come first; the class entry follows.
	if len(engine.Env) < 4 || engine.Env[len(engine.Env)-1].Name != "DATABASE_URL" {
		t.Errorf("engine Env = %+v, want operator vars then DATABASE_URL last", engine.Env)
	}
	for _, e := range engine.Env[:3] {
		if e.Name != computev1alpha1.EnginePodIndexEnvKey &&
			e.Name != computev1alpha1.EngineAllowAwsIrsaEnvKey &&
			e.Name != computev1alpha1.EngineCoreModeEnvKey {
			t.Errorf("operator env var displaced from leading position: %s", e.Name)
		}
	}

	if len(engine.EnvFrom) != 1 || engine.EnvFrom[0].ConfigMapRef == nil ||
		engine.EnvFrom[0].ConfigMapRef.Name != "shared-config" {
		t.Errorf("engine EnvFrom = %+v, want shared-config ConfigMap ref", engine.EnvFrom)
	}

	if engine.Lifecycle == nil || engine.Lifecycle.PreStop == nil {
		t.Error("engine Lifecycle missing class-supplied PreStop hook")
	}

	// Operator mounts (nodes-config + data) must remain at the head;
	// shared-cache appended after.
	if len(engine.VolumeMounts) < 3 {
		t.Fatalf("engine VolumeMounts = %+v, want operator mounts + class mount", engine.VolumeMounts)
	}
	if engine.VolumeMounts[0].Name != "nodes-config" || engine.VolumeMounts[1].Name != DataVolumeName {
		t.Errorf("operator volumeMounts displaced from leading position: %+v", engine.VolumeMounts[:2])
	}
	if engine.VolumeMounts[len(engine.VolumeMounts)-1].Name != "shared-cache" {
		t.Errorf("class volumeMount not appended at tail: %+v", engine.VolumeMounts)
	}

	// The pod must carry the matching pod-level volume so the kubelet
	// can resolve the sidecar mount; the operator's data + nodes-config
	// volumes stay at the head.
	volNames := make([]string, len(sts.Spec.Template.Spec.Volumes))
	for i := range sts.Spec.Template.Spec.Volumes {
		volNames[i] = sts.Spec.Template.Spec.Volumes[i].Name
	}
	found := false
	for _, n := range volNames {
		if n == "shared-cache" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("pod Volumes %v missing class-supplied shared-cache", volNames)
	}

	if !stsMatchesSpec(sts, spec, classInfo) {
		t.Error("stsMatchesSpec returned false for a freshly built STS with merged class fields")
	}
}

// TestBuildStatefulSet_AppendsClassImagePullSecrets covers the pod-level
// imagePullSecrets pass-through. Without it, class sidecars pulled from
// a private registry can't authenticate.
func TestBuildStatefulSet_AppendsClassImagePullSecrets(t *testing.T) {
	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{
		ImagePullSecrets: []corev1.LocalObjectReference{
			{Name: "ghcr-creds"},
			{Name: "ecr-creds"},
		},
	}))
	spec := testSpec()
	sts := buildStatefulSet(spec, testEngineName, testNamespace, 0, classInfo)

	got := sts.Spec.Template.Spec.ImagePullSecrets
	if len(got) != 2 || got[0].Name != "ghcr-creds" || got[1].Name != "ecr-creds" {
		t.Errorf("ImagePullSecrets = %+v, want [ghcr-creds, ecr-creds]", got)
	}
	if !stsMatchesSpec(sts, spec, classInfo) {
		t.Error("stsMatchesSpec returned false for STS with class imagePullSecrets")
	}
}

// TestAppendClassPodVolumes_OperatorReservedNamesWin covers the
// defense against a class redefining the operator-owned volume names
// (DataVolumeName, "nodes-config"): the operator entry must remain.
// Otherwise an EngineClass author could silently break engine startup
// or data persistence by collision.
func TestAppendClassPodVolumes_OperatorReservedNamesWin(t *testing.T) {
	operator := []corev1.Volume{
		{Name: "nodes-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "op-cfg"}}}},
		{Name: DataVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{
		Volumes: []corev1.Volume{
			// Names colliding with operator-owned volumes are dropped.
			{Name: "nodes-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "class-cfg"}}}},
			{Name: DataVolumeName, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/mnt"}}},
			{Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}))

	got := appendClassPodVolumes(operator, classInfo)
	if len(got) != 3 {
		t.Fatalf("got %d volumes, want 3 (2 operator + 1 non-colliding class)", len(got))
	}
	if got[0].ConfigMap == nil || got[0].ConfigMap.Name != "op-cfg" {
		t.Errorf("operator nodes-config replaced by class entry: %+v", got[0])
	}
	if got[1].EmptyDir == nil {
		t.Errorf("operator data volume replaced by class hostPath entry: %+v", got[1])
	}
	if got[2].Name != "extra" {
		t.Errorf("non-colliding class volume not appended: %+v", got[2])
	}
}

// TestSidecarsMatch_TolerantOfAPIServerDefaults pins down the fix for
// the read-back drift bug: the API server fills in imagePullPolicy /
// terminationMessagePath / terminationMessagePolicy on every sidecar
// at create time. sidecarsMatch must treat the defaulted "actual" form
// as equal to the user-supplied "expected" form (no defaults), or
// stsMatchesSpec would report drift and the engine reconciler would
// roll a fresh blue-green generation on every loop for any engine
// bound to a class that carries a sidecar.
func TestSidecarsMatch_TolerantOfAPIServerDefaults(t *testing.T) {
	// "expected" is the class-template form: image pinned to a tag, no
	// defaults stamped — that's how a user authors a sidecar.
	expected := []corev1.Container{{
		Name:  "log-shipper",
		Image: "fluent/fluent-bit:1.0",
	}}
	// "actual" is the same sidecar after the API server stamped its
	// standard defaults at create time.
	actual := []corev1.Container{{
		Name:                     "log-shipper",
		Image:                    "fluent/fluent-bit:1.0",
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}}
	if !sidecarsMatch(actual, expected) {
		t.Error("sidecarsMatch returned false after API-server defaults stamped on sidecar; would roll a new generation every reconcile")
	}
}

// TestEffectivePodSecurityContext_Precedence pins down spec > class >
// operator-default ordering on the pod-level securityContext, with the
// operator FSGroup default always stamped.
func TestEffectivePodSecurityContext_Precedence(t *testing.T) {
	classFsGroup := int64(7777)
	specFsGroup := int64(8888)
	runAsNonRoot := true

	classInfo := newEngineClassInfo(classWith(nil, &corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{
			FSGroup:      &classFsGroup,
			RunAsNonRoot: &runAsNonRoot,
		},
	}))

	t.Run("spec wins wholesale when set", func(t *testing.T) {
		spec := testSpec()
		spec.PodSecurityContext = &corev1.PodSecurityContext{
			FSGroup: &specFsGroup,
		}
		got := effectivePodSecurityContext(spec, classInfo)
		if got.FSGroup == nil || *got.FSGroup != specFsGroup {
			t.Errorf("FSGroup = %v, want %d (engine spec wins)", got.FSGroup, specFsGroup)
		}
		if got.RunAsNonRoot != nil {
			t.Errorf("RunAsNonRoot = %v, want nil (whole-struct ownership; class field must not leak)", got.RunAsNonRoot)
		}
	})

	t.Run("class fills in when spec unset", func(t *testing.T) {
		spec := testSpec()
		got := effectivePodSecurityContext(spec, classInfo)
		if got.FSGroup == nil || *got.FSGroup != classFsGroup {
			t.Errorf("FSGroup = %v, want %d (from class)", got.FSGroup, classFsGroup)
		}
		if got.RunAsNonRoot == nil || !*got.RunAsNonRoot {
			t.Errorf("RunAsNonRoot = %v, want true (from class)", got.RunAsNonRoot)
		}
	})

	t.Run("operator default applied when neither side sets it", func(t *testing.T) {
		spec := testSpec()
		got := effectivePodSecurityContext(spec, nil)
		if got.FSGroup == nil || *got.FSGroup != DefaultEngineFSGroup {
			t.Errorf("FSGroup = %v, want operator default %d", got.FSGroup, DefaultEngineFSGroup)
		}
		if got.FSGroupChangePolicy == nil {
			t.Error("FSGroupChangePolicy missing — operator default not applied")
		}
	})
}
