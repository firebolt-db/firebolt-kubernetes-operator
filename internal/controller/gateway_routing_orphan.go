// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Keep the positive shutdown evidence after the Instance disappears, until its
// independently managed Engines have completed their own deletion.
const gatewayRoutingStateFinalizer = "firebolt.io/gateway-routing-state"

// allowUninitializedEngineDeletion permits deleting an Engine that never made
// generation resources or published a route. It does not authorize removing any
// running generation when its coordination record is missing.
func (r *FireboltEngineReconciler) allowUninitializedEngineDeletion(ctx context.Context, engine *computev1alpha1.FireboltEngine) (bool, error) {
	status := engine.Status
	initialStatus := status.Phase == "" && status.ActiveGeneration == 0 ||
		status.Phase == computev1alpha1.PhaseCreating && status.ActiveGeneration == -1
	if !initialStatus || status.CurrentGeneration != 0 || status.DrainingGeneration != nil {
		return false, nil
	}
	if _, _, err := readRoutingState(ctx, r.sweepReader(), engine.Namespace, engine.Spec.InstanceRef); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	for _, list := range []client.ObjectList{&appsv1.StatefulSetList{}, &corev1.PodList{}, &corev1.ServiceList{}, &corev1.ConfigMapList{}} {
		if err := r.sweepReader().List(ctx, list, client.InNamespace(engine.Namespace), client.MatchingLabels{LabelEngine: engine.Name}); err != nil {
			return false, err
		}
		// PodList is intentionally explicit: extractItems handles the resource
		// kinds in Instance deletion and does not include Pods.
		if pods, ok := list.(*corev1.PodList); ok {
			if len(pods.Items) != 0 {
				return false, nil
			}
		} else if len(extractItems(list)) != 0 {
			return false, nil
		}
	}
	return true, nil
}

func closedGatewayRoutingProof(state routing.State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if !state.Closed || len(state.Sessions) != 0 {
		return fmt.Errorf("%w: instance routing has not permanently stopped every registered gateway", errGatewayWithdrawalPending)
	}
	for _, retirement := range state.Retirements {
		if len(retirement.Holders) != 0 {
			return fmt.Errorf("%w: instance routing retains gateway retirement obligations", errGatewayWithdrawalPending)
		}
	}
	return nil
}

// cleanupOrphanGatewayRouting runs on Instance NotFound and before initializing
// a same-name replacement. The existing ConfigMap ownership and Engine deletion
// watches enqueue it; the ConfigMap's initial informer event recovers a crash
// after the last Engine finalizer was removed. Never remove this proof before an
// Engine's finalizer: a retry of that Engine deletion would otherwise lose it.
func (r *FireboltInstanceReconciler) cleanupOrphanGatewayRouting(ctx context.Context, key types.NamespacedName) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, state, err := readRoutingState(ctx, r.routingReader(), key.Namespace, key.Name)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		instance := &computev1alpha1.FireboltInstance{}
		if err := r.routingReader().Get(ctx, key, instance); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
		} else if string(instance.UID) == state.InstanceUID {
			return nil
		}
		owner := metav1.GetControllerOf(cm)
		if owner == nil || owner.APIVersion != computev1alpha1.GroupVersion.String() ||
			owner.Kind != "FireboltInstance" || owner.Name != key.Name || string(owner.UID) != state.InstanceUID {
			return errors.New("orphan routing state has no matching Instance owner")
		}
		if err := closedGatewayRoutingProof(state); err != nil {
			return err
		}
		engines := &computev1alpha1.FireboltEngineList{}
		if err := r.routingReader().List(ctx, engines, client.InNamespace(key.Namespace)); err != nil {
			return err
		}
		for i := range engines.Items {
			if engines.Items[i].Spec.InstanceRef == key.Name {
				return nil
			}
		}
		if cm.DeletionTimestamp.IsZero() {
			// Mark for deletion first. A crash before finalizer removal leaves a
			// discoverable object, rather than a finalizer-free proof awaiting GC.
			if err := r.Delete(ctx, cm, client.Preconditions{UID: &cm.UID, ResourceVersion: &cm.ResourceVersion}); err != nil {
				return client.IgnoreNotFound(err)
			}
			// Deletion changes the resource version; re-read and re-check on retry.
			return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, errors.New("routing proof marked for deletion"))
		}
		if !controllerutil.RemoveFinalizer(cm, gatewayRoutingStateFinalizer) {
			return nil
		}
		return r.Update(ctx, cm)
	})
}
