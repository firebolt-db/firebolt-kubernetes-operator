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
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// applySSA server-side-applies a fully-populated typed object as the operator
// field manager (OperatorFieldManager), forcing ownership of the fields it
// sets. Callers populate obj — including its TypeMeta (apiVersion/kind) and any
// owner references — before calling; SSA is an upsert, so no Create fallback is
// needed.
//
// It bridges to controller-runtime 0.24's typed Client.Apply by converting the
// object to unstructured and wrapping it with ApplyConfigurationFromUnstructured.
// The wire payload is identical to the previous Patch(obj, client.Apply, ...)
// idiom — both marshal the typed object to JSON and PATCH it as an apply patch —
// so SSA field ownership is unchanged. controller-runtime notes that an
// unstructured built from a typed object cannot tell an unset field from an
// explicit zero value; that matches the prior behavior exactly and is not a
// regression. A future move to generated ApplyConfigurations would replace only
// this helper.
func applySSA(ctx context.Context, c client.Client, obj client.Object) error {
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return fmt.Errorf("converting %T to unstructured for server-side apply: %w", obj, err)
	}
	return c.Apply(ctx,
		client.ApplyConfigurationFromUnstructured(&unstructured.Unstructured{Object: u}),
		client.FieldOwner(OperatorFieldManager),
		client.ForceOwnership,
	)
}
