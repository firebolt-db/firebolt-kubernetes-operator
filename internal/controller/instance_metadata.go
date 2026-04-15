/*
Copyright 2025.

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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
	helmutil "github.com/firebolt-analytics/firebolt-kubernetes-operator/internal/helm"
)

const defaultMetadataChartVersion = "0.1.0"

// ensureMetadataService loads the metadata service Helm chart, renders it with
// values derived from the instance spec, and applies the resulting manifests.
func (r *FireboltInstanceReconciler) ensureMetadataService(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	version := instance.Spec.MetadataChartVersion
	if version == "" {
		version = defaultMetadataChartVersion
	}

	ch, err := r.ChartCache.GetOrLoad(r.MetadataChartSource, version)
	if err != nil {
		return fmt.Errorf("loading metadata chart: %w", err)
	}

	if err := r.ensureMetadataPostgresCredsSecret(ctx, instance); err != nil {
		return fmt.Errorf("ensuring metadata postgres credentials secret: %w", err)
	}

	values, err := r.buildMetadataValues(instance)
	if err != nil {
		return fmt.Errorf("building metadata values: %w", err)
	}
	releaseName := instance.Name + SuffixMetadataService
	manifest, err := helmutil.RenderChart(ch, releaseName, instance.Namespace, values)
	if err != nil {
		return fmt.Errorf("rendering metadata chart: %w", err)
	}

	if err := r.applyRenderedManifest(ctx, instance, manifest); err != nil {
		return fmt.Errorf("applying metadata manifests: %w", err)
	}

	log.Info("Metadata service chart applied", "version", version)
	return nil
}

// ensureMetadataPostgresCredsSecret creates the credentials secret that the
// metadata Helm chart reads via existingSecret. When using internal PG, the
// secret is already created by ensurePostgresSecret with the correct name, so
// this is a no-op. When using external PG, it copies credentials from the
// user-provided secret into the operator-managed secret.
func (r *FireboltInstanceReconciler) ensureMetadataPostgresCredsSecret(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	if instance.Spec.Metadata.Postgres == nil {
		return nil
	}

	secretName := instance.Name + SuffixMetadataPostgresCreds

	existing := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: instance.Namespace}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	sourceSecretName := instance.Spec.Metadata.Postgres.CredentialsSecretRef.Name
	sourceSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: sourceSecretName, Namespace: instance.Namespace}, sourceSecret); err != nil {
		return fmt.Errorf("getting source credentials secret %s: %w", sourceSecretName, err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: instance.Namespace,
			Labels:    instanceLabels(instance.Name, "metadata"),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"username": sourceSecret.Data["username"],
			"password": sourceSecret.Data["password"],
		},
	}

	if err := controllerutil.SetControllerReference(instance, secret, r.Scheme); err != nil {
		return err
	}

	return r.Create(ctx, secret)
}

func (r *FireboltInstanceReconciler) buildMetadataValues(instance *computev1alpha1.FireboltInstance) (map[string]interface{}, error) {
	pgHost := pgResourceName(instance.Name) + "." + instance.Namespace + ".svc.cluster.local"
	pgPort := int64(PostgresPort)
	pgDatabase := PostgresDBName

	if instance.Spec.Metadata.Postgres != nil {
		pgHost = instance.Spec.Metadata.Postgres.Host
		pgPort = int64(instance.Spec.Metadata.Postgres.Port)
		if pgPort == 0 {
			pgPort = int64(PostgresPort)
		}
		pgDatabase = instance.Spec.Metadata.Postgres.Database
	}

	values := map[string]interface{}{
		"fullnameOverride": instance.Name + SuffixMetadataService,
		"postgresql": map[string]interface{}{
			"host":     pgHost,
			"port":     pgPort,
			"database": pgDatabase,
			"credentials": map[string]interface{}{
				"existingSecret": instance.Name + SuffixMetadataPostgresCreds,
			},
		},
		"service": map[string]interface{}{
			"type":       "ClusterIP",
			"port":       int64(MetadataServicePort),
			"targetPort": int64(MetadataServicePort),
		},
	}

	spec := &instance.Spec.Metadata.ComponentSpec
	if spec.Image != nil {
		values["image"] = map[string]interface{}{
			"repository": spec.Image.Repository,
			"tag":        spec.Image.Tag,
		}
	}
	if spec.Replicas != nil {
		dep, _ := values["deployment"].(map[string]interface{})
		if dep == nil {
			dep = make(map[string]interface{})
		}
		dep["replicas"] = int64(*spec.Replicas)
		values["deployment"] = dep
	}
	if spec.Resources != nil {
		values["resources"] = buildResourceValues(spec.Resources)
	}

	if spec.ValuesOverride != nil && spec.ValuesOverride.Raw != nil {
		if err := mergeJSONOverride(values, spec.ValuesOverride.Raw); err != nil {
			return nil, fmt.Errorf("metadata values override: %w", err)
		}
	}

	return values, nil
}
