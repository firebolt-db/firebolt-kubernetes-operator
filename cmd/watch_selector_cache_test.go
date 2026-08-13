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

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// TestScopeManagerCache_LiveCacheBehavior starts a real informer cache from
// the exact cache.Options scopeManagerCache produces and pins the four
// runtime behaviors the flag relies on, against the pinned
// controller-runtime version rather than a reading of its source:
//
//  1. a Firebolt CR carrying the excluded label never enters the cache;
//  2. the negated-existence selector admits unlabeled CRs;
//  3. --namespaces composes: the selector keeps filtering inside the
//     enumerated namespaces while other namespaces disappear entirely;
//  4. Secrets stay cached unfiltered, even when they carry the excluded
//     label — only the three Firebolt CRD types are selector-scoped.
func TestScopeManagerCache_LiveCacheBehavior(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		dir := firstEnvtestBinaryDir(t)
		if dir == "" {
			t.Skip("KUBEBUILDER_ASSETS not set and no bin/k8s assets found; run via make test")
		}
		testEnv.BinaryAssetsDirectory = dir
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	sch := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(sch); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme: %v", err)
	}
	if err := computev1alpha1.AddToScheme(sch); err != nil {
		t.Fatalf("computev1alpha1.AddToScheme: %v", err)
	}

	kcli, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		t.Fatalf("building fixture client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stamp := map[string]string{"example.com/managed": "proj-a"}
	newEngine := func(name, namespace string, labels map[string]string) *computev1alpha1.FireboltEngine {
		return &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst", Replicas: 1},
		}
	}
	fixtures := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "watchsel-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "watchsel-out"}},
	}
	fixtures = append(fixtures,
		newEngine("adopted", "watchsel-a", nil),
		newEngine("foreign", "watchsel-a", stamp),
		newEngine("adopted-out", "watchsel-out", nil),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "stamped-secret", Namespace: "watchsel-a", Labels: stamp},
			StringData: map[string]string{"k": "v"},
		},
	)
	for _, obj := range fixtures {
		if err := kcli.Create(ctx, obj); err != nil {
			t.Fatalf("creating fixture %s/%s: %v", obj.GetNamespace(), obj.GetName(), err)
		}
	}

	sel, err := parseWatchLabelSelector("!example.com/managed")
	if err != nil {
		t.Fatalf("parseWatchLabelSelector: %v", err)
	}

	// startScopedCache builds the options exactly as main does and brings
	// the resulting cache up; fixtures were created above, so the initial
	// list is complete once WaitForCacheSync returns.
	startScopedCache := func(t *testing.T, namespaces []string) cache.Cache {
		t.Helper()
		var opts ctrl.Options
		scopeManagerCache(&opts, namespaces, sel)
		opts.Cache.Scheme = sch
		c, err := cache.New(cfg, opts.Cache)
		if err != nil {
			t.Fatalf("cache.New: %v", err)
		}
		go func() {
			if err := c.Start(ctx); err != nil {
				t.Errorf("cache.Start: %v", err)
			}
		}()
		if !c.WaitForCacheSync(ctx) {
			t.Fatal("cache never synced")
		}
		return c
	}

	engineNames := func(t *testing.T, c cache.Cache) map[string]bool {
		t.Helper()
		var list computev1alpha1.FireboltEngineList
		if err := c.List(ctx, &list); err != nil {
			t.Fatalf("listing engines from cache: %v", err)
		}
		names := make(map[string]bool, len(list.Items))
		for i := range list.Items {
			names[list.Items[i].Name] = true
		}
		return names
	}

	t.Run("selector filters Firebolt CRs cluster-wide", func(t *testing.T) {
		c := startScopedCache(t, nil)
		names := engineNames(t, c)
		if !names["adopted"] || !names["adopted-out"] {
			t.Errorf("cache = %v, want the unlabeled engines adopted and adopted-out", names)
		}
		if names["foreign"] {
			t.Errorf("cache = %v, want the stamped engine foreign excluded", names)
		}
		var eng computev1alpha1.FireboltEngine
		err := c.Get(ctx, client.ObjectKey{Name: "foreign", Namespace: "watchsel-a"}, &eng)
		if !apierrors.IsNotFound(err) {
			t.Errorf("Get(foreign) = %v, want NotFound (excluded from the informer)", err)
		}
		var sec corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Name: "stamped-secret", Namespace: "watchsel-a"}, &sec); err != nil {
			t.Errorf("Get(stamped-secret) = %v, want success (Secrets are never selector-scoped)", err)
		}
	})

	t.Run("selector composes with namespaces", func(t *testing.T) {
		c := startScopedCache(t, []string{"watchsel-a"})
		names := engineNames(t, c)
		if !names["adopted"] {
			t.Errorf("cache = %v, want adopted visible inside the enumerated namespace", names)
		}
		if names["foreign"] {
			t.Errorf("cache = %v, want foreign excluded by the selector inside the enumerated namespace", names)
		}
		if names["adopted-out"] {
			t.Errorf("cache = %v, want adopted-out excluded by the namespace scope", names)
		}
		var sec corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Name: "stamped-secret", Namespace: "watchsel-a"}, &sec); err != nil {
			t.Errorf("Get(stamped-secret) = %v, want success (Secrets are never selector-scoped)", err)
		}
	})
}

// firstEnvtestBinaryDir mirrors the controller suite's fallback for IDE
// runs: pick the first control-plane assets directory setup-envtest has
// already downloaded under bin/k8s.
func firstEnvtestBinaryDir(t *testing.T) string {
	t.Helper()
	base := filepath.Join("..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}
