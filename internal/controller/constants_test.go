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

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
)

func TestUseDirectEngineRepositoryPreservesUserOverride(t *testing.T) {
	originalRepository := DefaultEngineRepository
	originalImage := DefaultEngineImage
	t.Cleanup(func() {
		DefaultEngineRepository = originalRepository
		DefaultEngineImage = originalImage
	})

	UseDirectEngineRepository()

	if DefaultEngineRepository != images.DirectEngineRepository {
		t.Errorf("DefaultEngineRepository = %q, want %q", DefaultEngineRepository, images.DirectEngineRepository)
	}
	if got, want := DefaultEngineImage, images.DirectEngineRepository+":"+DefaultEngineTag; got != want {
		t.Errorf("DefaultEngineImage = %q, want %q", got, want)
	}
	override := &computev1alpha1.ImageSpec{Repository: "mirror.example.com/engine"}
	if got, want := resolveImageRef(override, DefaultEngineRepository, DefaultEngineTag), "mirror.example.com/engine:"+DefaultEngineTag; got != want {
		t.Errorf("user repository override = %q, want %q", got, want)
	}
}

// TestDisableEngineTelemetryInjectsDoNotTrack pins the engine-pod opt-out
// behavior: DO_NOT_TRACK=1 is stamped only after DisableEngineTelemetry(),
// it lands before user-supplied env (so a user value wins via Kubernetes'
// last-occurrence duplicate resolution), and the default rendering stays
// untouched.
func TestDisableEngineTelemetryInjectsDoNotTrack(t *testing.T) {
	t.Cleanup(func() { engineTelemetryDisabled = false })

	findDoNotTrack := func(env []corev1.EnvVar) []int {
		var idx []int
		for i, e := range env {
			if e.Name == EngineDoNotTrackEnvKey {
				idx = append(idx, i)
			}
		}
		return idx
	}

	engineTelemetryDisabled = false
	if got := findDoNotTrack(buildEngineContainerEnv(&computev1alpha1.FireboltEngineSpec{}, nil)); len(got) != 0 {
		t.Errorf("telemetry enabled: DO_NOT_TRACK unexpectedly injected at %v", got)
	}

	DisableEngineTelemetry()
	env := buildEngineContainerEnv(&computev1alpha1.FireboltEngineSpec{}, nil)
	idx := findDoNotTrack(env)
	if len(idx) != 1 {
		t.Fatalf("telemetry disabled: DO_NOT_TRACK occurrences = %d, want 1", len(idx))
	}
	if env[idx[0]].Value != "1" {
		t.Errorf("DO_NOT_TRACK = %q, want %q", env[idx[0]].Value, "1")
	}

	// User env is appended after operator env, so a user-supplied value
	// resolves last and wins.
	spec := &computev1alpha1.FireboltEngineSpec{
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: computev1alpha1.EngineContainerName,
					Env:  []corev1.EnvVar{{Name: EngineDoNotTrackEnvKey, Value: "0"}},
				}},
			},
		},
	}
	env = buildEngineContainerEnv(spec, nil)
	idx = findDoNotTrack(env)
	if len(idx) != 2 {
		t.Fatalf("with user override: DO_NOT_TRACK occurrences = %d, want 2 (operator then user)", len(idx))
	}
	if last := env[idx[len(idx)-1]]; last.Value != "0" {
		t.Errorf("last DO_NOT_TRACK = %q, want user value %q", last.Value, "0")
	}
}

// TestResolveImageRef pins the partial-override semantics that make
// ImageSpec.Repository and ImageSpec.Tag independently optional. Each
// dimension must fall back to its component default on its own so users
// can pull from a mirror without restating the tag (or pin a tag without
// restating the repository).
func TestResolveImageRef(t *testing.T) {
	const (
		defaultRepo = "ghcr.io/firebolt-db/engine"
		defaultTag  = "v9.9.9"
	)

	tests := []struct {
		name string
		spec *computev1alpha1.ImageSpec
		want string
	}{
		{
			name: "nil spec returns default reference",
			spec: nil,
			want: defaultRepo + ":" + defaultTag,
		},
		{
			name: "empty spec falls back to both defaults",
			spec: &computev1alpha1.ImageSpec{},
			want: defaultRepo + ":" + defaultTag,
		},
		{
			name: "repository-only override keeps default tag",
			spec: &computev1alpha1.ImageSpec{Repository: "mirror.example.com/engine"},
			want: "mirror.example.com/engine:" + defaultTag,
		},
		{
			name: "tag-only override keeps default repository",
			spec: &computev1alpha1.ImageSpec{Tag: "v1.2.3"},
			want: defaultRepo + ":v1.2.3",
		},
		{
			name: "both fields override completely",
			spec: &computev1alpha1.ImageSpec{
				Repository: "mirror.example.com/engine",
				Tag:        "v1.2.3",
			},
			want: "mirror.example.com/engine:v1.2.3",
		},
		{
			name: "pullPolicy alone does not affect repo/tag",
			spec: &computev1alpha1.ImageSpec{PullPolicy: corev1.PullAlways},
			want: defaultRepo + ":" + defaultTag,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveImageRef(tc.spec, defaultRepo, defaultTag)
			if got != tc.want {
				t.Errorf("resolveImageRef() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveWorkloadImagePullPolicy checks the default pull policy rule for
// the engine and metadata images: the Kubernetes tag-based default, with the
// mutable "dev" alias treated like ":latest".
func TestResolveWorkloadImagePullPolicy(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  corev1.PullPolicy
	}{
		{"dev tag", "ghcr.io/firebolt-db/engine:dev", corev1.PullAlways},
		{"latest tag", "ghcr.io/firebolt-db/engine:latest", corev1.PullAlways},
		{"no tag", "ghcr.io/firebolt-db/engine", corev1.PullAlways},
		{"pinned tag", "ghcr.io/firebolt-db/engine:release-5.0.1", corev1.PullIfNotPresent},
		{"dev-prefixed tag is not dev", "ghcr.io/firebolt-db/engine:dev-uptest", corev1.PullIfNotPresent},
		{"digest", "ghcr.io/firebolt-db/engine@sha256:0000000000000000000000000000000000000000000000000000000000000000", corev1.PullIfNotPresent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveWorkloadImagePullPolicy(tc.image)
			if got != tc.want {
				t.Errorf("resolveWorkloadImagePullPolicy(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
}

func TestResolveContainerImagePullPolicy(t *testing.T) {
	tests := []struct {
		name   string
		image  string
		policy corev1.PullPolicy
		want   corev1.PullPolicy
	}{
		{"explicit policy", "busybox:1.36", corev1.PullAlways, corev1.PullAlways},
		{"tagged image defaults IfNotPresent", "busybox:1.36", "", corev1.PullIfNotPresent},
		{":latest defaults Always", "busybox:latest", "", corev1.PullAlways},
		{"untagged defaults Always", "busybox", "", corev1.PullAlways},
		{"registry port without tag defaults Always", "myregistry:5000/myimage", "", corev1.PullAlways},
		{"registry port with tag defaults IfNotPresent", "myregistry:5000/myimage:v1", "", corev1.PullIfNotPresent},
		{"digest defaults IfNotPresent", "busybox@sha256:0000000000000000000000000000000000000000000000000000000000000000", "", corev1.PullIfNotPresent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveContainerImagePullPolicy(tc.image, tc.policy)
			if got != tc.want {
				t.Errorf("resolveContainerImagePullPolicy() = %q, want %q", got, tc.want)
			}
		})
	}
}
