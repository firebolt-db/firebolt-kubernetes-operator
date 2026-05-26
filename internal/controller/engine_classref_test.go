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
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	enginemetrics "github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

// engineRefingClassFixture returns a FireboltEngine in the given
// namespace referencing the named EngineClass. classRef == "" produces
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

// classOnlyFixture returns an EngineClass in the given namespace with a
// minimal user-allowed template (ServiceAccountName). Used by lookup
// tests that don't care about the rendered pod spec, only that
// resolveEngineClassInfo finds (or does not find) the class.
func classOnlyFixture(name, namespace string) *computev1alpha1.EngineClass {
	return &computev1alpha1.EngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: computev1alpha1.EngineClassSpec{
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

// TestResolveEngineClassInfo_NamespacedLookup pins down the
// namespace-coupled resolver: an EngineClass with the right name in a
// different namespace must NOT satisfy spec.engineClassRef. Kubernetes
// resolves the reference in the engine's own namespace; the resolver
// must agree.
func TestResolveEngineClassInfo_NamespacedLookup(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		// Class exists in "ns-a" only.
		classOnlyFixture("compute-optimized", "ns-a"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	t.Run("same-namespace engine resolves", func(t *testing.T) {
		eng := engineRefingClassFixture("e", "ns-a", "compute-optimized")
		info, err := r.resolveEngineClassInfo(context.Background(), eng)
		if err != nil {
			t.Fatalf("resolveEngineClassInfo: %v", err)
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
		_, err := r.resolveEngineClassInfo(context.Background(), eng)
		if err == nil {
			t.Fatal("resolveEngineClassInfo: expected error for cross-namespace reference, got nil")
		}
		if !strings.Contains(err.Error(), "ns-b") {
			t.Errorf("error %q does not name the engine's namespace", err.Error())
		}
	})

	t.Run("nil ref returns nil info", func(t *testing.T) {
		eng := engineRefingClassFixture("e", "ns-a", "")
		info, err := r.resolveEngineClassInfo(context.Background(), eng)
		if err != nil {
			t.Fatalf("resolveEngineClassInfo: %v", err)
		}
		if info != nil {
			t.Errorf("info = %+v, want nil for engine without engineClassRef", info)
		}
	})
}

// TestEngineClassToEngines_NamespaceScoped pins down the watch handler:
// a class event in namespace X enqueues only engines in namespace X
// that reference the class by name. Cross-namespace engines with
// matching ref are ignored — they could not have admitted (per the
// FireboltEngine validating webhook) and cannot resolve at reconcile
// time anyway.
func TestEngineClassToEngines_NamespaceScoped(t *testing.T) {
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

	class := &computev1alpha1.EngineClass{
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
	sort.Strings(gotNames)
	want := []string{"a", "b"}
	if len(gotNames) != len(want) || gotNames[0] != want[0] || gotNames[1] != want[1] {
		t.Errorf("enqueued engines = %v, want %v (cross-namespace engine e must be filtered out)", gotNames, want)
	}
}

// TestEngineClassToEngines_HonorsNamespaceFilter pins down the
// interaction with the reconciler's optional namespace filter
// (--watch-namespace). A class event for a namespace outside the
// filter must produce zero requests, otherwise the reconciler would
// try to reconcile engines it does not have RBAC for.
func TestEngineClassToEngines_HonorsNamespaceFilter(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		engineRefingClassFixture("a", "ns-a", "compute-optimized"),
	).Build()
	r := engineRefTestReconciler(cli, sch)
	r.Namespace = "ns-a"

	// Class event from a different namespace than the filter — must
	// produce no requests even though a matching engine in ns-a exists.
	classOutside := &computev1alpha1.EngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-optimized", Namespace: "ns-b"},
	}
	if got := r.engineClassToEngines(context.Background(), classOutside); len(got) != 0 {
		t.Errorf("expected zero requests for a class outside the watch namespace, got %v", got)
	}

	// Class event inside the filter still works.
	classInside := &computev1alpha1.EngineClass{
		ObjectMeta: metav1.ObjectMeta{Name: "compute-optimized", Namespace: "ns-a"},
	}
	got := r.engineClassToEngines(context.Background(), classInside)
	if len(got) != 1 || got[0].Name != "a" || got[0].Namespace != "ns-a" {
		t.Errorf("expected one request for engine a/ns-a, got %v", got)
	}
}
