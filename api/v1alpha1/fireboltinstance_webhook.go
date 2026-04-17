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
	"crypto/rand"
	"fmt"
	"sort"
	"strings"

	"github.com/oklog/ulid/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// reservedAnnotationLabelPrefix is the key prefix owned by the operator for
// its own labels and annotations (e.g. firebolt.io/config-hash,
// firebolt.io/generation). Users MUST NOT set any key with this prefix on
// spec.metadata / spec.gateway labels or annotations: the controller
// unconditionally overwrites some of these keys to drive behavior, and
// letting users set them silently freezes rollouts or corrupts routing.
const reservedAnnotationLabelPrefix = "firebolt.io/"

// FireboltInstanceDefaulter defaults FireboltInstance resources.
type FireboltInstanceDefaulter struct{}

// FireboltInstanceCustomValidator validates FireboltInstance resources.
type FireboltInstanceCustomValidator struct{}

var (
	_ webhook.CustomDefaulter = &FireboltInstanceDefaulter{}
	_ webhook.CustomValidator = &FireboltInstanceCustomValidator{}
)

// SetupFireboltInstanceWebhookWithManager registers the defaulting and
// validating webhooks with the manager.
func SetupFireboltInstanceWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&FireboltInstance{}).
		WithDefaulter(&FireboltInstanceDefaulter{}).
		WithValidator(&FireboltInstanceCustomValidator{}).
		Complete()
}

// Default sets default values for a FireboltInstance. If spec.id is empty, a
// new ULID is generated so every instance has a stable unique identifier.
func (d *FireboltInstanceDefaulter) Default(_ context.Context, obj runtime.Object) error {
	inst, ok := obj.(*FireboltInstance)
	if !ok {
		return fmt.Errorf("expected FireboltInstance, got %T", obj)
	}
	if inst.Spec.ID == "" {
		inst.Spec.ID = ulid.MustNew(ulid.Now(), rand.Reader).String()
	}
	return nil
}

// ValidateCreate validates a FireboltInstance on creation.
func (v *FireboltInstanceCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	inst, ok := obj.(*FireboltInstance)
	if !ok {
		return nil, fmt.Errorf("expected FireboltInstance, got %T", obj)
	}
	return nil, validateSpec(inst).ToAggregate()
}

// ValidateUpdate validates a FireboltInstance on update.
func (v *FireboltInstanceCustomValidator) ValidateUpdate(
	_ context.Context, _, newObj runtime.Object,
) (admission.Warnings, error) {
	// spec.id immutability is enforced by CEL on the CRD itself
	// (XValidation rule="oldSelf == '' || self == oldSelf"), so it works
	// even when webhooks are disabled. The empty->value transition is
	// explicitly allowed so the controller fallback can generate and
	// persist an ID when the defaulting webhook is not active.
	newInst, ok := newObj.(*FireboltInstance)
	if !ok {
		return nil, fmt.Errorf("expected FireboltInstance, got %T", newObj)
	}
	return nil, validateSpec(newInst).ToAggregate()
}

// ValidateDelete validates a FireboltInstance on deletion.
func (v *FireboltInstanceCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateSpec runs every spec-level validation check and collects the
// results. Individual checks return *field.Error (not a plain error) so
// they can be appended directly into a field.ErrorList; wrapping them as
// field.InternalError would surface to users as a 500-style internal
// error instead of a validation failure.
func validateSpec(inst *FireboltInstance) field.ErrorList {
	var errs field.ErrorList

	if err := validateMetadataReplicas(inst); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, validateReservedKeys(
		field.NewPath("spec", "metadata"), inst.Spec.Metadata.ComponentSpec)...)
	errs = append(errs, validateReservedKeys(
		field.NewPath("spec", "gateway"), inst.Spec.Gateway.ComponentSpec)...)

	if err := validateExternalPostgres(inst); err != nil {
		errs = append(errs, err)
	}

	return errs
}

// validateExternalPostgres enforces that any user configuring an external
// PostgreSQL also provides a non-empty Secret reference for credentials.
// Without this check the metadata Deployment is still scheduled; kubelet
// then fails to mount a Secret volume with an empty name and the pod sits
// in ContainerCreating with only a kubelet event explaining why, which is
// invisible from the FireboltInstance CR. Catching it at admission time
// keeps the error close to the offending apply.
func validateExternalPostgres(inst *FireboltInstance) *field.Error {
	pg := inst.Spec.Metadata.Postgres
	if pg == nil {
		return nil
	}
	if pg.CredentialsSecretRef.Name == "" {
		return field.Required(
			field.NewPath("spec", "metadata", "postgres", "credentialsSecretRef", "name"),
			"must be set when spec.metadata.postgres is configured",
		)
	}
	return nil
}

func validateMetadataReplicas(inst *FireboltInstance) *field.Error {
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

// validateReservedKeys rejects any label or annotation on a ComponentSpec
// whose key starts with reservedAnnotationLabelPrefix. Those keys are owned
// by the operator; allowing users to set them would let them clobber
// controller-managed keys (most dangerously firebolt.io/config-hash, which
// drives pod-template rollouts when a ConfigMap changes) and either freeze
// rollouts or corrupt routing.
func validateReservedKeys(base *field.Path, cs ComponentSpec) field.ErrorList {
	var errs field.ErrorList
	errs = append(errs,
		reservedKeyErrors(base.Child("labels"), cs.Labels)...)
	errs = append(errs,
		reservedKeyErrors(base.Child("annotations"), cs.Annotations)...)
	return errs
}

func reservedKeyErrors(path *field.Path, m map[string]string) field.ErrorList {
	reserved := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, reservedAnnotationLabelPrefix) {
			reserved = append(reserved, k)
		}
	}
	if len(reserved) == 0 {
		return nil
	}
	sort.Strings(reserved) // deterministic error messages for tests
	errs := make(field.ErrorList, 0, len(reserved))
	for _, k := range reserved {
		errs = append(errs, field.Forbidden(path.Key(k),
			fmt.Sprintf("keys with the %q prefix are reserved for the operator", reservedAnnotationLabelPrefix),
		))
	}
	return errs
}
