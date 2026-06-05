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
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// classInfoWith builds a FireboltEngineClassInfo from a class whose spec is
// shaped by mutate. Exercises the same newFireboltEngineClassInfo entrypoint
// the reconciler uses, so the copied-in field set stays honest.
func classInfoWith(mutate func(*computev1alpha1.FireboltEngineClassSpec)) *FireboltEngineClassInfo {
	ec := &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-class"},
	}
	mutate(&ec.Spec)
	return newFireboltEngineClassInfo(ec)
}

func jsonRaw(s string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(s)}
}

func TestEffectiveStorage(t *testing.T) {
	enginePVC := &computev1alpha1.FireboltEngineSpec{
		Storage: computev1alpha1.EngineStorageSpec{
			PersistentVolumeClaim: &computev1alpha1.EnginePersistentVolumeClaimSpec{},
		},
	}
	engineEmptyDir := &computev1alpha1.FireboltEngineSpec{
		Storage: computev1alpha1.EngineStorageSpec{EmptyDir: &computev1alpha1.EngineEmptyDirSpec{}},
	}
	bareEngine := &computev1alpha1.FireboltEngineSpec{}

	classPVC := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.Storage = computev1alpha1.EngineStorageSpec{
			PersistentVolumeClaim: &computev1alpha1.EnginePersistentVolumeClaimSpec{},
		}
	})

	tests := []struct {
		name      string
		spec      *computev1alpha1.FireboltEngineSpec
		classInfo *FireboltEngineClassInfo
		want      StorageBackend
	}{
		{"engine backend wins over class", engineEmptyDir, classPVC, BackendEmptyDir},
		{"class backend used when engine omits", bareEngine, classPVC, BackendPersistentVolumeClaim},
		{"default emptyDir when neither sets a backend", bareEngine, nil, BackendEmptyDir},
		{"engine PVC with nil class", enginePVC, nil, BackendPersistentVolumeClaim},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveStorageBackend(effectiveStorage(tc.spec, tc.classInfo)); got != tc.want {
				t.Errorf("effectiveStorage backend = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveRollout(t *testing.T) {
	classRecreate := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.Rollout = computev1alpha1.RolloutRecreate
	})
	engineGraceful := &computev1alpha1.FireboltEngineSpec{Rollout: computev1alpha1.RolloutGraceful}
	bare := &computev1alpha1.FireboltEngineSpec{}

	tests := []struct {
		name      string
		spec      *computev1alpha1.FireboltEngineSpec
		classInfo *FireboltEngineClassInfo
		want      computev1alpha1.RolloutStrategy
	}{
		{"engine wins over class", engineGraceful, classRecreate, computev1alpha1.RolloutGraceful},
		{"class fills in when engine unset", bare, classRecreate, computev1alpha1.RolloutRecreate},
		{"operator default when neither set", bare, nil, computev1alpha1.RolloutGraceful},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveRollout(tc.spec, tc.classInfo); got != tc.want {
				t.Errorf("effectiveRollout = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveDrainCheckEnabled(t *testing.T) {
	classDisabled := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.DrainCheckEnabled = ptr(false)
	})
	engineEnabled := &computev1alpha1.FireboltEngineSpec{DrainCheckEnabled: ptr(true)}
	bare := &computev1alpha1.FireboltEngineSpec{}

	tests := []struct {
		name      string
		spec      *computev1alpha1.FireboltEngineSpec
		classInfo *FireboltEngineClassInfo
		want      bool
	}{
		{"engine wins over class", engineEnabled, classDisabled, true},
		{"class fills in when engine nil", bare, classDisabled, false},
		{"operator default (true) when neither set", bare, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveDrainCheckEnabled(tc.spec, tc.classInfo); got != tc.want {
				t.Errorf("effectiveDrainCheckEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetDrainCheckIntervalWithClass(t *testing.T) {
	classInterval := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.DrainCheckInterval = &metav1.Duration{Duration: 11 * time.Second}
	})
	engineInterval := &computev1alpha1.FireboltEngineSpec{
		DrainCheckInterval: &metav1.Duration{Duration: 7 * time.Second},
	}
	bare := &computev1alpha1.FireboltEngineSpec{}

	tests := []struct {
		name      string
		spec      *computev1alpha1.FireboltEngineSpec
		classInfo *FireboltEngineClassInfo
		want      time.Duration
	}{
		{"engine wins over class", engineInterval, classInterval, 7 * time.Second},
		{"class fills in when engine nil", bare, classInterval, 11 * time.Second},
		{"operator default when neither set", bare, nil, DefaultDrainCheckInterval},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := getDrainCheckInterval(tc.spec, tc.classInfo); got != tc.want {
				t.Errorf("getDrainCheckInterval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffectiveAutoscaling(t *testing.T) {
	classAS := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.Autoscaling = &computev1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 5}
	})
	engineAS := &computev1alpha1.FireboltEngineSpec{
		Autoscaling: &computev1alpha1.AutoscalingSpec{Enabled: true, MaxReplicas: 2},
	}
	bare := &computev1alpha1.FireboltEngineSpec{}

	if got := effectiveAutoscaling(engineAS, classAS); got == nil || got.MaxReplicas != 2 {
		t.Errorf("engine autoscaling should win whole-struct, got %+v", got)
	}
	if got := effectiveAutoscaling(bare, classAS); got == nil || got.MaxReplicas != 5 {
		t.Errorf("class autoscaling should fill in when engine unset, got %+v", got)
	}
	if got := effectiveAutoscaling(bare, nil); got != nil {
		t.Errorf("autoscaling should be nil (disabled) when neither set, got %+v", got)
	}
}

func TestEffectiveCustomEngineConfig_ClassUnderEngine(t *testing.T) {
	classInfo := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.CustomEngineConfig = jsonRaw(`{"logging":{"level":"warn","format":"text"}}`)
	})
	spec := &computev1alpha1.FireboltEngineSpec{
		CustomEngineConfig: jsonRaw(`{"logging":{"level":"debug"}}`),
	}

	merged := effectiveCustomEngineConfig(spec, classInfo)
	logging, ok := merged["logging"].(map[string]interface{})
	if !ok {
		t.Fatalf("merged config missing logging object: %v", merged)
	}
	// Engine wins on the shared key; class-only key survives the deep merge.
	if logging["level"] != "debug" {
		t.Errorf("logging.level = %v, want debug (engine wins)", logging["level"])
	}
	if logging["format"] != "text" {
		t.Errorf("logging.format = %v, want text (inherited from class)", logging["format"])
	}
}

func TestEffectiveCustomEngineConfig_StripsOperatorPathsFromBothLayers(t *testing.T) {
	classInfo := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.CustomEngineConfig = jsonRaw(`{"instance":{"id":"class-forged"},"logging":{"level":"warn"}}`)
	})
	spec := &computev1alpha1.FireboltEngineSpec{
		CustomEngineConfig: jsonRaw(`{"engine":{"id":"engine-forged"},"auth":{"mode":"none"}}`),
	}

	merged := effectiveCustomEngineConfig(spec, classInfo)
	if _, present := merged["instance"]; present {
		t.Error("operator-owned instance.* must be stripped from the class layer")
	}
	if eng, present := merged["engine"].(map[string]interface{}); present {
		if _, idSet := eng["id"]; idSet {
			t.Error("operator-owned engine.id must be stripped from the engine layer")
		}
	}
	// Non-owned keys from both layers survive.
	if logging, ok := merged["logging"].(map[string]interface{}); !ok || logging["level"] != "warn" {
		t.Errorf("class logging.level should survive, got %v", merged["logging"])
	}
	if auth, ok := merged["auth"].(map[string]interface{}); !ok || auth["mode"] != "none" {
		t.Errorf("engine auth.mode should survive, got %v", merged["auth"])
	}
}

func TestCustomEngineConfigHash_ReflectsClassLayer(t *testing.T) {
	bare := &computev1alpha1.FireboltEngineSpec{}
	classA := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.CustomEngineConfig = jsonRaw(`{"logging":{"format":"text"}}`)
	})
	classB := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.CustomEngineConfig = jsonRaw(`{"logging":{"format":"csv"}}`)
	})

	if customEngineConfigHash(bare, nil) != "" {
		t.Error("hash must be empty when neither side sets config")
	}
	hA := customEngineConfigHash(bare, classA)
	hB := customEngineConfigHash(bare, classB)
	if hA == "" {
		t.Fatal("class-only config must produce a non-empty hash so class drift is detected")
	}
	if hA == hB {
		t.Error("changing the class config must change the hash")
	}

	// A config that is operator-owned-only contributes nothing and must hash
	// to empty (no spurious generation).
	classOwnedOnly := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.CustomEngineConfig = jsonRaw(`{"instance":{"id":"x"}}`)
	})
	if customEngineConfigHash(bare, classOwnedOnly) != "" {
		t.Error("config containing only operator-owned paths must hash to empty")
	}

	// Equal merged result hashes identically regardless of which layer
	// supplied it (the rendered config is the same).
	engineSide := &computev1alpha1.FireboltEngineSpec{CustomEngineConfig: jsonRaw(`{"logging":{"format":"text"}}`)}
	if customEngineConfigHash(engineSide, nil) != hA {
		t.Error("identical effective config must hash identically across layers")
	}
}

func TestBuildConfigMap_MergesClassConfigUnderEngine(t *testing.T) {
	// Class sets logging.format; engine leaves it unset → class value wins
	// over the operator default ("json").
	classInfo := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.CustomEngineConfig = jsonRaw(`{"logging":{"format":"text"}}`)
	})
	spec := testSpec()
	cm := buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), classInfo)
	var root map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	logging, _ := root["logging"].(map[string]interface{})
	if logging["format"] != "text" {
		t.Errorf("logging.format = %v, want text (from class)", logging["format"])
	}

	// Engine config overrides the same key from the class.
	spec.CustomEngineConfig = jsonRaw(`{"logging":{"format":"csv"}}`)
	cm = buildConfigMap(spec, testEngineName, testNamespace, 0, testInstanceInfo(), classInfo)
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &root); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	logging, _ = root["logging"].(map[string]interface{})
	if logging["format"] != "csv" {
		t.Errorf("logging.format = %v, want csv (engine overrides class)", logging["format"])
	}
}

func TestBuildStatefulSet_InheritsClassStorageBackend(t *testing.T) {
	// Engine declares no storage backend; the class supplies a PVC, so the
	// rendered StatefulSet must carry a VolumeClaimTemplate.
	classInfo := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.Storage = computev1alpha1.EngineStorageSpec{
			PersistentVolumeClaim: &computev1alpha1.EnginePersistentVolumeClaimSpec{},
		}
	})
	engine := &computev1alpha1.FireboltEngineSpec{InstanceRef: "i", Replicas: 1}

	sts := buildStatefulSet(engine, testEngineName, testNamespace, 0, classInfo)
	if len(sts.Spec.VolumeClaimTemplates) != 1 {
		t.Fatalf("expected 1 VolumeClaimTemplate from inherited class PVC, got %d", len(sts.Spec.VolumeClaimTemplates))
	}
	if !storageMatchesSpec(sts, engine, classInfo) {
		t.Error("storageMatchesSpec must accept an STS rendered from the same effective (class) storage")
	}
	// A class that instead asks for emptyDir is drift against the PVC-backed STS.
	classEmptyDir := classInfoWith(func(s *computev1alpha1.FireboltEngineClassSpec) {
		s.Storage = computev1alpha1.EngineStorageSpec{EmptyDir: &computev1alpha1.EngineEmptyDirSpec{}}
	})
	if storageMatchesSpec(sts, engine, classEmptyDir) {
		t.Error("storageMatchesSpec must detect drift when the class storage backend changes")
	}
}
