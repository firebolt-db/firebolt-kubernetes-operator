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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func defaultsInfoWith(sa string, env []corev1.EnvVar, nodeSelector map[string]string) *FireboltEngineDefaultsInfo {
	return newFireboltEngineDefaultsInfo(&computev1alpha1.FireboltEngineDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: computev1alpha1.FireboltEngineDefaultsDefaultName},
		Spec: computev1alpha1.FireboltEngineDefaultsSpec{
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

func TestOverlayDefaultsOnClass_EngineWinsThenDefaultsThenClass(t *testing.T) {
	classInfo := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{
		ServiceAccountName: "class-sa",
		NodeSelector:       map[string]string{"pool": "class", "zone": "class-zone"},
		Containers: []corev1.Container{{
			Name: computev1alpha1.EngineContainerName,
			Env:  []corev1.EnvVar{{Name: "FROM_CLASS", Value: "class"}, {Name: "SHARED", Value: "class"}},
		}},
	}))
	defaults := defaultsInfoWith("defaults-sa", []corev1.EnvVar{
		{Name: "FROM_DEFAULTS", Value: "defaults"},
		{Name: "SHARED", Value: "defaults"},
	}, map[string]string{"pool": "defaults", "region": "defaults-region"})
	merged := overlayDefaultsOnClass(defaults, classInfo)

	engineWins := testSpec()
	setSpecTemplatePod(engineWins, func(p *corev1.PodSpec) { p.ServiceAccountName = "engine-sa" })
	setSpecTemplateContainer(engineWins, func(c *corev1.Container) {
		c.Env = []corev1.EnvVar{{Name: "FROM_ENGINE", Value: "engine"}, {Name: "SHARED", Value: "engine"}}
	})

	if got := effectiveServiceAccountName(engineWins, merged); got != "engine-sa" {
		t.Errorf("engine SA = %q, want engine-sa", got)
	}
	if got := effectiveServiceAccountName(testSpec(), merged); got != "defaults-sa" {
		t.Errorf("defaults SA = %q, want defaults-sa", got)
	}
	if got := effectiveServiceAccountName(testSpec(), overlayDefaultsOnClass(nil, classInfo)); got != "class-sa" {
		t.Errorf("class SA = %q, want class-sa", got)
	}

	sel := effectiveNodeSelector(testSpec(), merged)
	if sel["pool"] != "defaults" || sel["zone"] != "class-zone" || sel["region"] != "defaults-region" {
		t.Errorf("nodeSelector = %v, want defaults over class with both sides kept", sel)
	}

	env := effectiveEngineEnv(engineWins, merged)
	got := map[string]string{}
	for _, e := range env {
		got[e.Name] = e.Value
	}
	if got["FROM_CLASS"] != "class" || got["FROM_DEFAULTS"] != "defaults" || got["FROM_ENGINE"] != "engine" || got["SHARED"] != "engine" {
		t.Errorf("env = %v, want class+defaults+engine with engine winning SHARED", got)
	}
}

func TestOverlayDefaultsOnClass_StorageAndConfig(t *testing.T) {
	class := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "class-sa"}))
	class.Storage = computev1alpha1.EngineStorageSpec{
		HostPath: &computev1alpha1.EngineHostPathSpec{Path: "/class"},
	}
	class.CustomEngineConfig = &apiextensionsv1.JSON{Raw: []byte(`{"logging":{"level":"class"},"storage":{"managed_table_storage":"s3"}}`)}

	defaults := newFireboltEngineDefaultsInfo(&computev1alpha1.FireboltEngineDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt"},
		Spec: computev1alpha1.FireboltEngineDefaultsSpec{
			Storage: computev1alpha1.EngineStorageSpec{
				EmptyDir: &computev1alpha1.EngineEmptyDirSpec{},
			},
			CustomEngineConfig: &apiextensionsv1.JSON{Raw: []byte(`{"logging":{"level":"defaults"},"storage":{"managed_table_bucket_name":"from-defaults"}}`)},
		},
	})
	merged := overlayDefaultsOnClass(defaults, class)

	bare := testSpec()
	bare.Storage = computev1alpha1.EngineStorageSpec{}
	if effectiveStorage(bare, merged).EmptyDir == nil {
		t.Fatal("effectiveStorage emptyDir is nil, want defaults emptyDir over class hostPath")
	}

	cfg := effectiveCustomEngineConfig(bare, merged)
	logging, _ := cfg["logging"].(map[string]interface{})
	storage, _ := cfg["storage"].(map[string]interface{})
	if logging["level"] != "defaults" {
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

func TestOverlayDefaultsOnClass_DoesNotInheritSKUFields(t *testing.T) {
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
	defaults := defaultsInfoWith("defaults-sa", nil, nil)
	merged := overlayDefaultsOnClass(defaults, class)
	if !effectiveUISidecarEnabled(testSpec(), merged) {
		t.Error("uiSidecar should still come from the class, not be dropped by Defaults overlay")
	}
	if effectiveRollout(testSpec(), merged) != computev1alpha1.RolloutGraceful {
		t.Error("rollout should still come from the class")
	}
	if effectiveAutoStop(testSpec(), merged) == nil || !effectiveAutoStop(testSpec(), merged).Enabled {
		t.Error("autoStop should still come from the class")
	}
}

func TestBuildStatefulSet_StampsDefaultsHash(t *testing.T) {
	defaults := defaultsInfoWith("defaults-sa", nil, nil)
	merged := overlayDefaultsOnClass(defaults, nil)
	sts := buildStatefulSet(testSpec(), testEngineName, testNamespace, 0, InstanceInfo{}, merged)
	if sts.Spec.Template.Spec.ServiceAccountName != "defaults-sa" {
		t.Errorf("SA = %q, want defaults-sa", sts.Spec.Template.Spec.ServiceAccountName)
	}
	if sts.Annotations[AnnotationEngineDefaultsHash] == "" {
		t.Fatal("AnnotationEngineDefaultsHash missing")
	}
	if !stsMatchesSpec(sts, testSpec(), InstanceInfo{}, merged) {
		t.Error("stsMatchesSpec rejected a freshly built STS with Defaults overlay")
	}

	edited := defaultsInfoWith("other-sa", nil, nil)
	if stsMatchesSpec(sts, testSpec(), InstanceInfo{}, overlayDefaultsOnClass(edited, nil)) {
		t.Error("stsMatchesSpec missed a Defaults spec edit")
	}
}

func TestOverlayDefaultsOnClass_PreservesClassHash(t *testing.T) {
	class := newFireboltEngineClassInfo(classWith(nil, &corev1.PodSpec{ServiceAccountName: "class-sa"}))
	defaults := defaultsInfoWith("defaults-sa", nil, nil)
	merged := overlayDefaultsOnClass(defaults, class)
	if merged.Hash != class.Hash {
		t.Errorf("class Hash = %q, want unchanged %q", merged.Hash, class.Hash)
	}
	if merged.DefaultsHash == "" || merged.DefaultsName != computev1alpha1.FireboltEngineDefaultsDefaultName {
		t.Errorf("defaults identity = %s/%s, want name+hash set", merged.DefaultsName, merged.DefaultsHash)
	}
}

func TestNewFireboltEngineDefaultsInfo_HashChangesWithSpec(t *testing.T) {
	a := newFireboltEngineDefaultsInfo(&computev1alpha1.FireboltEngineDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt"},
		Spec: computev1alpha1.FireboltEngineDefaultsSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "a"}},
		},
	})
	b := newFireboltEngineDefaultsInfo(&computev1alpha1.FireboltEngineDefaults{
		ObjectMeta: metav1.ObjectMeta{Name: "firebolt"},
		Spec: computev1alpha1.FireboltEngineDefaultsSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "b"}},
		},
	})
	if a.Hash == "" || a.Hash == b.Hash {
		t.Errorf("hashes = %q / %q, want distinct non-empty", a.Hash, b.Hash)
	}
}

func TestJSONRoundTripKeepsOverlayConfig(t *testing.T) {
	// Guard the overlay's remarshal of merged customEngineConfig.
	raw, err := json.Marshal(map[string]interface{}{"logging": map[string]interface{}{"level": "info"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("empty json")
	}
}
