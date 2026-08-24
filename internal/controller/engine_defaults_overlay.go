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
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// FireboltEngineDefaultsInfo is the resolved ambient overlay for one
// namespace. overlayDefaultsOnClass folds it underneath the engine and
// above the class so existing effective* helpers keep the shape
// (spec, classInfo).
type FireboltEngineDefaultsInfo struct {
	Name               string
	Hash               string
	Template           *corev1.PodTemplateSpec
	Storage            computev1alpha1.EngineStorageSpec
	CustomEngineConfig *apiextensionsv1.JSON
}

func newFireboltEngineDefaultsInfo(d *computev1alpha1.FireboltEngineDefaults) *FireboltEngineDefaultsInfo {
	if d == nil {
		return nil
	}
	raw, err := json.Marshal(d.Spec)
	hash := ""
	if err == nil {
		hash = contentHash(string(raw))
	}
	return &FireboltEngineDefaultsInfo{
		Name:               d.Name,
		Hash:               hash,
		Template:           d.Spec.Template.DeepCopy(),
		Storage:            *d.Spec.Storage.DeepCopy(),
		CustomEngineConfig: d.Spec.CustomEngineConfig.DeepCopy(),
	}
}

func defaultsAsEngineSpec(d *FireboltEngineDefaultsInfo) *computev1alpha1.FireboltEngineSpec {
	spec := &computev1alpha1.FireboltEngineSpec{
		Storage:            d.Storage,
		CustomEngineConfig: d.CustomEngineConfig,
	}
	if d.Template != nil {
		spec.Template = d.Template.DeepCopy()
	}
	return spec
}

// overlayDefaultsOnClass folds Defaults over the class using the same
// merge keys as engine-over-class. The result is consumed as classInfo
// by effective*, so the live resolution order is
// engine > Defaults > class > operator default. SKU-shaped class
// fields (rollout, autoStop, uiSidecar, drain checks) pass through
// unchanged. Class Hash is preserved so AnnotationEngineClassHash
// still means the class template only.
func overlayDefaultsOnClass(defaults *FireboltEngineDefaultsInfo, class *FireboltEngineClassInfo) *FireboltEngineClassInfo {
	if defaults == nil {
		return class
	}
	fake := defaultsAsEngineSpec(defaults)
	out := &FireboltEngineClassInfo{
		DefaultsName: defaults.Name,
		DefaultsHash: defaults.Hash,
	}
	if class != nil {
		out.Name = class.Name
		out.Hash = class.Hash
		out.UISidecar = class.UISidecar
		out.Rollout = class.Rollout
		out.DrainCheckEnabled = class.DrainCheckEnabled
		out.DrainCheckInterval = class.DrainCheckInterval
		out.AutoStop = class.AutoStop
	}
	if storageBackendSet(defaults.Storage) {
		out.Storage = defaults.Storage
	} else if class != nil {
		out.Storage = class.Storage
	}
	if merged := effectiveCustomEngineConfig(fake, class); len(merged) > 0 {
		if raw, err := json.Marshal(merged); err == nil {
			out.CustomEngineConfig = &apiextensionsv1.JSON{Raw: raw}
		}
	}
	out.Template = overlayDefaultsPodTemplate(fake, class)
	return out
}

func overlayDefaultsPodTemplate(fake *computev1alpha1.FireboltEngineSpec, class *FireboltEngineClassInfo) *corev1.PodTemplateSpec {
	tmpl := &corev1.PodTemplateSpec{}
	labels := map[string]string{}
	if class != nil && class.Template != nil {
		for k, v := range class.Template.Labels {
			if k == LabelEngine || k == LabelGeneration {
				continue
			}
			labels[k] = v
		}
	}
	if fake.Template != nil {
		for k, v := range fake.Template.Labels {
			if k == LabelEngine || k == LabelGeneration {
				continue
			}
			labels[k] = v
		}
	}
	if len(labels) > 0 {
		tmpl.Labels = labels
	}
	tmpl.Annotations = effectivePodAnnotations(fake, class)
	overlayDefaultsPodSpec(&tmpl.Spec, fake, class)
	return tmpl
}

func overlayDefaultsPodSpec(spec *corev1.PodSpec, fake *computev1alpha1.FireboltEngineSpec, class *FireboltEngineClassInfo) {
	spec.ServiceAccountName = effectiveServiceAccountName(fake, class)
	spec.NodeSelector = effectiveNodeSelector(fake, class)
	spec.Tolerations = effectiveTolerations(fake, class)
	spec.Affinity = effectiveAffinity(fake, class)
	spec.TopologySpreadConstraints = effectiveTopologySpreadConstraints(fake, class)
	spec.PriorityClassName = effectivePriorityClassName(fake, class)
	spec.RuntimeClassName = effectiveRuntimeClassName(fake, class)
	spec.DNSPolicy = effectiveDNSPolicy(fake, class)
	spec.DNSConfig = effectiveDNSConfig(fake, class)
	spec.SchedulerName = effectiveSchedulerName(fake, class)
	spec.PreemptionPolicy = effectivePreemptionPolicy(fake, class)
	spec.ReadinessGates = effectiveReadinessGates(fake, class)
	spec.ResourceClaims = effectivePodResourceClaims(fake, class)
	spec.HostAliases = effectiveHostAliases(fake, class)
	spec.OS = effectivePodOS(fake, class)
	spec.Overhead = effectivePodOverhead(fake, class)
	spec.ImagePullSecrets = effectiveImagePullSecrets(fake, class)
	if engineTemplate(fake).Spec.SecurityContext != nil {
		spec.SecurityContext = engineTemplate(fake).Spec.SecurityContext.DeepCopy()
	} else if class != nil && class.Template != nil && class.Template.Spec.SecurityContext != nil {
		spec.SecurityContext = class.Template.Spec.SecurityContext.DeepCopy()
	}
	spec.InitContainers = effectiveInitContainers(fake, class)
	spec.Volumes = overlayDefaultsVolumes(fake, class)
	sidecars := effectiveSidecars(fake, class)
	spec.Containers = append([]corev1.Container{overlayDefaultsEngineContainer(fake, class)}, sidecars...)
}

func overlayDefaultsVolumes(fake *computev1alpha1.FireboltEngineSpec, class *FireboltEngineClassInfo) []corev1.Volume {
	var vols []corev1.Volume
	if class != nil && class.Template != nil {
		for i := range class.Template.Spec.Volumes {
			vols = append(vols, *class.Template.Spec.Volumes[i].DeepCopy())
		}
	}
	if fake.Template != nil {
		for i := range fake.Template.Spec.Volumes {
			vols = append(vols, *fake.Template.Spec.Volumes[i].DeepCopy())
		}
	}
	return vols
}

func overlayDefaultsEngineContainer(fake *computev1alpha1.FireboltEngineSpec, class *FireboltEngineClassInfo) corev1.Container {
	engineC := corev1.Container{Name: computev1alpha1.EngineContainerName}
	if c := engineSpecContainer(fake); c != nil && c.Image != "" {
		engineC.Image = c.Image
	} else if c := classEngineContainer(class); c != nil && c.Image != "" {
		engineC.Image = c.Image
	}
	if p := engineExplicitPullPolicy(fake, class); p != "" {
		engineC.ImagePullPolicy = p
	}
	engineC.Resources = effectiveEngineResources(fake, class)
	engineC.Env = effectiveEngineEnv(fake, class)
	engineC.EnvFrom = effectiveEngineEnvFrom(fake, class)
	engineC.VolumeMounts = effectiveEngineVolumeMounts(fake, class)
	if c := engineSpecContainer(fake); c != nil && c.SecurityContext != nil {
		engineC.SecurityContext = c.SecurityContext.DeepCopy()
	} else if c := classEngineContainer(class); c != nil && c.SecurityContext != nil {
		engineC.SecurityContext = c.SecurityContext.DeepCopy()
	}
	engineC.Lifecycle = effectiveEngineLifecycle(fake, class)
	engineC.WorkingDir = effectiveEngineWorkingDir(fake, class)
	engineC.TerminationMessagePath = effectiveEngineTerminationMessagePath(fake, class)
	engineC.TerminationMessagePolicy = effectiveEngineTerminationMessagePolicy(fake, class)
	engineC.VolumeDevices = effectiveEngineVolumeDevices(fake, class)
	engineC.ResizePolicy = effectiveEngineResizePolicy(fake, class)
	return engineC
}
