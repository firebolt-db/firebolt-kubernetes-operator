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
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	gatewayContainerPort int32 = 8080
	gatewayAdminPort     int32 = 9901
	gatewayServicePort   int32 = 80
	gatewayContainerName       = "envoy"
	gatewayConfigKey           = "envoy.yaml"
)

// ensureGatewayResources creates or updates the ConfigMap, Deployment, Service,
// and PDB for the Envoy gateway proxy.
func (r *FireboltInstanceReconciler) ensureGatewayResources(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	envoyYAML := buildEnvoyConfigYAML(instance)

	if err := r.ensureGatewayConfigMap(ctx, instance, envoyYAML); err != nil {
		return fmt.Errorf("ensuring gateway configmap: %w", err)
	}

	if err := r.ensureGatewayDeployment(ctx, instance, envoyYAML); err != nil {
		return fmt.Errorf("ensuring gateway deployment: %w", err)
	}

	if err := r.ensureGatewayService(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway service: %w", err)
	}

	if err := r.ensureGatewayPDB(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway pdb: %w", err)
	}

	log.Info("Gateway resources ensured")
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

// buildEnvoyConfigYAML generates an Envoy static config that routes requests
// based on the X-Firebolt-Engine header to the corresponding engine ClusterIP
// Service ({engine}-service:3473) via the dynamic forward proxy.
func buildEnvoyConfigYAML(instance *computev1alpha1.FireboltInstance) string {
	return fmt.Sprintf(`static_resources:
  listeners:
    - name: listener
      address:
        socket_address:
          address: 0.0.0.0
          port_value: %d
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: gateway
                access_log:
                  - name: envoy.access_loggers.stdout
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog
                http_filters:
                  - name: envoy.filters.http.health_check
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.health_check.v3.HealthCheck
                      pass_through_mode: false
                      headers:
                        - name: ":path"
                          string_match:
                            exact: "/healthz"
                  - name: envoy.filters.http.lua
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.lua.v3.Lua
                      default_source_code:
                        inline_string: |
                          function envoy_on_request(handle)
                            local engine = handle:headers():get("x-firebolt-engine")
                            if engine == nil or engine == "" then
                              handle:respond({[":status"] = "400"}, "missing X-Firebolt-Engine header")
                              return
                            end
                            -- TODO: remove advanced_mode once Core supports x-request-id
                            local path = handle:headers():get(":path")
                            if path:find("?", 1, true) then
                              handle:headers():replace(":path", path .. "&advanced_mode=true")
                            else
                              handle:headers():replace(":path", path .. "?advanced_mode=true")
                            end
                            handle:headers():replace(":authority", engine .. "-service.%s.svc.cluster.local:3473")
                          end
                  - name: envoy.filters.http.dynamic_forward_proxy
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_forward_proxy.v3.FilterConfig
                      dns_cache_config:
                        name: dynamic_forward_proxy_cache
                        dns_lookup_family: V4_ONLY
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: local_route
                  virtual_hosts:
                    - name: default
                      domains: ["*"]
                      routes:
                        - match:
                            prefix: "/"
                          route:
                            cluster: dynamic_forward_proxy
                            timeout: 0s
  clusters:
    - name: dynamic_forward_proxy
      lb_policy: CLUSTER_PROVIDED
      cluster_type:
        name: envoy.clusters.dynamic_forward_proxy
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.clusters.dynamic_forward_proxy.v3.ClusterConfig
          dns_cache_config:
            name: dynamic_forward_proxy_cache
            dns_lookup_family: V4_ONLY
admin:
  address:
    socket_address:
      address: 127.0.0.1
      port_value: %d
`,
		gatewayContainerPort,
		instance.Namespace,
		gatewayAdminPort,
	)
}

func (r *FireboltInstanceReconciler) ensureGatewayConfigMap(ctx context.Context, instance *computev1alpha1.FireboltInstance, envoyYAML string) error {
	name := instance.Name + SuffixGateway + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			gatewayConfigKey: envoyYAML,
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

func (r *FireboltInstanceReconciler) ensureGatewayDeployment(ctx context.Context, instance *computev1alpha1.FireboltInstance, envoyYAML string) error {
	name := instance.Name + SuffixGateway
	configMapName := name + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	spec := &instance.Spec.Gateway.ComponentSpec

	var replicas int32 = 2
	if spec.Replicas != nil {
		replicas = *spec.Replicas
	}

	image := "envoyproxy/envoy:v1.33-latest"
	pullPolicy := corev1.PullIfNotPresent
	if spec.Image != nil {
		image = spec.Image.Repository + ":" + spec.Image.Tag
		pullPolicy = spec.Image.PullPolicy
		if pullPolicy == "" {
			pullPolicy = corev1.PullIfNotPresent
		}
	}

	configHash := contentHash(envoyYAML)

	maxSurge := intstr.FromString("25%")
	maxUnavailable := intstr.FromInt32(0)
	var runAsUser int64 = 101 // Envoy default UID

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
					Containers: []corev1.Container{{
						Name:            gatewayContainerName,
						Image:           image,
						ImagePullPolicy: pullPolicy,
						Args:            []string{"envoy", "-c", "/etc/envoy/envoy.yaml"},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: gatewayContainerPort, Protocol: corev1.ProtocolTCP},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("http"),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       15,
							TimeoutSeconds:      3,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromString("http"),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       3,
							TimeoutSeconds:      3,
						},
						SecurityContext: &corev1.SecurityContext{
							RunAsUser:                &runAsUser,
							RunAsNonRoot:             boolPtr(true),
							ReadOnlyRootFilesystem:   boolPtr(true),
							AllowPrivilegeEscalation: boolPtr(false),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "config-volume",
								MountPath: "/etc/envoy",
								ReadOnly:  true,
							},
							{
								Name:      "tmp",
								MountPath: "/tmp",
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
									Items: []corev1.KeyToPath{
										{Key: gatewayConfigKey, Path: gatewayConfigKey},
									},
								},
							},
						},
						{
							Name:         "tmp",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
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

func (r *FireboltInstanceReconciler) ensureGatewayService(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway
	labels := instanceLabels(instance.Name, "gateway")

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
				{Name: "http", Port: gatewayServicePort, TargetPort: intstr.FromInt32(gatewayContainerPort), Protocol: corev1.ProtocolTCP},
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

func (r *FireboltInstanceReconciler) ensureGatewayPDB(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway
	labels := instanceLabels(instance.Name, "gateway")
	maxUnavailable := intstr.FromInt32(1)

	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec.MaxUnavailable = desired.Spec.MaxUnavailable
	existing.Spec.Selector = desired.Spec.Selector
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func boolPtr(v bool) *bool { return &v }
