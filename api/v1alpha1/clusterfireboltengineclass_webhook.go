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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// ClusterFireboltEngineClassCustomValidator validates ClusterFireboltEngineClass
// at admission. Create and Update reject operator-owned paths and
// namespace-resolved identifiers. Delete is refused while any engine in
// any namespace has resolved to this cluster object.
//
// +kubebuilder:object:generate=false
type ClusterFireboltEngineClassCustomValidator struct {
	Reader client.Reader
}

var _ admission.Validator[*ClusterFireboltEngineClass] = &ClusterFireboltEngineClassCustomValidator{}

// SetupClusterFireboltEngineClassWebhookWithManager registers the validating webhook.
func SetupClusterFireboltEngineClassWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ClusterFireboltEngineClass{}).
		WithValidator(&ClusterFireboltEngineClassCustomValidator{Reader: mgr.GetAPIReader()}).
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

// ValidateDelete refuses deletion while any engine in any namespace has
// resolved to this catalog object.
func (v *ClusterFireboltEngineClassCustomValidator) ValidateDelete(
	ctx context.Context, cc *ClusterFireboltEngineClass,
) (admission.Warnings, error) {
	if v.Reader == nil {
		return nil, errors.New("ClusterFireboltEngineClass delete webhook has no API reader configured")
	}
	count, err := countEnginesResolvedToClusterClass(ctx, v.Reader, cc.Name)
	if err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, nil
	}
	return nil, field.Forbidden(
		field.NewPath("metadata", "name"),
		fmt.Sprintf(
			"ClusterFireboltEngineClass %q is resolved by %d FireboltEngine(s); "+
				"clear spec.engineClassRef on those engines before deleting the catalog object",
			cc.Name, count),
	)
}

func validateClusterFireboltEngineClassSpec(cc *ClusterFireboltEngineClass) field.ErrorList {
	base := field.NewPath("spec", "template")
	errs := ValidateOperatorOwnedPodTemplate(&cc.Spec.Template, base)
	return append(errs, ValidateClusterEngineClassSKUOnly(&cc.Spec.Template, base)...)
}

// countEnginesResolvedToClusterClass counts engines whose
// spec.engineClassRef matches clusterName and whose namespace has no
// FireboltEngineClass of that name.
func countEnginesResolvedToClusterClass(ctx context.Context, reader client.Reader, clusterName string) (int, error) {
	var engines FireboltEngineList
	if err := reader.List(ctx, &engines); err != nil {
		return 0, fmt.Errorf("listing FireboltEngines to check cluster class references: %w", err)
	}
	var count int
	for i := range engines.Items {
		ref := engines.Items[i].Spec.EngineClassRef
		if ref == nil || *ref != clusterName {
			continue
		}
		nsClass := &FireboltEngineClass{}
		key := client.ObjectKey{Name: clusterName, Namespace: engines.Items[i].Namespace}
		if err := reader.Get(ctx, key, nsClass); err != nil {
			if apierrors.IsNotFound(err) {
				count++
				continue
			}
			return 0, fmt.Errorf("looking up FireboltEngineClass %q in namespace %q: %w", clusterName, engines.Items[i].Namespace, err)
		}
	}
	return count, nil
}
