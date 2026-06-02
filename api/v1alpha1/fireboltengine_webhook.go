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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// EngineResourceBounds caps individual values inside
// FireboltEngine.spec.resources at admission time. Each entry is a maximum
// for both Requests and Limits of the matching ResourceName; a zero-value
// quantity disables the bound for that dimension. The struct value is
// passed by the operator entrypoint, sourced from CLI flags / Helm values,
// so platform teams can tune the ceiling per deployment without recompiling.
type EngineResourceBounds struct {
	// MaxCPU caps spec.resources.requests[cpu] and spec.resources.limits[cpu].
	// IsZero() disables the bound.
	MaxCPU resource.Quantity
	// MaxMemory caps spec.resources.requests[memory] and
	// spec.resources.limits[memory]. IsZero() disables the bound.
	MaxMemory resource.Quantity
	// MaxEphemeralStorage caps
	// spec.resources.requests[ephemeral-storage] and
	// spec.resources.limits[ephemeral-storage]. IsZero() disables the
	// bound.
	MaxEphemeralStorage resource.Quantity
}

// IsEmpty reports whether all bounds are zero, i.e. validation is a no-op.
// Pointer receiver because EngineResourceBounds embeds three
// resource.Quantity values and is too large to pass by value efficiently.
func (b *EngineResourceBounds) IsEmpty() bool {
	return b.MaxCPU.IsZero() && b.MaxMemory.IsZero() && b.MaxEphemeralStorage.IsZero()
}

// max returns the configured bound for the given ResourceName, or a zero
// quantity when no bound applies. Unknown ResourceNames (e.g. extended
// resources, GPU vendors) intentionally fall through with a zero bound so
// users can keep declaring them without the operator gatekeeping
// dimensions it does not understand.
func (b *EngineResourceBounds) max(name corev1.ResourceName) resource.Quantity {
	switch name {
	case corev1.ResourceCPU:
		return b.MaxCPU
	case corev1.ResourceMemory:
		return b.MaxMemory
	case corev1.ResourceEphemeralStorage:
		return b.MaxEphemeralStorage
	default:
		return resource.Quantity{}
	}
}

// Validate walks resources.Requests and resources.Limits and returns a
// field.ErrorList containing one *field.Error per entry whose value
// exceeds the configured per-dimension bound. base is the field.Path
// the caller used to reach the ResourceRequirements (e.g.
// field.NewPath("spec","resources")). Both webhook admission and the
// FireboltEngine controller's defense-in-depth gate consume this so a
// CR rejected at admission and a CR caught at reconcile carry the
// same field path and the same "exceeds operator-configured maximum"
// message. An empty bounds value (IsEmpty()) short-circuits — the
// platform team has not opted into bounds, so every value passes.
func (b *EngineResourceBounds) Validate(resources corev1.ResourceRequirements, base *field.Path) field.ErrorList {
	if b.IsEmpty() {
		return nil
	}
	var errs field.ErrorList
	errs = append(errs, b.validateResourceList(base.Child("requests"), resources.Requests)...)
	errs = append(errs, b.validateResourceList(base.Child("limits"), resources.Limits)...)
	return errs
}

// validateResourceList is the per-map walker behind Validate. Kept
// private because callers always reach it via Validate, which seeds
// the Requests / Limits split and the IsEmpty short-circuit.
func (b *EngineResourceBounds) validateResourceList(path *field.Path, list corev1.ResourceList) field.ErrorList {
	var errs field.ErrorList
	for name, qty := range list {
		bound := b.max(name)
		if bound.IsZero() {
			continue
		}
		if qty.Cmp(bound) > 0 {
			errs = append(errs, field.Invalid(
				path.Key(string(name)),
				qty.String(),
				fmt.Sprintf("exceeds operator-configured maximum %s", bound.String()),
			))
		}
	}
	return errs
}

// FireboltEngineCustomValidator validates FireboltEngine resources at
// admission time. It performs two checks:
//
//  1. spec.engineClassRef, when set, points to a FireboltEngineClass that
//     exists in the engine's own namespace — the reference is hard-rejected
//     so users see a typo (or a class-applied-in-the-wrong-namespace mistake)
//     immediately at apply time rather than via engine status. Apply
//     ordering matters: a FireboltEngineClass must exist in the same namespace
//     before any FireboltEngine that references it (GitOps tooling such as
//     Argo CD sync-waves or Flux dependsOn handles this in practice).
//
//  2. Each value in spec.resources.requests / spec.resources.limits sits
//     at or below the operator-configured ceiling in ResourceBounds. The
//     bounds protect a namespace from accidentally admitting an engine
//     whose requests would starve sibling workloads at scheduling time
//     and an engine whose limits would OOM the node hosting it.
//
// The validator reads through mgr.GetAPIReader (live, non-cached) because
// the informer cache may not yet have the FireboltEngineClass at the moment
// of admission — particularly in `kubectl apply -f class.yaml -f engine.yaml`
// where both objects land within the same poll interval.
//
// +kubebuilder:object:generate=false
type FireboltEngineCustomValidator struct {
	Reader         client.Reader
	ResourceBounds EngineResourceBounds
}

var _ webhook.CustomValidator = &FireboltEngineCustomValidator{}

// SetupFireboltEngineWebhookWithManager wires the validator into the
// manager's webhook server. The validator holds an APIReader rather than
// the cached Client because admission must reflect the live API state.
// bounds is passed by pointer because EngineResourceBounds embeds three
// resource.Quantity values; callers wanting a no-op validator pass
// either a zero value or a pointer to one, both of which short-circuit
// in validateResources via IsEmpty.
func SetupFireboltEngineWebhookWithManager(mgr ctrl.Manager, bounds *EngineResourceBounds) error {
	v := &FireboltEngineCustomValidator{Reader: mgr.GetAPIReader()}
	if bounds != nil {
		v.ResourceBounds = *bounds
	}
	return ctrl.NewWebhookManagedBy(mgr).
		For(&FireboltEngine{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate rejects a new FireboltEngine when spec.engineClassRef
// references a FireboltEngineClass that does not exist, or when spec.resources
// carries a value above the configured bound.
func (v *FireboltEngineCustomValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	eng, ok := obj.(*FireboltEngine)
	if !ok {
		return nil, fmt.Errorf("expected FireboltEngine, got %T", obj)
	}
	return nil, v.validate(ctx, eng).ToAggregate()
}

// ValidateUpdate enforces the same existence and bound checks as
// ValidateCreate. Symmetric handling matches FB-1145: a typo on edit
// deserves the same immediate feedback as a typo on create. Recovery
// from a broken state (class deleted somehow, bound lowered after the
// engine was created) is always possible by setting spec.engineClassRef
// to nil / to another existing class, or by reducing spec.resources to
// fit the new bound.
func (v *FireboltEngineCustomValidator) ValidateUpdate(
	ctx context.Context, _, newObj runtime.Object,
) (admission.Warnings, error) {
	eng, ok := newObj.(*FireboltEngine)
	if !ok {
		return nil, fmt.Errorf("expected FireboltEngine, got %T", newObj)
	}
	return nil, v.validate(ctx, eng).ToAggregate()
}

// ValidateDelete is a no-op. The engine has no cross-resource invariants
// to enforce on deletion; the controller cleans up generation-scoped
// resources via owner references.
func (v *FireboltEngineCustomValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate concatenates all field errors so the admission response
// surfaces every problem in a single round-trip — users editing a
// resource that fails on both a class-ref typo and a resources bound
// see both issues at once rather than fixing one and re-submitting.
//
// The class is loaded at most once per admission so the resources
// gate can fall through to class-supplied values without a second
// round-trip to the API server.
func (v *FireboltEngineCustomValidator) validate(ctx context.Context, eng *FireboltEngine) field.ErrorList {
	var errs field.ErrorList
	class, refErrs := v.resolveEngineClass(ctx, eng)
	errs = append(errs, refErrs...)
	errs = append(errs, v.validateTemplate(eng)...)
	errs = append(errs, v.validateResources(eng, class)...)
	return errs
}

// validateTemplate runs the per-component pod-template allowlist on the
// engine's own spec.template. Reuses FireboltEngineClassPodTemplateRules
// because the engine template and the class template are both validated
// against the same operator-owned-fields contract: they merge into the
// same pod and therefore must obey the same set of operator-owned paths.
func (v *FireboltEngineCustomValidator) validateTemplate(eng *FireboltEngine) field.ErrorList {
	return ValidatePodTemplate(
		eng.Spec.Template,
		field.NewPath("spec", "template"),
		FireboltEngineClassPodTemplateRules,
	)
}

// resolveEngineClass loads the FireboltEngineClass referenced by
// spec.engineClassRef from the engine's own namespace (FireboltEngineClass
// is namespaced; the lookup matches how Kubernetes will resolve the
// reference at reconcile time). It returns the loaded class on success
// or a non-nil ErrorList on failure (NotFound for typos / wrong-
// namespace; InternalError for transient API problems). A nil ref is
// allowed and returns (nil, nil) — the engine falls back to operator
// defaults.
//
// The returned class is consumed by downstream validators (notably
// validateResources, which has to inspect class-supplied container
// resources when the engine template does not declare its own). Loading
// once per admission keeps the round-trip count to the API server
// bounded.
func (v *FireboltEngineCustomValidator) resolveEngineClass(
	ctx context.Context, eng *FireboltEngine,
) (*FireboltEngineClass, field.ErrorList) {
	if eng.Spec.EngineClassRef == nil || *eng.Spec.EngineClassRef == "" {
		return nil, nil
	}
	classPath := field.NewPath("spec", "engineClassRef")
	class := &FireboltEngineClass{}
	key := client.ObjectKey{Name: *eng.Spec.EngineClassRef, Namespace: eng.Namespace}
	if err := v.Reader.Get(ctx, key, class); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, field.ErrorList{field.NotFound(classPath, *eng.Spec.EngineClassRef)}
		}
		return nil, field.ErrorList{field.InternalError(classPath, fmt.Errorf("looking up FireboltEngineClass: %w", err))}
	}
	return class, nil
}

// validateResources rejects engine-container resources entries whose
// effective value (after the operator's merge layer) exceeds the
// operator-configured maximum.
//
// Effective resources follow the same precedence the engine reconciler
// uses: when the engine's own spec.template carries an engine container
// with any requests/limits, that wins wholesale. Otherwise the class's
// container resources fill in. Either way the value rendered onto the
// StatefulSet pod must clear the ceiling, so both sources are checked
// here (and not only the engine's own template — that pre-FB-1426 gap
// let a class with oversized requests escape admission whenever the
// engine relied on class-supplied resources).
//
// The error's field path points at the merged location
// (spec.template.spec.containers[engine].resources.*) regardless of
// source: it is where the user should edit to override. When the value
// came from class, the error detail names the class so the user knows
// they can fix it on either side.
func (v *FireboltEngineCustomValidator) validateResources(eng *FireboltEngine, class *FireboltEngineClass) field.ErrorList {
	mergedPath := field.NewPath("spec", "template", "spec", "containers").Key(EngineContainerName).Child("resources")

	if c := EngineContainerInTemplate(eng.Spec.Template); c != nil && HasContainerResources(c.Resources) {
		return v.ResourceBounds.Validate(c.Resources, mergedPath)
	}
	if class != nil {
		if c := EngineContainerInTemplate(&class.Spec.Template); c != nil && HasContainerResources(c.Resources) {
			errs := v.ResourceBounds.Validate(c.Resources, mergedPath)
			for i := range errs {
				errs[i].Detail = fmt.Sprintf(
					"%s (inherited from FireboltEngineClass %q; set spec.template on this engine to override)",
					errs[i].Detail, class.Name,
				)
			}
			return errs
		}
	}
	return nil
}

// EngineContainerInTemplate returns the container named
// EngineContainerName from a pod template, or nil when the template
// is nil or carries no such container. The lookup is exported so
// callers outside this package (the controller's resource-bounds
// defense-in-depth check, e.g.) can use the same definition.
func EngineContainerInTemplate(template *corev1.PodTemplateSpec) *corev1.Container {
	if template == nil {
		return nil
	}
	for i := range template.Spec.Containers {
		c := &template.Spec.Containers[i]
		if c.Name == EngineContainerName {
			return c
		}
	}
	return nil
}
