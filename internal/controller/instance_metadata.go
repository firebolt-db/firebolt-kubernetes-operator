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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	metadataCredsMount  = "/secrets/postgres" //nolint:gosec // mount path, not a credential
	metadataConfigMount = "/configs"
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
	// Internal Postgres is bootstrapped with the default "public" schema and is
	// not user-configurable; only the external-postgres path honors a custom
	// schema below.
	pgSchema := PostgresDefaultSchema

	if instance.Spec.Metadata.Postgres != nil {
		pgHost = instance.Spec.Metadata.Postgres.Host
		pgPort = instance.Spec.Metadata.Postgres.Port
		if pgPort == 0 {
			pgPort = int32(PostgresPort)
		}
		pgDatabase = instance.Spec.Metadata.Postgres.Database
		// Fall back to the default schema when the field is empty so the
		// controller stays correct on CRs created before the schema field
		// existed, or when the defaulting admission path is bypassed.
		if instance.Spec.Metadata.Postgres.Schema != "" {
			pgSchema = instance.Spec.Metadata.Postgres.Schema
		}
	}

	// All string fields interpolated below originate from user-controlled
	// CRD inputs (spec.id, spec.metadata.postgres.{host,database,schema})
	// and MUST be XML-escaped to prevent injection of additional XML
	// elements that would alter the pensieve configuration. The CRD also
	// applies a Pattern admission check on host/database/schema as
	// defense-in-depth (FB-1163).
	return fmt.Sprintf(`<?xml version="1.0"?>
<config>
  <pensieve_lite>
    <default_account_id>%s</default_account_id>
    <host>0.0.0.0</host>
    <port>%d</port>
    <server_threads>0</server_threads>
    <log_level>information</log_level>
    <metadata_storage>
      <postgresql>
        <host>%s</host>
        <port>%d</port>
        <database>%s</database>
        <schema>%s</schema>
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
		xmlEscape(instance.Spec.ID), MetadataServicePort,
		xmlEscape(pgHost), pgPort, xmlEscape(pgDatabase), xmlEscape(pgSchema))
}

// xmlEscape returns s with XML metacharacters replaced by their entity
// references, suitable for safe interpolation as element content. Used
// for every user-controlled field that buildMetadataConfigXML pastes
// into the pensieve config template.
func xmlEscape(s string) string {
	var buf bytes.Buffer
	// xml.EscapeText only fails when the writer fails; bytes.Buffer
	// never returns an error from Write, so the error is unreachable.
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func metadataConfigMapName(instanceName string) string {
	return instanceName + SuffixMetadataService + "-config"
}

// Each metadata ensure* function below writes its resource via
// Server-Side Apply (applySSA) with FieldManager
// OperatorFieldManager and ForceOwnership. SSA lets the operator
// declare exactly the fields it owns (everything in the `desired`
// literal) while preserving foreign-managed fields a user may have
// added via kubectl/SSA from a different field manager — extra
// labels, annotations, sidecar containers, additional volumes. The
// ForceOwnership flag means the operator wins on every conflict over
// fields it does declare; users wanting to override an
// operator-managed field must use spec.metadata.template (the
// operator-rendered fields then read from the user's template, which
// goes through the validating webhook).
//
// The apiserver short-circuits no-op applies — generation is not
// bumped when the resulting object matches what is already stored,
// so the Deployment controller does not see spurious rollouts even
// though the operator applies on every reconcile.
func (r *FireboltInstanceReconciler) ensureMetadataConfigMap(ctx context.Context, instance *computev1alpha1.FireboltInstance, configXML string) error {
	log := logf.FromContext(ctx).WithValues("instance", instance.Name)

	name := metadataConfigMapName(instance.Name)
	labels := instanceLabels(instance.Name, "metadata")

	desired := &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
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

	log.V(1).Info("Applying metadata ConfigMap", "name", name)
	return applySSA(ctx, r.Client, desired)
}

// ensureMetadataDeployment creates or updates the metadata Deployment for a
// FireboltInstance.
//
// NOTE: no PodDisruptionBudget is created for this Deployment, on purpose.
// Metadata is currently pinned to replicas=1 at the CRD level (CEL rule
// on MetadataSpec + webhook defense-in-depth). Any PDB we could write in
// that configuration is either a no-op (maxUnavailable=1) or actively
// harmful (minAvailable=1 blocks `kubectl drain` on the node hosting the
// metadata pod, forcing an operator to manually delete the PDB for
// routine node maintenance with no availability gain, because there's no
// peer to fail over to). The time to add a PDB is when metadata grows a
// genuine multi-replica HA story (quorum, leader election); revisit then.
func (r *FireboltInstanceReconciler) ensureMetadataDeployment(ctx context.Context, instance *computev1alpha1.FireboltInstance, configXML string) error {
	desired := buildMetadataDeployment(instance, configXML)
	desired.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	log := logf.FromContext(ctx).WithValues("instance", instance.Name)
	log.V(1).Info("Applying metadata Deployment",
		"name", desired.Name,
		"replicas", *desired.Spec.Replicas,
		"image", desired.Spec.Template.Spec.Containers[0].Image)
	return applySSA(ctx, r.Client, desired)
}

// buildMetadataDeployment returns the desired Deployment object for the
// metadata (pensieve) service. The pod is hardened to the same standard as
// the internal PostgreSQL and Envoy gateway pods: it runs as the image's
// built-in non-root `dedicated-pensieve` user (MetadataUID), drops all
// Linux capabilities, sets `RuntimeDefault` seccomp, denies privilege
// escalation, and uses a read-only root filesystem backed by an emptyDir
// at `/tmp` for the binary's transient files. `automountServiceAccountToken`
// is false because pensieve does not call the Kubernetes API; an attacker
// with code execution inside the container therefore has neither a SA
// token to reach the API server nor a writable rootfs to stage payloads on.
func buildMetadataDeployment(instance *computev1alpha1.FireboltInstance, configXML string) *appsv1.Deployment {
	name := instance.Name + SuffixMetadataService
	labels := instanceLabels(instance.Name, "metadata")

	var replicas int32 = 1
	if instance.Spec.Metadata.Replicas != nil {
		replicas = *instance.Spec.Metadata.Replicas
	}

	configHash := contentHash(configXML)

	// Surge=0 + maxUnavailable=1 means the old pod is terminated before the
	// new one is created. The metadata service assumes single-writer against
	// Postgres, so we must never have two metadata pods running concurrently.
	// This trades a brief metadata-unavailable window during rollouts for
	// that guarantee.
	maxSurge := intstr.FromInt32(0)
	maxUnavailable := intstr.FromInt32(1)

	podTemplate := effectiveMetadataPodTemplate(instance, configHash, labels)

	return &appsv1.Deployment{
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
			Template: podTemplate,
		},
	}
}

// effectiveMetadataPodTemplate produces the metadata Deployment's pod
// template by merging the user-supplied
// FireboltInstance.spec.metadata.template with operator-rendered
// fields. Mirrors effectiveGatewayPodTemplate field-for-field; see
// that function's documentation for the precedence rules.
func effectiveMetadataPodTemplate(
	instance *computev1alpha1.FireboltInstance,
	configHash string,
	baseLabels map[string]string,
) corev1.PodTemplateSpec {
	var userPodMeta metav1.ObjectMeta
	var userPodSpec corev1.PodSpec
	if t := instance.Spec.Metadata.Template; t != nil {
		user := t.DeepCopy()
		userPodMeta = user.ObjectMeta
		userPodSpec = user.Spec
	}

	userPrimary, userSidecars := splitUserContainers(userPodSpec.Containers, computev1alpha1.MetadataContainerName)

	image := metadataImageFromUser(userPrimary)
	pullPolicy := metadataImagePullPolicyFromUser(userPrimary)

	metadataUID := MetadataUID
	configMapName := metadataConfigMapName(instance.Name)
	secretName := metadataCredsSecretName(instance)

	pensieve := corev1.Container{
		Name:            computev1alpha1.MetadataContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/dedicated-pensieve", "--config", "/configs/config.xml"},
		Ports: []corev1.ContainerPort{
			{Name: "grpc", ContainerPort: int32(MetadataServicePort), Protocol: corev1.ProtocolTCP},
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             boolPtr(true),
			RunAsUser:                &metadataUID,
			ReadOnlyRootFilesystem:   boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
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
			{Name: computev1alpha1.MetadataPostgresUsernameEnvKey, Value: metadataCredsMount + "/username"},
			{Name: computev1alpha1.MetadataPostgresPasswordEnvKey, Value: metadataCredsMount + "/password"},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: computev1alpha1.MetadataConfigVolumeName, MountPath: metadataConfigMount, ReadOnly: true},
			{Name: computev1alpha1.MetadataPostgresCredsVolumeName, MountPath: metadataCredsMount, ReadOnly: true},
			// Scratch space outside the read-only root fs. The
			// pensieve binary has not been audited for filesystem
			// writes, so back /tmp with an emptyDir as a defensive
			// default; logs go to stderr by config.
			{Name: computev1alpha1.MetadataTmpVolumeName, MountPath: "/tmp"},
		},
	}
	if userPrimary != nil && computev1alpha1.HasContainerResources(userPrimary.Resources) {
		pensieve.Resources = *userPrimary.Resources.DeepCopy()
	}

	operatorVolumes := []corev1.Volume{
		{
			Name: computev1alpha1.MetadataConfigVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
				},
			},
		},
		{
			Name: computev1alpha1.MetadataPostgresCredsVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: secretName},
			},
		},
		{
			Name:         computev1alpha1.MetadataTmpVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	containers := append([]corev1.Container{pensieve}, userSidecars...)
	volumes := appendUserVolumes(operatorVolumes, userPodSpec.Volumes,
		computev1alpha1.MetadataConfigVolumeName,
		computev1alpha1.MetadataPostgresCredsVolumeName,
		computev1alpha1.MetadataTmpVolumeName,
	)

	// metadataPodSecurityContext starts from the user-supplied
	// PodSecurityContext (deep-copied) and stamps the operator's
	// non-root posture on top. RunAsUser/RunAsGroup are forced to
	// MetadataUID because pensieve's data on disk is owned by that
	// UID and the binary is hardcoded to that user inside the image;
	// a different RunAsUser would mismatch the on-disk ownership.
	podSC := metadataPodSecurityContext(userPodSpec.SecurityContext, metadataUID)

	sa := userPodSpec.ServiceAccountName

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: mergeMaps(userPodMeta.Labels, baseLabels),
			Annotations: mergeMaps(userPodMeta.Annotations, map[string]string{
				AnnotationConfigHash: configHash,
			}),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName:            sa,
			TerminationGracePeriodSeconds: int64Ptr(30),
			AutomountServiceAccountToken:  boolPtr(false),
			EnableServiceLinks:            boolPtr(false),
			NodeSelector:                  userPodSpec.NodeSelector,
			Tolerations:                   userPodSpec.Tolerations,
			Affinity:                      userPodSpec.Affinity,
			TopologySpreadConstraints:     userPodSpec.TopologySpreadConstraints,
			PriorityClassName:             userPodSpec.PriorityClassName,
			SecurityContext:               podSC,
			ImagePullSecrets:              userPodSpec.ImagePullSecrets,
			InitContainers:                userPodSpec.InitContainers,
			Containers:                    containers,
			Volumes:                       volumes,
		},
	}
}

// metadataImageFromUser returns the user-supplied image on the
// metadata primary container, falling back to the operator's default
// pensieve image when the user did not set one.
func metadataImageFromUser(primary *corev1.Container) string {
	if primary != nil && primary.Image != "" {
		return primary.Image
	}
	return resolveImageRef(nil, DefaultMetadataRepository, DefaultMetadataTag)
}

// metadataImagePullPolicyFromUser returns the user-supplied pull
// policy on the metadata primary container, falling back to the
// operator's default-resolution rule.
func metadataImagePullPolicyFromUser(primary *corev1.Container) corev1.PullPolicy {
	if primary != nil && primary.ImagePullPolicy != "" {
		return primary.ImagePullPolicy
	}
	if primary != nil && primary.Image != "" {
		return resolveContainerImagePullPolicy(primary.Image, "")
	}
	return resolveImagePullPolicy(nil)
}

// metadataPodSecurityContext composes the pod-level securityContext
// stamped on the metadata Deployment's pod template. The operator
// forces RunAsNonRoot/RunAsUser/RunAsGroup to MetadataUID and pins
// SeccompProfile to RuntimeDefault; everything else passes through
// from the user-supplied PodSecurityContext.
func metadataPodSecurityContext(user *corev1.PodSecurityContext, uid int64) *corev1.PodSecurityContext {
	var out *corev1.PodSecurityContext
	if user != nil {
		out = user.DeepCopy()
	} else {
		out = &corev1.PodSecurityContext{}
	}
	out.RunAsNonRoot = boolPtr(true)
	out.RunAsUser = &uid
	out.RunAsGroup = &uid
	if out.SeccompProfile == nil {
		out.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	}
	return out
}

func (r *FireboltInstanceReconciler) ensureMetadataService(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixMetadataService
	labels := instanceLabels(instance.Name, "metadata")

	desired := &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
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

	log := logf.FromContext(ctx).WithValues("instance", instance.Name)
	log.V(1).Info("Applying metadata Service", "name", name)
	return applySSA(ctx, r.Client, desired)
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
