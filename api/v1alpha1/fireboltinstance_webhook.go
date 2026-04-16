/*
Copyright 2025.

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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// FireboltInstanceCustomValidator validates FireboltInstance resources.
type FireboltInstanceCustomValidator struct{}

var _ webhook.CustomValidator = &FireboltInstanceCustomValidator{}

// SetupFireboltInstanceWebhookWithManager registers the validating webhook with the manager.
func SetupFireboltInstanceWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&FireboltInstance{}).
		WithValidator(&FireboltInstanceCustomValidator{}).
		Complete()
}

// ValidateCreate validates a FireboltInstance on creation.
func (v *FireboltInstanceCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	inst, ok := obj.(*FireboltInstance)
	if !ok {
		return nil, fmt.Errorf("expected FireboltInstance, got %T", obj)
	}
	return nil, validateMetadataReplicas(inst)
}

// ValidateUpdate validates a FireboltInstance on update.
func (v *FireboltInstanceCustomValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	inst, ok := newObj.(*FireboltInstance)
	if !ok {
		return nil, fmt.Errorf("expected FireboltInstance, got %T", newObj)
	}
	return nil, validateMetadataReplicas(inst)
}

// ValidateDelete validates a FireboltInstance on deletion.
func (v *FireboltInstanceCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func validateMetadataReplicas(inst *FireboltInstance) error {
	r := inst.Spec.Metadata.Replicas
	if r != nil && *r != 1 {
		return field.Invalid(
			field.NewPath("spec", "metadata", "replicas"),
			*r,
			"metadata replicas must be 1; multi-replica metadata is not currently supported",
		)
	}
	return nil
}
