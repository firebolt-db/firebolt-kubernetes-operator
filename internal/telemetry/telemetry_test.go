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

package telemetry

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
)

type fakeLogger struct {
	enabled bool
	events  []map[string]any
}

func (f *fakeLogger) Enabled() bool { return f.enabled }

func (f *fakeLogger) LogEvent(payload map[string]any) error {
	f.events = append(f.events, payload)
	return nil
}

func TestTagOf(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/firebolt-db/engine:release-4.32.0":      "release-4.32.0",
		"ghcr.io/firebolt-db/engine":                     "",
		"engine:dev":                                     "dev",
		"localhost:5000/firebolt-db/engine:tag":          "tag",
		"localhost:5000/firebolt-db/engine":              "",
		"ghcr.io/firebolt-db/engine@sha256:abc123":       "",
		"ghcr.io/firebolt-db/engine:1.2.3@sha256:abc123": "1.2.3",
	}
	for input, want := range cases {
		if got := tagOf(input); got != want {
			t.Errorf("tagOf(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCountBucket(t *testing.T) {
	cases := []struct {
		count int
		want  string
	}{
		{-1, "0"}, {0, "0"}, {1, "1"}, {2, "2-4"}, {4, "2-4"},
		{5, "5-16"}, {16, "5-16"}, {17, "17+"}, {1000, "17+"},
	}
	for _, test := range cases {
		if got := countBucket(test.count); got != test.want {
			t.Errorf("countBucket(%d) = %q, want %q", test.count, got, test.want)
		}
	}
}

func engineWithImage(name, image string, replicas int32) computev1alpha1.FireboltEngine {
	engine := computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "firebolt"},
		Spec:       computev1alpha1.FireboltEngineSpec{Replicas: replicas},
	}
	if image != "" {
		engine.Spec.Template = templateWithImage(image)
	}
	return engine
}

func templateWithImage(image string) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  computev1alpha1.EngineContainerName,
				Image: image,
			}},
		},
	}
}

func classWithImage(name, image string) *computev1alpha1.FireboltEngineClass {
	return &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "firebolt"},
		Spec: computev1alpha1.FireboltEngineClassSpec{
			Template: *templateWithImage(image),
		},
	}
}

func engineWithClassRef(name, ref, image string) computev1alpha1.FireboltEngine {
	engine := engineWithImage(name, image, 1)
	engine.Spec.EngineClassRef = &ref
	return engine
}

func TestEngineTagPrecedence(t *testing.T) {
	classes := map[string]*computev1alpha1.FireboltEngineClass{
		"firebolt/pinned": classWithImage("pinned", "ghcr.io/firebolt-db/engine:release-class"),
	}

	tests := []struct {
		name   string
		engine computev1alpha1.FireboltEngine
		want   string
	}{
		{
			name:   "operator default",
			engine: engineWithImage("default", "", 1),
			want:   images.EngineTag,
		},
		{
			name:   "class image",
			engine: engineWithClassRef("class", "pinned", ""),
			want:   "release-class",
		},
		{
			name:   "engine override wins",
			engine: engineWithClassRef("override", "pinned", "ghcr.io/firebolt-db/engine:release-engine"),
			want:   "release-engine",
		},
		{
			name:   "missing class uses default",
			engine: engineWithClassRef("missing", "does-not-exist", ""),
			want:   images.EngineTag,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := engineTag(&test.engine, classes); got != test.want {
				t.Errorf("engineTag() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEngineVersionsDeduplicateAndSort(t *testing.T) {
	engines := []computev1alpha1.FireboltEngine{
		engineWithImage("a", "ghcr.io/firebolt-db/engine:release-4.32.0", 1),
		engineWithImage("b", "ghcr.io/firebolt-db/engine:release-4.31.0", 3),
		engineWithImage("c", "ghcr.io/firebolt-db/engine:release-4.32.0", 5),
	}
	if got, want := engineVersions(engines, nil), "release-4.31.0,release-4.32.0"; got != want {
		t.Errorf("engineVersions() = %q, want %q", got, want)
	}
}

func TestReplicaSizeBuckets(t *testing.T) {
	engines := []computev1alpha1.FireboltEngine{
		engineWithImage("a", "engine:1", 1),
		engineWithImage("b", "engine:1", 8),
		engineWithImage("c", "engine:1", 3),
		engineWithImage("d", "engine:1", 12),
	}
	if got, want := replicaSizeBuckets(engines), "1,2-4,5-16"; got != want {
		t.Errorf("replicaSizeBuckets() = %q, want %q", got, want)
	}
}

func newFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := computev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestCollectPayload(t *testing.T) {
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "firebolt"},
	}
	class := classWithImage("shared", "ghcr.io/firebolt-db/engine:release-4.40.0")
	engine1 := engineWithClassRef("e1", "shared", "")
	engine2 := engineWithImage("e2", "ghcr.io/firebolt-db/engine:release-4.40.0", 6)
	reporter := &Reporter{
		Client:          newFakeClient(t, instance, class, &engine1, &engine2),
		OperatorVersion: "v9.9.9",
	}

	payload, err := reporter.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	expected := map[string]string{
		"event":                 "operator_daily_summary",
		"operator_version":      "v9.9.9",
		"instance_count_bucket": "1",
		"engine_count_bucket":   "2-4",
		"engine_versions":       "release-4.40.0",
		"replica_size_buckets":  "1,5-16",
	}
	for key, want := range expected {
		if got, _ := payload[key].(string); got != want {
			t.Errorf("payload[%q] = %v, want %q", key, payload[key], want)
		}
	}

	for _, forbidden := range []string{"instance_id", "name", "names", "ip", "namespace"} {
		if _, exists := payload[forbidden]; exists {
			t.Errorf("payload contains forbidden identifying field %q", forbidden)
		}
	}
}

func TestReporterRequiresLeaderElection(t *testing.T) {
	if !(&Reporter{}).NeedLeaderElection() {
		t.Error("Reporter must require leader election")
	}
}

func TestStartDisabled(t *testing.T) {
	tests := []struct {
		name     string
		reporter *Reporter
	}{
		{
			name: "flag",
			reporter: &Reporter{
				Client:  newFakeClient(t),
				Enabled: false,
				logger:  &fakeLogger{enabled: true},
			},
		},
		{
			name: "environment",
			reporter: &Reporter{
				Client:  newFakeClient(t),
				Enabled: true,
				logger:  &fakeLogger{enabled: false},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.reporter.Start(context.Background()); err != nil {
				t.Fatalf("Start() returned error: %v", err)
			}
			logger := test.reporter.logger.(*fakeLogger)
			if len(logger.events) != 0 {
				t.Errorf("disabled reporter sent %d events, want 0", len(logger.events))
			}
		})
	}
}
