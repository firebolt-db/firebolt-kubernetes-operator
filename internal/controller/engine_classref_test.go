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
	"context"
	stderrors "errors"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	enginemetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// engineRefingClassFixture returns a FireboltEngine in the given
// namespace referencing the named FireboltEngineClass. classRef == "" produces
// an engine with nil spec.engineClassRef (no class).
func engineRefingClassFixture(name, namespace, classRef string) *computev1alpha1.FireboltEngine {
	spec := computev1alpha1.FireboltEngineSpec{InstanceRef: "inst", Replicas: 1}
	if classRef != "" {
		ref := classRef
		spec.EngineClassRef = &ref
	}
	return &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
}

// classOnlyFixture returns a FireboltEngineClass in the given namespace with a
// minimal user-allowed template (ServiceAccountName). Used by lookup
// tests that don't care about the rendered pod spec, only that
// resolveFireboltEngineClassInfo finds (or does not find) the class.
func classOnlyFixture(name, namespace string) *computev1alpha1.FireboltEngineClass {
	return &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: computev1alpha1.FireboltEngineClassSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{ServiceAccountName: name + "-sa"},
			},
		},
	}
}

func classRefTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}
	return s
}

// engineRefTestReconciler returns a FireboltEngineReconciler wired with
// the given fake client. The Namespace filter is left empty so the
// reconciler watches all namespaces (matching production default).
func engineRefTestReconciler(cli client.Client, sch *runtime.Scheme) *FireboltEngineReconciler {
	return &FireboltEngineReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: enginemetrics.NoOpEngineRecorder{},
	}
}

// TestResolveFireboltEngineClassInfo_NamespacedLookup pins down the
// namespace-coupled resolver: a FireboltEngineClass with the right name in a
// different namespace must NOT satisfy spec.engineClassRef. Kubernetes
// resolves the reference in the engine's own namespace; the resolver
// must agree.
func TestResolveFireboltEngineClassInfo_NamespacedLookup(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		// Class exists in "ns-a" only.
		classOnlyFixture("compute-optimized", "ns-a"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	t.Run("same-namespace engine resolves", func(t *testing.T) {
		eng := engineRefingClassFixture("e", "ns-a", "compute-optimized")
		info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err != nil {
			t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
		}
		if info == nil {
			t.Fatal("info = nil, want non-nil")
		}
		if info.Name != "compute-optimized" {
			t.Errorf("info.Name = %q, want compute-optimized", info.Name)
		}
		if info.Hash == "" {
			t.Error("info.Hash empty, want a content hash so stsMatchesSpec can compare against the STS annotation")
		}
	})

	t.Run("cross-namespace engine fails to resolve", func(t *testing.T) {
		// Engine in "ns-b" referencing a class that exists only in "ns-a".
		eng := engineRefingClassFixture("e", "ns-b", "compute-optimized")
		_, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err == nil {
			t.Fatal("resolveFireboltEngineClassInfo: expected error for cross-namespace reference, got nil")
		}
		if !strings.Contains(err.Error(), "ns-b") {
			t.Errorf("error %q does not name the engine's namespace", err.Error())
		}
	})

	t.Run("nil ref returns nil info", func(t *testing.T) {
		eng := engineRefingClassFixture("e", "ns-a", "")
		info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err != nil {
			t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
		}
		if info != nil {
			t.Errorf("info = %+v, want nil for engine without engineClassRef", info)
		}
	})
}

func clusterClassOnlyFixture() *computev1alpha1.ClusterFireboltEngineClass {
	return &computev1alpha1.ClusterFireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "s-amd-co"},
		Spec: computev1alpha1.ClusterFireboltEngineClassSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					NodeSelector: map[string]string{"node.kubernetes.io/instance-type": "c6id.2xlarge"},
					Containers: []corev1.Container{{
						Name: computev1alpha1.EngineContainerName,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("6120m")},
						},
					}},
				},
			},
		},
	}
}

// TestResolveFireboltEngineClassInfo_ClusterFallback pins namespaced-first
// resolve: a ClusterFireboltEngineClass of the same name is used only when
// no FireboltEngineClass exists in the engine namespace. Same-name objects
// in both scopes are allowed; the namespaced object is the override.
func TestResolveFireboltEngineClassInfo_ClusterFallback(t *testing.T) {
	sch := classRefTestScheme(t)

	t.Run("cluster-only resolve", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			clusterClassOnlyFixture(),
		).Build()
		r := engineRefTestReconciler(cli, sch)
		eng := engineRefingClassFixture("e", "ns-a", "s-amd-co")
		info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err != nil {
			t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
		}
		if info == nil {
			t.Fatal("info = nil, want cluster catalog")
		}
		if info.Name != "s-amd-co" {
			t.Errorf("info.Name = %q, want s-amd-co", info.Name)
		}
		if info.Hash == "" {
			t.Error("info.Hash empty, want a content hash so a catalog edit rolls")
		}
		if info.Template == nil || info.Template.Spec.NodeSelector["node.kubernetes.io/instance-type"] != "c6id.2xlarge" {
			t.Errorf("info.Template missing cluster SKU nodeSelector, got %+v", info.Template)
		}
		if info.Template.Spec.ServiceAccountName != "" {
			t.Error("cluster catalog must not carry serviceAccountName")
		}
	})

	t.Run("namespaced class in another namespace does not override", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			classOnlyFixture("s-amd-co", "ns-b"),
			clusterClassOnlyFixture(),
		).Build()
		r := engineRefTestReconciler(cli, sch)
		eng := engineRefingClassFixture("e", "ns-a", "s-amd-co")
		info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err != nil {
			t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
		}
		if info.Template == nil || info.Template.Spec.NodeSelector["node.kubernetes.io/instance-type"] != "c6id.2xlarge" {
			t.Errorf("want cluster SKU; namespaced class in ns-b must not satisfy ns-a, got %+v", info.Template)
		}
	})

	t.Run("namespaced same-name wins over cluster", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
			classOnlyFixture("s-amd-co", "ns-a"),
			clusterClassOnlyFixture(),
		).Build()
		r := engineRefTestReconciler(cli, sch)
		eng := engineRefingClassFixture("e", "ns-a", "s-amd-co")
		info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err != nil {
			t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
		}
		if info == nil {
			t.Fatal("info = nil")
		}
		if info.Template == nil || info.Template.Spec.ServiceAccountName != "s-amd-co-sa" {
			t.Errorf("want namespaced override SA s-amd-co-sa, got %+v", info.Template)
		}
		if info.Template.Spec.NodeSelector["node.kubernetes.io/instance-type"] != "" {
			t.Error("namespaced class won; cluster SKU nodeSelector must not leak through")
		}
	})

	t.Run("both absent is not found", func(t *testing.T) {
		cli := fake.NewClientBuilder().WithScheme(sch).Build()
		r := engineRefTestReconciler(cli, sch)
		eng := engineRefingClassFixture("e", "ns-a", "s-amd-co")
		_, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err == nil {
			t.Fatal("expected error when both scopes are empty")
		}
		if !stderrors.Is(err, errFireboltEngineClassNotFound) {
			t.Errorf("error %q does not wrap errFireboltEngineClassNotFound", err.Error())
		}
		if !strings.Contains(err.Error(), "FireboltEngineClass ns-a/s-amd-co") {
			t.Errorf("error %q does not name the namespaced lookup", err.Error())
		}
		if !strings.Contains(err.Error(), "ClusterFireboltEngineClass s-amd-co") {
			t.Errorf("error %q does not name the catalog lookup", err.Error())
		}
	})

	t.Run("live serviceAccountName on catalog is gated", func(t *testing.T) {
		cc := clusterClassOnlyFixture()
		cc.Spec.Template.Spec.ServiceAccountName = "tenant-sa"
		cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(cc).Build()
		r := engineRefTestReconciler(cli, sch)
		eng := engineRefingClassFixture("e", "ns-a", "s-amd-co")
		info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
		if err == nil {
			t.Fatal("expected unready error for catalog serviceAccountName")
		}
		if !stderrors.Is(err, errFireboltEngineClassUnready) {
			t.Errorf("error %q does not wrap errFireboltEngineClassUnready", err.Error())
		}
		if !strings.Contains(err.Error(), "serviceAccountName") {
			t.Errorf("error %q does not name serviceAccountName", err.Error())
		}
		if info == nil {
			t.Error("info = nil, want the cluster class so a mid-rollout drain can still read it")
		}
	})

	// The chart ships webhooks off by default, so a catalog object carrying a
	// namespaced reference can reach the API server unvalidated. The resolver's
	// live-spec check is the backstop for the ConfigMap and PVC paths just as
	// it is for serviceAccountName: they would otherwise merge into every
	// consumer pod and bind to whatever exists in that namespace.
	for _, tc := range []struct {
		name      string
		mutate    func(*computev1alpha1.ClusterFireboltEngineClass)
		wantInErr string
	}{
		{
			name: "live ConfigMap volume on catalog is gated",
			mutate: func(cc *computev1alpha1.ClusterFireboltEngineClass) {
				cc.Spec.Template.Spec.Volumes = []corev1.Volume{{
					Name: "tuning",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"},
						},
					},
				}}
			},
			wantInErr: "volumes",
		},
		{
			name: "live persistentVolumeClaim volume on catalog is gated",
			mutate: func(cc *computev1alpha1.ClusterFireboltEngineClass) {
				cc.Spec.Template.Spec.Volumes = []corev1.Volume{{
					Name: "scratch",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "scratch-claim",
						},
					},
				}}
			},
			wantInErr: "volumes",
		},
		{
			name: "live ConfigMap env on catalog is gated",
			mutate: func(cc *computev1alpha1.ClusterFireboltEngineClass) {
				cc.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
					ConfigMapRef: &corev1.ConfigMapEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "tuning"},
					},
				}}
			},
			wantInErr: "containers",
		},
		{
			// effectivePodResourceClaims copies class claims straight into
			// the rendered pod, so an unvalidated catalog claim would bind
			// whatever ResourceClaim shares that name in each tenant.
			name: "live resourceClaims on catalog is gated",
			mutate: func(cc *computev1alpha1.ClusterFireboltEngineClass) {
				name := "shared-gpu"
				cc.Spec.Template.Spec.ResourceClaims = []corev1.PodResourceClaim{{
					Name:              "gpu",
					ResourceClaimName: &name,
				}}
			},
			wantInErr: "resourceClaims",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc := clusterClassOnlyFixture()
			tc.mutate(cc)
			cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(cc).Build()
			r := engineRefTestReconciler(cli, sch)
			eng := engineRefingClassFixture("e", "ns-a", "s-amd-co")
			_, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
			if err == nil {
				t.Fatal("expected unready error, got nil")
			}
			if !stderrors.Is(err, errFireboltEngineClassUnready) {
				t.Errorf("error %q does not wrap errFireboltEngineClassUnready", err.Error())
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not name %q", err.Error(), tc.wantInErr)
			}
		})
	}
}

// TestFireboltEngineClassToEngines_NamespaceScoped pins down the watch handler:
// a class event in namespace X enqueues only engines in namespace X
// that reference the class by name. Cross-namespace engines with
// matching ref are ignored — they could not have admitted (per the
// FireboltEngine validating webhook) and cannot resolve at reconcile
// time anyway.
func TestFireboltEngineClassToEngines_NamespaceScoped(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		// Same namespace + matching ref → enqueued.
		engineRefingClassFixture("a", "ns-a", "compute-optimized"),
		engineRefingClassFixture("b", "ns-a", "compute-optimized"),
		// Same namespace, different ref → not enqueued.
		engineRefingClassFixture("c", "ns-a", "other"),
		// Same namespace, no ref → not enqueued.
		engineRefingClassFixture("d", "ns-a", ""),
		// Different namespace, matching ref → NOT enqueued.
		engineRefingClassFixture("e", "ns-b", "compute-optimized"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	class := &computev1alpha1.FireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-optimized", Namespace: "ns-a"},
	}
	got := r.engineClassToEngines(context.Background(), class)

	gotNames := make([]string, 0, len(got))
	for _, req := range got {
		if req.Namespace != "ns-a" {
			t.Errorf("request %+v carries wrong namespace; want ns-a", req)
		}
		gotNames = append(gotNames, req.Name)
	}
	slices.Sort(gotNames)
	want := []string{"a", "b"}
	if len(gotNames) != len(want) || gotNames[0] != want[0] || gotNames[1] != want[1] {
		t.Errorf("enqueued engines = %v, want %v (cross-namespace engine e must be filtered out)", gotNames, want)
	}
}

func TestClusterEngineClassToEngines_AllNamespaces(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		engineRefingClassFixture("a", "ns-a", "s-amd-co"),
		engineRefingClassFixture("b", "ns-b", "s-amd-co"),
		engineRefingClassFixture("c", "ns-a", "other"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	got := r.clusterEngineClassToEngines(context.Background(), &computev1alpha1.ClusterFireboltEngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "s-amd-co"},
	})
	if len(got) != 2 {
		t.Fatalf("enqueued = %d, want 2 (every namespace that names the catalog)", len(got))
	}
}

// classWithReadyCondition returns a class fixture (always in
// namespace "ns-a", matching the rest of this file's fixtures) with a
// specific FireboltEngineClassConditionReady status / reason / message
// stamped. Used by the consumption-gate tests below.
func classWithReadyCondition(name string, status metav1.ConditionStatus, reason, message string) *computev1alpha1.FireboltEngineClass {
	class := classOnlyFixture(name, "ns-a")
	apimeta.SetStatusCondition(&class.Status.Conditions, metav1.Condition{
		Type:    computev1alpha1.FireboltEngineClassConditionReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return class
}

// TestResolveFireboltEngineClassInfo_BlocksOnOperatorOwnedFieldSet pins the
// consumption gate: a class the FireboltEngineClassReconciler marked
// Ready=False/OperatorOwnedFieldSet must not be rendered into a
// StatefulSet. The resolver returns errFireboltEngineClassUnready wrapping the
// class name + namespace + condition message so the caller can surface
// an actionable pointer on the engine condition.
func TestResolveFireboltEngineClassInfo_BlocksOnOperatorOwnedFieldSet(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		classWithReadyCondition("bad-class",
			metav1.ConditionFalse, reasonOperatorOwnedFieldSet,
			"spec.template.spec.containers[0].command: Forbidden: engine container command is operator-owned"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	eng := engineRefingClassFixture("e", "ns-a", "bad-class")
	info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
	if err == nil {
		t.Fatal("resolveFireboltEngineClassInfo: expected error for unready class, got nil")
	}
	if info == nil {
		t.Error("info = nil, want the class so a mid-rollout drain can still read inherited drain settings")
	}
	if !stderrors.Is(err, errFireboltEngineClassUnready) {
		t.Errorf("error %q does not wrap errFireboltEngineClassUnready", err.Error())
	}
	if !strings.Contains(err.Error(), "bad-class") {
		t.Errorf("error %q does not name the class", err.Error())
	}
	if !strings.Contains(err.Error(), "ns-a") {
		t.Errorf("error %q does not name the namespace", err.Error())
	}
	if !strings.Contains(err.Error(), "operator-owned") {
		t.Errorf("error %q does not propagate the class condition message", err.Error())
	}
}

// TestResolveFireboltEngineClassInfo_PassesOnReadyTrue is the false-positive
// guard: a class with Ready=True/Admissible (the happy path the class
// reconciler stamps on every valid template) resolves cleanly, matching
// the pre-W3 behavior for well-formed classes.
func TestResolveFireboltEngineClassInfo_PassesOnReadyTrue(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		classWithReadyCondition("ok-class",
			metav1.ConditionTrue, "Admissible",
			"spec.template contains no operator-owned paths"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	eng := engineRefingClassFixture("e", "ns-a", "ok-class")
	info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
	if err != nil {
		t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
	}
	if info == nil || info.Name != "ok-class" {
		t.Errorf("info = %+v, want non-nil with Name=ok-class", info)
	}
}

// TestResolveFireboltEngineClassInfo_PassesWhenReadyConditionMissing pins the
// race-tolerance behavior: a class freshly created where the
// FireboltEngineClassReconciler has not yet stamped a Ready condition must not
// be gated as unready (that would deadlock the engine until the class
// controller catches up). Resolution proceeds; the next reconcile,
// driven by the engine controller's FireboltEngineClass watch, will re-evaluate
// once the class status appears.
func TestResolveFireboltEngineClassInfo_PassesWhenReadyConditionMissing(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		// No status conditions set.
		classOnlyFixture("fresh-class", "ns-a"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	eng := engineRefingClassFixture("e", "ns-a", "fresh-class")
	info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
	if err != nil {
		t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
	}
	if info == nil {
		t.Error("info = nil, want non-nil while class is awaiting its first status stamp")
	}
}

// TestResolveFireboltEngineClassInfo_PassesOnDeletionBlocked pins the no-
// deadlock invariant for the W1 deletion guard: a class Terminating
// with Ready=False/DeletionBlocked must keep resolving so its bound
// engines continue to reconcile normally. Blocking here would prevent
// engines from being deleted, which is the exact action that unbinds
// them from the class and lets the deletion finalize.
func TestResolveFireboltEngineClassInfo_PassesOnDeletionBlocked(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		classWithReadyCondition("terminating-class",
			metav1.ConditionFalse, reasonDeletionBlocked,
			"FireboltEngineClass \"terminating-class\" in namespace \"ns-a\" is referenced by 2 FireboltEngine(s)"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	eng := engineRefingClassFixture("e", "ns-a", "terminating-class")
	info, err := r.resolveFireboltEngineClassInfo(context.Background(), eng)
	if err != nil {
		t.Fatalf("resolveFireboltEngineClassInfo: %v", err)
	}
	if info == nil {
		t.Error("info = nil, want non-nil so bound engines keep reconciling against a Terminating class")
	}
}

// TestEngineReconcile_UnreadyClassSurfacesCondition pins the Reconcile-
// level wiring of the consumption gate: when the resolver returns
// errFireboltEngineClassUnready, Reconcile must set the engine's
// ConditionReady=False with reason FireboltEngineClassUnready and a message
// that points at the unready class, persist that status, and short-
// circuit before any StatefulSet is rendered. Verifies the end-to-end
// translation of "class status says no" → "engine status says why".
func TestEngineReconcile_UnreadyClassSurfacesCondition(t *testing.T) {
	sch := classRefTestScheme(t)
	const (
		ns        = "ns-a"
		instName  = "parent-instance"
		engName   = "engine-blocked"
		className = "bad-class"
	)

	// Ready FireboltInstance so resolveInstanceInfo passes through.
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata.ns-a.svc.cluster.local:50051",
		},
	}
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       engName,
			Namespace:  ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef:    instName,
			EngineClassRef: func() *string { s := className; return &s }(),
			Replicas:       1,
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase: computev1alpha1.PhaseCreating,
		},
	}
	class := classWithReadyCondition(className,
		metav1.ConditionFalse, reasonOperatorOwnedFieldSet,
		"spec.template.spec.containers[0].command: Forbidden: engine container command is operator-owned")

	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(instance, engine, class).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()

	r := &FireboltEngineReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: enginemetrics.NoOpEngineRecorder{},
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: engName, Namespace: ns},
	}); err != nil {
		t.Fatalf("Reconcile: unexpected error (gate should set status and requeue, not return err): %v", err)
	}

	updated := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: engName, Namespace: ns}, updated); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", cond.Status)
	}
	if cond.Reason != reasonFireboltEngineClassUnready {
		t.Errorf("Ready.Reason = %q, want %q", cond.Reason, reasonFireboltEngineClassUnready)
	}
	if !strings.Contains(cond.Message, className) {
		t.Errorf("Ready.Message = %q, want it to name the offending class %q", cond.Message, className)
	}
	// Belt and braces: no StatefulSet should have been rendered.
	var stsList appsv1.StatefulSetList
	if err := cli.List(context.Background(), &stsList, client.InNamespace(ns)); err != nil {
		t.Fatalf("List StatefulSets: %v", err)
	}
	if len(stsList.Items) > 0 {
		names := make([]string, 0, len(stsList.Items))
		for i := range stsList.Items {
			names = append(names, stsList.Items[i].Name)
		}
		t.Errorf("StatefulSets = %v, want none (gate must short-circuit before applyEngineState)", names)
	}
}

// TestEngineReconcile_MissingClassEmitsEvent pins the user-facing
// signal when spec.engineClassRef matches nothing in either scope:
// a Warning Event on the engine naming both lookups, no Ready
// condition rewrite, and the error still bubbles for backoff.
func TestEngineReconcile_MissingClassEmitsEvent(t *testing.T) {
	sch := classRefTestScheme(t)
	const (
		ns       = "ns-a"
		instName = "parent-instance"
		engName  = "engine-missing-class"
		classRef = "s-amd-co"
	)
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata.ns-a.svc.cluster.local:50051",
		},
	}
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       engName,
			Namespace:  ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef:    instName,
			EngineClassRef: func() *string { s := classRef; return &s }(),
			Replicas:       1,
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase: computev1alpha1.PhaseCreating,
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(instance, engine).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()
	rec := events.NewFakeRecorder(8)
	r := &FireboltEngineReconciler{
		Client:          cli,
		Scheme:          sch,
		MetricsRecorder: enginemetrics.NoOpEngineRecorder{},
		EventRecorder:   rec,
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: engName, Namespace: ns},
	}); err == nil {
		t.Fatal("Reconcile: expected missing-class error for backoff")
	} else if !stderrors.Is(err, errFireboltEngineClassNotFound) {
		t.Fatalf("Reconcile: %v, want errFireboltEngineClassNotFound", err)
	}

	evs := drainEvents(rec)
	if len(evs) != 1 {
		t.Fatalf("events = %v, want exactly one Warning", evs)
	}
	ev := evs[0]
	if !strings.Contains(ev, "Warning") || !strings.Contains(ev, eventReasonEngineClassNotFound) {
		t.Errorf("Event %q does not look like Warning/%s", ev, eventReasonEngineClassNotFound)
	}
	if !strings.Contains(ev, "FireboltEngineClass ns-a/s-amd-co") {
		t.Errorf("Event %q does not name the namespaced lookup", ev)
	}
	if !strings.Contains(ev, "ClusterFireboltEngineClass s-amd-co") {
		t.Errorf("Event %q does not name the catalog lookup", ev)
	}

	updated := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), types.NamespacedName{Name: engName, Namespace: ns}, updated); err != nil {
		t.Fatalf("Get engine: %v", err)
	}
	if cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady); cond != nil {
		t.Errorf("Ready = %+v, want no condition rewrite on missing class", cond)
	}
}

// engineInRolloutPhaseFixture is an engine already past creating, so
// fail-closed Class/Preset gates must not abort the pass. CurrentGeneration
// is 1; switching still serves generation 0, while draining/cleaning have
// generation 0 recorded as DrainingGeneration.
func engineInRolloutPhaseFixture(name, namespace, classRef string, phase computev1alpha1.EnginePhase) *computev1alpha1.FireboltEngine {
	eng := engineRefingClassFixture(name, namespace, classRef)
	eng.Finalizers = []string{finalizerName}
	eng.Generation = 1
	eng.Status.Phase = phase
	eng.Status.CurrentGeneration = 1
	eng.Status.ActiveGeneration = 1
	eng.Status.ObservedGeneration = 1
	switch phase {
	case computev1alpha1.PhaseSwitching:
		eng.Status.ActiveGeneration = 0
	case computev1alpha1.PhaseDraining, computev1alpha1.PhaseCleaning:
		dg := 0
		eng.Status.DrainingGeneration = &dg
	default:
		// Helper is only used for switching / draining / cleaning.
	}
	return eng
}

// TestEngineReconcile_UnreadyClassDoesNotAbortRollout pins the render-gate
// window: OperatorOwnedFieldSet refuse a new StatefulSet in creating, but
// switching / draining / cleaning already hold rendered resources and must
// still run computeEngineReconcile. A closed class must not freeze cutover.
func TestEngineReconcile_UnreadyClassDoesNotAbortRollout(t *testing.T) {
	sch := classRefTestScheme(t)
	const (
		ns        = "ns-a"
		instName  = "parent-instance"
		className = "bad-class"
	)
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status: computev1alpha1.FireboltInstanceStatus{
			MetadataEndpoint: "metadata.ns-a.svc.cluster.local:50051",
		},
	}
	class := classWithReadyCondition(className,
		metav1.ConditionFalse, reasonOperatorOwnedFieldSet,
		"spec.template.spec.containers[0].command: Forbidden: engine container command is operator-owned")

	phases := []computev1alpha1.EnginePhase{
		computev1alpha1.PhaseSwitching,
		computev1alpha1.PhaseDraining,
		computev1alpha1.PhaseCleaning,
	}
	for _, phase := range phases {
		t.Run(string(phase), func(t *testing.T) {
			engName := "engine-" + string(phase)
			engine := engineInRolloutPhaseFixture(engName, ns, className, phase)
			engine.Spec.InstanceRef = instName
			cli := fake.NewClientBuilder().
				WithScheme(sch).
				WithObjects(instance, engine, class).
				WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
				Build()
			r := engineRefTestReconciler(cli, sch)
			if _, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: engName, Namespace: ns},
			}); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			updated := &computev1alpha1.FireboltEngine{}
			if err := cli.Get(context.Background(), types.NamespacedName{Name: engName, Namespace: ns}, updated); err != nil {
				t.Fatalf("Get engine: %v", err)
			}
			cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
			if cond != nil && cond.Reason == reasonFireboltEngineClassUnready {
				t.Fatalf("Ready.Reason = %q in phase %s; fail-closed must not abort a rollout", cond.Reason, phase)
			}
			if updated.Status.Phase == phase && phase == computev1alpha1.PhaseDraining {
				t.Fatal("phase still draining: computeDraining did not run (no draining STS, so the pass should have advanced to cleaning)")
			}
			if updated.Status.Phase == phase && phase == computev1alpha1.PhaseCleaning {
				t.Fatal("phase still cleaning: computeCleaning did not run (should have returned to a terminal phase)")
			}
		})
	}
}
