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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// FireboltEngineCustomValidator validates FireboltEngine resources at
// admission time. The only cross-resource check is that
// spec.engineClassRef, when set, points to an EngineClass that exists
// in the engine's own namespace — the reference is hard-rejected so
// users see a typo (or a class-applied-in-the-wrong-namespace mistake)
// immediately at apply time rather than via engine status. Apply
// ordering matters: an EngineClass must exist in the same namespace
// before any FireboltEngine that references it (GitOps tooling such as
// Argo CD sync-waves or Flux dependsOn handles this in practice).
//
// The validator reads through mgr.GetAPIReader (live, non-cached) because
// the informer cache may not yet have the EngineClass at the moment of
// admission — particularly in `kubectl apply -f class.yaml -f engine.yaml`
// where both objects land within the same poll interval.
//
// +kubebuilder:object:generate=false
type FireboltEngineCustomValidator struct {
	Reader client.Reader
}

var _ webhook.CustomValidator = &FireboltEngineCustomValidator{}

// SetupFireboltEngineWebhookWithManager wires the validator into the
// manager's webhook server. The validator holds an APIReader rather than
// the cached Client because admission must reflect the live API state.
func SetupFireboltEngineWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&FireboltEngine{}).
		WithValidator(&FireboltEngineCustomValidator{Reader: mgr.GetAPIReader()}).
		Complete()
}

// ValidateCreate rejects a new FireboltEngine when spec.engineClassRef
// references an EngineClass that does not exist. Existence-only check;
// the EngineClass's own webhook handles spec validity.
func (v *FireboltEngineCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	eng, ok := obj.(*FireboltEngine)
	if !ok {
		return nil, fmt.Errorf("expected FireboltEngine, got %T", obj)
	}
	return nil, v.validateEngineClassRef(ctx, eng).ToAggregate()
}

// ValidateUpdate enforces the same existence check as ValidateCreate.
// Symmetric handling matches FB-1145: a typo on edit deserves the same
// immediate feedback as a typo on create. Recovery from a broken state
// (class deleted somehow) is always possible by setting
// spec.engineClassRef to nil or to another existing class — both pass
// validation.
func (v *FireboltEngineCustomValidator) ValidateUpdate(
	ctx context.Context, _, newObj runtime.Object,
) (admission.Warnings, error) {
	eng, ok := newObj.(*FireboltEngine)
	if !ok {
		return nil, fmt.Errorf("expected FireboltEngine, got %T", newObj)
	}
	return nil, v.validateEngineClassRef(ctx, eng).ToAggregate()
}

// ValidateDelete is a no-op. The engine has no cross-resource invariants
// to enforce on deletion; the controller cleans up generation-scoped
// resources via owner references.
func (v *FireboltEngineCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateEngineClassRef returns field.NotFound when spec.engineClassRef
// names an EngineClass that does not exist in the engine's namespace.
// EngineClass is namespaced; the lookup is therefore scoped to
// engine.Namespace, matching how Kubernetes will resolve the reference
// at reconcile time. A nil ref is allowed (the engine falls back to
// operator defaults). Any non-NotFound API error surfaces as a generic
// internal error so the user can retry once the API server / RBAC
// issue clears.
func (v *FireboltEngineCustomValidator) validateEngineClassRef(ctx context.Context, eng *FireboltEngine) field.ErrorList {
	if eng.Spec.EngineClassRef == nil || *eng.Spec.EngineClassRef == "" {
		return nil
	}
	classPath := field.NewPath("spec", "engineClassRef")
	class := &EngineClass{}
	key := client.ObjectKey{Name: *eng.Spec.EngineClassRef, Namespace: eng.Namespace}
	if err := v.Reader.Get(ctx, key, class); err != nil {
		if apierrors.IsNotFound(err) {
			return field.ErrorList{field.NotFound(classPath, *eng.Spec.EngineClassRef)}
		}
		return field.ErrorList{field.InternalError(classPath, fmt.Errorf("looking up EngineClass: %w", err))}
	}
	return nil
}
