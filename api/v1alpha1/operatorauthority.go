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
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// This file is the single source of truth for fields the operator owns
// across user-supplied templating surfaces (spec.customEngineConfig,
// EngineClass.spec.template, and reserved label/annotation key prefixes
// on every CR that lets users set metadata). Consumers reference these
// declarations directly instead of restating the path lists, so a future
// addition lands in one place and propagates to every strip / reject
// site.

// ReservedFireboltKeyPrefix is the label and annotation key prefix owned by
// the operator. Users MUST NOT set any key with this prefix on a CR that
// supports user-supplied labels/annotations — the controller unconditionally
// overwrites several keys to drive behavior (firebolt.io/config-hash,
// firebolt.io/generation, etc.), and letting users seed them silently freezes
// rollouts or corrupts routing.
const ReservedFireboltKeyPrefix = "firebolt.io/"

// EngineContainerName is the fixed name of the firebolt engine container
// inside each generation's StatefulSet pod template. The drain check, the
// stsMatchesSpec drift detection, and the EngineClass validating webhook
// all rely on it being stable and operator-owned. The name matches the
// public CRD ("engine"); the binary entrypoint inside the image is still
// `firebolt-core`, but that's an image-internal path and doesn't surface
// on the pod template.
const EngineContainerName = "engine"

// Operator-injected environment variables on the engine container. These
// keys carry pod-index plumbing (POD_INDEX) and AWS SDK + runtime mode
// signals that the operator must control end to end: a user override
// would either crash the engine or silently divert its identity. They
// are rejected at admission time on EngineClass spec.template and would
// be stripped if injected through another template channel.
const (
	EnginePodIndexEnvKey     = "POD_INDEX"
	EngineAllowAwsIrsaEnvKey = "FIREBOLT_ALLOW_AWS_IRSA"
	EngineCoreModeEnvKey     = "FIREBOLT_CORE_MODE"
)

// operatorOwnedEngineEnvKeys is the set of env names the operator injects on
// the engine container and that user templates may not redefine. Maintained
// as a slice so iteration order is deterministic when reporting violations.
var operatorOwnedEngineEnvKeys = []string{
	EnginePodIndexEnvKey,
	EngineAllowAwsIrsaEnvKey,
	EngineCoreModeEnvKey,
}

// EngineConfigOwnedSection enumerates one operator-owned section of the
// rendered engine config.yaml. Section is the top-level key (empty string
// for the document root). Keys lists the immediate child keys under Section
// that the operator manages exclusively.
//
// When Section is non-empty and the user-supplied value at that section is
// not a JSON object, the entire section is dropped from user input: a deep
// merge would otherwise replace the operator-built section wholesale with
// the user's scalar, losing every authoritative key.
type EngineConfigOwnedSection struct {
	// Section is the top-level key in the rendered config document, or "" for
	// the document root.
	Section string

	// Keys are the immediate children of Section managed exclusively by the
	// operator. User input at any of these paths is silently stripped.
	Keys []string
}

// OperatorOwnedEngineConfigPaths declares every path in the rendered engine
// config.yaml that the operator manages exclusively. It is consumed by
// stripProtectedEngineConfigPaths (internal/controller/engine_reconcile.go),
// which removes these paths from spec.customEngineConfig before the deep
// merge into the canonical document.
//
// Stripping is silent so that the same FireboltEngine spec stays portable
// across operator releases even when this list grows: users do not need to
// chase the protected set in their CRs to keep them applying cleanly.
var OperatorOwnedEngineConfigPaths = []EngineConfigOwnedSection{
	{Section: "", Keys: []string{"schema_version"}},
	{Section: "instance", Keys: []string{"id", "type", "multi_engine"}},
	{Section: "engine", Keys: []string{"id", "nodes", "termination_grace_period"}},
}

// ValidateReservedKeyPrefix rejects any key in m whose name starts with
// ReservedFireboltKeyPrefix. Used by every webhook that accepts user-set
// label or annotation maps on a CR. Returns one *field.Error per offending
// key, sorted alphabetically so test fixtures stay stable.
func ValidateReservedKeyPrefix(path *field.Path, m map[string]string) field.ErrorList {
	reserved := make([]string, 0, len(m))
	for k := range m {
		if strings.HasPrefix(k, ReservedFireboltKeyPrefix) {
			reserved = append(reserved, k)
		}
	}
	if len(reserved) == 0 {
		return nil
	}
	sort.Strings(reserved)
	errs := make(field.ErrorList, 0, len(reserved))
	for _, k := range reserved {
		errs = append(errs, field.Forbidden(path.Key(k),
			fmt.Sprintf("keys with the %q prefix are reserved for the operator", ReservedFireboltKeyPrefix),
		))
	}
	return errs
}

// ValidateOperatorOwnedPodTemplate rejects any user-supplied input on a
// PodTemplateSpec that lands at a path the operator owns end to end. It is
// the central enforcement function for EngineClass.spec.template; the
// EngineClass validating webhook returns these errors verbatim. The
// rejection set covers:
//
//   - pod-template metadata.labels / metadata.annotations under the
//     ReservedFireboltKeyPrefix (operator-managed keys like config-hash)
//   - the engine container ("engine")'s identity, command, ports, probes,
//     and the env keys the operator injects (POD_INDEX, FIREBOLT_*)
//   - any second container or initContainer named "engine" (collides with
//     the operator-rendered engine container in the merge)
//   - pod-level fields the operator stamps from FireboltEngineSpec or
//     hard-codes for the headless-DNS / StatefulSet contract:
//     terminationGracePeriodSeconds, subdomain, hostname, restartPolicy,
//     activeDeadlineSeconds
//
// User-allowed engine-container fields: image, imagePullPolicy, resources,
// non-reserved env / envFrom, non-reserved volumeMounts, securityContext,
// lifecycle. Sidecar containers (any container whose name is not
// EngineContainerName) and additional initContainers are fully user-owned —
// their image, command, ports, env, mounts, and so on pass through
// untouched.
//
// base is the field.Path that the caller used to reach this PodTemplateSpec
// in its own object (e.g. field.NewPath("spec","template") for EngineClass).
// Returned errors carry the full nested path so kubectl apply surfaces
// every violation at the exact offending coordinate.
func ValidateOperatorOwnedPodTemplate(template *corev1.PodTemplateSpec, base *field.Path) field.ErrorList {
	if template == nil {
		return nil
	}
	var errs field.ErrorList

	metaPath := base.Child("metadata")
	errs = append(errs, ValidateReservedKeyPrefix(metaPath.Child("labels"), template.Labels)...)
	errs = append(errs, ValidateReservedKeyPrefix(metaPath.Child("annotations"), template.Annotations)...)

	specPath := base.Child("spec")
	errs = append(errs, validateOwnedPodFields(&template.Spec, specPath)...)
	errs = append(errs, validateInitContainersOwnership(template.Spec.InitContainers, specPath.Child("initContainers"))...)
	errs = append(errs, validateContainersOwnership(template.Spec.Containers, specPath.Child("containers"))...)

	return errs
}

// validateOwnedPodFields enforces the pod-level (non-container) ownership
// rules. Fields rejected here are either set by the operator from
// FireboltEngineSpec (TerminationGracePeriodSeconds) or hard-coded to keep
// the StatefulSet / headless-DNS contract intact (Subdomain, Hostname,
// RestartPolicy, ActiveDeadlineSeconds).
func validateOwnedPodFields(spec *corev1.PodSpec, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	if spec.TerminationGracePeriodSeconds != nil {
		errs = append(errs, field.Forbidden(base.Child("terminationGracePeriodSeconds"),
			"terminationGracePeriodSeconds is stamped from spec.terminationGracePeriodSeconds on the FireboltEngine"))
	}
	if spec.Subdomain != "" {
		errs = append(errs, field.Forbidden(base.Child("subdomain"),
			"subdomain is owned by the operator for headless-DNS routing"))
	}
	if spec.Hostname != "" {
		errs = append(errs, field.Forbidden(base.Child("hostname"),
			"hostname is owned by the operator (set per pod ordinal by the StatefulSet)"))
	}
	if spec.RestartPolicy != "" {
		errs = append(errs, field.Forbidden(base.Child("restartPolicy"),
			"restartPolicy is fixed by the StatefulSet controller"))
	}
	if spec.ActiveDeadlineSeconds != nil {
		errs = append(errs, field.Forbidden(base.Child("activeDeadlineSeconds"),
			"activeDeadlineSeconds is incompatible with long-lived engine pods"))
	}
	return errs
}

// validateContainersOwnership walks template.spec.containers. The container
// named EngineContainerName is the operator-rendered engine container; user
// input on its identity, command, ports, probes, and reserved env keys is
// rejected. Sidecars (any other name) are fully user-owned. A second
// container whose name is also EngineContainerName is rejected: the merge
// would otherwise produce two containers with the same name and the pod
// would never be created.
func validateContainersOwnership(containers []corev1.Container, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	engineSeen := false
	for i := range containers {
		c := &containers[i]
		path := base.Index(i)
		if c.Name != EngineContainerName {
			continue
		}
		if engineSeen {
			errs = append(errs, field.Duplicate(path.Child("name"), EngineContainerName))
			continue
		}
		engineSeen = true
		errs = append(errs, validateEngineContainerOwnership(c, path)...)
	}
	return errs
}

// validateInitContainersOwnership rejects any initContainer whose name is
// EngineContainerName (collides with the engine container) and otherwise
// leaves user-supplied init containers untouched.
func validateInitContainersOwnership(initContainers []corev1.Container, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i := range initContainers {
		c := &initContainers[i]
		if c.Name == EngineContainerName {
			errs = append(errs, field.Forbidden(base.Index(i).Child("name"),
				fmt.Sprintf("init container name %q collides with the engine container; pick a different name", EngineContainerName)))
		}
	}
	return errs
}

// validateEngineContainerOwnership returns one *field.Error per user-set
// field that the operator owns on the engine container.
func validateEngineContainerOwnership(c *corev1.Container, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	if len(c.Command) > 0 {
		errs = append(errs, field.Forbidden(base.Child("command"),
			"engine container command is hardcoded by the operator (EngineStartupScript)"))
	}
	if len(c.Args) > 0 {
		errs = append(errs, field.Forbidden(base.Child("args"),
			"engine container args are hardcoded by the operator"))
	}
	if len(c.Ports) > 0 {
		errs = append(errs, field.Forbidden(base.Child("ports"),
			"engine container ports are hardcoded by the operator (drain check, headless service)"))
	}
	if c.ReadinessProbe != nil {
		errs = append(errs, field.Forbidden(base.Child("readinessProbe"),
			"engine container readinessProbe is owned by the operator (/health/ready contract)"))
	}
	if c.LivenessProbe != nil {
		errs = append(errs, field.Forbidden(base.Child("livenessProbe"),
			"engine container livenessProbe is owned by the operator"))
	}
	if c.StartupProbe != nil {
		errs = append(errs, field.Forbidden(base.Child("startupProbe"),
			"engine container startupProbe is owned by the operator"))
	}
	for ei := range c.Env {
		name := c.Env[ei].Name
		if !isReservedEngineEnvKey(name) {
			continue
		}
		errs = append(errs, field.Forbidden(base.Child("env").Index(ei).Child("name"),
			fmt.Sprintf("env key %q is injected by the operator; pick a different name", name)))
	}
	return errs
}

// isReservedEngineEnvKey reports whether name is one of the env keys the
// operator injects on the engine container.
func isReservedEngineEnvKey(name string) bool {
	for _, k := range operatorOwnedEngineEnvKeys {
		if name == k {
			return true
		}
	}
	return false
}
