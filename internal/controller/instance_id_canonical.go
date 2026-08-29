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
	"regexp"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	reasonInstanceIDCanonical    = "Canonical"
	reasonInstanceIDBelowFloor   = "ImageBelowFloor"
	reasonInstanceIDResolveError = "ImageResolveFailed"
	reasonInstanceIDRejected     = "UpdateRejected"
)

var releaseStyleTag = regexp.MustCompile(
	`^(release|debug)-(\d+)\.(\d+)\.(\d+)(?:-pre\.(\d+)\.(\d+)\.[0-9a-fA-F]+)?$`,
)

type parsedReleaseTag struct {
	major, minor, patch int
	hasPre              bool
	preN                int
	timestamp           int64
}

// ensureInstanceID mints a Crockford ULID when spec.id is empty (webhook
// fallback) and lowercases an existing uppercase Crockford ULID once
// images meet the canonicalize floor. Returns requeue=true after a spec
// Update so Reconcile reloads the persisted CR.
func (r *FireboltInstanceReconciler) ensureInstanceID(
	ctx context.Context, instance *computev1alpha1.FireboltInstance,
) (requeue bool, err error) {
	if instance.Spec.ID == "" {
		instance.Spec.ID = computev1alpha1.MintInstanceID()
		logf.FromContext(ctx).Info("Generated instance ID", "id", instance.Spec.ID)
		if err := r.Update(ctx, instance); err != nil {
			return false, err
		}
		return true, nil
	}
	return r.canonicalizeInstanceID(ctx, instance)
}

// canonicalizeInstanceID lowercases an existing uppercase Crockford ULID
// on spec.id once every resolved metadata and bound-engine image meets
// CanonicalInstanceIDImageFloor. Returns rewritten=true after a spec
// Update so the caller requeues against the persisted CR.
func (r *FireboltInstanceReconciler) canonicalizeInstanceID(
	ctx context.Context, instance *computev1alpha1.FireboltInstance,
) (rewritten bool, err error) {
	id := instance.Spec.ID
	if !isUppercaseCrockfordULID(id) {
		// Nothing for this gate to do: either a lowercase Crockford ULID
		// (already canonical) or a user-supplied id the operator never
		// minted and must not rewrite. Record True for both so the
		// condition matches its documented contract instead of being
		// absent on every non-ULID instance.
		message := "spec.id is not a Crockford ULID; nothing to canonicalize"
		if _, parseErr := ulid.ParseStrict(id); parseErr == nil {
			message = "spec.id is a lowercase Crockford ULID"
		}
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionInstanceIDCanonical, metav1.ConditionTrue,
			reasonInstanceIDCanonical, message)
		return false, nil
	}
	if computev1alpha1.CanonicalInstanceIDImageFloor == "" {
		// Floor unpublished: uppercase is the encoding current images
		// consume, so there is no gate result to report. Drop any
		// condition a floor-published build left behind, otherwise a
		// rollback strands a stale ImageBelowFloor forever.
		apimeta.RemoveStatusCondition(&instance.Status.Conditions,
			computev1alpha1.InstanceConditionInstanceIDCanonical)
		return false, nil
	}

	ready, message, err := r.canonicalImagesReady(ctx, instance)
	if err != nil {
		// Same continue-reconcile shape as ImageBelowFloor: persist
		// the condition on the in-memory object and do not return the
		// error. Returning it would skip metadata, gateway, and
		// reconcileDelete, so a dangling EngineClassRef would stall
		// the Instance — including finalizer removal.
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionInstanceIDCanonical, metav1.ConditionFalse,
			reasonInstanceIDResolveError, err.Error())
		return false, nil
	}
	if !ready {
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionInstanceIDCanonical, metav1.ConditionFalse,
			reasonInstanceIDBelowFloor, message)
		return false, nil
	}

	lowered := strings.ToLower(id)
	instance.Spec.ID = lowered
	logf.FromContext(ctx).Info("Canonicalized instance ID to lowercase", "id", lowered)
	if err := r.Update(ctx, instance); err != nil {
		// A deterministic admission rejection — a stale CRD whose CEL rule
		// still forbids case-only updates, or a cluster policy engine that
		// refuses spec mutations — rejects this Update on every pass.
		// Returning it would abort Reconcile before metadata and gateway,
		// stalling an otherwise healthy instance forever; same reasoning as
		// the ImageResolveFailed branch above. Restore the in-memory id so
		// the rest of the pass renders against the value that is actually
		// persisted, and surface the rejection as a condition. Transient
		// errors (Conflict, timeouts) still return so the request retries.
		if !errors.IsInvalid(err) && !errors.IsForbidden(err) {
			return false, err
		}
		instance.Spec.ID = id
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionInstanceIDCanonical, metav1.ConditionFalse,
			reasonInstanceIDRejected,
			fmt.Sprintf("spec.id could not be canonicalized to %q: %v", lowered, err))
		return false, nil
	}
	return true, nil
}

// isUppercaseCrockfordULID reports whether id is a ULID the operator
// minted in the uppercase encoding. ParseStrict, not Parse: Parse checks
// only the 26-byte length and the leading-character overflow, so it
// accepts any 26-character string. spec.id carries no CRD pattern, so a
// customer account id of that length ("0Customer-Account-ID-12345")
// would otherwise be classified as a ULID and silently lowercased.
func isUppercaseCrockfordULID(id string) bool {
	if _, err := ulid.ParseStrict(id); err != nil {
		return false
	}
	return id != strings.ToLower(id)
}

func (r *FireboltInstanceReconciler) canonicalImagesReady(
	ctx context.Context, instance *computev1alpha1.FireboltInstance,
) (bool, string, error) {
	metaImage := resolvedMetadataImage(instance)
	if !imageMeetsCanonicalFloor(metaImage) {
		return false, belowFloorMessage("metadata", metaImage), nil
	}

	var engines computev1alpha1.FireboltEngineList
	if err := r.List(ctx, &engines, client.InNamespace(instance.Namespace)); err != nil {
		return false, "", fmt.Errorf("listing engines to gate spec.id canonicalize: %w", err)
	}
	for i := range engines.Items {
		eng := &engines.Items[i]
		if eng.Spec.InstanceRef != instance.Name {
			continue
		}
		image, err := r.resolvedBoundEngineImage(ctx, eng)
		if err != nil {
			return false, "", err
		}
		if !imageMeetsCanonicalFloor(image) {
			return false, belowFloorMessage(fmt.Sprintf("engine %q", eng.Name), image), nil
		}
	}
	return true, "", nil
}

// belowFloorMessage explains why image does not meet the canonicalize
// floor. A digest-pinned reference carries no tag to compare, so it can
// never clear the floor until it is repinned — telling the user to "bump
// to that tag" would send them looking for a tag that is not there.
func belowFloorMessage(subject, image string) string {
	if containerImageDefaultTag(image) == "" {
		return fmt.Sprintf(
			"spec.id is an uppercase Crockford ULID; %s image %q is pinned by digest, so its version cannot be compared against the canonicalize floor %q — repin it by tag at or above the floor (or unset the pin) before the id is lowercased",
			subject, image, computev1alpha1.CanonicalInstanceIDImageFloor,
		)
	}
	return fmt.Sprintf(
		"spec.id is an uppercase Crockford ULID; %s image %q is older than the canonicalize floor %q — bump metadata and every bound engine image to that tag (or unset the pins) before the id is lowercased",
		subject, image, computev1alpha1.CanonicalInstanceIDImageFloor,
	)
}

func resolvedMetadataImage(instance *computev1alpha1.FireboltInstance) string {
	var primary *corev1.Container
	if instance.Spec.Metadata.Template != nil {
		primary, _ = splitUserContainers(instance.Spec.Metadata.Template.Spec.Containers, computev1alpha1.MetadataContainerName)
	}
	return metadataImageFromUser(primary)
}

func (r *FireboltInstanceReconciler) resolvedBoundEngineImage(
	ctx context.Context, engine *computev1alpha1.FireboltEngine,
) (string, error) {
	var classInfo *FireboltEngineClassInfo
	if engine.Spec.EngineClassRef != nil && *engine.Spec.EngineClassRef != "" {
		class := &computev1alpha1.FireboltEngineClass{}
		key := client.ObjectKey{Namespace: engine.Namespace, Name: *engine.Spec.EngineClassRef}
		if err := r.Get(ctx, key, class); err != nil {
			if errors.IsNotFound(err) {
				return "", fmt.Errorf("cannot resolve engine %q image: FireboltEngineClass %q not found", engine.Name, *engine.Spec.EngineClassRef)
			}
			return "", fmt.Errorf("getting FireboltEngineClass %q for engine %q: %w", key.Name, engine.Name, err)
		}
		classInfo = newFireboltEngineClassInfo(class)
	}

	// Resolve the Preset through the same fail-closed path the engine
	// reconciler uses, not a bare Get: a Preset it refuses
	// (Ready=False/OperatorOwnedFieldSet, or a live spec.template still
	// carrying operator-owned paths) must not supply the image this gate
	// decides on, or the gate passes on an image the engine will never
	// run. The error surfaces as ImageResolveFailed and blocks the
	// rewrite.
	presetInfo, err := resolveFireboltEnginePresetInfo(ctx, r.Client, engine)
	if err != nil {
		return "", fmt.Errorf("resolving preset for engine %q: %w", engine.Name, err)
	}
	classInfo = overlayPresetOnClass(presetInfo, classInfo)
	image, _ := effectiveEngineImage(&engine.Spec, classInfo)
	return image, nil
}

func imageMeetsCanonicalFloor(image string) bool {
	if computev1alpha1.CanonicalInstanceIDImageFloor == "" {
		return false
	}
	tag := containerImageDefaultTag(image)
	if tag == "" {
		return false
	}
	if tag == computev1alpha1.CanonicalInstanceIDImageFloor {
		return true
	}
	// "dev" is the dev-variant alias of the current build, so it counts as
	// at-or-above the floor. "latest" deliberately does not: it is not
	// published for engine or metadata, and containerImageDefaultTag
	// defaults every untagged reference to it — accepting it would let an
	// untagged pin of an arbitrarily old image clear the gate, which is
	// the exact skew the floor exists to prevent.
	if tag == "dev" {
		return true
	}
	return releaseTagAtLeast(tag, computev1alpha1.CanonicalInstanceIDImageFloor)
}

func parseReleaseStyleTag(tag string) (parsedReleaseTag, bool) {
	m := releaseStyleTag.FindStringSubmatch(tag)
	if m == nil {
		return parsedReleaseTag{}, false
	}
	major, _ := strconv.Atoi(m[2])
	minor, _ := strconv.Atoi(m[3])
	patch, _ := strconv.Atoi(m[4])
	out := parsedReleaseTag{major: major, minor: minor, patch: patch}
	if m[5] != "" {
		out.hasPre = true
		out.preN, _ = strconv.Atoi(m[5])
		out.timestamp, _ = strconv.ParseInt(m[6], 10, 64)
	}
	return out, true
}

func releaseTagAtLeast(tag, floor string) bool {
	got, okGot := parseReleaseStyleTag(tag)
	want, okWant := parseReleaseStyleTag(floor)
	if !okGot || !okWant {
		return false
	}
	switch {
	case got.major != want.major:
		return got.major > want.major
	case got.minor != want.minor:
		return got.minor > want.minor
	case got.patch != want.patch:
		return got.patch > want.patch
	case got.hasPre != want.hasPre:
		// A final tag (no -pre) is newer than a pre-release of the same version.
		return !got.hasPre
	case !got.hasPre:
		return true
	case got.preN != want.preN:
		return got.preN > want.preN
	default:
		return got.timestamp >= want.timestamp
	}
}
