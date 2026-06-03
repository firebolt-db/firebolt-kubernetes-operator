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
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	// SuffixGatewayWakeRole is appended to the instance name to form the
	// per-instance RoleBinding name that binds the chart-managed
	// gateway-wake ClusterRole to the gateway ServiceAccount. The name
	// is also the legacy Role name; the migration path in
	// ensureGatewayWakeRoleBinding tears that Role down.
	SuffixGatewayWakeRole = "-gateway-wake"
)

// errGatewayWakeClusterRoleUnset is returned when the operator is
// configured to manage gateway RBAC (no user-supplied gateway SA) but
// the `--gateway-wake-cluster-role` flag is empty. Surfaced as a
// GatewayReady=False/RBACMisconfigured condition so the operator
// admin sees the missing flag instead of a silent broken gateway.
var errGatewayWakeClusterRoleUnset = errors.New(
	"--gateway-wake-cluster-role is empty; set it to the chart-managed ClusterRole name",
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

// gatewayWakeRoleName returns the per-instance RoleBinding name (and
// the legacy Role name, used by the migration path). The basename is
// intentionally the same so a single resource label query
// (LabelInstance=<instance>) covers both old and new shapes during the
// rolling upgrade window.
func gatewayWakeRoleName(instanceName string) string {
	return instanceName + SuffixGatewayWakeRole
}

// ensureGatewayRBAC creates or updates the ServiceAccount and the
// per-instance RoleBinding used by the gateway pods. The RoleBinding
// targets a chart-managed ClusterRole (named via
// `--gateway-wake-cluster-role`) that grants `get/list/patch` on
// FireboltEngines.
//
// The Role+RoleBinding pair the previous operator versions created has
// been replaced by ClusterRole+RoleBinding so the operator no longer
// needs `roles: create/update/patch` cluster-wide. The retained
// `roles: get;list;watch;delete` verbs let the reconciler clean up the
// legacy per-instance Role during upgrade — see
// ensureGatewayWakeRoleBinding for the migration handling.
func (r *FireboltInstanceReconciler) ensureGatewayRBAC(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	if r.GatewayWakeClusterRole == "" {
		return errGatewayWakeClusterRoleUnset
	}
	if err := r.ensureGatewayServiceAccount(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway ServiceAccount: %w", err)
	}
	if err := r.ensureGatewayWakeRoleBinding(ctx, instance); err != nil {
		return fmt.Errorf("ensuring gateway wake RoleBinding: %w", err)
	}
	return nil
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
	return r.Patch(ctx, desired, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
}

// ensureGatewayWakeRoleBinding creates or updates a RoleBinding in the
// instance namespace that binds the chart-managed gateway-wake
// ClusterRole to the gateway ServiceAccount.
//
// Migration: earlier operator versions created a per-instance Role and a
// RoleBinding whose roleRef pointed at that Role. RoleBinding.roleRef
// is immutable in Kubernetes, so a server-side apply with a different
// roleRef returns a validation error. When the existing RoleBinding's
// roleRef doesn't match the desired ClusterRole reference, delete the
// stale binding (and the now-orphaned legacy Role) and re-create via
// the standard apply path. Subsequent reconciles see a matching
// roleRef and the migration step is a no-op.
func (r *FireboltInstanceReconciler) ensureGatewayWakeRoleBinding(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)
	name := gatewayWakeRoleName(instance.Name)
	saName := gatewayServiceAccountName(instance.Name)
	key := types.NamespacedName{Name: name, Namespace: instance.Namespace}

	desiredRoleRef := rbacv1.RoleRef{
		APIGroup: rbacv1.GroupName,
		Kind:     "ClusterRole",
		Name:     r.GatewayWakeClusterRole,
	}

	existing := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, key, existing); err == nil {
		if existing.RoleRef != desiredRoleRef {
			log.Info("Deleting legacy gateway wake RoleBinding to switch roleRef",
				"name", name, "oldRoleRef", existing.RoleRef, "newRoleRef", desiredRoleRef)
			if err := r.Delete(ctx, existing); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting legacy RoleBinding: %w", err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading existing RoleBinding: %w", err)
	}

	if err := r.deleteLegacyGatewayWakeRole(ctx, instance.Namespace, name); err != nil {
		return err
	}

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
		RoleRef: desiredRoleRef,
	}
	if err := controllerutil.SetControllerReference(instance, desired, r.Scheme); err != nil {
		return err
	}
	log.V(1).Info("Applying gateway wake RoleBinding", "name", name)
	return r.Patch(ctx, desired, client.Apply, client.FieldOwner(OperatorFieldManager), client.ForceOwnership)
}

// deleteLegacyGatewayWakeRole tears down the per-instance Role created
// by earlier operator versions. The Role's permissions are now carried by
// the chart-managed ClusterRole, so the per-instance copy is dead
// weight after the RoleBinding switches roleRef. Idempotent: missing
// Role is a no-op.
func (r *FireboltInstanceReconciler) deleteLegacyGatewayWakeRole(ctx context.Context, namespace, name string) error {
	legacy := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if err := r.Delete(ctx, legacy); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting legacy gateway wake Role: %w", err)
	}
	return nil
}
