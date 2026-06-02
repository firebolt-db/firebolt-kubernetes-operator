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

// fireboltEngineClassWebhookScheme returns a scheme registering core types
// and the compute v1alpha1 API so the fake client can serve FireboltEngine
// lists for the delete-webhook tests.
func fireboltEngineClassWebhookScheme(t *testing.T) *runtime.Scheme {
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

// validFireboltEngineClass returns a FireboltEngineClass whose spec.template
// contains only user-allowed fields. Used as the baseline for negative tests
// that add one rejected field at a time. Lives in namespace "firebolt"
// so the delete-webhook tests (which list FireboltEngines scoped to
// the class's namespace) exercise the real same-namespace filter
// rather than the empty-namespace special case the fake client treats
// as "all namespaces".
func validFireboltEngineClass() *FireboltEngineClass {
	gracePeriod := int64(60)
	_ = gracePeriod
	return &FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-optimized", Namespace: "firebolt"},
		Spec: FireboltEngineClassSpec{
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

func TestFireboltEngineClassValidator_CreateAcceptsValid(t *testing.T) {
	v := &FireboltEngineClassCustomValidator{}
	if _, err := v.ValidateCreate(context.Background(), validFireboltEngineClass()); err != nil {
		t.Fatalf("ValidateCreate: unexpected error on valid spec: %v", err)
	}
}

func TestFireboltEngineClassValidator_UpdateAcceptsValid(t *testing.T) {
	v := &FireboltEngineClassCustomValidator{}
	if _, err := v.ValidateUpdate(context.Background(), validFireboltEngineClass(), validFireboltEngineClass()); err != nil {
		t.Fatalf("ValidateUpdate: unexpected error on valid spec: %v", err)
	}
}

// TestFireboltEngineClassValidator_RejectsOwnedFields runs every rejection in one
// table. Each row mutates the baseline to introduce a single owned-field
// violation; the test asserts ValidateCreate returns an error mentioning
// the expected field path. Bundling lets us notice when adding a row
// regresses a sibling check, and keeps the file from sprawling into one
// near-identical test per field.
func TestFireboltEngineClassValidator_RejectsOwnedFields(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*FireboltEngineClass)
		wantField string
	}{
		{
			name: "reserved label prefix",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Labels["firebolt.io/config-hash"] = "abc"
			},
			wantField: "spec.template.metadata.labels",
		},
		{
			name: "reserved annotation prefix",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Annotations["firebolt.io/generation"] = "1"
			},
			wantField: "spec.template.metadata.annotations",
		},
		{
			name: "pod terminationGracePeriodSeconds",
			mutate: func(ec *FireboltEngineClass) {
				v := int64(30)
				ec.Spec.Template.Spec.TerminationGracePeriodSeconds = &v
			},
			wantField: "spec.template.spec.terminationGracePeriodSeconds",
		},
		{
			name: "pod subdomain",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Subdomain = "headless"
			},
			wantField: "spec.template.spec.subdomain",
		},
		{
			name: "pod hostname",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Hostname = "engine-0"
			},
			wantField: "spec.template.spec.hostname",
		},
		{
			name: "pod restartPolicy",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
			},
			wantField: "spec.template.spec.restartPolicy",
		},
		{
			name: "pod activeDeadlineSeconds",
			mutate: func(ec *FireboltEngineClass) {
				v := int64(300)
				ec.Spec.Template.Spec.ActiveDeadlineSeconds = &v
			},
			wantField: "spec.template.spec.activeDeadlineSeconds",
		},
		{
			name: "engine container command",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].Command = []string{"/bin/sh"}
			},
			wantField: "spec.template.spec.containers[0].command",
		},
		{
			name: "engine container args",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].Args = []string{"-x"}
			},
			wantField: "spec.template.spec.containers[0].args",
		},
		{
			name: "engine container ports",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 1234}}
			},
			wantField: "spec.template.spec.containers[0].ports",
		},
		{
			name: "engine container readinessProbe",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].ReadinessProbe = &corev1.Probe{}
			},
			wantField: "spec.template.spec.containers[0].readinessProbe",
		},
		{
			name: "engine container livenessProbe",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].LivenessProbe = &corev1.Probe{}
			},
			wantField: "spec.template.spec.containers[0].livenessProbe",
		},
		{
			name: "engine container startupProbe",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].StartupProbe = &corev1.Probe{}
			},
			wantField: "spec.template.spec.containers[0].startupProbe",
		},
		{
			name: "engine container reserved env POD_INDEX",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: EnginePodIndexEnvKey, Value: "x"}}
			},
			wantField: "spec.template.spec.containers[0].env[0].name",
		},
		{
			name: "engine container reserved env FB_AWS_EC2_METADATA_CLIENT_ENABLED",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: EngineAwsEC2MetadataClientEnabledEnvKey, Value: "false"}}
			},
			wantField: "spec.template.spec.containers[0].env[0].name",
		},
		{
			name: "engine container reserved env FIREBOLT_CORE_MODE",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: EngineCoreModeEnvKey, Value: "0"}}
			},
			wantField: "spec.template.spec.containers[0].env[0].name",
		},
		{
			name: "duplicate engine container",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers = append(ec.Spec.Template.Spec.Containers, corev1.Container{
					Name:  EngineContainerName,
					Image: "other",
				})
			},
			wantField: "spec.template.spec.containers[2].name",
		},
		{
			name: "init container named engine",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.InitContainers = append(ec.Spec.Template.Spec.InitContainers, corev1.Container{
					Name:  EngineContainerName,
					Image: "x",
				})
			},
			wantField: "spec.template.spec.initContainers[1].name",
		},
		// Security / footgun pod-level fields (FB-1426 follow-up).
		{
			name: "pod hostNetwork",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.HostNetwork = true
			},
			wantField: "spec.template.spec.hostNetwork",
		},
		{
			name: "pod hostPID",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.HostPID = true
			},
			wantField: "spec.template.spec.hostPID",
		},
		{
			name: "pod hostIPC",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.HostIPC = true
			},
			wantField: "spec.template.spec.hostIPC",
		},
		{
			name: "pod shareProcessNamespace",
			mutate: func(ec *FireboltEngineClass) {
				v := true
				ec.Spec.Template.Spec.ShareProcessNamespace = &v
			},
			wantField: "spec.template.spec.shareProcessNamespace",
		},
		{
			name: "pod hostUsers",
			mutate: func(ec *FireboltEngineClass) {
				v := false
				ec.Spec.Template.Spec.HostUsers = &v
			},
			wantField: "spec.template.spec.hostUsers",
		},
		// Engine-container interactive-orchestration rejections.
		{
			name: "engine container restartPolicy",
			mutate: func(ec *FireboltEngineClass) {
				v := corev1.ContainerRestartPolicyAlways
				ec.Spec.Template.Spec.Containers[0].RestartPolicy = &v
			},
			wantField: "spec.template.spec.containers[0].restartPolicy",
		},
		{
			name: "engine container stdin",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].Stdin = true
			},
			wantField: "spec.template.spec.containers[0].stdin",
		},
		{
			name: "engine container stdinOnce",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].StdinOnce = true
			},
			wantField: "spec.template.spec.containers[0].stdinOnce",
		},
		{
			name: "engine container tty",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Spec.Containers[0].TTY = true
			},
			wantField: "spec.template.spec.containers[0].tty",
		},
		// Pod-template metadata silent-drop fields.
		{
			name: "pod template metadata.name",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Name = "user-named-pod"
			},
			wantField: "spec.template.metadata.name",
		},
		{
			name: "pod template metadata.generateName",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.GenerateName = "user-generated-"
			},
			wantField: "spec.template.metadata.generateName",
		},
		{
			name: "pod template metadata.namespace",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Namespace = "elsewhere"
			},
			wantField: "spec.template.metadata.namespace",
		},
		{
			name: "pod template metadata.ownerReferences",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: "v1", Kind: "Pod", Name: "x", UID: "00000000-0000-0000-0000-000000000000",
				}}
			},
			wantField: "spec.template.metadata.ownerReferences",
		},
		{
			name: "pod template metadata.finalizers",
			mutate: func(ec *FireboltEngineClass) {
				ec.Spec.Template.Finalizers = []string{"user.example.com/protect"}
			},
			wantField: "spec.template.metadata.finalizers",
		},
	}

	v := &FireboltEngineClassCustomValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ec := validFireboltEngineClass()
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

// TestFireboltEngineClassValidator_SidecarsUnconstrained pins down that sidecar
// containers can carry whatever the user wants — image, command, ports,
// env, probes. Sidecar parity is a deliberate FB-1145 design choice;
// regressing it would silently turn the FireboltEngineClass into a strict
// pod-spec allowlist.
func TestFireboltEngineClassValidator_SidecarsUnconstrained(t *testing.T) {
	v := &FireboltEngineClassCustomValidator{}
	ec := validFireboltEngineClass()
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

func TestFireboltEngineClassValidator_RejectsDeleteWhileBound(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	ec := validFireboltEngineClass()
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		// Same namespace as the class — counted.
		fireboltEngineRefingClass("e1", ec.Namespace, ec.Name),
		fireboltEngineRefingClass("e2", ec.Namespace, ec.Name),
		// Same namespace, different class — not counted.
		fireboltEngineRefingClass("other", ec.Namespace, "different-class"),
		// Different namespace, same name — NOT counted. FireboltEngineClass is
		// namespaced; cross-namespace references cannot resolve.
		fireboltEngineRefingClass("cross-ns", "other-ns", ec.Name),
	).Build()
	v := &FireboltEngineClassCustomValidator{Reader: reader}
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

// TestFireboltEngineClassValidator_AllowsDeleteWhenOnlyCrossNamespaceRefs pins
// down the cross-namespace isolation: a class with engines in OTHER
// namespaces referencing the same class name must NOT be protected.
// Those references are dangling by Kubernetes semantics (an engine in
// namespace X cannot resolve a FireboltEngineClass in namespace Y), so the
// deletion gate must let the class go.
func TestFireboltEngineClassValidator_AllowsDeleteWhenOnlyCrossNamespaceRefs(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	ec := validFireboltEngineClass() // namespace "firebolt"
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		fireboltEngineRefingClass("cross1", "ns-a", ec.Name),
		fireboltEngineRefingClass("cross2", "ns-b", ec.Name),
	).Build()
	v := &FireboltEngineClassCustomValidator{Reader: reader}
	if _, err := v.ValidateDelete(context.Background(), ec); err != nil {
		t.Fatalf("ValidateDelete: expected to allow delete when only cross-namespace engines reference the class, got %v", err)
	}
}

func TestFireboltEngineClassValidator_AllowsDeleteWhenUnbound(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	ec := validFireboltEngineClass()
	reader := fake.NewClientBuilder().WithScheme(scheme).Build()
	v := &FireboltEngineClassCustomValidator{Reader: reader}
	if _, err := v.ValidateDelete(context.Background(), ec); err != nil {
		t.Fatalf("ValidateDelete: unexpected refusal with no engines: %v", err)
	}
}

// TestFireboltEngineClassValidator_RejectsDeleteWhenStatusZeroButRefExists pins
// down the race the live-list switch is designed to close: an engine
// references the class but status.boundEngines is still at its zero-value
// default (no reconcile yet). Pre-FB-1145-fix this delete would succeed
// and orphan the engine; with the live list it must be refused.
func TestFireboltEngineClassValidator_RejectsDeleteWhenStatusZeroButRefExists(t *testing.T) {
	scheme := fireboltEngineClassWebhookScheme(t)
	ec := validFireboltEngineClass()
	ec.Status.BoundEngines = 0
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		// Same namespace so the cross-namespace filter does not mask
		// the race-closure behavior the test is asserting.
		fireboltEngineRefingClass("racy", ec.Namespace, ec.Name),
	).Build()
	v := &FireboltEngineClassCustomValidator{Reader: reader}
	_, err := v.ValidateDelete(context.Background(), ec)
	if err == nil {
		t.Fatal("ValidateDelete: expected refusal when an engine references the class even with zero status, got nil")
	}
	if !strings.Contains(err.Error(), "1 FireboltEngine") {
		t.Errorf("ValidateDelete: error %q does not mention bound engine count", err.Error())
	}
}

// TestFireboltEngineClassValidator_DeleteFailsWithoutReader guards the
// initialization path: SetupFireboltEngineClassWebhookWithManager wires the API
// reader, but a misconfigured test or future refactor that drops it must
// not silently allow deletes.
func TestFireboltEngineClassValidator_DeleteFailsWithoutReader(t *testing.T) {
	v := &FireboltEngineClassCustomValidator{}
	_, err := v.ValidateDelete(context.Background(), validFireboltEngineClass())
	if err == nil {
		t.Fatal("ValidateDelete: expected error when Reader is unset, got nil")
	}
}
