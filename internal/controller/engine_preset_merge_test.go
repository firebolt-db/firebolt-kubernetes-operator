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
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func presetInfoWith(sa string, env []corev1.EnvVar, nodeSelector map[string]string) *FireboltEnginePresetInfo {
	return newFireboltEnginePresetInfo(&computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: computev1alpha1.FireboltEnginePresetDefaultName},
		Spec: computev1alpha1.FireboltEnginePresetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: sa,
					NodeSelector:       nodeSelector,
					Containers: []corev1.Container{{
						Name: computev1alpha1.EngineContainerName,
						Env:  env,
					}},
				},
			},
		},
	})
}

func TestOverlayPresetOnClass_EngineWinsThenPresetThenClass(t *testing.T) {
	classInfo := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{
		ServiceAccountName: "class-sa",
		NodeSelector:       map[string]string{"pool": "class", "zone": "class-zone"},
		Containers: []corev1.Container{{
			Name: computev1alpha1.EngineContainerName,
			Env:  []corev1.EnvVar{{Name: "FROM_CLASS", Value: "class"}, {Name: "SHARED", Value: "class"}},
		}},
	}))
	defaults := presetInfoWith("preset-sa", []corev1.EnvVar{
		{Name: "FROM_DEFAULTS", Value: "preset"},
		{Name: "SHARED", Value: "preset"},
	}, map[string]string{"pool": "preset", "region": "defaults-region"})
	merged := overlayPresetOnClass(defaults, classInfo)

	engineWins := testSpec()
	setSpecTemplatePod(engineWins, func(p *corev1.PodSpec) { p.ServiceAccountName = "engine-sa" })
	setSpecTemplateContainer(engineWins, func(c *corev1.Container) {
		c.Env = []corev1.EnvVar{{Name: "FROM_ENGINE", Value: "engine"}, {Name: "SHARED", Value: "engine"}}
	})

	if got := effectiveServiceAccountName(engineWins, merged); got != "engine-sa" {
		t.Errorf("engine SA = %q, want engine-sa", got)
	}
	if got := effectiveServiceAccountName(testSpec(), merged); got != "preset-sa" {
		t.Errorf("defaults SA = %q, want preset-sa", got)
	}
	if got := effectiveServiceAccountName(testSpec(), overlayPresetOnClass(nil, classInfo)); got != "class-sa" {
		t.Errorf("class SA = %q, want class-sa", got)
	}

	classOff := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{AutomountServiceAccountToken: ptr(false)}))
	presetOn := newFireboltEnginePresetInfo(&computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: computev1alpha1.FireboltEnginePresetDefaultName},
		Spec: computev1alpha1.FireboltEnginePresetSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr(true)}},
		},
	})
	mergedAutomount := overlayPresetOnClass(presetOn, classOff)
	engineOff := testSpec()
	setSpecTemplatePod(engineOff, func(p *corev1.PodSpec) { p.AutomountServiceAccountToken = ptr(false) })
	if got := effectiveAutomountServiceAccountToken(engineOff, mergedAutomount); got == nil || *got {
		t.Errorf("engine automount = %v, want false", formatBoolPtr(got))
	}
	if got := effectiveAutomountServiceAccountToken(testSpec(), mergedAutomount); got == nil || !*got {
		t.Errorf("preset automount = %v, want true", formatBoolPtr(got))
	}
	if got := effectiveAutomountServiceAccountToken(testSpec(), overlayPresetOnClass(nil, classOff)); got == nil || *got {
		t.Errorf("class automount = %v, want false", formatBoolPtr(got))
	}

	sel := effectiveNodeSelector(testSpec(), merged)
	if sel["pool"] != "preset" || sel["zone"] != "class-zone" || sel["region"] != "defaults-region" {
		t.Errorf("nodeSelector = %v, want defaults over class with both sides kept", sel)
	}

	env := effectiveEngineEnv(engineWins, merged)
	got := map[string]string{}
	for _, e := range env {
		got[e.Name] = e.Value
	}
	if got["FROM_CLASS"] != "class" || got["FROM_DEFAULTS"] != "preset" || got["FROM_ENGINE"] != "engine" || got["SHARED"] != "engine" {
		t.Errorf("env = %v, want class+preset+engine with engine winning SHARED", got)
	}
}

func TestOverlayPresetOnClass_StorageAndConfig(t *testing.T) {
	class := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "class-sa"}))
	class.Storage = computev1alpha1.EngineStorageSpec{
		HostPath: &computev1alpha1.EngineHostPathSpec{Path: "/class"},
	}
	class.CustomEngineConfig = &apiextensionsv1.JSON{Raw: []byte(`{"logging":{"level":"class"},"storage":{"managed_table_storage":"s3"}}`)}

	defaults := newFireboltEnginePresetInfo(&computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt"},
		Spec: computev1alpha1.FireboltEnginePresetSpec{
			Storage: computev1alpha1.EngineStorageSpec{
				EmptyDir: &computev1alpha1.EngineEmptyDirSpec{},
			},
			CustomEngineConfig: &apiextensionsv1.JSON{Raw: []byte(`{"logging":{"level":"preset"},"storage":{"managed_table_bucket_name":"from-defaults"}}`)},
		},
	})
	merged := overlayPresetOnClass(defaults, class)

	bare := testSpec()
	bare.Storage = computev1alpha1.EngineStorageSpec{}
	if effectiveStorage(bare, merged).EmptyDir == nil {
		t.Fatal("effectiveStorage emptyDir is nil, want defaults emptyDir over class hostPath")
	}

	cfg := effectiveCustomEngineConfig(bare, merged)
	logging, _ := cfg["logging"].(map[string]interface{})
	storage, _ := cfg["storage"].(map[string]interface{})
	if logging["level"] != "preset" {
		t.Errorf("logging.level = %v, want defaults", logging["level"])
	}
	if storage["managed_table_storage"] != "s3" {
		t.Errorf("storage.managed_table_storage = %v, want s3 from class", storage["managed_table_storage"])
	}
	if storage["managed_table_bucket_name"] != "from-defaults" {
		t.Errorf("storage.managed_table_bucket_name = %v, want from-defaults", storage["managed_table_bucket_name"])
	}

	engineCfg := testSpec()
	engineCfg.CustomEngineConfig = &apiextensionsv1.JSON{Raw: []byte(`{"logging":{"level":"engine"}}`)}
	cfg = effectiveCustomEngineConfig(engineCfg, merged)
	logging, _ = cfg["logging"].(map[string]interface{})
	if logging["level"] != "engine" {
		t.Errorf("engine logging.level = %v, want engine", logging["level"])
	}
}

func TestOverlayPresetOnClass_DoesNotInheritSKUFields(t *testing.T) {
	graceful := computev1alpha1.RolloutGraceful
	class := newFireboltEngineClassInfo(&computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "sku"},
		Spec: computev1alpha1.FireboltEngineClassSpec{
			Template:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "class-sa"}},
			UISidecar: ptr(true),
			Rollout:   graceful,
			AutoStop:  &computev1alpha1.AutoStopSpec{Enabled: true, ActiveReplicas: 1},
		},
	})
	defaults := presetInfoWith("preset-sa", nil, nil)
	merged := overlayPresetOnClass(defaults, class)
	if !effectiveUISidecarEnabled(testSpec(), merged) {
		t.Error("uiSidecar should still come from the class, not be dropped by Preset overlay")
	}
	if effectiveRollout(testSpec(), merged) != computev1alpha1.RolloutGraceful {
		t.Error("rollout should still come from the class")
	}
	if effectiveAutoStop(testSpec(), merged) == nil || !effectiveAutoStop(testSpec(), merged).Enabled {
		t.Error("autoStop should still come from the class")
	}
}

func TestBuildStatefulSet_StampsPresetHash(t *testing.T) {
	defaults := presetInfoWith("preset-sa", nil, nil)
	merged := overlayPresetOnClass(defaults, nil)
	sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, InstanceInfo{}, merged)
	if sts.Spec.Template.Spec.ServiceAccountName != "preset-sa" {
		t.Errorf("SA = %q, want preset-sa", sts.Spec.Template.Spec.ServiceAccountName)
	}
	if sts.Annotations[AnnotationEnginePresetHash] == "" {
		t.Fatal("AnnotationEnginePresetHash missing")
	}
	if !stsMatchesSpec(sts, testSpec(), InstanceInfo{}, merged) {
		t.Error("stsMatchesSpec rejected a freshly built STS with Preset overlay")
	}

	edited := presetInfoWith("other-sa", nil, nil)
	if stsMatchesSpec(sts, testSpec(), InstanceInfo{}, overlayPresetOnClass(edited, nil)) {
		t.Error("stsMatchesSpec missed a Preset spec edit")
	}
}

func TestOverlayPresetOnClass_PreservesClassHash(t *testing.T) {
	class := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "class-sa"}))
	defaults := presetInfoWith("preset-sa", nil, nil)
	merged := overlayPresetOnClass(defaults, class)
	if merged.Hash != class.Hash {
		t.Errorf("class Hash = %q, want unchanged %q", merged.Hash, class.Hash)
	}
	if merged.PresetHash == "" || merged.PresetName != computev1alpha1.FireboltEnginePresetDefaultName {
		t.Errorf("defaults identity = %s/%s, want name+hash set", merged.PresetName, merged.PresetHash)
	}
}

func TestNewFireboltEnginePresetInfo_HashChangesWithSpec(t *testing.T) {
	a := newFireboltEnginePresetInfo(&computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt"},
		Spec: computev1alpha1.FireboltEnginePresetSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "a"}},
		},
	})
	b := newFireboltEnginePresetInfo(&computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt"},
		Spec: computev1alpha1.FireboltEnginePresetSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "b"}},
		},
	})
	if a.Hash == "" || a.Hash == b.Hash {
		t.Errorf("hashes = %q / %q, want distinct non-empty", a.Hash, b.Hash)
	}
}

func assertNamesInOrder(t *testing.T, field string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", field, got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", field, got, want)
			return
		}
	}
}

// TestOverlayPresetOnClass_ListMergeKeysAcrossTiers locks the merge
// order for the list-shaped fields at the Preset tier: class first,
// then Preset, then engine — the same concat rule the class layer
// uses for engine-over-class. Each field flows through a dedicated
// line in overlayPresetPodSpec / overlayPresetEngineContainer, so
// a dropped line loses the field with no other test noticing.
func TestOverlayPresetOnClass_ListMergeKeysAcrossTiers(t *testing.T) {
	secretEnvFrom := func(name string) corev1.EnvFromSource {
		return corev1.EnvFromSource{SecretRef: &corev1.SecretEnvSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: name},
		}}
	}
	classInfo := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{
		Tolerations:      []corev1.Toleration{{Key: "class-taint"}},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "class-pull"}},
		InitContainers:   []corev1.Container{{Name: "class-init"}},
		Containers: []corev1.Container{
			{Name: computev1alpha1.EngineContainerName, EnvFrom: []corev1.EnvFromSource{secretEnvFrom("class-creds")}},
			{Name: "class-sidecar"},
		},
	}))
	defaults := newFireboltEnginePresetInfo(&computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: computev1alpha1.FireboltEnginePresetDefaultName},
		Spec: computev1alpha1.FireboltEnginePresetSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Tolerations:      []corev1.Toleration{{Key: "defaults-taint"}},
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: "defaults-pull"}},
				InitContainers:   []corev1.Container{{Name: "defaults-init"}},
				Containers: []corev1.Container{
					{Name: computev1alpha1.EngineContainerName, EnvFrom: []corev1.EnvFromSource{secretEnvFrom("defaults-creds")}},
					{Name: "defaults-sidecar"},
				},
			}},
		},
	})
	merged := overlayPresetOnClass(defaults, classInfo)

	engine := testSpec()
	setSpecTemplatePod(engine, func(p *corev1.PodSpec) {
		p.Tolerations = []corev1.Toleration{{Key: "engine-taint"}}
		p.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "engine-pull"}}
		p.InitContainers = []corev1.Container{{Name: "engine-init"}}
		p.Containers = append(p.Containers, corev1.Container{Name: "engine-sidecar"})
	})
	setSpecTemplateContainer(engine, func(c *corev1.Container) {
		c.EnvFrom = []corev1.EnvFromSource{secretEnvFrom("engine-creds")}
	})

	var tolKeys []string
	for _, tol := range effectiveTolerations(engine, merged) {
		tolKeys = append(tolKeys, tol.Key)
	}
	assertNamesInOrder(t, "tolerations", tolKeys, "class-taint", "defaults-taint", "engine-taint")

	var pullNames []string
	for _, s := range effectiveImagePullSecrets(engine, merged) {
		pullNames = append(pullNames, s.Name)
	}
	assertNamesInOrder(t, "imagePullSecrets", pullNames, "class-pull", "defaults-pull", "engine-pull")

	var initNames []string
	for _, c := range effectiveInitContainers(engine, merged) {
		initNames = append(initNames, c.Name)
	}
	assertNamesInOrder(t, "initContainers", initNames, "class-init", "defaults-init", "engine-init")

	var sidecarNames []string
	for _, c := range effectiveSidecars(engine, merged) {
		sidecarNames = append(sidecarNames, c.Name)
	}
	assertNamesInOrder(t, "sidecars", sidecarNames, "class-sidecar", "defaults-sidecar", "engine-sidecar")

	var envFromNames []string
	for _, e := range effectiveEngineEnvFrom(engine, merged) {
		envFromNames = append(envFromNames, e.SecretRef.Name)
	}
	assertNamesInOrder(t, "envFrom", envFromNames, "class-creds", "defaults-creds", "engine-creds")
}

// TestOverlayPresetOnClass_EmptyPresetIsRenderInert pins the
// overlay/effective* lockstep: folding an empty Preset object over a
// class must not change anything the class contributed to the rendered
// StatefulSet. overlayPresetPodSpec rebuilds the merged template
// field-by-field, so a field the render path reads through an
// effective* helper but the overlay does not copy would silently drop
// the class value in every namespace that carries a Preset object.
// The class template here populates every field the overlay handles;
// the render comparison fails on the first field the overlay loses.
func TestOverlayPresetOnClass_EmptyPresetIsRenderInert(t *testing.T) {
	class := newFireboltEngineClassInfo(classWith(
		&metav1.ObjectMeta{
			Labels:      map[string]string{"team": "analytics"},
			Annotations: map[string]string{"class-note": "yes"},
		},
		&corev1.PodSpec{
			ServiceAccountName:           "class-sa",
			AutomountServiceAccountToken: ptr(false),
			NodeSelector:                 map[string]string{"pool": "class"},
			Tolerations:                  []corev1.Toleration{{Key: "class-taint"}},
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
					Weight: 1,
					Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: "zone", Operator: corev1.NodeSelectorOpExists},
					}},
				}},
			}},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone", WhenUnsatisfiable: corev1.DoNotSchedule,
			}},
			PriorityClassName: "class-priority",
			RuntimeClassName:  ptr("class-runtime"),
			DNSPolicy:         corev1.DNSClusterFirst,
			DNSConfig:         &corev1.PodDNSConfig{Nameservers: []string{"10.96.0.10"}},
			SchedulerName:     "class-scheduler",
			PreemptionPolicy:  ptr(corev1.PreemptLowerPriority),
			ReadinessGates:    []corev1.PodReadinessGate{{ConditionType: "example.com/gate"}},
			ResourceClaims:    []corev1.PodResourceClaim{{Name: "class-claim"}},
			HostAliases:       []corev1.HostAlias{{IP: "10.1.1.1", Hostnames: []string{"db.local"}}},
			OS:                &corev1.PodOS{Name: corev1.Linux},
			Overhead:          corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			ImagePullSecrets:  []corev1.LocalObjectReference{{Name: "class-pull"}},
			SecurityContext:   &corev1.PodSecurityContext{FSGroup: ptr(int64(2000))},
			InitContainers:    []corev1.Container{{Name: "class-init", Image: "docker.io/library/busybox:1"}},
			Volumes: []corev1.Volume{{
				Name:         "extra",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
			Containers: []corev1.Container{
				{
					Name:            computev1alpha1.EngineContainerName,
					Image:           "example.com/engine:v9",
					ImagePullPolicy: corev1.PullIfNotPresent,
					Env:             []corev1.EnvVar{{Name: "CLASS_ONLY", Value: "1"}},
					EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "class-creds"},
					}}},
					VolumeMounts:    []corev1.VolumeMount{{Name: "extra", MountPath: "/extra"}},
					SecurityContext: &corev1.SecurityContext{ReadOnlyRootFilesystem: ptr(true)},
					Lifecycle: &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{Command: []string{"sleep", "1"}},
					}},
					WorkingDir:               "/work",
					TerminationMessagePath:   "/tmp/term",
					TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
					VolumeDevices:            []corev1.VolumeDevice{{Name: "class-block", DevicePath: "/dev/class-block"}},
					ResizePolicy: []corev1.ContainerResizePolicy{{
						ResourceName: corev1.ResourceCPU, RestartPolicy: corev1.NotRequired,
					}},
				},
				{Name: "class-sidecar", Image: "docker.io/library/busybox:1"},
			},
		},
	))
	class.Storage = computev1alpha1.EngineStorageSpec{
		HostPath: &computev1alpha1.EngineHostPathSpec{Path: "/class"},
	}
	class.CustomEngineConfig = &apiextensionsv1.JSON{Raw: []byte(`{"logging":{"level":"class"}}`)}

	empty := newFireboltEnginePresetInfo(&computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: computev1alpha1.FireboltEnginePresetDefaultName},
	})
	merged := overlayPresetOnClass(empty, class)

	spec := testSpec()
	direct := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, class)
	overlaid := buildStatefulSet(spec, testEngineName, testNamespace, 0, InstanceInfo{}, merged)

	directJSON, err := json.Marshal(direct.Spec)
	if err != nil {
		t.Fatal(err)
	}
	overlaidJSON, err := json.Marshal(overlaid.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(directJSON, overlaidJSON) {
		t.Errorf("empty Preset overlay changed the rendered STS spec:\nwithout overlay: %s\nwith overlay:    %s", directJSON, overlaidJSON)
	}

	// The Preset hash annotation is the only allowed metadata delta.
	wantAnnotations := map[string]string{AnnotationEnginePresetHash: merged.PresetHash}
	for k, v := range direct.Annotations {
		wantAnnotations[k] = v
	}
	if !reflect.DeepEqual(overlaid.Annotations, wantAnnotations) {
		t.Errorf("STS annotations = %v, want %v", overlaid.Annotations, wantAnnotations)
	}
}
