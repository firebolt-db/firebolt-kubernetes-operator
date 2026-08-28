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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	reasonInstanceIDCanonical    = "Canonical"
	reasonInstanceIDBelowFloor   = "ImageBelowFloor"
	reasonInstanceIDResolveError = "ImageResolveFailed"
)

// canonicalInstanceIDImageFloor is the engine and metadata tag at which
// lowercase FireboltInstance.spec.id is required. Empty means the floor
// is not published: new ids are minted lowercase, existing CRs are left
// unchanged so an older image pin cannot receive lowercase YAML.
var canonicalInstanceIDImageFloor string

var releaseStyleTag = regexp.MustCompile(
	`^(release|debug)-(\d+)\.(\d+)\.(\d+)(?:-pre\.(\d+)\.(\d+)\.[0-9a-fA-F]+)?$`,
)

type parsedReleaseTag struct {
	major, minor, patch int
	hasPre              bool
	preN                int
	timestamp           int64
}

// ensureInstanceID mints a lowercase ULID when spec.id is empty (webhook
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
// canonicalInstanceIDImageFloor. Returns rewritten=true after a spec
// Update so the caller requeues against the persisted CR.
func (r *FireboltInstanceReconciler) canonicalizeInstanceID(
	ctx context.Context, instance *computev1alpha1.FireboltInstance,
) (rewritten bool, err error) {
	id := instance.Spec.ID
	if !isUppercaseCrockfordULID(id) {
		if _, parseErr := ulid.Parse(id); parseErr == nil {
			setInstanceCondition(instance,
				computev1alpha1.InstanceConditionInstanceIDCanonical, metav1.ConditionTrue,
				reasonInstanceIDCanonical, "spec.id is a lowercase Crockford ULID")
		}
		return false, nil
	}
	if canonicalInstanceIDImageFloor == "" {
		return false, nil
	}

	ready, message, err := r.canonicalImagesReady(ctx, instance)
	if err != nil {
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionInstanceIDCanonical, metav1.ConditionFalse,
			reasonInstanceIDResolveError, err.Error())
		return false, err
	}
	if !ready {
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionInstanceIDCanonical, metav1.ConditionFalse,
			reasonInstanceIDBelowFloor, message)
		return false, nil
	}

	instance.Spec.ID = strings.ToLower(id)
	logf.FromContext(ctx).Info("Canonicalized instance ID to lowercase", "id", instance.Spec.ID)
	if err := r.Update(ctx, instance); err != nil {
		return false, err
	}
	return true, nil
}

func isUppercaseCrockfordULID(id string) bool {
	if _, err := ulid.Parse(id); err != nil {
		return false
	}
	return id != strings.ToLower(id)
}

func (r *FireboltInstanceReconciler) canonicalImagesReady(
	ctx context.Context, instance *computev1alpha1.FireboltInstance,
) (bool, string, error) {
	metaImage := resolvedMetadataImage(instance)
	if !imageMeetsCanonicalFloor(metaImage) {
		return false, fmt.Sprintf(
			"spec.id is an uppercase Crockford ULID; metadata image %q is older than the canonicalize floor %q — bump metadata and every bound engine image to that tag (or unset the pins) before the id is lowercased",
			metaImage, canonicalInstanceIDImageFloor,
		), nil
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
			return false, fmt.Sprintf(
				"spec.id is an uppercase Crockford ULID; engine %q image %q is older than the canonicalize floor %q — bump metadata and every bound engine image to that tag (or unset the pins) before the id is lowercased",
				eng.Name, image, canonicalInstanceIDImageFloor,
			), nil
		}
	}
	return true, "", nil
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

	preset := &computev1alpha1.FireboltEnginePreset{}
	presetKey := client.ObjectKey{Namespace: engine.Namespace, Name: computev1alpha1.FireboltEnginePresetDefaultName}
	if err := r.Get(ctx, presetKey, preset); err != nil {
		if !errors.IsNotFound(err) {
			return "", fmt.Errorf("getting FireboltEnginePreset for engine %q: %w", engine.Name, err)
		}
		preset = nil
	}
	var presetInfo *FireboltEnginePresetInfo
	if preset != nil {
		presetInfo = newFireboltEnginePresetInfo(preset)
	}
	classInfo = overlayPresetOnClass(presetInfo, classInfo)
	image, _ := effectiveEngineImage(&engine.Spec, classInfo)
	return image, nil
}

func imageMeetsCanonicalFloor(image string) bool {
	if canonicalInstanceIDImageFloor == "" {
		return false
	}
	tag := containerImageDefaultTag(image)
	if tag == "" {
		return false
	}
	if tag == canonicalInstanceIDImageFloor {
		return true
	}
	if tag == "dev" || tag == "latest" {
		return true
	}
	return releaseTagAtLeast(tag, canonicalInstanceIDImageFloor)
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
