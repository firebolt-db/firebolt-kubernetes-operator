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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
	helmutil "github.com/firebolt-analytics/firebolt-kubernetes-operator/internal/helm"
)

const defaultGatewayChartVersion = "0.1.0"

// ensureGateway loads the core-gateway Helm chart, renders it with values
// derived from the instance spec, and applies the resulting manifests.
func (r *FireboltInstanceReconciler) ensureGateway(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	version := instance.Spec.GatewayChartVersion
	if version == "" {
		version = defaultGatewayChartVersion
	}

	ch, err := r.ChartCache.GetOrLoad(r.GatewayChartSource, version)
	if err != nil {
		return fmt.Errorf("loading gateway chart: %w", err)
	}

	values, err := r.buildGatewayValues(instance)
	if err != nil {
		return fmt.Errorf("building gateway values: %w", err)
	}
	releaseName := instance.Name
	manifest, err := helmutil.RenderChart(ch, releaseName, instance.Namespace, values)
	if err != nil {
		return fmt.Errorf("rendering gateway chart: %w", err)
	}

	if err := r.applyRenderedManifest(ctx, instance, manifest); err != nil {
		return fmt.Errorf("applying gateway manifests: %w", err)
	}

	log.Info("Gateway chart applied", "version", version)
	return nil
}

func (r *FireboltInstanceReconciler) isGatewayReady(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	name := instance.Name + SuffixGateway
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &dep); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas > 0, nil
}

func (r *FireboltInstanceReconciler) buildGatewayValues(instance *computev1alpha1.FireboltInstance) (map[string]interface{}, error) {
	values := map[string]interface{}{
		"service": map[string]interface{}{
			"type": "ClusterIP",
			"port": int64(80),
		},
		"gatewayConfig": map[string]interface{}{
			"organization": map[string]interface{}{
				"account_id": instance.Status.AccountID,
				"namespace":  instance.Namespace,
			},
		},
	}

	spec := &instance.Spec.Gateway.ComponentSpec
	if spec.Image != nil {
		values["image"] = map[string]interface{}{
			"repository": spec.Image.Repository,
			"tag":        spec.Image.Tag,
		}
	}
	if spec.Replicas != nil {
		values["replicaCount"] = int64(*spec.Replicas)
	}
	if spec.Resources != nil {
		values["resources"] = buildResourceValues(spec.Resources)
	}
	if len(spec.NodeSelector) > 0 {
		values["nodeSelector"] = toStringInterfaceMap(spec.NodeSelector)
	}

	if spec.ValuesOverride != nil && spec.ValuesOverride.Raw != nil {
		if err := mergeJSONOverride(values, spec.ValuesOverride.Raw); err != nil {
			return nil, fmt.Errorf("gateway values override: %w", err)
		}
	}

	return values, nil
}

func toStringInterfaceMap(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
