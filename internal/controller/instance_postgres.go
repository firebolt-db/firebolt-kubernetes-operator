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
	"crypto/rand"
	"encoding/hex"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

func (r *FireboltInstanceReconciler) ensurePostgreSQL(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	if err := r.ensurePostgresSecret(ctx, instance); err != nil {
		return fmt.Errorf("ensuring postgres secret: %w", err)
	}

	if err := r.ensurePostgresStatefulSet(ctx, instance); err != nil {
		return fmt.Errorf("ensuring postgres statefulset: %w", err)
	}

	if err := r.ensurePostgresService(ctx, instance); err != nil {
		return fmt.Errorf("ensuring postgres service: %w", err)
	}

	log.Info("PostgreSQL resources ensured")
	return nil
}

func pgResourceName(instanceName string) string {
	return instanceName + SuffixMetadataPG
}

func pgCredentialsSecretName(instanceName string) string {
	return instanceName + SuffixMetadataPostgresCreds
}

func (r *FireboltInstanceReconciler) ensurePostgresSecret(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := pgCredentialsSecretName(instance.Name)
	existing := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	password, err := generatePassword(24)
	if err != nil {
		return fmt.Errorf("generating postgres password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    instanceLabels(instance.Name, "postgres"),
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": PostgresUser,
			"password": password,
			"database": PostgresDBName,
		},
	}

	if err := controllerutil.SetControllerReference(instance, secret, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, secret)
}

func (r *FireboltInstanceReconciler) ensurePostgresStatefulSet(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := pgResourceName(instance.Name)
	secretName := pgCredentialsSecretName(instance.Name)
	labels := instanceLabels(instance.Name, "postgres")
	var replicas int32 = 1

	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "postgresql",
						Image:           PostgresImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{
							{Name: "tcp-postgresql", ContainerPort: PostgresPort, Protocol: corev1.ProtocolTCP},
						},
						Env: []corev1.EnvVar{
							envFromSecret("POSTGRES_USER", secretName, "username"),
							envFromSecret("POSTGRES_PASSWORD", secretName, "password"),
							envFromSecret("POSTGRES_DB", secretName, "database"),
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("25m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "data", MountPath: "/var/lib/postgresql/data", SubPath: "pgdata"},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{
									Port: intstr.FromInt32(PostgresPort),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       5,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"pg_isready", "-U", PostgresUser},
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       10,
						},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "data",
					Labels: labels,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(PostgresPVCSize),
						},
					},
				},
			}},
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	return r.Update(ctx, existing)
}

func (r *FireboltInstanceReconciler) ensurePostgresService(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := pgResourceName(instance.Name)
	labels := instanceLabels(instance.Name, "postgres")

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  labels,
			Ports: []corev1.ServicePort{
				{Name: "tcp-postgresql", Port: PostgresPort, TargetPort: intstr.FromInt32(PostgresPort), Protocol: corev1.ProtocolTCP},
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

func envFromSecret(envName, secretName, secretKey string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  secretKey,
			},
		},
	}
}

// generatePassword returns a hex-encoded random string of exactly `length`
// characters. Each output character is backed by a fresh random byte:
// hex encoding produces 2 characters per byte, so (length+1)/2 bytes yield
// at least `length` hex characters, which we then truncate to `length`.
// This gives callers the entropy they implicitly expect from a string of
// N hex characters (~4 bits per character), rather than the 2-bits-per-char
// effective entropy that would result from allocating exactly `length`
// bytes and then discarding half of their hex encoding.
func generatePassword(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("generatePassword: length must be > 0, got %d", length)
	}
	b := make([]byte, (length+1)/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b)[:length], nil
}
