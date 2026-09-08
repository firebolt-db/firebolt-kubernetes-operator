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

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
)

const (
	// SuffixGatewayWakeRole is appended to the instance name to form the
	// per-instance RoleBinding name that binds the chart-managed
	// gateway-wake ClusterRole to the gateway ServiceAccount.
	SuffixGatewayWakeRole = "-gateway-wake"
)

// gatewayServiceAccountName returns the ServiceAccount name attached to
// gateway pods. The wake-agent sidecar uses this identity to watch
// EndpointSlices and its instance routing ConfigMap; Envoy
// itself never holds a token (see automountForGateway).
func gatewayServiceAccountName(instanceName string) string {
	return instanceName + SuffixGateway
}

// userGatewayServiceAccountName returns the user-supplied
// spec.gateway.template.spec.serviceAccountName, or "" when the user
// did not set one. A custom identity still receives the read-only routing
// RoleBinding, while its ServiceAccount object remains user-managed.
// See docs/crd-reference/instance-crd-reference.mdx "Gateway custom ServiceAccount"
// for the verb set the user must bind.
func userGatewayServiceAccountName(instance *computev1alpha1.FireboltInstance) string {
	if t := instance.Spec.Gateway.Template; t != nil {
		return t.Spec.ServiceAccountName
	}
	return ""
}

// ensureGatewayRBAC gives the gateway agent read-only access to its routing
// table and namespace EndpointSlices. Envoy has no Kubernetes credentials.
func (r *FireboltInstanceReconciler) ensureGatewayRBAC(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	saName := userGatewayServiceAccountName(instance)
	if saName == "" {
		saName = gatewayServiceAccountName(instance.Name)
		if err := r.ensureGatewayServiceAccount(ctx, instance); err != nil {
			return err
		}
	}
	name := routing.ConfigMapName(instance.Name)
	role := &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace, Labels: instanceLabels(instance.Name, "gateway")}, Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{name}, Verbs: []string{"get", "list", "watch"}},
		{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
	}}
	if err := controllerutil.SetControllerReference(instance, role, r.Scheme); err != nil {
		return err
	}
	if err := applySSA(ctx, r.Client, role); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instance.Namespace, Labels: instanceLabels(instance.Name, "gateway")}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: instance.Namespace}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}}
	if err := controllerutil.SetControllerReference(instance, binding, r.Scheme); err != nil {
		return err
	}
	return applySSA(ctx, r.Client, binding)
}

// ensureGatewayServiceAccount writes through Server-Side Apply with
// FieldManager OperatorFieldManager and ForceOwnership for the same
// reasons documented above ensureGatewayConfigMap in
// instance_gateway.go.
func (r *FireboltInstanceReconciler) ensureGatewayServiceAccount(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)
	name := gatewayServiceAccountName(instance.Name)
	desired := &corev1.ServiceAccount{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    instanceLabels(instance.Name, "gateway"),
		},
	}
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}
	log.V(1).Info("Applying gateway ServiceAccount", "name", name)
	return applySSA(ctx, r.Client, desired)
}
