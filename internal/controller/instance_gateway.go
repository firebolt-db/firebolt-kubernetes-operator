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
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	gatewayContainerPort = 5050
	gatewayServicePort   = 80
	gatewayContainerName = "core-gateway"
	gatewayConfigKey     = "core-gateway.yaml"
)

// ensureGatewayResources creates or updates the ConfigMap, Deployment, Service,
// ServiceAccount, Role, RoleBinding, and PDB for the gateway.
func (r *FireboltInstanceReconciler) ensureGatewayResources(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)

	gatewayYAML := buildGatewayConfigYAML(instance)

	if err := r.ensureGatewayConfigMap(ctx, instance, gatewayYAML); err != nil {
		return fmt.Errorf("ensuring gateway configmap: %w", err)
	}

	if err := r.ensureGatewayServiceAccount(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway service account: %w", err)
	}

	if err := r.ensureGatewayRole(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway role: %w", err)
	}

	if err := r.ensureGatewayRoleBinding(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway role binding: %w", err)
	}

	if err := r.ensureGatewayDeployment(ctx, instance, gatewayYAML); err != nil {
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

func buildGatewayConfigYAML(instance *computev1alpha1.FireboltInstance) string {
	authEnabled := instance.Spec.Auth != nil && instance.Spec.Auth.Mode == computev1alpha1.AuthModeOpenID

	// deployment_suffix must match the engine StatefulSet naming pattern used
	// by genResourceName: "{engine}-g{N}". The gateway discovers per-node
	// resources as "{engine}{suffix}{N}" (no extra separator). Features that
	// depend on this (autoscaler, rendezvous_routing, autochaos) are currently
	// incompatible with the operator's StatefulSet-based engine model and must
	// remain disabled.
	return fmt.Sprintf(`organization:
  account_id: %q
  namespace: %q
  internal_service_suffix: "-service"
  internal_service_port: 3473
  deployment_suffix: "-g"
http_server:
  address: ":%d"
  shutdown_delay: 5s
  shutdown_timeout: 30s
auth:
  enabled: %t
telemetry:
  log_level: "info"
  log_all_requests: false
transport_config:
  max_idle_conns_per_host: 100
  idle_conn_timeout: 60s
  flush_interval: -1
filter_headers: true
`,
		instance.Status.AccountID,
		instance.Namespace,
		gatewayContainerPort,
		authEnabled,
	)
}

func (r *FireboltInstanceReconciler) ensureGatewayConfigMap(ctx context.Context, instance *computev1alpha1.FireboltInstance, gatewayYAML string) error {
	name := instance.Name + SuffixGateway + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Data: map[string]string{
			gatewayConfigKey: gatewayYAML,
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

func (r *FireboltInstanceReconciler) ensureGatewayDeployment(ctx context.Context, instance *computev1alpha1.FireboltInstance, gatewayYAML string) error {
	name := instance.Name + SuffixGateway
	configMapName := name + "-config"
	labels := instanceLabels(instance.Name, "gateway")

	spec := &instance.Spec.Gateway.ComponentSpec

	saName := instance.Name + SuffixGateway + "-sa"
	if spec.ServiceAccountName != "" {
		saName = spec.ServiceAccountName
	}

	var replicas int32 = 2
	if spec.Replicas != nil {
		replicas = *spec.Replicas
	}

	image := "core-gateway:latest"
	pullPolicy := corev1.PullIfNotPresent
	if spec.Image != nil {
		image = spec.Image.Repository + ":" + spec.Image.Tag
		pullPolicy = spec.Image.PullPolicy
		if pullPolicy == "" {
			pullPolicy = corev1.PullIfNotPresent
		}
	}

	configHash := contentHash(gatewayYAML)

	maxSurge := intstr.FromString("25%")
	maxUnavailable := intstr.FromInt32(0)
	var runAsUser int64 = 65534

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
					ServiceAccountName: saName,
					Containers: []corev1.Container{{
						Name:            gatewayContainerName,
						Image:           image,
						ImagePullPolicy: pullPolicy,
						Args:            []string{"serve", "--config=/etc/core-gateway.yaml"},
						Env: []corev1.EnvVar{
							{
								Name: "FIREBOLT_ORGANIZATION_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
						},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: gatewayContainerPort, Protocol: corev1.ProtocolTCP},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health/live",
									Port: intstr.FromString("http"),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       15,
							TimeoutSeconds:      5,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health/ready",
									Port: intstr.FromString("http"),
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       3,
							TimeoutSeconds:      5,
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
								MountPath: "/etc/core-gateway.yaml",
								SubPath:   gatewayConfigKey,
								ReadOnly:  true,
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

// TODO: ServiceAccount, Role, and RoleBinding will be removed in the future.
// The features they support (autoscaler, autochaos, query_buffering, etc.)
// will be moved to the operator.
func (r *FireboltInstanceReconciler) ensureGatewayServiceAccount(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway + "-sa"
	labels := instanceLabels(instance.Name, "gateway")

	desired := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

// TODO: Role will be removed in the future; the related features will be
// moved to the operator.
func (r *FireboltInstanceReconciler) ensureGatewayRole(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway + "-role"
	labels := instanceLabels(instance.Name, "gateway")

	desired := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"apps"},
				Resources: []string{"deployments"},
				Verbs:     []string{"get", "list", "update"},
			},
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &rbacv1.Role{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Rules = desired.Rules
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

// TODO: RoleBinding will be removed in the future; the related features will
// be moved to the operator.
func (r *FireboltInstanceReconciler) ensureGatewayRoleBinding(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	name := instance.Name + SuffixGateway + "-binding"
	roleName := instance.Name + SuffixGateway + "-role"
	saName := instance.Name + SuffixGateway + "-sa"
	labels := instanceLabels(instance.Name, "gateway")

	desired := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    labels,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     roleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: instance.Namespace,
			},
		},
	}

	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}

	existing := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// roleRef is immutable; only update subjects and labels
	existing.Subjects = desired.Subjects
	existing.Labels = desired.Labels
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
