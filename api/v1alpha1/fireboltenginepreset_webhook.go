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

// FireboltEnginePresetCustomValidator implements the validating
// admission webhook for FireboltEnginePreset. Create and Update
// reject operator-owned pod-template paths via
// ValidateOperatorOwnedPodTemplate. The one-object-per-namespace
// invariant needs no webhook: the CRD CEL rule pins metadata.name to
// "firebolt", so apiserver name uniqueness enforces it in every
// admission posture. Delete is refused while at least one
// FireboltEngine exists in the same namespace: Preset is ambient and
// every engine in the namespace merges it.
//
// +kubebuilder:object:generate=false
type FireboltEnginePresetCustomValidator struct {
	Reader client.Reader
}

var _ admission.Validator[*FireboltEnginePreset] = &FireboltEnginePresetCustomValidator{}

// SetupFireboltEnginePresetWebhookWithManager registers the validating
// webhook. There is no defaulting webhook: kubebuilder defaults are
// enforced via OpenAPI schema.
func SetupFireboltEnginePresetWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &FireboltEnginePreset{}).
		WithValidator(&FireboltEnginePresetCustomValidator{Reader: mgr.GetAPIReader()}).
		Complete()
}

// ValidateCreate rejects operator-owned paths on spec.template.
func (v *FireboltEnginePresetCustomValidator) ValidateCreate(_ context.Context, d *FireboltEnginePreset) (admission.Warnings, error) {
	return nil, validateFireboltEnginePresetSpec(d).ToAggregate()
}

// ValidateUpdate enforces the same operator-owned-path rejection set
// as ValidateCreate.
func (v *FireboltEnginePresetCustomValidator) ValidateUpdate(
	_ context.Context, _, d *FireboltEnginePreset,
) (admission.Warnings, error) {
	return nil, validateFireboltEnginePresetSpec(d).ToAggregate()
}

// ValidateDelete refuses deletion while any FireboltEngine exists in
// the same namespace. Preset is ambient, so the guard is the
// namespace engine count rather than a named reference.
func (v *FireboltEnginePresetCustomValidator) ValidateDelete(ctx context.Context, d *FireboltEnginePreset) (admission.Warnings, error) {
	if v.Reader == nil {
		return nil, errors.New("FireboltEnginePreset delete webhook has no API reader configured")
	}
	var engines FireboltEngineList
	if err := v.Reader.List(ctx, &engines, client.InNamespace(d.Namespace)); err != nil {
		return nil, fmt.Errorf("listing FireboltEngines in namespace %q to check Preset bindings: %w", d.Namespace, err)
	}
	count := len(engines.Items)
	if count == 0 {
		return nil, nil
	}
	return nil, field.Forbidden(
		field.NewPath("metadata", "name"),
		fmt.Sprintf(
			"%d FireboltEngine(s) in namespace %q consume FireboltEnginePreset %q as their ambient overlay; "+
				"delete those engines before deleting the Preset object",
			count, d.Namespace, d.Name),
	)
}

func validateFireboltEnginePresetSpec(d *FireboltEnginePreset) field.ErrorList {
	return ValidateOperatorOwnedPodTemplate(&d.Spec.Template, field.NewPath("spec", "template"))
}
