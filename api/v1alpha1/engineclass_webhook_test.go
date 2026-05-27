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

package v1alpha1

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// engineClassWebhookScheme returns a scheme registering core types and
// the compute v1alpha1 API so the fake client can serve FireboltEngine
// lists for the delete-webhook tests.
func engineClassWebhookScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(AddToScheme(scheme))
	return scheme
}

// fireboltEngineRefingClass returns a minimal FireboltEngine with the
// given name, namespace, and spec.engineClassRef. Used by webhook tests
// that need the fake client to return references at list time.
func fireboltEngineRefingClass(name, namespace, className string) *FireboltEngine {
	return &FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: FireboltEngineSpec{
			InstanceRef:    "test-instance",
			EngineClassRef: ptr.To(className),
		},
	}
}

// validEngineClass returns an EngineClass whose spec.template contains
// only user-allowed fields. Used as the baseline for negative tests
// that add one rejected field at a time. Lives in namespace "firebolt"
// so the delete-webhook tests (which list FireboltEngines scoped to
// the class's namespace) exercise the real same-namespace filter
// rather than the empty-namespace special case the fake client treats
// as "all namespaces".
func validEngineClass() *EngineClass {
	gracePeriod := int64(60)
	_ = gracePeriod
	return &EngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-optimized", Namespace: "firebolt"},
		Spec: EngineClassSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"team": "data"},
					// kube2iam-style annotation: a pod annotation that
					// carries the IAM role ARN. IRSA's
					// eks.amazonaws.com/role-arn belongs on the
					// ServiceAccount, not the pod template, so it
					// would teach the wrong pattern here.
					Annotations: map[string]string{"iam.amazonaws.com/role": "arn:aws:iam::1:role/x"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "my-irsa",
					NodeSelector:       map[string]string{"pool": "engine"},
					Tolerations: []corev1.Toleration{{
						Key:      "pool",
						Operator: corev1.TolerationOpEqual,
						Value:    "engine",
						Effect:   corev1.TaintEffectNoSchedule,
					}},
					Containers: []corev1.Container{
						{
							Name:  EngineContainerName,
							Image: "ghcr.io/firebolt-db/engine:dev",
						},
						{
							Name:    "log-shipper",
							Image:   "fluent/fluent-bit:latest",
							Command: []string{"/bin/sh", "-c", "fluent-bit"},
						},
					},
					InitContainers: []corev1.Container{
						{
							Name:    "warmup",
							Image:   "busybox",
							Command: []string{"sh", "-c", "echo warm"},
						},
					},
				},
			},
		},
	}
}

func TestEngineClassValidator_CreateAcceptsValid(t *testing.T) {
	v := &EngineClassCustomValidator{}
	if _, err := v.ValidateCreate(context.Background(), validEngineClass()); err != nil {
		t.Fatalf("ValidateCreate: unexpected error on valid spec: %v", err)
	}
}

func TestEngineClassValidator_UpdateAcceptsValid(t *testing.T) {
	v := &EngineClassCustomValidator{}
	if _, err := v.ValidateUpdate(context.Background(), validEngineClass(), validEngineClass()); err != nil {
		t.Fatalf("ValidateUpdate: unexpected error on valid spec: %v", err)
	}
}

// TestEngineClassValidator_RejectsOwnedFields runs every rejection in one
// table. Each row mutates the baseline to introduce a single owned-field
// violation; the test asserts ValidateCreate returns an error mentioning
// the expected field path. Bundling lets us notice when adding a row
// regresses a sibling check, and keeps the file from sprawling into one
// near-identical test per field.
func TestEngineClassValidator_RejectsOwnedFields(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*EngineClass)
		wantField string
	}{
		{
			name: "reserved label prefix",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Labels["firebolt.io/config-hash"] = "abc"
			},
			wantField: "spec.template.metadata.labels",
		},
		{
			name: "reserved annotation prefix",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Annotations["firebolt.io/generation"] = "1"
			},
			wantField: "spec.template.metadata.annotations",
		},
		{
			name: "pod terminationGracePeriodSeconds",
			mutate: func(ec *EngineClass) {
				v := int64(30)
				ec.Spec.Template.Spec.TerminationGracePeriodSeconds = &v
			},
			wantField: "spec.template.spec.terminationGracePeriodSeconds",
		},
		{
			name: "pod subdomain",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Subdomain = "headless"
			},
			wantField: "spec.template.spec.subdomain",
		},
		{
			name: "pod hostname",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Hostname = "engine-0"
			},
			wantField: "spec.template.spec.hostname",
		},
		{
			name: "pod restartPolicy",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
			},
			wantField: "spec.template.spec.restartPolicy",
		},
		{
			name: "pod activeDeadlineSeconds",
			mutate: func(ec *EngineClass) {
				v := int64(300)
				ec.Spec.Template.Spec.ActiveDeadlineSeconds = &v
			},
			wantField: "spec.template.spec.activeDeadlineSeconds",
		},
		{
			name: "engine container command",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh"}
			},
			wantField: "spec.template.spec.containers[0].command",
		},
		{
			name: "engine container args",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].Args = []string{"-x"}
			},
			wantField: "spec.template.spec.containers[0].args",
		},
		{
			name: "engine container ports",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 1234}}
			},
			wantField: "spec.template.spec.containers[0].ports",
		},
		{
			name: "engine container readinessProbe",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{}
			},
			wantField: "spec.template.spec.containers[0].readinessProbe",
		},
		{
			name: "engine container livenessProbe",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{}
			},
			wantField: "spec.template.spec.containers[0].livenessProbe",
		},
		{
			name: "engine container startupProbe",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].StartupProbe = &corev1.Probe{}
			},
			wantField: "spec.template.spec.containers[0].startupProbe",
		},
		{
			name: "engine container reserved env POD_INDEX",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: EnginePodIndexEnvKey, Value: "x"}}
			},
			wantField: "spec.template.spec.containers[0].env[0].name",
		},
		{
			name: "engine container reserved env FB_AWS_EC2_METADATA_CLIENT_ENABLED",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: EngineAwsEC2MetadataClientEnabledEnvKey, Value: "false"}}
			},
			wantField: "spec.template.spec.containers[0].env[0].name",
		},
		{
			name: "engine container reserved env FIREBOLT_CORE_MODE",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: EngineCoreModeEnvKey, Value: "0"}}
			},
			wantField: "spec.template.spec.containers[0].env[0].name",
		},
		{
			name: "duplicate engine container",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.Containers = append(ec.Spec.Template.Spec.Containers, corev1.Container{
					Name:  EngineContainerName,
					Image: "other",
				})
			},
			wantField: "spec.template.spec.containers[2].name",
		},
		{
			name: "init container named engine",
			mutate: func(ec *EngineClass) {
				ec.Spec.Template.Spec.InitContainers = append(ec.Spec.Template.Spec.InitContainers, corev1.Container{
					Name:  EngineContainerName,
					Image: "x",
				})
			},
			wantField: "spec.template.spec.initContainers[1].name",
		},
	}

	v := &EngineClassCustomValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ec := validEngineClass()
			tc.mutate(ec)
			_, err := v.ValidateCreate(context.Background(), ec)
			if err == nil {
				t.Fatalf("ValidateCreate: expected error containing %q, got nil", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("ValidateCreate: error %q does not mention field path %q", err.Error(), tc.wantField)
			}
		})
	}
}

// TestEngineClassValidator_SidecarsUnconstrained pins down that sidecar
// containers can carry whatever the user wants — image, command, ports,
// env, probes. Sidecar parity is a deliberate FB-1145 design choice;
// regressing it would silently turn the EngineClass into a strict pod-spec
// allowlist.
func TestEngineClassValidator_SidecarsUnconstrained(t *testing.T) {
	v := &EngineClassCustomValidator{}
	ec := validEngineClass()
	sidecar := &ec.Spec.Template.Spec.Containers[1]
	sidecar.Command = []string{"/bin/sh"}
	sidecar.Args = []string{"-c", "true"}
	sidecar.Ports = []corev1.ContainerPort{{ContainerPort: 4242}}
	sidecar.ReadinessProbe = &corev1.Probe{}
	sidecar.LivenessProbe = &corev1.Probe{}
	sidecar.StartupProbe = &corev1.Probe{}
	sidecar.Env = []corev1.EnvVar{
		// Sidecars may carry env keys that are reserved on the engine
		// container — the reservation is per-container, not pod-wide.
		{Name: EnginePodIndexEnvKey, Value: "ignored-on-sidecar"},
		{Name: "ANY_OTHER", Value: "ok"},
	}
	if _, err := v.ValidateCreate(context.Background(), ec); err != nil {
		t.Fatalf("ValidateCreate: sidecar mutations triggered error: %v", err)
	}
}

func TestEngineClassValidator_RejectsDeleteWhileBound(t *testing.T) {
	scheme := engineClassWebhookScheme(t)
	ec := validEngineClass()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		// Same namespace as the class — counted.
		fireboltEngineRefingClass("e1", ec.Namespace, ec.Name),
		fireboltEngineRefingClass("e2", ec.Namespace, ec.Name),
		// Same namespace, different class — not counted.
		fireboltEngineRefingClass("other", ec.Namespace, "different-class"),
		// Different namespace, same name — NOT counted. EngineClass is
		// namespaced; cross-namespace references cannot resolve.
		fireboltEngineRefingClass("cross-ns", "other-ns", ec.Name),
	).Build()
	v := &EngineClassCustomValidator{Reader: reader}
	_, err := v.ValidateDelete(context.Background(), ec)
	if err == nil {
		t.Fatal("ValidateDelete: expected refusal while engines reference the class, got nil")
	}
	if !strings.Contains(err.Error(), "2 FireboltEngine") {
		t.Errorf("ValidateDelete: error %q does not mention bound engine count (expected 2 in same namespace)", err.Error())
	}
	if !strings.Contains(err.Error(), ec.Namespace) {
		t.Errorf("ValidateDelete: error %q does not mention the class's namespace %q", err.Error(), ec.Namespace)
	}
}

// TestEngineClassValidator_AllowsDeleteWhenOnlyCrossNamespaceRefs pins
// down the cross-namespace isolation: a class with engines in OTHER
// namespaces referencing the same class name must NOT be protected.
// Those references are dangling by Kubernetes semantics (an engine in
// namespace X cannot resolve an EngineClass in namespace Y), so the
// deletion gate must let the class go.
func TestEngineClassValidator_AllowsDeleteWhenOnlyCrossNamespaceRefs(t *testing.T) {
	scheme := engineClassWebhookScheme(t)
	ec := validEngineClass() // namespace "firebolt"
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		fireboltEngineRefingClass("cross1", "ns-a", ec.Name),
		fireboltEngineRefingClass("cross2", "ns-b", ec.Name),
	).Build()
	v := &EngineClassCustomValidator{Reader: reader}
	if _, err := v.ValidateDelete(context.Background(), ec); err != nil {
		t.Fatalf("ValidateDelete: expected to allow delete when only cross-namespace engines reference the class, got %v", err)
	}
}

func TestEngineClassValidator_AllowsDeleteWhenUnbound(t *testing.T) {
	scheme := engineClassWebhookScheme(t)
	ec := validEngineClass()
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &EngineClassCustomValidator{Reader: reader}
	if _, err := v.ValidateDelete(context.Background(), ec); err != nil {
		t.Fatalf("ValidateDelete: unexpected refusal with no engines: %v", err)
	}
}

// TestEngineClassValidator_RejectsDeleteWhenStatusZeroButRefExists pins
// down the race the live-list switch is designed to close: an engine
// references the class but status.boundEngines is still at its zero-value
// default (no reconcile yet). Pre-FB-1145-fix this delete would succeed
// and orphan the engine; with the live list it must be refused.
func TestEngineClassValidator_RejectsDeleteWhenStatusZeroButRefExists(t *testing.T) {
	scheme := engineClassWebhookScheme(t)
	ec := validEngineClass()
	ec.Status.BoundEngines = 0
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		// Same namespace so the cross-namespace filter does not mask
		// the race-closure behavior the test is asserting.
		fireboltEngineRefingClass("racy", ec.Namespace, ec.Name),
	).Build()
	v := &EngineClassCustomValidator{Reader: reader}
	_, err := v.ValidateDelete(context.Background(), ec)
	if err == nil {
		t.Fatal("ValidateDelete: expected refusal when an engine references the class even with zero status, got nil")
	}
	if !strings.Contains(err.Error(), "1 FireboltEngine") {
		t.Errorf("ValidateDelete: error %q does not mention bound engine count", err.Error())
	}
}

// TestEngineClassValidator_DeleteFailsWithoutReader guards the
// initialization path: SetupEngineClassWebhookWithManager wires the API
// reader, but a misconfigured test or future refactor that drops it must
// not silently allow deletes.
func TestEngineClassValidator_DeleteFailsWithoutReader(t *testing.T) {
	v := &EngineClassCustomValidator{}
	_, err := v.ValidateDelete(context.Background(), validEngineClass())
	if err == nil {
		t.Fatal("ValidateDelete: expected error when Reader is unset, got nil")
	}
}
