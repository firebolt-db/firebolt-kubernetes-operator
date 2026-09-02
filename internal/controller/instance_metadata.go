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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	metadataCredsMount         = "/secrets/postgres" //nolint:gosec // mount path, not a credential
	metadataPostgresCAMount    = "/secrets/postgres-ca"
	metadataPostgresCAFileName = "ca.crt"
	metadataConfigMount        = "/configs"
)

// ensureMetadataResources creates or updates the ConfigMap, Deployment, and
// Service for the metadata service.
func (r *FireboltInstanceReconciler) ensureMetadataResources(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	configYAML := buildMetadataConfigYAML(instance)

	if err := r.ensureMetadataConfigMap(ctx, instance, configYAML); err != nil {
		return fmt.Errorf("ensuring metadata configmap: %w", err)
	}

	if err := r.ensureMetadataDeployment(ctx, instance, configYAML); err != nil {
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

func buildMetadataConfigYAML(instance *computev1alpha1.FireboltInstance) string {
	pgHost := pgResourceName(instance.Name) + "." + instance.Namespace + ".svc.cluster.local"
	pgPort := int32(PostgresPort)
	pgDatabase := PostgresDBName
	// Internal Postgres is bootstrapped with the default "public" schema and is
	// not user-configurable; only the external-postgres path honors a custom
	// schema below. Legacy service only; metadata-ng does not render it.
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
	// and MUST be rendered as quoted YAML scalars (via yamlString) to prevent
	// injection of additional YAML keys that would alter the pensieve
	// configuration. The CRD also applies a Pattern admission check on
	// host/database/schema as defense-in-depth.
	//
	// Both metadata service generations load a YAML map rooted at pensieve_lite.
	// metadata-ng renders only the keys it acts on. It omits the legacy-only
	// keepalive, garbage-collection, thread-count and logging settings, and
	// default_account_id and postgresql.schema (it is isolated by database and
	// manages its own schemas). It warns at startup about any of them.
	if instance.Spec.MetadataNG {
		return fmt.Sprintf(`pensieve_lite:
  host: 0.0.0.0
  port: %d
  metadata_storage:
    postgresql:
      host: %s
      port: %d
      database: %s
`,
			MetadataServicePort, yamlString(pgHost), pgPort, yamlString(pgDatabase))
	}

	// The legacy metadata image loads YAML config since FB-2743; the document
	// root must be a YAML map (a scalar/sequence root is rejected).
	return fmt.Sprintf(`pensieve_lite:
  default_account_id: %s
  host: 0.0.0.0
  port: %d
  server_threads: 0
  log_level: information
  metadata_storage:
    postgresql:
      host: %s
      port: %d
      database: %s
      schema: %s
      keepalive:
        enabled: 1
        idle_sec: 120
        interval_sec: 30
        count: 5
      connect_timeout_sec: 5
    garbage_collection:
      enabled: true
      interval_ms: 3600000
      time_horizon_sec: 86400
`,
		yamlString(instance.Spec.ID), MetadataServicePort,
		yamlString(pgHost), pgPort, yamlString(pgDatabase), yamlString(pgSchema))
}

// yamlString renders s as a YAML-safe double-quoted scalar. A JSON string
// literal is always a valid YAML double-quoted scalar (YAML is a JSON
// superset), so json.Marshal yields correct quoting and escaping for every
// user-controlled field interpolated into the pensieve config template,
// preventing YAML injection. json.Marshal never returns an error for a string.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
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
func (r *FireboltInstanceReconciler) ensureMetadataConfigMap(ctx context.Context, instance *computev1alpha1.FireboltInstance, configYAML string) error {
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
			"config.yaml": configYAML,
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
func (r *FireboltInstanceReconciler) ensureMetadataDeployment(ctx context.Context, instance *computev1alpha1.FireboltInstance, configYAML string) error {
	desired := buildMetadataDeployment(instance, configYAML)
	desired.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}

	configHash, err := r.metadataConfigHash(ctx, instance, configYAML)
	if err != nil {
		return err
	}
	desired.Spec.Template.Annotations[AnnotationConfigHash] = configHash

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

// metadataConfigHash preserves the config-only rollout hash for internal
// PostgreSQL. For external PostgreSQL it also includes the referenced
// credentials and CA Secret ResourceVersions, when applicable, so an in-place
// rotation rolls the metadata pod. ResourceVersion, not Secret bytes, detects
// every write without moving Secret material into a pod annotation.
func (r *FireboltInstanceReconciler) metadataConfigHash(
	ctx context.Context,
	instance *computev1alpha1.FireboltInstance,
	configYAML string,
) (string, error) {
	if instance.Spec.Metadata.Postgres == nil {
		return contentHash(configYAML), nil
	}

	name := instance.Spec.Metadata.Postgres.CredentialsSecretRef.Name
	credentialsVersion, err := secretResourceVersion(ctx, r.Client, instance.Namespace, name)
	if err != nil {
		return "", fmt.Errorf("reading metadata credentials Secret %s/%s for rollout hash: %w",
			instance.Namespace, name, err)
	}
	hashParts := [][]byte{
		[]byte("metadata-config-and-postgres-secret-versions-v1"),
		[]byte(configYAML),
		[]byte(name),
		[]byte(credentialsVersion),
	}

	tls := instance.Spec.Metadata.Postgres.TLS
	if tls != nil {
		caName := tls.CASecretRef.Name
		caKey := tls.CASecretRef.Key
		if _, err := checkSecretKeyPresent(
			ctx,
			r.Client,
			instance.Namespace,
			caName,
			caKey,
			"external postgres CA Secret",
		); err != nil {
			return "", fmt.Errorf("reading metadata postgres CA for rollout hash: %w", err)
		}
		caVersion, err := secretResourceVersion(ctx, r.Client, instance.Namespace, caName)
		if err != nil {
			return "", fmt.Errorf("reading metadata postgres CA version for rollout hash: %w", err)
		}
		hashParts = append(hashParts, []byte(caName), []byte(caKey), []byte(caVersion))
	}
	return aggregateContentHash(hashParts...), nil
}

// secretResourceVersion requests only object metadata. Keeping Secret data out
// of this rollout-hash path makes the non-sensitive input boundary explicit.
func secretResourceVersion(ctx context.Context, cli client.Client, namespace, name string) (string, error) {
	metadata := &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
	}
	if err := cli.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, metadata); err != nil {
		return "", err
	}
	return metadata.ResourceVersion, nil
}

// buildMetadataDeployment returns the desired Deployment object for the
// metadata service. The pod is hardened to the same standard as
// the internal PostgreSQL and Envoy gateway pods: it runs as the image's
// generation-specific built-in non-root user, drops all
// Linux capabilities, sets `RuntimeDefault` seccomp, denies privilege
// escalation, and uses a read-only root filesystem backed by an emptyDir
// at `/tmp` for the binary's transient files. `automountServiceAccountToken`
// is false because pensieve does not call the Kubernetes API; an attacker
// with code execution inside the container therefore has neither a SA
// token to reach the API server nor a writable rootfs to stage payloads on.
func buildMetadataDeployment(instance *computev1alpha1.FireboltInstance, configYAML string) *appsv1.Deployment {
	name := instance.Name + SuffixMetadataService
	labels := instanceLabels(instance.Name, "metadata")

	var replicas int32 = 1
	if instance.Spec.Metadata.Replicas != nil {
		replicas = *instance.Spec.Metadata.Replicas
	}

	configHash := contentHash(configYAML)

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
	pullPolicy := metadataImagePullPolicy(userPrimary, image)

	metadataUID := metadataRunAsUID(instance)
	configMapName := metadataConfigMapName(instance.Name)
	secretName := metadataCredsSecretName(instance)

	pensieve := corev1.Container{
		Name:            computev1alpha1.MetadataContainerName,
		Image:           image,
		ImagePullPolicy: pullPolicy,
		Command:         []string{"/dedicated-pensieve", "--config", "/configs/config.yaml"},
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
	if tls := instance.Spec.Metadata.Postgres; tls != nil && tls.TLS != nil {
		pensieve.Env = append(pensieve.Env,
			corev1.EnvVar{
				Name:  computev1alpha1.MetadataPostgresSSLModeEnvKey,
				Value: string(computev1alpha1.PostgresTLSModeVerifyFull),
			},
			corev1.EnvVar{
				Name:  computev1alpha1.MetadataPostgresSSLRootCertEnvKey,
				Value: metadataPostgresCAMount + "/" + metadataPostgresCAFileName,
			},
		)
		pensieve.VolumeMounts = append(pensieve.VolumeMounts, corev1.VolumeMount{
			Name:      computev1alpha1.MetadataPostgresCAVolumeName,
			MountPath: metadataPostgresCAMount,
			ReadOnly:  true,
		})
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
	if pg := instance.Spec.Metadata.Postgres; pg != nil && pg.TLS != nil {
		operatorVolumes = append(operatorVolumes, corev1.Volume{
			Name: computev1alpha1.MetadataPostgresCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: pg.TLS.CASecretRef.Name,
					Items: []corev1.KeyToPath{{
						Key:  pg.TLS.CASecretRef.Key,
						Path: metadataPostgresCAFileName,
					}},
				},
			},
		})
	}

	containers := append([]corev1.Container{pensieve}, userSidecars...)
	volumes := appendUserVolumes(operatorVolumes, userPodSpec.Volumes, instanceProtectedSecret(instance),
		computev1alpha1.MetadataConfigVolumeName,
		computev1alpha1.MetadataPostgresCredsVolumeName,
		computev1alpha1.MetadataPostgresCAVolumeName,
		computev1alpha1.MetadataTmpVolumeName,
	)

	// metadataPodSecurityContext starts from the user-supplied
	// PodSecurityContext (deep-copied) and stamps the operator's
	// non-root posture on top. RunAsUser/RunAsGroup are forced to
	// the generation-specific metadata UID because files in each image are
	// owned by that built-in user;
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

// metadataRunAsUID returns the built-in user identity for the selected metadata
// service generation. The mode does not infer or rewrite the image reference.
func metadataRunAsUID(instance *computev1alpha1.FireboltInstance) int64 {
	if instance.Spec.MetadataNG {
		return MetadataNGUID
	}
	return MetadataUID
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

// metadataImagePullPolicy returns the user-supplied pull policy on the
// metadata primary container, falling back to the workload default rule for
// the resolved image (the Kubernetes tag-based default, with "dev" treated
// like ":latest" — see resolveWorkloadImagePullPolicy).
func metadataImagePullPolicy(primary *corev1.Container, image string) corev1.PullPolicy {
	if primary != nil && primary.ImagePullPolicy != "" {
		return primary.ImagePullPolicy
	}
	return resolveWorkloadImagePullPolicy(image)
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

// aggregateContentHash length-prefixes its inputs before hashing them so byte
// boundaries cannot collide. Only the final aggregate digest is returned.
func aggregateContentHash(parts ...[]byte) string {
	var content []byte
	for _, part := range parts {
		content = binary.BigEndian.AppendUint64(content, uint64(len(part)))
		content = append(content, part...)
	}
	h := sha256.Sum256(content)
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
