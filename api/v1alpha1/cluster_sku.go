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

// ValidateClusterEngineClassSKUOnly rejects namespace-resolved identifiers
// on a ClusterFireboltEngineClass template: serviceAccountName, Secret
// refs (volumes, env, envFrom, imagePullSecrets), and IAM annotations.
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
		if refs := VolumeSecretRefs(vol); len(refs) > 0 {
			errs = append(errs, field.Forbidden(
				specPath.Child("volumes").Index(i),
				clusterClassSKUOnlyDetail+": Secret volume"))
		}
	}

	errs = append(errs, forbidContainerSecretRefs(
		template.Spec.Containers, specPath.Child("containers"))...)
	errs = append(errs, forbidContainerSecretRefs(
		template.Spec.InitContainers, specPath.Child("initContainers"))...)
	return errs
}

func forbidContainerSecretRefs(containers []corev1.Container, base *field.Path) field.ErrorList {
	var errs field.ErrorList
	for i := range containers {
		if refs := ContainerSecretRefs(&containers[i]); len(refs) > 0 {
			errs = append(errs, field.Forbidden(
				base.Index(i),
				clusterClassSKUOnlyDetail+": Secret env"))
		}
	}
	return errs
}
