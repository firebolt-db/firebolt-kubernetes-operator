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
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	metadataCredsMount    = "/secrets/postgres" //nolint:gosec // mount path, not a credential
	metadataConfigMount   = "/configs"
	metadataContainerName = "dedicated-pensieve"
)

// ensureMetadataResources creates or updates the ConfigMap, Deployment, and
// Service for the metadata service.
func (r *FireboltInstanceReconciler) ensureMetadataResources(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	configXML := buildMetadataConfigXML(instance)

	if err := r.ensureMetadataConfigMap(ctx, instance, configXML); err != nil {
		return fmt.Errorf("ensuring metadata configmap: %w", err)
	}

	if err := r.ensureMetadataDeployment(ctx, instance, configXML); err != nil {
		return fmt.Errorf("ensuring metadata deployment: %w", err)
	}

	if err := r.ensureMetadataService(ctx, instance); err != nil {
		return fmt.Errorf("ensuring metadata service: %w", err)
	}

	log.Info("Metadata service resources ensured")
	return nil
}

// metadataCredsSecretName returns the name of the Kubernetes Secret that holds
// the PostgreSQL credentials for the metadata service. For internal PG, this
// is the operator-created secret. For external PG, this is the user-provided
// secret referenced in the CRD — no copy is made.
func metadataCredsSecretName(instance *computev1alpha1.FireboltInstance) string {
	if instance.Spec.Metadata.Postgres != nil {
		return instance.Spec.Metadata.Postgres.CredentialsSecretRef.Name
	}
	return pgCredentialsSecretName(instance.Name)
}

func buildMetadataConfigXML(instance *computev1alpha1.FireboltInstance) string {
	pgHost := pgResourceName(instance.Name) + "." + instance.Namespace + ".svc.cluster.local"
	pgPort := int32(PostgresPort)
	pgDatabase := PostgresDBName

	if instance.Spec.Metadata.Postgres != nil {
		pgHost = instance.Spec.Metadata.Postgres.Host
		pgPort = instance.Spec.Metadata.Postgres.Port
		if pgPort == 0 {
			pgPort = int32(PostgresPort)
		}
		pgDatabase = instance.Spec.Metadata.Postgres.Database
	}

	return fmt.Sprintf(`<?xml version="1.0"?>
<config>
  <pensieve_lite>
    <host>0.0.0.0</host>
    <port>%d</port>
    <server_threads>0</server_threads>
    <log_level>information</log_level>
    <metadata_storage>
      <postgresql>
        <host>%s</host>
        <port>%d</port>
        <database>%s</database>
        <schema>public</schema>
        <keepalive>
          <enabled>1</enabled>
          <idle_sec>120</idle_sec>
          <interval_sec>30</interval_sec>
          <count>5</count>
        </keepalive>
        <connect_timeout_sec>5</connect_timeout_sec>
      </postgresql>
      <garbage_collection>
        <enabled>true</enabled>
        <interval_seconds>3600</interval_seconds>
        <max_age_seconds>86400</max_age_seconds>
      </garbage_collection>
    </metadata_storage>
  </pensieve_lite>
</config>`,
		MetadataServicePort, pgHost, pgPort, pgDatabase)
}

func metadataConfigMapName(instanceName string) string {
	return instanceName + SuffixMetadataService + "-config"
}

func (r *FireboltInstanceReconciler) ensureMetadataConfigMap(ctx context.Context, instance *computev1alpha1.FireboltInstance, configXML string) error {
	name := metadataConfigMapName(instance.Name)
	labels := instanceLabels(instance.Name, "metadata")

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			"config.xml": configXML,
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Data = desired.Data
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *FireboltInstanceReconciler) ensureMetadataDeployment(ctx context.Context, instance *computev1alpha1.FireboltInstance, configXML string) error {
	name := instance.Name + SuffixMetadataService
	configMapName := metadataConfigMapName(instance.Name)
	labels := instanceLabels(instance.Name, "metadata")
	secretName := metadataCredsSecretName(instance)

	spec := &instance.Spec.Metadata.ComponentSpec

	var replicas int32 = 1
	if instance.Spec.Metadata.Replicas != nil {
		replicas = *instance.Spec.Metadata.Replicas
	}

	image := DefaultMetadataImage
	pullPolicy := corev1.PullIfNotPresent
	if spec.Image != nil {
		image = spec.Image.Repository + ":" + spec.Image.Tag
		pullPolicy = spec.Image.PullPolicy
		if pullPolicy == "" {
			pullPolicy = corev1.PullIfNotPresent
		}
	}

	configHash := contentHash(configXML)

	maxSurge := intstr.FromInt32(1)
	maxUnavailable := intstr.FromInt32(0)

	podLabels := mergeMaps(labels, spec.Labels)
	podAnnotations := mergeMaps(map[string]string{
		"firebolt.io/config-hash": configHash,
	}, spec.Annotations)

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: &maxUnavailable,
					MaxSurge:       &maxSurge,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: int64Ptr(30),
					Containers: []corev1.Container{{
						Name:            metadataContainerName,
						Image:           image,
						ImagePullPolicy: pullPolicy,
						Command:         []string{"/dedicated-pensieve", "--config", "/configs/config.xml"},
						Ports: []corev1.ContainerPort{
							{Name: "grpc", ContainerPort: int32(MetadataServicePort), Protocol: corev1.ProtocolTCP},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{
									Port: intstr.FromInt32(int32(MetadataServicePort)),
								},
							},
							InitialDelaySeconds: 2,
							PeriodSeconds:       10,
							FailureThreshold:    3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								GRPC: &corev1.GRPCAction{
									Port:    int32(MetadataServicePort),
									Service: strPtr(""),
								},
							},
							InitialDelaySeconds: 2,
							PeriodSeconds:       5,
							FailureThreshold:    3,
						},
						Env: []corev1.EnvVar{
							{Name: "POSTGRES_USERNAME_FILE", Value: metadataCredsMount + "/username"},
							{Name: "POSTGRES_PASSWORD_FILE", Value: metadataCredsMount + "/password"},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: metadataConfigMount, ReadOnly: true},
							{Name: "postgres-creds", MountPath: metadataCredsMount, ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
								},
							},
						},
						{
							Name: "postgres-creds",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secretName,
								},
							},
						},
					},
				},
			},
		},
	}

	if spec.Resources != nil {
		desired.Spec.Template.Spec.Containers[0].Resources = *spec.Resources
	}
	if len(spec.NodeSelector) > 0 {
		desired.Spec.Template.Spec.NodeSelector = spec.NodeSelector
	}
	if len(spec.Tolerations) > 0 {
		desired.Spec.Template.Spec.Tolerations = spec.Tolerations
	}
	if spec.Affinity != nil {
		desired.Spec.Template.Spec.Affinity = spec.Affinity
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Strategy = desired.Spec.Strategy
	return r.Update(ctx, existing)
}

func (r *FireboltInstanceReconciler) ensureMetadataService(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixMetadataService
	labels := instanceLabels(instance.Name, "metadata")

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: int32(MetadataServicePort), TargetPort: intstr.FromInt32(int32(MetadataServicePort)), Protocol: corev1.ProtocolTCP},
			},
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	return r.Update(ctx, existing)
}

// contentHash returns a truncated SHA-256 hash of the given string, used
// as a pod-template annotation to trigger rollouts on config changes.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])[:16]
}

func int64Ptr(v int64) *int64 { return &v }
func strPtr(v string) *string { return &v }

// mergeMaps returns a map containing all entries from base, with overrides
// merged on top. Nil-safe: treats either argument being empty as a no-op.
//
// WARNING: when overrides is empty this returns base BY REFERENCE, not a
// copy. A caller that subsequently mutates the returned map will mutate
// the base map as well. All current call sites pass a freshly constructed
// base (e.g. a map literal, or the output of an `instanceLabels`-style
// helper) so the aliasing is harmless today. If you add a caller that
// threads a shared/cached map through `base`, either hand in a copy or
// change this helper to always allocate.
func mergeMaps(base, overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}
