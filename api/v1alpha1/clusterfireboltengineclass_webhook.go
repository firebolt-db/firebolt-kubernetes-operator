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

	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ClusterFireboltEngineClassCustomValidator validates ClusterFireboltEngineClass
// at admission. Create and Update reject operator-owned paths and
// namespace-resolved identifiers. Delete is a no-op: the catalog is
// shared; engines that still name a deleted object fail-closed on
// not-found and keep serving the last applied pods.
//
// +kubebuilder:object:generate=false
type ClusterFireboltEngineClassCustomValidator struct{}

var _ admission.Validator[*ClusterFireboltEngineClass] = &ClusterFireboltEngineClassCustomValidator{}

// SetupClusterFireboltEngineClassWebhookWithManager registers the validating webhook.
func SetupClusterFireboltEngineClassWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ClusterFireboltEngineClass{}).
		WithValidator(&ClusterFireboltEngineClassCustomValidator{}).
		Complete()
}

// ValidateCreate rejects operator-owned paths and namespace-resolved
// identifiers on a new ClusterFireboltEngineClass.
func (v *ClusterFireboltEngineClassCustomValidator) ValidateCreate(
	_ context.Context, cc *ClusterFireboltEngineClass,
) (admission.Warnings, error) {
	return nil, validateClusterFireboltEngineClassSpec(cc).ToAggregate()
}

// ValidateUpdate enforces the same SKU-only and operator-owned-path
// rejection set as ValidateCreate.
func (v *ClusterFireboltEngineClassCustomValidator) ValidateUpdate(
	_ context.Context, _, cc *ClusterFireboltEngineClass,
) (admission.Warnings, error) {
	return nil, validateClusterFireboltEngineClassSpec(cc).ToAggregate()
}

// ValidateDelete is a no-op. Catalog delete is an admin/plane action;
// engines that still resolve to the name surface EngineClassNotFound.
func (v *ClusterFireboltEngineClassCustomValidator) ValidateDelete(
	_ context.Context, _ *ClusterFireboltEngineClass,
) (admission.Warnings, error) {
	return nil, nil
}

func validateClusterFireboltEngineClassSpec(cc *ClusterFireboltEngineClass) field.ErrorList {
	base := field.NewPath("spec", "template")
	errs := ValidateOperatorOwnedPodTemplate(&cc.Spec.Template, base)
	return append(errs, ValidateClusterEngineClassSKUOnly(&cc.Spec.Template, base)...)
}
