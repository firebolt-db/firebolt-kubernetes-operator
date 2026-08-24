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

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// FireboltEngineDefaultsCustomValidator implements the validating
// admission webhook for FireboltEngineDefaults. Create and Update
// reject operator-owned pod-template paths via
// ValidateOperatorOwnedPodTemplate. Create also refuses a second
// object in the same namespace (v1 admits at most one). Delete is
// refused while at least one FireboltEngine exists in the same
// namespace: Defaults is ambient and every engine in the namespace
// merges it.
//
// +kubebuilder:object:generate=false
type FireboltEngineDefaultsCustomValidator struct {
	Reader client.Reader
}

var _ admission.Validator[*FireboltEngineDefaults] = &FireboltEngineDefaultsCustomValidator{}

// SetupFireboltEngineDefaultsWebhookWithManager registers the validating
// webhook. There is no defaulting webhook: kubebuilder defaults are
// enforced via OpenAPI schema.
func SetupFireboltEngineDefaultsWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &FireboltEngineDefaults{}).
		WithValidator(&FireboltEngineDefaultsCustomValidator{Reader: mgr.GetAPIReader()}).
		Complete()
}

// ValidateCreate rejects operator-owned paths on spec.template and
// refuses a second FireboltEngineDefaults in the same namespace.
func (v *FireboltEngineDefaultsCustomValidator) ValidateCreate(ctx context.Context, d *FireboltEngineDefaults) (admission.Warnings, error) {
	if errs := validateFireboltEngineDefaultsSpec(d); len(errs) > 0 {
		return nil, errs.ToAggregate()
	}
	return nil, v.validateUniqueInNamespace(ctx, d)
}

// ValidateUpdate enforces the same operator-owned-path rejection set
// as ValidateCreate.
func (v *FireboltEngineDefaultsCustomValidator) ValidateUpdate(
	_ context.Context, _, d *FireboltEngineDefaults,
) (admission.Warnings, error) {
	return nil, validateFireboltEngineDefaultsSpec(d).ToAggregate()
}

// ValidateDelete refuses deletion while any FireboltEngine exists in
// the same namespace. Defaults is ambient, so the guard is the
// namespace engine count rather than a named reference.
func (v *FireboltEngineDefaultsCustomValidator) ValidateDelete(ctx context.Context, d *FireboltEngineDefaults) (admission.Warnings, error) {
	if v.Reader == nil {
		return nil, errors.New("FireboltEngineDefaults delete webhook has no API reader configured")
	}
	var engines FireboltEngineList
	if err := v.Reader.List(ctx, &engines, client.InNamespace(d.Namespace)); err != nil {
		return nil, fmt.Errorf("listing FireboltEngines in namespace %q to check Defaults bindings: %w", d.Namespace, err)
	}
	count := len(engines.Items)
	if count == 0 {
		return nil, nil
	}
	return nil, field.Forbidden(
		field.NewPath("metadata", "name"),
		fmt.Sprintf(
			"FireboltEngineDefaults %q in namespace %q is referenced by %d FireboltEngine(s); "+
				"delete those engines before deleting the Defaults object",
			d.Name, d.Namespace, count),
	)
}

func validateFireboltEngineDefaultsSpec(d *FireboltEngineDefaults) field.ErrorList {
	return ValidateOperatorOwnedPodTemplate(&d.Spec.Template, field.NewPath("spec", "template"))
}

// validateUniqueInNamespace refuses create when another
// FireboltEngineDefaults already exists in the same namespace. The
// incoming object is not stored yet, so any listed peer is a
// conflict. An object of the same name is skipped so a replayed
// create of the admitted object is not rejected as a sibling.
func (v *FireboltEngineDefaultsCustomValidator) validateUniqueInNamespace(ctx context.Context, d *FireboltEngineDefaults) error {
	if v.Reader == nil {
		return errors.New("FireboltEngineDefaults create webhook has no API reader configured")
	}
	var list FireboltEngineDefaultsList
	if err := v.Reader.List(ctx, &list, client.InNamespace(d.Namespace)); err != nil {
		return fmt.Errorf("listing FireboltEngineDefaults in namespace %q to enforce at most one: %w", d.Namespace, err)
	}
	for i := range list.Items {
		existing := &list.Items[i]
		if existing.Name == d.Name {
			continue
		}
		return field.Forbidden(
			field.NewPath("metadata", "name"),
			fmt.Sprintf(
				"FireboltEngineDefaults %q already exists in namespace %q; v1 admits at most one object per namespace",
				existing.Name, d.Namespace),
		)
	}
	return nil
}
