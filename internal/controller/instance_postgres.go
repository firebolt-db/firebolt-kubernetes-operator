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

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
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
	desired := buildPostgresStatefulSet(instance)

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Drift propagation: re-apply the fields we own on the pod template.
	// Pod-level SecurityContext and Volumes are propagated alongside
	// Containers because the container security hardening relies on them
	// (RunAsNonRoot=true requires FSGroup ownership of the data PVC; the
	// container's read-only root filesystem requires writable emptyDir
	// volumes mounted at /var/run/postgresql and /tmp). If we propagated
	// only Containers we would silently leave older StatefulSets running
	// without the new hardening, or break the pod by mounting a volume
	// that is not declared at the pod level.
	existing.Spec.Template.Spec.SecurityContext = desired.Spec.Template.Spec.SecurityContext
	existing.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
	existing.Spec.Template.Spec.Containers = desired.Spec.Template.Spec.Containers
	existing.Spec.Template.Spec.EnableServiceLinks = desired.Spec.Template.Spec.EnableServiceLinks
	return r.Update(ctx, existing)
}

// buildPostgresStatefulSet returns the desired StatefulSet for the
// per-namespace internal PostgreSQL instance. Kept as a pure function so
// the security-hardened pod template can be unit-tested without envtest.
//
// The pod is hardened to run as the postgres image's built-in non-root
// user (UID 70 in the alpine variant; see PostgresUID) with a read-only
// root filesystem and all Linux capabilities dropped. Two emptyDir
// volumes back the writable paths the postgres entrypoint requires:
// `/var/run/postgresql` (the unix-socket directory and postmaster.pid
// location) and `/tmp` (initdb scratch space). The data directory is a
// PVC, mounted via the StatefulSet's volumeClaimTemplate and chowned to
// the postgres group at mount time via the pod-level FSGroup.
func buildPostgresStatefulSet(instance *computev1alpha1.FireboltInstance) *appsv1.StatefulSet {
	name := pgResourceName(instance.Name)
	secretName := pgCredentialsSecretName(instance.Name)
	labels := instanceLabels(instance.Name, "postgres")
	var replicas int32 = 1

	pgUID := PostgresUID

	return &appsv1.StatefulSet{
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
					// See the equivalent comment in engine_reconcile.go's
					// PodSpec: kill legacy service-link env injection, DNS
					// is the only service-discovery channel here.
					EnableServiceLinks: boolPtr(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: boolPtr(true),
						RunAsUser:    &pgUID,
						RunAsGroup:   &pgUID,
						FSGroup:      &pgUID,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
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
							// PGDATA must be a sub-directory of the
							// mounted PVC, not the mount point itself.
							// kubelet's FSGroup recursive chgrp does
							// not propagate ownership to the volume's
							// mount point (it stays root-owned with
							// only the group set to FSGroup), so
							// initdb's `chmod 0700 $PGDATA` would fail
							// with EPERM on a non-root pod
							// (kubernetes/kubernetes#57923). Pointing
							// PGDATA at a sub-directory the postgres
							// process creates itself sidesteps this:
							// the new directory is owned by UID
							// PostgresUID and chmod succeeds. The
							// on-disk layout inside the PVC is
							// unchanged (data still lives under
							// `pgdata/`) so existing PVCs migrate
							// cleanly when this StatefulSet is rolled.
							{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
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
						SecurityContext: &corev1.SecurityContext{
							RunAsNonRoot:             boolPtr(true),
							RunAsUser:                &pgUID,
							ReadOnlyRootFilesystem:   boolPtr(true),
							AllowPrivilegeEscalation: boolPtr(false),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							// Mount the PVC root, not a SubPath: see
							// the PGDATA env var above for why.
							{Name: "data", MountPath: "/var/lib/postgresql/data"},
							// Backing for the unix-domain socket and
							// postmaster.pid; the postgres entrypoint
							// writes both here. Cannot live on the
							// read-only root fs.
							{Name: "run-postgresql", MountPath: "/var/run/postgresql"},
							// initdb and the postgres process write
							// transient files to /tmp; an emptyDir
							// keeps that compatible with a read-only
							// root fs without leaking onto the data PVC.
							{Name: "tmp", MountPath: "/tmp"},
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
					Volumes: []corev1.Volume{
						{
							Name:         "run-postgresql",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
						{
							Name:         "tmp",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
					},
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
