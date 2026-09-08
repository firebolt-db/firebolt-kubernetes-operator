// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// seedEngineRoutingFixture materializes API identities and an explicitly empty
// gateway registry for component tests that do not run gateway processes. These
// tests still execute the production withdrawal guard; no holder is fabricated.
func seedEngineRoutingFixture(t *testing.T, c client.Client, engine *computev1alpha1.FireboltEngine) {
	t.Helper()
	ctx := context.Background()
	live := &computev1alpha1.FireboltEngine{}
	err := c.Get(ctx, client.ObjectKeyFromObject(engine), live)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	if engine.UID == "" {
		engine.UID = types.UID("fixture-engine-" + engine.Name)
	}
	if engine.Spec.InstanceRef == "" {
		engine.Spec.InstanceRef = "routing-instance"
	}
	if apierrors.IsNotFound(err) {
		if err := c.Create(ctx, engine.DeepCopy()); err != nil {
			t.Fatal(err)
		}
	} else {
		changed := false
		if live.UID == "" {
			live.UID = engine.UID
			changed = true
		}
		if live.Spec.InstanceRef == "" {
			live.Spec.InstanceRef = engine.Spec.InstanceRef
			changed = true
		}
		if changed {
			if err := c.Update(ctx, live); err != nil {
				t.Fatal(err)
			}
		}
	}
	instance := &computev1alpha1.FireboltInstance{}
	key := client.ObjectKey{Namespace: engine.Namespace, Name: engine.Spec.InstanceRef}
	err = c.Get(ctx, key, instance)
	switch {
	case apierrors.IsNotFound(err):
		instance = &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: types.UID("fixture-instance-" + key.Name)}}
		if err := c.Create(ctx, instance); err != nil {
			t.Fatal(err)
		}
	case err != nil:
		t.Fatal(err)
	case instance.UID == "":
		instance.UID = types.UID("fixture-instance-" + key.Name)
		if err := c.Update(ctx, instance); err != nil {
			t.Fatal(err)
		}
	}
	cm := &corev1.ConfigMap{}
	key.Name = routing.ConfigMapName(instance.Name)
	err = c.Get(ctx, key, cm)
	if apierrors.IsNotFound(err) {
		data, err := routing.Encode(routing.NewState(string(instance.UID)))
		if err != nil {
			t.Fatal(err)
		}
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Data: map[string]string{routing.DataKey: string(data)}}
		if err := c.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	// Fake clients do not populate the caller's resource version when their
	// preloaded object is updated through a separate Get/Update pair.
	if err := c.Get(ctx, client.ObjectKeyFromObject(engine), live); err != nil {
		t.Fatal(err)
	}
	engine.ResourceVersion = live.ResourceVersion
}

func runGatewayAwareGC(ctx context.Context, t *testing.T, r *FireboltEngineReconciler, engine *computev1alpha1.FireboltEngine) bool {
	t.Helper()
	seedEngineRoutingFixture(t, r.Client, engine)
	if live, ok := r.APIReader.(client.Client); ok {
		seedEngineRoutingFixture(t, live, engine)
	}
	return r.gcOrphanedResources(ctx, engine)
}

// seedSwitchingGeneration supplies the already-created target implied by a
// fixture that starts in Switching, after the Creating readiness gate.
func seedSwitchingGeneration(t *testing.T, c client.Client, engine *computev1alpha1.FireboltEngine) {
	t.Helper()
	if engine.Status.Phase != computev1alpha1.PhaseSwitching {
		return
	}
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: engine.Namespace, Name: genResourceName(engine.Name, engine.Status.CurrentGeneration, "")}
	err := c.Get(context.Background(), key, sts)
	switch {
	case apierrors.IsNotFound(err):
		sts = &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec:   appsv1.StatefulSetSpec{Replicas: ptr(engine.Spec.Replicas)},
			Status: appsv1.StatefulSetStatus{ReadyReplicas: engine.Spec.Replicas}}
		sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: computev1alpha1.GroupVersion.String(), Kind: "FireboltEngine", Name: engine.Name, UID: engine.UID, Controller: ptr(true)}}
		if err := c.Create(context.Background(), sts); err != nil {
			t.Fatal(err)
		}
	case err != nil:
		t.Fatal(err)
	case metav1.GetControllerOf(sts) == nil:
		sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: computev1alpha1.GroupVersion.String(), Kind: "FireboltEngine", Name: engine.Name, UID: engine.UID, Controller: ptr(true)}}
		if err := c.Update(context.Background(), sts); err != nil {
			t.Fatal(err)
		}
	}
}
