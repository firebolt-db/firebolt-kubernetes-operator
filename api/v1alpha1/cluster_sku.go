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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// clusterClassIAMAnnotationKeys are pod-annotation keys that bind a
// namespace-resolved IAM identity (kube2iam / kiam role, IRSA). They
// belong on the ServiceAccount the FireboltEnginePreset names, not on
// a cluster-scoped SKU catalog object visible to every namespace.
const (
	annotationIAMRole    = "iam.amazonaws.com/role"
	annotationEKSRoleARN = "eks.amazonaws.com/role-arn"
)

var clusterClassIAMAnnotationKeys = []string{
	annotationIAMRole,
	annotationEKSRoleARN,
}

const clusterClassSKUOnlyDetail = "ClusterFireboltEngineClass is SKU-only; " +
	"namespace-resolved identifiers belong on FireboltEnginePreset"

// clusterClassNamespacedRefDetail explains the ConfigMap and PVC
// rejections. These are not identity, so FireboltEnginePreset is not the
// only place they can go — but they are still names a cluster-scoped
// catalog cannot resolve. The catalog has no namespace of its own, so a
// bare ConfigMap or claim name binds to whatever happens to exist in each
// consumer's namespace: the same object renders differently per namespace,
// or wedges the pod on a missing reference the catalog author cannot see.
const clusterClassNamespacedRefDetail = "ClusterFireboltEngineClass is SKU-only; " +
	"a cluster-scoped catalog cannot reference a namespaced object that may not exist in a " +
	"consumer's namespace — move it to the namespaced FireboltEngineClass or FireboltEnginePreset"

// ValidateClusterEngineClassSKUOnly rejects every namespaced reference a
// ClusterFireboltEngineClass template could carry:
//
//   - serviceAccountName and imagePullSecrets;
//   - IAM annotations;
//   - Secret refs (volumes, projected sources, env, envFrom);
//   - ConfigMap refs (volumes, projected sources, env, envFrom);
//   - persistentVolumeClaim volumes, and the dataSource / dataSourceRef of
//     an otherwise-allowed ephemeral claim template;
//   - resourceClaims, and the container resources.claims that consume them.
//
// This is the enforcement half of the namespaced/cluster split — the
// namespaced FireboltEngineClass exists precisely because its template
// may name ServiceAccounts, Secrets, ConfigMaps, and claims; the cluster
// catalog carries SKU shape only.
//
// The set above is complete against the render surface rather than against
// all of PodTemplateSpec. overlayPresetPodSpec merges a class template into
// the engine pod field by field, through its own explicit allowlist, so a
// namespaced reference in a PodSpec field that allowlist does not name can
// never reach a pod: `serviceAccount` (the deprecated serviceAccountName
// alias) and `ephemeralContainers` both sit in the CRD schema, are both
// namespace-bound, and are both inert for exactly that reason. Adding a
// field to overlayPresetPodSpec is therefore what obliges a new check here.
//
// Operator-owned paths are rejected separately by
// ValidateOperatorOwnedPodTemplate.
func ValidateClusterEngineClassSKUOnly(template *corev1.PodTemplateSpec, base *field.Path) field.ErrorList {
	if template == nil {
		return nil
	}
	var errs field.ErrorList
	specPath := base.Child("spec")

	if template.Spec.ServiceAccountName != "" {
		errs = append(errs, field.Forbidden(
			specPath.Child("serviceAccountName"),
			clusterClassSKUOnlyDetail+": serviceAccountName"))
	}
	if len(template.Spec.ImagePullSecrets) > 0 {
		errs = append(errs, field.Forbidden(
			specPath.Child("imagePullSecrets"),
			clusterClassSKUOnlyDetail+": imagePullSecrets"))
	}
	for i := range template.Spec.ResourceClaims {
		errs = append(errs, field.Forbidden(
			specPath.Child("resourceClaims").Index(i),
			clusterClassNamespacedRefDetail+": "+podResourceClaimRefKind(&template.Spec.ResourceClaims[i])))
	}

	annPath := base.Child("metadata", "annotations")
	for _, key := range clusterClassIAMAnnotationKeys {
		if _, ok := template.Annotations[key]; ok {
			errs = append(errs, field.Forbidden(
				annPath.Key(key),
				clusterClassSKUOnlyDetail+": IAM annotation"))
		}
	}

	for i := range template.Spec.Volumes {
		vol := &template.Spec.Volumes[i]
		volPath := specPath.Child("volumes").Index(i)
		if refs := VolumeSecretRefs(vol); len(refs) > 0 {
			errs = append(errs, field.Forbidden(
				volPath, clusterClassSKUOnlyDetail+": Secret volume"))
		}
		errs = append(errs, forbidNamespacedVolumeRefs(vol, volPath)...)
	}

	errs = append(errs, forbidContainerNamespacedRefs(
		template.Spec.Containers, specPath.Child("containers"))...)
	errs = append(errs, forbidContainerNamespacedRefs(
		template.Spec.InitContainers, specPath.Child("initContainers"))...)
	return errs
}

func forbidContainerNamespacedRefs(containers []corev1.Container, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i := range containers {
		if refs := ContainerSecretRefs(&containers[i]); len(refs) > 0 {
			errs = append(errs, field.Forbidden(
				base.Index(i),
				clusterClassSKUOnlyDetail+": Secret env"))
		}
		if refs := containerConfigMapRefs(&containers[i]); len(refs) > 0 {
			errs = append(errs, field.Forbidden(
				base.Index(i),
				clusterClassNamespacedRefDetail+": ConfigMap env"))
		}
		if len(containers[i].Resources.Claims) > 0 {
			// A container claim is satisfiable only by a pod-level
			// resourceClaims entry, which the catalog cannot carry, so
			// leaving this one to the pod-level check would surface as a
			// rejected StatefulSet rather than a named field.
			errs = append(errs, field.Forbidden(
				base.Index(i).Child("resources", "claims"),
				clusterClassNamespacedRefDetail+": resources.claims"))
		}
	}
	return errs
}

// podResourceClaimRefKind names which namespaced DRA object the claim
// entry binds. Exactly one of the two is set on a valid entry; an entry
// with neither is already invalid, and is reported as a claim either way
// so the rejection never depends on well-formedness.
func podResourceClaimRefKind(claim *corev1.PodResourceClaim) string {
	if claim.ResourceClaimTemplateName != nil && *claim.ResourceClaimTemplateName != "" {
		return "resourceClaimTemplateName"
	}
	return "resourceClaimName"
}

// forbidNamespacedVolumeRefs rejects the namespaced objects volume v
// binds by name, beyond the Secret sources VolumeSecretRefs already
// covers. VolumeSource is a union, so at most one branch applies —
// except a projected volume, which can carry both a Secret and a
// ConfigMap source; the Secret half is the caller's other check, so this
// half reports the ConfigMap.
//
// `emptyDir` and `downwardAPI` name nothing and are always allowed.
//
// `ephemeral` is allowed too, but only as far as its claim template
// really is namespace-independent. The template creates a fresh claim in
// the consumer's own namespace instead of binding an existing one, and
// storageClassName / volumeAttributesClassName / volumeName all name
// cluster-scoped objects. dataSource and dataSourceRef are the exception:
// they name a PVC or VolumeSnapshot resolved in the consuming namespace
// (dataSourceRef can even name one in a third namespace), so a catalog
// carrying either would seed every tenant's engine from whatever object
// happened to share that name — the exact per-namespace binding this
// function exists to prevent.
func forbidNamespacedVolumeRefs(v *corev1.Volume, volPath *field.Path) field.ErrorList {
	forbid := func(p *field.Path, kind string) *field.Error {
		return field.Forbidden(p, clusterClassNamespacedRefDetail+": "+kind)
	}
	switch {
	case v.ConfigMap != nil:
		return field.ErrorList{forbid(volPath.Child("configMap"), "ConfigMap volume")}
	case v.PersistentVolumeClaim != nil:
		return field.ErrorList{forbid(
			volPath.Child("persistentVolumeClaim"), "persistentVolumeClaim volume")}
	case v.Projected != nil:
		for i := range v.Projected.Sources {
			if v.Projected.Sources[i].ConfigMap != nil {
				return field.ErrorList{forbid(
					volPath.Child("projected", "sources").Index(i).Child("configMap"),
					"projected ConfigMap volume")}
			}
		}
	case v.Ephemeral != nil && v.Ephemeral.VolumeClaimTemplate != nil:
		claimPath := volPath.Child("ephemeral", "volumeClaimTemplate", "spec")
		claim := &v.Ephemeral.VolumeClaimTemplate.Spec
		var errs field.ErrorList
		if claim.DataSource != nil {
			errs = append(errs, forbid(
				claimPath.Child("dataSource"), "ephemeral claim dataSource"))
		}
		if claim.DataSourceRef != nil {
			errs = append(errs, forbid(
				claimPath.Child("dataSourceRef"), "ephemeral claim dataSourceRef"))
		}
		return errs
	}
	return nil
}

// containerConfigMapRefs returns every ConfigMap name c pulls into its
// environment, through either a single-key env reference or a
// whole-ConfigMap envFrom. It mirrors ContainerSecretRefs for the
// non-Secret half of the namespace-resolved env surface.
func containerConfigMapRefs(c *corev1.Container) []string {
	var out []string
	for i := range c.Env {
		if vf := c.Env[i].ValueFrom; vf != nil && vf.ConfigMapKeyRef != nil && vf.ConfigMapKeyRef.Name != "" {
			out = append(out, vf.ConfigMapKeyRef.Name)
		}
	}
	for i := range c.EnvFrom {
		if ref := c.EnvFrom[i].ConfigMapRef; ref != nil && ref.Name != "" {
			out = append(out, ref.Name)
		}
	}
	return out
}
