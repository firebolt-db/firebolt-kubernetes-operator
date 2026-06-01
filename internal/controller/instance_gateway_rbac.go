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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	// SuffixGatewayWakeRole is appended to the instance name to form the
	// Role and RoleBinding names that grant the gateway permission to
	// stamp the wake-up annotation on FireboltEngines.
	SuffixGatewayWakeRole = "-gateway-wake"
)

// gatewayServiceAccountName returns the ServiceAccount name attached to
// gateway pods. The gateway uses this identity to patch the wake-up
// annotation on FireboltEngines when a request lands for a stopped engine.
func gatewayServiceAccountName(instanceName string) string {
	return instanceName + SuffixGateway
}

// userGatewayServiceAccountName returns the user-supplied
// spec.gateway.template.spec.serviceAccountName, or "" when the user
// did not set one. Treated as the explicit opt-out signal for
// operator-managed gateway RBAC: when non-empty, ensureGatewayRBAC is
// skipped and the user takes ownership of the ServiceAccount + RBAC.
// See docs/crd-reference/instance-crd-reference.mdx "Gateway custom ServiceAccount"
// for the verb set the user must bind.
func userGatewayServiceAccountName(instance *computev1alpha1.FireboltInstance) string {
	if t := instance.Spec.Gateway.Template; t != nil {
		return t.Spec.ServiceAccountName
	}
	return ""
}

// gatewayWakeRoleName returns the Role / RoleBinding name granting the
// gateway permission to update FireboltEngines.
func gatewayWakeRoleName(instanceName string) string {
	return instanceName + SuffixGatewayWakeRole
}

// ensureGatewayRBAC creates or updates the ServiceAccount, Role, and
// RoleBinding used by the gateway pods. The Role grants `get/list/patch`
// on FireboltEngines in the same namespace — get/list to look up the
// engine that needs waking, patch to stamp the wake annotation.
//
// RBAC cannot restrict patch to a specific subresource or field, so the
// gateway's identity carries patch on the whole CR. The wake protocol
// constrains the gateway to a strategic-merge patch that only touches
// metadata.annotations[AnnotationWakeRequested]; misuse beyond that is
// out of scope for the operator and would be reviewed via Kubernetes
// audit logs.
func (r *FireboltInstanceReconciler) ensureGatewayRBAC(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	if err := r.ensureGatewayServiceAccount(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway ServiceAccount: %w", err)
	}
	if err := r.ensureGatewayWakeRole(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway wake Role: %w", err)
	}
	if err := r.ensureGatewayWakeRoleBinding(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway wake RoleBinding: %w", err)
	}
	return nil
}

// The three RBAC ensure* functions below write through Server-Side
// Apply with FieldManager OperatorFieldManager and ForceOwnership for
// the same reasons documented above ensureGatewayConfigMap in
// instance_gateway.go. RoleBinding.roleRef is immutable in Kubernetes;
// an apply that would change it returns a validation error from the
// apiserver, which is the correct behavior — the only way to "change"
// a roleRef is to delete and re-create, and the operator does not own
// that lifecycle.
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
	return r.Patch(ctx, desired, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
}

func (r *FireboltInstanceReconciler) ensureGatewayWakeRole(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)
	name := gatewayWakeRoleName(instance.Name)
	rules := []rbacv1.PolicyRule{
		{
			APIGroups: []string{"compute.firebolt.io"},
			Resources: []string{"fireboltengines"},
			Verbs:     []string{"get", "list", "patch"},
		},
	}
	desired := &rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    instanceLabels(instance.Name, "gateway"),
		},
		Rules: rules,
	}
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}
	log.V(1).Info("Applying gateway wake Role", "name", name)
	return r.Patch(ctx, desired, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
}

func (r *FireboltInstanceReconciler) ensureGatewayWakeRoleBinding(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)
	name := gatewayWakeRoleName(instance.Name)
	saName := gatewayServiceAccountName(instance.Name)
	desired := &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    instanceLabels(instance.Name, "gateway"),
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      saName,
			Namespace: instance.Namespace,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     name,
		},
	}
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}
	log.V(1).Info("Applying gateway wake RoleBinding", "name", name)
	return r.Patch(ctx, desired, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
}
