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

// ensurePostgresSecret is the single exception to the SSA-Apply idiom
// used by every other ensure* path. The Secret is created exactly
// once per FireboltInstance with a freshly-generated password and
// every subsequent reconcile MUST skip the write so the password is
// preserved — the metadata and PostgreSQL pods bind to whatever the
// Secret carried at startup.
//
// SSA Apply is the wrong primitive here on two counts:
//
//  1. With ForceOwnership, Apply is an upsert that would silently
//     overwrite the stored password with a freshly-generated value
//     whenever this function ran a second time (e.g. after a cached
//     Get returned a stale NotFound because the informer had not yet
//     observed the Secret at operator startup or after a restart).
//     The running pods would keep their old password and start
//     failing authentication against a rotated Secret — a silent
//     outage with no operator log line marking the moment of damage.
//
//  2. Without ForceOwnership, Apply would conflict against the
//     operator field manager's own previous claim on the password
//     field whenever we tried to re-apply with a different value,
//     turning the same stale-cache scenario into a perpetually
//     erroring reconcile loop instead of an outage. Neither failure
//     mode is acceptable.
//
// r.Create handles both concerns atomically: the API server rejects a
// duplicate Create with AlreadyExists, which we then resolve into a
// fresh Get + validatePostgresSecret to confirm the surviving Secret
// is structurally compatible with what the metadata and PostgreSQL
// pods bind to. The cached Get on the fast path runs the same
// validator so a pre-existing Secret that is the wrong shape — wrong
// keys, wrong username, wrong database, wrong type — surfaces as a
// loud reconcile error instead of being silently accepted. A
// pre-existing matching Secret (e.g. operator restart, manual restore
// from backup with the right contents) passes both paths without
// touching the password.
func (r *FireboltInstanceReconciler) ensurePostgresSecret(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := pgCredentialsSecretName(instance.Name)
	key := types.NamespacedName{Name: name, Namespace: instance.Namespace}

	existing := &corev1.Secret{}
	err := r.Get(ctx, key, existing)
	if err == nil {
		return validatePostgresSecret(existing)
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

	if err := r.Create(ctx, secret); err != nil {
		if !errors.IsAlreadyExists(err) {
			return err
		}
		// Cache lag or interleaved reconcile created the Secret
		// between our cached Get and our Create. Re-fetch with the
		// live client and run the same validator the fast path
		// uses — accepting a stale, wrong-shape Secret here would
		// hand the metadata and PostgreSQL pods a password their
		// peers do not have.
		if err := r.Get(ctx, key, existing); err != nil {
			return fmt.Errorf("re-Get postgres secret after AlreadyExists: %w", err)
		}
		return validatePostgresSecret(existing)
	}
	return nil
}

// validatePostgresSecret confirms an existing Secret carries the
// shape the metadata and PostgreSQL pods bind to. Returns nil when
// the Secret is structurally compatible (right type, right keys,
// constants match), or a descriptive error otherwise. The password
// value is checked for non-emptiness only — it is generated per
// FireboltInstance and the operator has no expected value to compare
// against; the postgres pod's `initdb` will accept any non-empty
// password and bake it into its on-disk auth state.
func validatePostgresSecret(s *corev1.Secret) error {
	if s.Type != corev1.SecretTypeOpaque {
		return fmt.Errorf("existing postgres-credentials Secret %q has type %q, want %q",
			s.Name, s.Type, corev1.SecretTypeOpaque)
	}
	for _, k := range []string{"username", "password", "database"} {
		if v, ok := s.Data[k]; !ok || len(v) == 0 {
			return fmt.Errorf("existing postgres-credentials Secret %q is missing or empty for required key %q", s.Name, k)
		}
	}
	if got := string(s.Data["username"]); got != PostgresUser {
		return fmt.Errorf("existing postgres-credentials Secret %q has username=%q, want %q",
			s.Name, got, PostgresUser)
	}
	if got := string(s.Data["database"]); got != PostgresDBName {
		return fmt.Errorf("existing postgres-credentials Secret %q has database=%q, want %q",
			s.Name, got, PostgresDBName)
	}
	return nil
}

// ensurePostgresStatefulSet uses Server-Side Apply (see the design
// note above ensureGatewayConfigMap in instance_gateway.go). The
// operator owns the StatefulSet's spec including pod-level
// SecurityContext, Volumes, Containers, and EnableServiceLinks — the
// fields the pre-SSA implementation re-asserted explicitly because
// the container hardening depends on them as a set (RunAsNonRoot
// requires FSGroup PVC ownership; read-only root FS requires the
// writable emptyDirs at /var/run/postgresql and /tmp). With SSA the
// "as a set" invariant becomes structural: the operator's apply
// declares all of them, so the apiserver carries them together and
// any future hardening that adds a new field is automatically owned
// alongside the existing ones.
func (r *FireboltInstanceReconciler) ensurePostgresStatefulSet(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	desired := buildPostgresStatefulSet(instance)
	desired.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}
	return applySSA(ctx, r.Client, desired)
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
						ImagePullPolicy: resolveContainerImagePullPolicy(PostgresImage, ""),
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
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
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
	return applySSA(ctx, r.Client, desired)
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
