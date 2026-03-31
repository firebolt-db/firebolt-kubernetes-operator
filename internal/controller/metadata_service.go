package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/core-operator/api/v1alpha1"
	"github.com/firebolt-analytics/core-operator/charts"
	helmrender "github.com/firebolt-analytics/core-operator/internal/helm"
)

func (r *FireboltEngineReconciler) ensureMetadataService(ctx context.Context, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec) error {
	log := logf.FromContext(ctx)
	log.V(1).Info("Ensuring metadata service resources")

	if err := r.ensurePostgreSQL(ctx, engine); err != nil {
		return fmt.Errorf("failed to ensure PostgreSQL: %w", err)
	}

	if err := r.ensurePensieve(ctx, engine, spec); err != nil {
		return fmt.Errorf("failed to ensure metadata service: %w", err)
	}

	return nil
}

// --- PostgreSQL resources ---

func (r *FireboltEngineReconciler) ensurePostgreSQL(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	if err := r.ensurePostgresSecret(ctx, engine); err != nil {
		return err
	}
	if err := r.ensurePostgresPVC(ctx, engine); err != nil {
		return err
	}
	if err := r.ensurePostgresDeployment(ctx, engine); err != nil {
		return err
	}
	return r.ensurePostgresService(ctx, engine)
}

func pgSecretName(engineName string) string { return engineName + SuffixMetadataPGCreds }
func pgName(engineName string) string       { return engineName + SuffixMetadataPG }
func metadataName(engineName string) string  { return engineName + SuffixMetadata }

// deterministicPassword generates a stable password from the engine name so the
// secret can be created idempotently without storing state.
func deterministicPassword(engineName string) string {
	h := sha256.New()
	io.WriteString(h, "firebolt-pg-creds:")
	io.WriteString(h, engineName)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (r *FireboltEngineReconciler) ensurePostgresSecret(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	name := pgSecretName(engine.Name)
	ns := engine.Namespace

	existing := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{LabelEngine: engine.Name},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": PostgresUser,
			"password": deterministicPassword(engine.Name),
		},
	}

	if err := controllerutil.SetControllerReference(engine, secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}
	return r.Create(ctx, secret)
}

func (r *FireboltEngineReconciler) ensurePostgresPVC(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	name := pgName(engine.Name)
	ns := engine.Namespace

	existing := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{LabelEngine: engine.Name},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(PostgresPVCSize),
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(engine, pvc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}
	return r.Create(ctx, pvc)
}

func (r *FireboltEngineReconciler) ensurePostgresDeployment(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	name := pgName(engine.Name)
	ns := engine.Namespace
	secretName := pgSecretName(engine.Name)

	existing := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	replicas := int32(1)
	labels := map[string]string{
		LabelEngine:                engine.Name,
		"app.kubernetes.io/name":   "postgresql",
		"app.kubernetes.io/part-of": engine.Name,
	}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{LabelEngine: engine.Name},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            "postgresql",
						Image:           PostgresImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Ports: []corev1.ContainerPort{{
							Name:          "postgres",
							ContainerPort: int32(PostgresPort),
							Protocol:      corev1.ProtocolTCP,
						}},
						Env: []corev1.EnvVar{
							{Name: "POSTGRES_DB", Value: PostgresDBName},
							{Name: "POSTGRES_USER", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
									Key:                  "username",
								},
							}},
							{Name: "POSTGRES_PASSWORD", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
									Key:                  "password",
								},
							}},
							{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/var/lib/postgresql/data",
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("50m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{
									Port: intstr.FromInt(PostgresPort),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       5,
						},
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: name,
							},
						},
					}},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(engine, deploy, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}
	return r.Create(ctx, deploy)
}

func (r *FireboltEngineReconciler) ensurePostgresService(ctx context.Context, engine *computev1alpha1.FireboltEngine) error {
	name := pgName(engine.Name)
	ns := engine.Namespace

	existing := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{LabelEngine: engine.Name},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Selector: map[string]string{
				LabelEngine:                engine.Name,
				"app.kubernetes.io/name":   "postgresql",
				"app.kubernetes.io/part-of": engine.Name,
			},
			Ports: []corev1.ServicePort{{
				Name:       "postgres",
				Port:       int32(PostgresPort),
				TargetPort: intstr.FromInt(PostgresPort),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}

	if err := controllerutil.SetControllerReference(engine, svc, r.Scheme); err != nil {
		return fmt.Errorf("failed to set owner reference: %w", err)
	}
	return r.Create(ctx, svc)
}

// --- Dedicated Pensieve (rendered from Helm chart) ---

func (r *FireboltEngineReconciler) ensurePensieve(ctx context.Context, engine *computev1alpha1.FireboltEngine, spec *computev1alpha1.FireboltEngineSpec) error {
	releaseName := metadataName(engine.Name)
	ns := engine.Namespace

	pensieveImage := r.resolvePensieveImage(spec)

	values := map[string]interface{}{
		"image": map[string]interface{}{
			"repository": pensieveImage.Repository,
			"tag":        pensieveImage.Tag,
		},
		"postgresql": map[string]interface{}{
			"host":     pgName(engine.Name),
			"port":     PostgresPort,
			"database": PostgresDBName,
			"credentials": map[string]interface{}{
				"existingSecret": pgSecretName(engine.Name),
			},
		},
		"fullnameOverride": releaseName,
	}

	rendered, err := helmrender.RenderChart(
		charts.DedicatedPensieveChart,
		"dedicated-pensieve",
		releaseName,
		ns,
		values,
	)
	if err != nil {
		return fmt.Errorf("failed to render metadata service chart: %w", err)
	}

	for templateName, manifest := range rendered {
		if strings.Contains(templateName, "secret.yaml") {
			continue
		}
		if err := r.applyRenderedManifest(ctx, engine, manifest); err != nil {
			return fmt.Errorf("failed to apply %s: %w", templateName, err)
		}
	}

	return nil
}

func (r *FireboltEngineReconciler) resolvePensieveImage(spec *computev1alpha1.FireboltEngineSpec) computev1alpha1.ImageSpec {
	if spec.MetadataService != nil && spec.MetadataService.Image != nil {
		img := *spec.MetadataService.Image
		if img.PullPolicy == "" {
			img.PullPolicy = corev1.PullIfNotPresent
		}
		return img
	}

	repo := spec.Image.Repository
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		repo = repo[:idx] + "/dedicated-pensieve"
	} else {
		repo = "dedicated-pensieve"
	}

	return computev1alpha1.ImageSpec{
		Repository: repo,
		Tag:        spec.Image.Tag,
		PullPolicy: corev1.PullIfNotPresent,
	}
}

func manifestHash(manifest string) string {
	h := sha256.Sum256([]byte(manifest))
	return hex.EncodeToString(h[:])[:16]
}

func (r *FireboltEngineReconciler) applyRenderedManifest(ctx context.Context, engine *computev1alpha1.FireboltEngine, manifest string) error {
	log := logf.FromContext(ctx)
	hash := manifestHash(manifest)

	decoder := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to decode manifest: %w", err)
		}

		if obj.GetKind() == "" {
			continue
		}

		obj.SetNamespace(engine.Namespace)

		labels := obj.GetLabels()
		if labels == nil {
			labels = make(map[string]string)
		}
		labels[LabelEngine] = engine.Name
		obj.SetLabels(labels)

		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[AnnotationManifestHash] = hash
		obj.SetAnnotations(annotations)

		if err := controllerutil.SetControllerReference(engine, obj, r.Scheme); err != nil {
			return fmt.Errorf("failed to set owner reference on %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}

		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GroupVersionKind())
		err := r.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
		if errors.IsNotFound(err) {
			if err := r.Create(ctx, obj); err != nil {
				return fmt.Errorf("failed to create %s/%s: %w", obj.GetKind(), obj.GetName(), err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("failed to get %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}

		existingHash := existing.GetAnnotations()[AnnotationManifestHash]
		if existingHash == hash {
			continue
		}

		log.Info("Updating metadata service resource",
			"kind", obj.GetKind(), "name", obj.GetName())

		// Carry over all non-metadata fields from the desired object
		for key, value := range obj.Object {
			if key == "metadata" || key == "status" {
				continue
			}
			existing.Object[key] = value
		}

		existingAnnotations := existing.GetAnnotations()
		if existingAnnotations == nil {
			existingAnnotations = make(map[string]string)
		}
		existingAnnotations[AnnotationManifestHash] = hash
		existing.SetAnnotations(existingAnnotations)

		existingLabels := existing.GetLabels()
		if existingLabels == nil {
			existingLabels = make(map[string]string)
		}
		for k, v := range obj.GetLabels() {
			existingLabels[k] = v
		}
		existing.SetLabels(existingLabels)

		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("failed to update %s/%s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}

	return nil
}

// MetadataServiceEndpoint returns the in-cluster endpoint for the metadata service.
// isMetadataServiceReady returns true when the metadata service (dedicated-pensieve)
// Deployment has at least one ready replica.
func (r *FireboltEngineReconciler) isMetadataServiceReady(ctx context.Context, engine *computev1alpha1.FireboltEngine) (bool, error) {
	var dep appsv1.Deployment
	key := types.NamespacedName{Name: metadataName(engine.Name), Namespace: engine.Namespace}
	if err := r.Get(ctx, key, &dep); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas > 0, nil
}

func MetadataServiceEndpoint(engineName, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", metadataName(engineName), namespace, MetadataServicePort)
}
