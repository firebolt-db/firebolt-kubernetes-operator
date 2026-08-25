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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func defaultsOnlyFixture(name, namespace string) *computev1alpha1.FireboltEnginePreset {
	return &computev1alpha1.FireboltEnginePreset{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: computev1alpha1.FireboltEnginePresetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{ServiceAccountName: name + "-sa"},
			},
		},
	}
}

func defaultsWithReadyCondition(name, namespace string, status metav1.ConditionStatus, reason, message string) *computev1alpha1.FireboltEnginePreset {
	d := defaultsOnlyFixture(name, namespace)
	apimeta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type:    computev1alpha1.FireboltEnginePresetConditionReady,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
	return d
}

func TestResolveFireboltEnginePresetInfo_AbsentIsOptional(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := engineRefTestReconciler(cli, sch)

	info, err := r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-a", ""))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if info != nil {
		t.Errorf("info = %+v, want nil when no Preset exist and requirePreset is unset", info)
	}
}

func TestResolveFireboltEnginePresetInfo_RequiredWhenMissing(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).Build()
	r := engineRefTestReconciler(cli, sch)

	eng := engineRefingClassFixture("e", "ns-a", "")
	eng.Spec.RequirePreset = ptr(true)
	_, err := r.resolveFireboltEnginePresetInfo(context.Background(), eng)
	if err == nil {
		t.Fatal("expected errFireboltEnginePresetRequired")
	}
	if !stderrors.Is(err, errFireboltEnginePresetRequired) {
		t.Errorf("error %q does not wrap errFireboltEnginePresetRequired", err)
	}
}

// TestResolveFireboltEnginePresetInfo_OffNameObjectIsNotSelected pins
// that resolution is a Get by the CEL-enforced fixed name: an object
// under any other name (only writable against a CRD missing the CEL
// rule) is not consumed, and its absence-equivalent keeps the
// requirePreset gate closed.
func TestResolveFireboltEnginePresetInfo_OffNameObjectIsNotSelected(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		defaultsOnlyFixture("shadow", "ns-a"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	info, err := r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-a", ""))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if info != nil {
		t.Errorf("info = %+v, want nil: an off-name object must not be selected", info)
	}

	eng := engineRefingClassFixture("e", "ns-a", "")
	eng.Spec.RequirePreset = ptr(true)
	_, err = r.resolveFireboltEnginePresetInfo(context.Background(), eng)
	if !stderrors.Is(err, errFireboltEnginePresetRequired) {
		t.Errorf("error %v does not wrap errFireboltEnginePresetRequired for an off-name-only namespace", err)
	}
}

func TestResolveFireboltEnginePresetInfo_BlocksOnOperatorOwnedFieldSet(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		defaultsWithReadyCondition("firebolt", "ns-a",
			metav1.ConditionFalse, reasonOperatorOwnedFieldSet,
			"spec.template.spec.subdomain: Forbidden"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	_, err := r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-a", ""))
	if err == nil {
		t.Fatal("expected errFireboltEnginePresetUnready")
	}
	if !stderrors.Is(err, errFireboltEnginePresetUnready) {
		t.Errorf("error %q does not wrap errFireboltEnginePresetUnready", err)
	}
	if !strings.Contains(err.Error(), "firebolt") || !strings.Contains(err.Error(), "ns-a") {
		t.Errorf("error %q should name the object", err)
	}
}

func TestResolveFireboltEnginePresetInfo_PassesOnReadyAndMissingCondition(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		defaultsWithReadyCondition("firebolt", "ns-a",
			metav1.ConditionTrue, reasonAdmissible, "ok"),
	).Build()
	r := engineRefTestReconciler(cli, sch)

	info, err := r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-a", ""))
	if err != nil {
		t.Fatalf("ready Preset: %v", err)
	}
	if info == nil || info.Name != "firebolt" || info.Hash == "" {
		t.Errorf("info = %+v, want name+hash", info)
	}

	cli = fake.NewClientBuilder().WithScheme(sch).WithObjects(
		defaultsOnlyFixture("firebolt", "ns-b"),
	).Build()
	r = engineRefTestReconciler(cli, sch)
	info, err = r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-b", ""))
	if err != nil {
		t.Fatalf("missing Ready: %v", err)
	}
	if info == nil || info.Name != "firebolt" {
		t.Errorf("info = %+v, want the freshly created object despite its missing Ready condition", info)
	}
}

func TestResolveFireboltEnginePresetInfo_PassesOnDeletionBlocked(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		defaultsWithReadyCondition("firebolt", "ns-a",
			metav1.ConditionFalse, reasonDeletionBlocked, "held"),
	).Build()
	r := engineRefTestReconciler(cli, sch)
	info, err := r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-a", ""))
	if err != nil {
		t.Fatalf("DeletionBlocked: %v", err)
	}
	if info == nil {
		t.Fatal("info = nil, want non-nil so engines keep reconciling")
	}
}

func defaultsWithReservedEngineEnv(name, namespace string) *computev1alpha1.FireboltEnginePreset {
	d := defaultsOnlyFixture(name, namespace)
	d.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: computev1alpha1.EngineContainerName,
		Env:  []corev1.EnvVar{{Name: "POD_INDEX", Value: "0"}},
	}}
	return d
}

func TestResolveFireboltEnginePresetInfo_BlocksOnLiveOwnedFieldsWithoutReady(t *testing.T) {
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(
		defaultsWithReservedEngineEnv("firebolt", "ns-a"),
	).Build()
	r := engineRefTestReconciler(cli, sch)
	_, err := r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-a", ""))
	if err == nil {
		t.Fatal("expected errFireboltEnginePresetUnready when live spec has reserved env and Ready is unset")
	}
	if !stderrors.Is(err, errFireboltEnginePresetUnready) {
		t.Errorf("error %q does not wrap errFireboltEnginePresetUnready", err)
	}
	if !strings.Contains(err.Error(), "POD_INDEX") {
		t.Errorf("error %q should name the reserved env key", err)
	}
}

func TestResolveFireboltEnginePresetInfo_BlocksOnLiveOwnedFieldsWhileDeletionBlocked(t *testing.T) {
	d := defaultsWithReservedEngineEnv("firebolt", "ns-a")
	apimeta.SetStatusCondition(&d.Status.Conditions, metav1.Condition{
		Type:    computev1alpha1.FireboltEnginePresetConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  reasonDeletionBlocked,
		Message: "held",
	})
	sch := classRefTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).WithObjects(d).Build()
	r := engineRefTestReconciler(cli, sch)
	_, err := r.resolveFireboltEnginePresetInfo(context.Background(), engineRefingClassFixture("e", "ns-a", ""))
	if err == nil {
		t.Fatal("expected errFireboltEnginePresetUnready when DeletionBlocked Preset spec carries reserved env")
	}
	if !stderrors.Is(err, errFireboltEnginePresetUnready) {
		t.Errorf("error %q does not wrap errFireboltEnginePresetUnready", err)
	}
}

func TestEngineReconcile_RequiredPresetSurfacesCondition(t *testing.T) {
	sch := classRefTestScheme(t)
	const ns, instName, engName = "ns-a", "parent-instance", "engine-blocked"
	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: instName, Namespace: ns},
		Spec:       computev1alpha1.FireboltInstanceSpec{ID: "01H000000000000000000DUMMY"},
		Status:     computev1alpha1.FireboltInstanceStatus{MetadataEndpoint: "metadata.ns-a.svc:50051"},
	}
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       engName,
			Namespace:  ns,
			Finalizers: []string{finalizerName},
			Generation: 1,
		},
		Spec: computev1alpha1.FireboltEngineSpec{
			InstanceRef:   instName,
			Replicas:      1,
			RequirePreset: ptr(true),
		},
		Status: computev1alpha1.FireboltEngineStatus{
			Phase:             computev1alpha1.PhaseCreating,
			AppliedPresetName: "stale-preset",
			AppliedPresetHash: "stale-hash",
		},
	}
	cli := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(instance, engine).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &computev1alpha1.FireboltInstance{}).
		Build()
	r := engineRefTestReconciler(cli, sch)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: client.ObjectKey{Name: engName, Namespace: ns},
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	updated := &computev1alpha1.FireboltEngine{}
	if err := cli.Get(context.Background(), client.ObjectKey{Name: engName, Namespace: ns}, updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, computev1alpha1.ConditionReady)
	if cond == nil {
		t.Fatal("Ready condition missing")
	}
	if cond.Reason != reasonFireboltEnginePresetRequired {
		t.Errorf("Ready.Reason = %q, want %s", cond.Reason, reasonFireboltEnginePresetRequired)
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("Ready.Status = %s, want False", cond.Status)
	}
	if updated.Status.AppliedPresetName != "" || updated.Status.AppliedPresetHash != "" {
		t.Errorf("AppliedPreset = %q/%q, want cleared when Preset resolve fails closed",
			updated.Status.AppliedPresetName, updated.Status.AppliedPresetHash)
	}
}
