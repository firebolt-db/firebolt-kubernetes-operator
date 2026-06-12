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
)

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

// TestResolveImagePullPolicy checks that the helper preserves the user's
// explicit pull policy and falls back to IfNotPresent only when no value is
// supplied. The kubebuilder default makes the empty branch unreachable in
// production (the API server defaults `pullPolicy` on admission), but unit
// tests that build specs directly rely on the fallback.
func TestResolveImagePullPolicy(t *testing.T) {
	tests := []struct {
		name string
		spec *computev1alpha1.ImageSpec
		want corev1.PullPolicy
	}{
		{"nil spec", nil, corev1.PullIfNotPresent},
		{"empty spec", &computev1alpha1.ImageSpec{}, corev1.PullIfNotPresent},
		{"explicit Always", &computev1alpha1.ImageSpec{PullPolicy: corev1.PullAlways}, corev1.PullAlways},
		{"explicit Never", &computev1alpha1.ImageSpec{PullPolicy: corev1.PullNever}, corev1.PullNever},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveImagePullPolicy(tc.spec)
			if got != tc.want {
				t.Errorf("resolveImagePullPolicy() = %q, want %q", got, tc.want)
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
