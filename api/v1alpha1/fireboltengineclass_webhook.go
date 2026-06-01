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
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// FireboltEngineClassCustomValidator implements the validating admission webhook
// for FireboltEngineClass. The webhook rejects any spec.template input that lands
// at a path the operator manages exclusively (the engine container's
// identity / command / probes / reserved env keys; pod-template metadata
// keys under firebolt.io/; pod-level fields tied to the StatefulSet and
// headless-DNS contracts). Sidecar containers and additional init
// containers pass through unconstrained.
//
// Deletion is rejected while at least one FireboltEngine in the same
// namespace references the class. The check lists FireboltEngines live
// via Reader at admission time (rather than reading status.boundEngines)
// so a class bound between reconciler runs still refuses deletion.
// Configured failurePolicy: Fail so a webhook outage cannot open a
// deletion window that would orphan referencing engines.
//
// +kubebuilder:object:generate=false
type FireboltEngineClassCustomValidator struct {
	// Reader is an uncached API client used by ValidateDelete to list
	// FireboltEngines at admission time. Wired from mgr.GetAPIReader() so
	// the gate is consistent with API-server state and not subject to
	// status / cache staleness.
	Reader client.Reader
}

var _ webhook.CustomValidator = &FireboltEngineClassCustomValidator{}

// SetupFireboltEngineClassWebhookWithManager registers the validating webhook
// with the manager. FireboltEngineClass has no defaulting webhook today:
// every kubebuilder default is enforced via openapi schema rather than
// mutating admission, so a Default() implementation would have nothing to do.
func SetupFireboltEngineClassWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&FireboltEngineClass{}).
		WithValidator(&FireboltEngineClassCustomValidator{Reader: mgr.GetAPIReader()}).
		Complete()
}

// ValidateCreate runs on every FireboltEngineClass create. It enforces the
// operator-owned-path rejection set against spec.template via
// ValidateOperatorOwnedPodTemplate, returning every violation in one
// admission response so users see all errors at once.
func (v *FireboltEngineClassCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	ec, ok := obj.(*FireboltEngineClass)
	if !ok {
		return nil, fmt.Errorf("expected FireboltEngineClass, got %T", obj)
	}
	return nil, validateFireboltEngineClassSpec(ec).ToAggregate()
}

// ValidateUpdate enforces the same path rejection set as ValidateCreate.
// FireboltEngineClass spec is mutable (per the FB-1145 design call), but
// every rejected path stays rejected: a typo that admission caught at
// Create must still be caught on a subsequent Update.
func (v *FireboltEngineClassCustomValidator) ValidateUpdate(
	_ context.Context, _, newObj runtime.Object,
) (admission.Warnings, error) {
	ec, ok := newObj.(*FireboltEngineClass)
	if !ok {
		return nil, fmt.Errorf("expected FireboltEngineClass, got %T", newObj)
	}
	return nil, validateFireboltEngineClassSpec(ec).ToAggregate()
}

// ValidateDelete rejects deletion while at least one FireboltEngine in
// the same namespace references this class via spec.engineClassRef.
// FireboltEngineClass is namespaced so engineClassRef resolves in the
// engine's own namespace; only engines in the class's namespace can bind
// to it, and only those count toward the deletion gate.
//
// The count is recomputed live from the API server, not read from
// status.boundEngines, because status defaults to zero on a freshly
// created class — relying on it would open a race where the class can
// be deleted between the moment an engine first references it and the
// next reconcile that increments the field.
//
// Configured failurePolicy: Fail on the ValidatingWebhookConfiguration
// so that a webhook outage cannot bypass this guard. List errors
// propagate as admission errors for the same reason: better to refuse
// the delete than to admit it on incomplete information.
func (v *FireboltEngineClassCustomValidator) ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	ec, ok := obj.(*FireboltEngineClass)
	if !ok {
		return nil, fmt.Errorf("expected FireboltEngineClass, got %T", obj)
	}
	if v.Reader == nil {
		return nil, errors.New("FireboltEngineClass delete webhook has no API reader configured")
	}
	var engines FireboltEngineList
	if err := v.Reader.List(ctx, &engines, client.InNamespace(ec.Namespace)); err != nil {
		return nil, fmt.Errorf("listing FireboltEngines in namespace %q to check class references: %w", ec.Namespace, err)
	}
	var count int
	for i := range engines.Items {
		ref := engines.Items[i].Spec.EngineClassRef
		if ref != nil && *ref == ec.Name {
			count++
		}
	}
	if count == 0 {
		return nil, nil
	}
	return nil, field.Forbidden(
		field.NewPath("metadata", "name"),
		fmt.Sprintf(
			"FireboltEngineClass %q in namespace %q is referenced by %d FireboltEngine(s); "+
				"clear spec.engineClassRef on those engines before deleting the class",
			ec.Name, ec.Namespace, count),
	)
}

// validateFireboltEngineClassSpec is the central spec check called by
// ValidateCreate and ValidateUpdate. Returning a field.ErrorList lets the
// caller convert all errors into one aggregate response without losing
// per-field paths.
func validateFireboltEngineClassSpec(ec *FireboltEngineClass) field.ErrorList {
	return ValidateOperatorOwnedPodTemplate(&ec.Spec.Template, field.NewPath("spec", "template"))
}
