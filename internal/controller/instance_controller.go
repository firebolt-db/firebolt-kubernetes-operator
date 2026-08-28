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
	stderrors "errors"
	"fmt"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/metrics"
)

const (
	instanceFinalizerName = "compute.firebolt.io/instance-cleanup"

	externalPostgresCredentialsSecretIndexField = "spec.metadata.postgres.credentialsSecretRef.name" //nolint:gosec // field-index path, not a credential
)

// reasonTemplateRejected is the per-component Ready=False reason
// surfaced when validatePodTemplates rejects spec.gateway.template or
// spec.metadata.template against its operator-owned-field ruleset. The
// validating webhook normally rejects these at admission; this branch
// fires when admission is bypassed (chart-default install) and is
// strict enough to refuse rendering rather than silently dropping the
// forbidden field. Reusing the existing component conditions keeps the
// Ready roll-up consumer unchanged.
const reasonTemplateRejected = "TemplateRejected"

// errPostgresSecretRefEmpty is returned at runtime when the webhook is
// bypassed and an instance still has an empty credentialsSecretRef.Name.
// Normally the validating webhook rejects this at admission time.
var errPostgresSecretRefEmpty = stderrors.New(
	"spec.metadata.postgres.credentialsSecretRef.name is empty",
)

// FireboltInstanceReconciler reconciles FireboltInstance objects by deploying
// PostgreSQL, the metadata service, and the gateway.
type FireboltInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MetricsRecorder records Prometheus metrics for instance CRs.
	// Must be non-nil; use metrics.NoOpInstanceRecorder{} in tests.
	MetricsRecorder metrics.InstanceRecorder

	// EventRecorder emits Kubernetes Events on the FireboltInstance CR.
	// Populated in SetupWithManager when nil; unit tests that exercise
	// event-emitting paths inject an events.FakeRecorder. Nil is tolerated: the
	// emit helpers no-op, so tests that do not care about events leave it unset.
	// Mirrors FireboltEngineReconciler.EventRecorder; the operator's events RBAC
	// grant (see engine_controller.go) covers both controllers.
	EventRecorder events.EventRecorder

	// NameFilter, when non-empty, restricts this reconciler to a single
	// FireboltInstance by name. Requests for any other instance are dropped.
	// Intended for E2E tests that run multiple isolated operator instances
	// in the same namespace; in production this is left empty so the
	// reconciler processes every FireboltInstance it watches.
	NameFilter string

	// GatewayWakeClusterRole is the name of the chart-managed ClusterRole
	// bound to each gateway ServiceAccount via a per-instance RoleBinding.
	// It grants get/list/watch on EndpointSlices — the wake agent's entire
	// Kubernetes grant, and read-only. The operator never creates the
	// ClusterRole itself, so the cluster-wide `roles` verbs stay out of the
	// operator's own RBAC.
	//
	// Empty is not fatal: the binding is skipped and wake stops working
	// while query routing continues. The gateway is not degraded by the
	// absence of a capability it only needs in order to wake stopped
	// engines.
	GatewayWakeClusterRole string

	// WakeAgentImage is the image the gateway's wake-agent sidecar runs.
	// The chart sets this to the operator's own image: the agent is a
	// subcommand of the manager binary, so the two cannot drift out of
	// sync on the demand-endpoint contract they share. Empty omits the
	// sidecar entirely and disables wake (see wakeAgentConfig).
	WakeAgentImage string

	// WakeAgentImagePullPolicy is the pull policy for the sidecar. Empty
	// lets Kubernetes apply its own default from the image tag.
	WakeAgentImagePullPolicy corev1.PullPolicy
}

// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups=compute.firebolt.io,resources=fireboltengines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// The operator does not read EndpointSlices itself. It holds this grant so it
// can bind the gateway-wake ClusterRole to each gateway ServiceAccount:
// Kubernetes rejects a RoleBinding whose referenced role carries permissions
// the creator lacks, so without it ensureGatewayWakeRoleBinding 403s on every
// reconcile and the gateway is never created.
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures the PostgreSQL, metadata service, and gateway components
// described by a FireboltInstance are running and healthy.
func (r *FireboltInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	if r.NameFilter != "" && req.Name != r.NameFilter {
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx).WithValues("instance", req.Name)

	instance := &computev1alpha1.FireboltInstance{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !controllerutil.ContainsFinalizer(instance, instanceFinalizerName) {
		controllerutil.AddFinalizer(instance, instanceFinalizerName)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Teardown runs before any spec.id work: reconcileDelete sweeps owned
	// objects by the LabelInstance label, never by spec.id, so minting or
	// canonicalizing the id on a terminating instance buys nothing. Worse,
	// ensureInstanceID's error is fatal to the pass, so an admission
	// rejection of the case-only Update would keep reconcileDelete — and
	// therefore finalizer removal — from ever running, wedging the instance
	// in Terminating.
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.reconcileDelete(ctx, instance)
	}

	if requeue, err := r.ensureInstanceID(ctx, instance); err != nil {
		return ctrl.Result{}, err
	} else if requeue {
		return ctrl.Result{Requeue: true}, nil
	}

	// Record metrics on every return path beyond this point so that error
	// branches (failWithCondition / status-update failures) still publish the
	// current in-memory state. Without this, an instance that successfully
	// reconciled once and then keeps hitting a transient failure would leave
	// the firebolt_instance_* gauges empty in Prometheus, even though the CR
	// still has its stable status conditions set. The deferred call reads
	// `instance` at function-exit time so post-Update status changes are
	// captured. The Engine reconciler uses the same deferred pattern.
	defer func() {
		r.recordReconcileMetrics(instance, err)
	}()

	if instance.Status.Phase == "" {
		instance.Status.Phase = computev1alpha1.InstancePhaseProvisioning
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// InstancePhaseFailed is terminal. The long RequeueAfter is a safety
	// net: owned-object events will also re-enqueue, so this poll only
	// matters if the human edits the status (e.g. kubectl patch) without
	// touching any watched resource.
	if instance.Status.Phase == computev1alpha1.InstancePhaseFailed {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Defense-in-depth for the FireboltInstance validating webhook: walk
	// spec.gateway.template and spec.metadata.template against the
	// operator-owned-field rulesets every reconcile. Admission catches
	// these at apply time when the webhook is enabled; when it isn't
	// (chart default), the reconciler's pod-template merge would silently
	// drop forbidden user input — most dangerously, a user-set
	// containers[envoy].lifecycle.preStop that overrides Envoy's drain
	// hook and breaks the zero-downtime contract. Refuse to render the
	// offending component until the user fixes the template; the
	// metadata branch runs first to match the rendering order.
	gwTplErrs, mdTplErrs := validateInstanceTemplates(instance)
	if len(mdTplErrs) > 0 {
		return ctrl.Result{}, r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionMetadataReady, reasonTemplateRejected,
			mdTplErrs.ToAggregate())
	}
	if len(gwTplErrs) > 0 {
		return ctrl.Result{}, r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionGatewayReady, reasonTemplateRejected,
			gwTplErrs.ToAggregate())
	}

	// Step 0: Ensure Instance-wide auth (admin credentials preflight +
	// JWT signing keypair). See ensureAuth's doc comment for why a
	// failure here is logged rather than blocking Steps 1-4: neither
	// PostgreSQL, metadata, nor the gateway read spec.auth, and engines
	// gate their own reconcile on Status.Auth independently.
	if err := r.ensureAuth(ctx, instance); err != nil {
		log.Error(err, "Failed to ensure auth")
	}

	// Step 0.5: Ensure engine-listener TLS. Same failure-isolation
	// reasoning as auth, with one difference: unlike auth, the gateway's
	// rendered config DOES read Status.EngineTLS (to re-encrypt
	// gateway->engine traffic once engine TLS is enabled — see
	// buildEnvoyConfigYAML), so this must run before Step 4 below.
	if err := r.ensureEngineTLS(ctx, instance); err != nil {
		log.Error(err, "Failed to ensure engine TLS")
	}

	// Step 0.6: Ensure the gateway's downstream (client-facing) TLS
	// certificate. Same failure-isolation and before-Step-4 ordering
	// reasoning as Step 0.5: buildEnvoyConfigYAML and
	// effectiveGatewayPodTemplate both read Status.GatewayTLS.
	if err := r.ensureGatewayTLS(ctx, instance); err != nil {
		log.Error(err, "Failed to ensure gateway TLS")
	}

	// Step 0.7: Assemble the gateway's engine trust bundle — the union of every
	// live engine generation's CA plus the instance anchor — so the gateway
	// keeps trusting engines across a CA rotation behind the (name-immutable)
	// issuer. Reads Status.EngineTLS and the engine fleet; must run
	// before Step 4 because the gateway pod mounts this bundle and folds its
	// ResourceVersion into the config hash. Same failure-isolation as the TLS
	// steps: a failure leaves the previous bundle in place rather than blocking.
	// The returned fingerprints are published to Status.RolledEngineTrustCAs
	// after Step 4, but only once the gateway has actually rolled the bundle out —
	// and only when assembly succeeded (see the bundleErr guard there).
	engineTrustCAFingerprints, bundleErr := r.ensureEngineCABundle(ctx, instance)
	if bundleErr != nil {
		log.Error(bundleErr, "Failed to ensure engine CA bundle")
	}

	// Step 1: Ensure PostgreSQL and metadata in the same reconcile pass.
	// Postgres and metadata are not separate phases: the metadata service
	// retries its DB connection internally for up to ~60s on startup, which
	// comfortably covers the time the postgres StatefulSet needs to become
	// ready on a fresh provisioning. Applying both resources concurrently
	// and letting the metadata-readiness check at Step 2 gate the whole
	// stack is enough. This mirrors firebolt-instance-helm, which has no
	// Helm hook ordering postgres ahead of metadata. There is no separate
	// PostgresReady condition for the same reason — a metadata pod that
	// cannot reach Postgres surfaces in the MetadataReady condition's
	// Reason/Message.
	if instance.Spec.Metadata.Postgres == nil {
		if err := r.ensurePostgreSQL(ctx, instance); err != nil {
			return ctrl.Result{}, r.failWithCondition(ctx, instance,
				computev1alpha1.InstanceConditionMetadataReady, "PostgresEnsureFailed", err)
		}
	} else {
		// External Postgres: make sure the user-referenced credentials
		// Secret actually exists before we roll a Deployment that mounts
		// it. Without this pre-flight the metadata pod gets scheduled,
		// kubelet fails to mount a missing Secret, and the pod sits in
		// ContainerCreating with the root cause visible only in the pod
		// events — invisible from the FireboltInstance CR.
		if err := r.checkExternalPostgresSecret(ctx, instance); err != nil {
			instance.Status.MetadataReady = false
			instance.Status.MetadataEndpoint = ""
			return ctrl.Result{}, r.failWithCondition(ctx, instance,
				computev1alpha1.InstanceConditionMetadataReady, "PostgresSecretPreflightFailed", err)
		}
	}

	// Step 2: Ensure metadata service (native Go resources)
	if err := r.ensureMetadataResources(ctx, instance); err != nil {
		return ctrl.Result{}, r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionMetadataReady, "EnsureFailed", err)
	}

	// Step 3: Check metadata readiness
	ready, err := r.isMetadataServiceReady(ctx, instance)
	if err != nil {
		return ctrl.Result{}, r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionMetadataReady, "ProbeFailed", err)
	}
	if !ready {
		log.Info("Metadata service not ready yet, requeueing")
		instance.Status.MetadataReady = false
		instance.Status.MetadataEndpoint = ""
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionMetadataReady, metav1.ConditionFalse,
			"Provisioning", "metadata Deployment has no ready replicas yet")
		return r.writeStatusAndPoll(ctx, instance, 5*time.Second)
	}

	instance.Status.MetadataReady = true
	instance.Status.MetadataEndpoint = metadataServiceEndpoint(instance.Name, instance.Namespace)
	setInstanceCondition(instance,
		computev1alpha1.InstanceConditionMetadataReady, metav1.ConditionTrue,
		"Ready", "metadata Deployment has at least one ready replica")

	// Step 4: Ensure gateway (native Go resources)
	if err := r.ensureGatewayResources(ctx, instance); err != nil {
		return ctrl.Result{}, r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionGatewayReady, "EnsureFailed", err)
	}

	// CRL Secrets are optional; once referenced (and mounted) they must
	// carry crl.pem. ensureGateway already ran with the unusable Secret
	// omitted from the roll hash so a bad edit cannot wedge new pods.
	// Surface GatewayReady=False without clearing TLS status / draining
	// the listener — a broken CRL must not take the Gateway offline.
	if err := r.checkGatewayCRLSecrets(ctx, instance); err != nil {
		instance.Status.GatewayReady = false
		instance.Status.GatewayEndpoint = ""
		return ctrl.Result{}, r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionGatewayReady, "CRLSecretPreflightFailed", err)
	}

	gwReady, err := r.isGatewayReady(ctx, instance)
	if err != nil {
		return ctrl.Result{}, r.failWithCondition(ctx, instance,
			computev1alpha1.InstanceConditionGatewayReady, "ProbeFailed", err)
	}
	instance.Status.GatewayReady = gwReady
	if gwReady {
		instance.Status.GatewayEndpoint = fmt.Sprintf("%s%s.%s.svc.cluster.local",
			instance.Name, SuffixGateway, instance.Namespace)
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionGatewayReady, metav1.ConditionTrue,
			"Ready", "gateway Deployment has at least one ready replica")
	} else {
		instance.Status.GatewayEndpoint = ""
		setInstanceCondition(instance,
			computev1alpha1.InstanceConditionGatewayReady, metav1.ConditionFalse,
			"Provisioning", "gateway Deployment has no ready replicas yet")
	}

	// PublishRolledEngineTrustCAs preserves the prior confirmed set
	// when assembly failed (bundleErr != nil), rather than clobbering it with nil.
	r.publishRolledEngineTrustCAs(ctx, instance, engineTrustCAFingerprints, bundleErr)

	setInstanceReadyRollup(instance)
	instance.Status.Phase = r.computePhase(instance)

	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *FireboltInstanceReconciler) recordReconcileMetrics(
	instance *computev1alpha1.FireboltInstance,
	reconcileErr error,
) {
	r.MetricsRecorder.Record(instance)
	if reconcileErr == nil {
		r.MetricsRecorder.RecordSuccessfulReconcile(instance.Namespace, instance.Name)
	}
}

// publishRolledEngineTrustCAs records, in Status.RolledEngineTrustCAs, the
// engine CA fingerprints the gateway has CONFIRMED rolled out — so the engine's
// blue-green cutover gate can verify a new generation's CA is trusted before
// flipping its Service selector. The set is updated only once the
// gateway is actually serving the config that embeds the current bundle
// (gatewayServingCurrentConfig): mid-roll the old pods still serve the previous
// (subset) bundle, so keeping the last-confirmed set is the safe floor. It is
// cleared when engine upstream TLS is not engaged — the gate is then vacuous.
//
// bundleErr is the error from assembling the bundle (ensureEngineCABundle). When
// it is non-nil the fingerprints are nil but the previous bundle Secret and
// gateway Deployment may still match and be fully serving, so this preserves the
// last-confirmed set rather than clobbering it — otherwise a transient assembly
// error would wrongly block engine cutovers the gateway can still serve.
// ensureEngineCABundle returns (nil, nil) when engine upstream TLS
// is not engaged, so the clear-on-disable path below still runs in that case.
func (r *FireboltInstanceReconciler) publishRolledEngineTrustCAs(ctx context.Context, instance *computev1alpha1.FireboltInstance, fingerprints []string, bundleErr error) {
	if bundleErr != nil {
		return
	}
	if !engineUpstreamTLSReady(instance) {
		instance.Status.RolledEngineTrustCAs = nil
		return
	}
	serving, err := r.gatewayServingCurrentConfig(ctx, instance)
	if err != nil {
		logf.FromContext(ctx).Error(err, "checking gateway engine-trust-bundle rollout")
		return
	}
	if serving {
		instance.Status.RolledEngineTrustCAs = fingerprints
	}
}

func (r *FireboltInstanceReconciler) reconcileDelete(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx).WithValues("instance", instance.Name)
	log.Info("Handling instance deletion")

	ns := instance.Namespace
	matchLabels := client.MatchingLabels{LabelInstance: instance.Name}
	var errs []error

	deleteList := func(list client.ObjectList, kind string) {
		if err := r.List(ctx, list, client.InNamespace(ns), matchLabels); err != nil {
			if apimeta.IsNoMatchError(err) {
				// The kind's CRD isn't installed on this cluster (e.g.
				// cert-manager's Certificate against envtest, which has
				// no cert-manager CRDs) — nothing of that kind could
				// exist, so there is nothing to clean up.
				return
			}
			log.Error(err, "Failed to list resources for cleanup", "kind", kind)
			errs = append(errs, err)
			return
		}
		items := extractItems(list)
		for i := range items {
			log.Info("Deleting resource", "kind", kind, "name", items[i].GetName())
			if err := r.Delete(ctx, items[i]); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete resource", "kind", kind, "name", items[i].GetName())
				errs = append(errs, err)
			}
		}
	}

	deleteList(&appsv1.StatefulSetList{}, "StatefulSet")
	deleteList(&appsv1.DeploymentList{}, "Deployment")
	deleteList(&corev1.ServiceList{}, "Service")
	deleteList(&corev1.ConfigMapList{}, "ConfigMap")
	// Certificate MUST be deleted before Secret: cert-manager's
	// Certificate controller recreates its target Secret whenever that
	// Secret goes missing while the Certificate still exists (cert-manager
	// does not owner-reference the Secret to the Certificate unless
	// --enable-certificate-owner-ref is set, which this operator does not
	// require). Sweeping the Secret first would leave a window — between
	// this sweep and the Certificate's later, asynchronous owner-reference
	// GC — in which cert-manager recreates the signing-key Secret, now
	// orphaned and carrying LabelInstance for an instance that no longer
	// exists. Deleting the Certificate first stops that reconciliation
	// before the Secret sweep runs.
	deleteList(&certmanagerv1.CertificateList{}, "Certificate")
	deleteList(&corev1.SecretList{}, "Secret")
	deleteList(&corev1.PersistentVolumeClaimList{}, "PersistentVolumeClaim")
	deleteList(&policyv1.PodDisruptionBudgetList{}, "PodDisruptionBudget")
	deleteList(&corev1.ServiceAccountList{}, "ServiceAccount")
	deleteList(&rbacv1.RoleBindingList{}, "RoleBinding")

	if len(errs) > 0 {
		return fmt.Errorf("cleanup failed with %d errors, first: %w", len(errs), errs[0])
	}

	controllerutil.RemoveFinalizer(instance, instanceFinalizerName)
	if err := r.Update(ctx, instance); err != nil {
		return err
	}

	r.MetricsRecorder.Delete(instance.Namespace, instance.Name)

	log.Info("Instance deletion complete")
	return nil
}

// extractItems returns the individual objects from a typed list. The type
// switch dispatches an interface-typed list to its concrete element type;
// boxItems then handles the uniform &items[i] boxing without reflection.
func extractItems(list client.ObjectList) []client.Object {
	switch l := list.(type) {
	case *appsv1.StatefulSetList:
		return boxItems[appsv1.StatefulSet, *appsv1.StatefulSet](l.Items)
	case *appsv1.DeploymentList:
		return boxItems[appsv1.Deployment, *appsv1.Deployment](l.Items)
	case *corev1.ServiceList:
		return boxItems[corev1.Service, *corev1.Service](l.Items)
	case *corev1.ConfigMapList:
		return boxItems[corev1.ConfigMap, *corev1.ConfigMap](l.Items)
	case *certmanagerv1.CertificateList:
		return boxItems[certmanagerv1.Certificate, *certmanagerv1.Certificate](l.Items)
	case *corev1.SecretList:
		return boxItems[corev1.Secret, *corev1.Secret](l.Items)
	case *corev1.PersistentVolumeClaimList:
		return boxItems[corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim](l.Items)
	case *policyv1.PodDisruptionBudgetList:
		return boxItems[policyv1.PodDisruptionBudget, *policyv1.PodDisruptionBudget](l.Items)
	case *corev1.ServiceAccountList:
		return boxItems[corev1.ServiceAccount, *corev1.ServiceAccount](l.Items)
	case *rbacv1.RoleBindingList:
		return boxItems[rbacv1.RoleBinding, *rbacv1.RoleBinding](l.Items)
	default:
		return nil
	}
}

// boxItems converts []T to []client.Object by taking the address of each
// element. The PT constraint encodes "pointer-to-T that implements
// client.Object", which is the shape every typed K8s list satisfies.
func boxItems[T any, PT interface {
	*T
	client.Object
}](items []T) []client.Object {
	out := make([]client.Object, len(items))
	for i := range items {
		out[i] = PT(&items[i])
	}
	return out
}

// validateInstanceTemplates re-runs the FireboltInstance webhook's
// pod-template ownership check against spec.gateway.template and
// spec.metadata.template. Returns the per-component error lists so
// Reconcile can surface each failure on the matching component
// condition (GatewayReady / MetadataReady) with the field path the
// user needs to fix.
//
// This is defense-in-depth, not bypass: when the validating webhook is
// in the request path, both error lists are empty by construction
// (admission already rejected the apply). When the webhook is off,
// this is the only place the operator-owned-field rules are enforced.
// A nil template returns nil, so users with no template at all pass
// through.
func validateInstanceTemplates(inst *computev1alpha1.FireboltInstance) (gateway, metadata field.ErrorList) {
	gateway = computev1alpha1.ValidatePodTemplate(
		inst.Spec.Gateway.Template,
		field.NewPath("spec", "gateway", "template"),
		&computev1alpha1.GatewayPodTemplateRules,
	)
	metadata = computev1alpha1.ValidatePodTemplate(
		inst.Spec.Metadata.Template,
		field.NewPath("spec", "metadata", "template"),
		&computev1alpha1.MetadataPodTemplateRules,
	)
	// One Instance-wide predicate for BOTH templates, not a per-component list
	// each: the gateway template has no business reaching the admin password or a
	// JWT signing key, and the metadata template has no business reaching the
	// gateway's serving key. Screening each template only against the Secrets its
	// own pod mounts left exactly those cross-component routes open.
	protected := instanceProtectedSecret(inst)
	if inst.Spec.Gateway.Template != nil {
		gateway = append(gateway, validateTemplateSecretRefs(inst.Spec.Gateway.Template,
			field.NewPath("spec", "gateway", "template", "spec"), protected, "gateway")...)
	}
	if inst.Spec.Metadata.Template != nil {
		metadata = append(metadata, validateTemplateSecretRefs(inst.Spec.Metadata.Template,
			field.NewPath("spec", "metadata", "template", "spec"), protected, "metadata")...)
	}
	return gateway, metadata
}

// validateTemplateSecretRefs rejects every route a component template has to the
// operator's own Secrets: a volume whose source reaches one, or a container that
// reads one into its environment.
//
// Unlike the engine, both routes block here. The gateway and metadata pods are
// Deployments the operator rolls directly, so there is no generation to be frozen
// on by declining to render.
func validateTemplateSecretRefs(
	tpl *corev1.PodTemplateSpec, base *field.Path, isProtected func(string) bool, component string,
) field.ErrorList {
	errs := computev1alpha1.ValidateNoSecretAliasVolumes(
		tpl.Spec.Volumes, base.Child("volumes"), isProtected, component)
	errs = append(errs, computev1alpha1.ValidateNoSecretRefEnv(
		tpl.Spec.Containers, base.Child("containers"), isProtected, component)...)
	return append(errs, computev1alpha1.ValidateNoSecretRefEnv(
		tpl.Spec.InitContainers, base.Child("initContainers"), isProtected, component)...)
}

// instanceProtectedSecret is the authoritative, complete predicate for "no user
// pod template may reach this Secret". Every template gate in the operator — the
// gateway and metadata templates here, the engine and engine-class templates in
// engine_controller.go — resolves through this one function, so a Secret is
// protected on every path the moment it is protected on any.
//
// It is the union of three sources:
//
//   - computev1alpha1.InstanceOperatorSecretNames: everything derivable from the
//     CR (admin password, signing keys, engine-TLS anchor, gateway serving cert,
//     gateway client CA, an external Postgres credential).
//   - the two names formed from suffixes private to this package: the engine CA
//     bundle, and the operator-generated Postgres credential.
//   - a SHAPE match for per-generation engine-TLS Secrets, deliberately NOT bound
//     to any one engine's name. Binding it to the engine under review — as the
//     per-engine predicate used to — let engine A's template alias engine B's
//     serving private key, since B's Secret name does not start with A's.
func instanceProtectedSecret(inst *computev1alpha1.FireboltInstance) func(string) bool {
	exact := make(map[string]struct{})
	for _, n := range instanceProtectedSecretNames(inst) {
		exact[n] = struct{}{}
	}
	return func(name string) bool {
		if name == "" {
			return false
		}
		if _, hit := exact[name]; hit {
			return true
		}
		return isGeneratedEngineTLSSecretName(name) ||
			computev1alpha1.IsSigningKeySecretName(name)
	}
}

// instanceProtectedSecretNames is the exact-match half of
// instanceProtectedSecret: every operator-managed Secret name for this Instance
// that can be named outright. Exposed separately so the engine controller can
// carry the list across into InstanceInfo (which is built from the Instance and
// then consumed without it) and rebuild the same predicate on the far side.
// Empty entries are dropped, so a component still provisioning protects nothing.
func instanceProtectedSecretNames(inst *computev1alpha1.FireboltInstance) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(n string) {
		if n == "" {
			return
		}
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	for _, n := range computev1alpha1.InstanceOperatorSecretNames(inst) {
		add(n)
	}
	add(engineCABundleSecretName(inst.Name))
	add(metadataCredsSecretName(inst))
	return out
}

// isGeneratedEngineTLSSecretName reports whether name has the shape of a
// per-generation engine-TLS Secret (genResourceName(engine, gen,
// SuffixEngineTLS)) for ANY engine. Shared with admission, which screens
// spec.tls.*.secretRef against the same shape.
func isGeneratedEngineTLSSecretName(name string) bool {
	return computev1alpha1.IsGeneratedEngineTLSSecretName(name)
}

// checkExternalPostgresSecret verifies the Secret referenced by
// spec.metadata.postgres.credentialsSecretRef exists in the instance's
// namespace. It does NOT inspect the Secret's data (key presence,
// formatting, rotation): users who mis-key the Secret will still hit a
// crash-loop on the metadata pod itself, but the far more common
// mistakes — typoed Secret name, forgotten Secret creation, deleted
// Secret — are caught here with a message that names the missing Secret.
//
// Admission-time webhook validation already rejects empty
// credentialsSecretRef.Name; this function guards against the runtime
// case where the Name is set but the Secret does not (yet) exist.
func (r *FireboltInstanceReconciler) checkExternalPostgresSecret(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	pg := instance.Spec.Metadata.Postgres
	if pg == nil {
		return nil
	}
	name := pg.CredentialsSecretRef.Name
	if name == "" {
		return errPostgresSecretRefEmpty
	}
	var secret corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: name}, &secret)
	if err == nil {
		return nil
	}
	if errors.IsNotFound(err) {
		return fmt.Errorf("external postgres credentials secret %s/%s not found", instance.Namespace, name)
	}
	return fmt.Errorf("getting external postgres credentials secret %s/%s: %w", instance.Namespace, name, err)
}

// writeStatusAndPoll persists the current in-memory status and schedules a
// fixed-interval poll. Use this for "condition is False but no error
// occurred" transient states (e.g. waiting for pods to report Ready). An
// exponential backoff would be wrong here: the polled signal becomes True
// on an event that is NOT tied to reconcile retries (pod readiness
// transition, external Secret creation), so the poll interval should stay
// short regardless of how many times we have already looped.
//
// For actual errors, use failWithCondition instead; it returns the error to
// controller-runtime so its work-queue rate-limiter applies exponential
// backoff.
func (r *FireboltInstanceReconciler) writeStatusAndPoll(
	ctx context.Context,
	instance *computev1alpha1.FireboltInstance,
	every time.Duration,
) (ctrl.Result, error) {
	// Order matters: computePhase reads InstanceConditionReady, so the
	// roll-up must be refreshed first. See computePhase godoc.
	setInstanceReadyRollup(instance)
	instance.Status.Phase = r.computePhase(instance)
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: every}, nil
}

// failWithCondition records a per-component condition as False, refreshes the
// roll-up Ready condition, persists the status best-effort, and returns the
// original error to controller-runtime so its exponential work-queue backoff
// applies to retries. This replaces the previous "log.Error + requeue-after-
// 10s with nil error" pattern, which silently capped retry frequency,
// hid failures from controller-runtime metrics, and never populated any
// user-visible condition explaining the failure.
//
// The status-write error is logged and deliberately NOT returned: we want
// the caller to see the ORIGINAL root-cause error (that is what the user
// needs to debug and what controller-runtime should back off on). A
// subsequent reconcile will retry the status write; if status writes are
// persistently failing, unrelated code paths that do `return ctrl.Result{},
// err` for status updates will surface that failure mode directly. Joining
// both errors would make the returned error message less focused and is
// not worth the trade-off given this pattern is called only on the failure
// path.
func (r *FireboltInstanceReconciler) failWithCondition(
	ctx context.Context,
	instance *computev1alpha1.FireboltInstance,
	condType, reason string,
	err error,
) error {
	log := logf.FromContext(ctx)
	setInstanceCondition(instance, condType, metav1.ConditionFalse, reason, err.Error())
	// Order matters: computePhase reads InstanceConditionReady, so the
	// roll-up must be refreshed first. See computePhase godoc.
	setInstanceReadyRollup(instance)
	instance.Status.Phase = r.computePhase(instance)
	if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
		log.Error(updateErr, "Failed to persist failure condition",
			"condition", condType, "reason", reason, "originalError", err.Error())
	}
	return fmt.Errorf("%s (%s): %w", condType, reason, err)
}

// setInstanceCondition writes a condition on the instance's status.
// apimeta.SetStatusCondition dedupes internally: when Type/Status/Reason/
// Message all match, LastTransitionTime is not bumped, so repeated calls
// with the same values do not generate /status churn or spam watchers.
func setInstanceCondition(
	instance *computev1alpha1.FireboltInstance,
	condType string,
	status metav1.ConditionStatus,
	reason, message string,
) {
	apimeta.SetStatusCondition(&instance.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: instance.Generation,
	})
}

// setInstanceReadyRollup recomputes InstanceConditionReady from the
// per-component conditions. Ready is True iff every required component
// condition is present AND True; otherwise False with the Reason/Message
// of the FIRST not-True component in pipeline order (Metadata → Gateway →
// EngineTLS → GatewayTLS). Propagating the first blocker's Reason makes
// `kubectl describe fireboltinstance` surface the actual root cause on
// the headline condition, so users do not have to scan every condition
// to find the one that is False.
//
// EngineTLSReady and GatewayTLSReady are part of the roll-up (they report
// True with reason "Disabled" when their feature is off, so they never
// block a non-TLS instance). When TLS is requested the Instance must not
// advertise Ready until the requested secure path is actually serving, not
// merely provisioned:
//   - EngineTLSReady is convergence-gated: True only once the
//     engine fleet has rolled onto TLS and the gateway is re-encrypting
//     upstream, so the Instance never reports Ready over a plaintext hop.
//   - GatewayTLSReady is serving-gated: True only once the secure
//     client-facing listener has actually rolled out, so the Instance never
//     reports Ready while the client port still rejects.
//
// There is no deadlock — cert-manager issues the certificates independently,
// and engines gate their own reconcile on the provisioned Status.EngineTLS
// directly (resolveInstanceInfo), NOT on the convergence-gated EngineTLSReady
// condition, so the enable ramp still produces the convergence that flips it.
func setInstanceReadyRollup(instance *computev1alpha1.FireboltInstance) {
	components := []string{
		computev1alpha1.InstanceConditionMetadataReady,
		computev1alpha1.InstanceConditionGatewayReady,
		computev1alpha1.InstanceConditionEngineTLSReady,
		computev1alpha1.InstanceConditionGatewayTLSReady,
	}
	for _, c := range components {
		cond := apimeta.FindStatusCondition(instance.Status.Conditions, c)
		if cond == nil {
			setInstanceCondition(instance, computev1alpha1.InstanceConditionReady,
				metav1.ConditionFalse, "Initializing",
				fmt.Sprintf("%s has not been observed yet", c))
			return
		}
		if cond.Status != metav1.ConditionTrue {
			setInstanceCondition(instance, computev1alpha1.InstanceConditionReady,
				metav1.ConditionFalse, cond.Reason,
				fmt.Sprintf("%s: %s", c, cond.Message))
			return
		}
	}
	setInstanceCondition(instance, computev1alpha1.InstanceConditionReady,
		metav1.ConditionTrue, "AllComponentsReady",
		"metadata, gateway, and TLS certificates are ready")
}

// computePhase derives the instance Phase from InstanceConditionReady,
// which is itself the roll-up of the per-component conditions
// (Metadata, Gateway, EngineTLS, GatewayTLS) computed by setInstanceReadyRollup.
// The invariant is:
//
//	Phase == Ready  ⇔  InstanceConditionReady.Status == True
//
// There is exactly one source of truth for "is this instance ready".
// Callers MUST refresh the roll-up (via setInstanceReadyRollup) before
// calling computePhase; otherwise a stale condition will produce a
// stale Phase. The three call sites in this file observe that order.
//
// Historical note: this function used to compute Phase from the boolean
// Status.MetadataReady && Status.GatewayReady, which diverged from
// InstanceConditionReady in two ways:
//
//  1. A per-component condition that flipped False post-rollout was
//     ignored. For example, an external-Postgres instance whose
//     credentials Secret was deleted post-rollout kept Phase=Ready
//     (mounted creds keep the metadata pod running) while
//     InstanceConditionReady correctly flipped to False on the next
//     preflight.
//  2. The mid-reconcile booleans are not reset between passes, leaving
//     stale-true values that would re-assert Phase=Ready while a
//     freshly-set component condition was False.
//
// Both cases were user-visible lies on the headline Phase field.
// Deriving Phase from the same condition "Ready" that kubectl describe
// shows eliminates them. The per-component booleans are preserved as a
// lower-level signal (and for printcolumn display) but no longer feed
// into Phase.
//
// Phase state machine:
//
//	Failed is terminal and is never overwritten by this function.
//	Provisioning → Ready    when ConditionReady flips True.
//	Ready       → Degraded  when ConditionReady flips back to False.
//	Degraded    → Ready     when ConditionReady recovers to True.
func (r *FireboltInstanceReconciler) computePhase(instance *computev1alpha1.FireboltInstance) computev1alpha1.InstancePhase {
	if instance.Status.Phase == computev1alpha1.InstancePhaseFailed {
		return computev1alpha1.InstancePhaseFailed
	}

	ready := apimeta.FindStatusCondition(
		instance.Status.Conditions,
		computev1alpha1.InstanceConditionReady,
	)
	if ready != nil && ready.Status == metav1.ConditionTrue {
		return computev1alpha1.InstancePhaseReady
	}

	if instance.Status.Phase == computev1alpha1.InstancePhaseReady ||
		instance.Status.Phase == computev1alpha1.InstancePhaseDegraded {
		return computev1alpha1.InstancePhaseDegraded
	}

	return computev1alpha1.InstancePhaseProvisioning
}

func (r *FireboltInstanceReconciler) isMetadataServiceReady(ctx context.Context, instance *computev1alpha1.FireboltInstance) (bool, error) {
	name := instance.Name + SuffixMetadataService
	var dep appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &dep); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.ReadyReplicas > 0, nil
}

func metadataServiceEndpoint(instanceName, namespace string) string {
	return fmt.Sprintf("%s%s.%s.svc.cluster.local:%d",
		instanceName, SuffixMetadataService, namespace, MetadataServicePort)
}

// instanceLabels returns the standard labels for resources owned by this instance.
func instanceLabels(instanceName, component string) map[string]string {
	return map[string]string{
		LabelInstance:  instanceName,
		LabelComponent: component,
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *FireboltInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerNamed(mgr, "fireboltinstance")
}

// SetupWithManagerNamed sets up the controller with the Manager using a
// custom controller name. Useful for E2E tests that spin up multiple in-process
// reconcilers per suite and need unique metric names across them.
func (r *FireboltInstanceReconciler) SetupWithManagerNamed(mgr ctrl.Manager, name string) error {
	if r.EventRecorder == nil {
		r.EventRecorder = mgr.GetEventRecorder(name)
	}
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&computev1alpha1.FireboltInstance{},
		externalPostgresCredentialsSecretIndexField,
		externalPostgresCredentialsSecretIndexValues,
	); err != nil {
		return fmt.Errorf("indexing external postgres credential Secret references: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&computev1alpha1.FireboltInstance{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapMetadataCredentialsSecretToInstances)).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(
			&computev1alpha1.FireboltEngine{},
			handler.EnqueueRequestsFromMapFunc(enqueueInstanceFromEngine),
			// Spec changes only. FireboltEngineReconciler.updateStatus
			// stamps Status.LastReconciled on every status write, so an
			// unfiltered watch would enqueue a full Instance reconcile —
			// metadata and gateway render, PVC list, status write — on
			// every engine reconcile in the namespace. The gate only
			// cares about the engine's image pin, which lives in spec.
			builder.WithPredicates(predicate.GenerationChangedPredicate{}),
		).
		Named(name).
		Complete(r)
}

// externalPostgresCredentialsSecretIndexValues indexes only explicit external
// PostgreSQL credential references. Internal PostgreSQL uses an
// operator-generated Secret, whose events must remain covered solely by the
// ordinary owned-resource reconcile path.
func externalPostgresCredentialsSecretIndexValues(obj client.Object) []string {
	instance, ok := obj.(*computev1alpha1.FireboltInstance)
	if !ok || instance.Spec.Metadata.Postgres == nil {
		return nil
	}
	name := instance.Spec.Metadata.Postgres.CredentialsSecretRef.Name
	if name == "" {
		return nil
	}
	return []string{name}
}

// mapMetadataCredentialsSecretToInstances makes an external credentials Secret
// refresh an explicit reconcile input. Secret synchronization controllers do
// not set the FireboltInstance as owner, so Owns cannot observe these updates.
// The field index keeps unrelated and internal Secret events from scanning
// every Instance in the namespace.
func (r *FireboltInstanceReconciler) mapMetadataCredentialsSecretToInstances(
	ctx context.Context,
	obj client.Object,
) []ctrl.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}
	instances := &computev1alpha1.FireboltInstanceList{}
	if err := r.List(
		ctx,
		instances,
		client.InNamespace(secret.Namespace),
		client.MatchingFields{externalPostgresCredentialsSecretIndexField: secret.Name},
	); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(instances.Items))
	for i := range instances.Items {
		instance := &instances.Items[i]
		if instance.Spec.Metadata.Postgres == nil ||
			instance.Spec.Metadata.Postgres.CredentialsSecretRef.Name != secret.Name {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(instance)})
	}
	return requests
}

// enqueueInstanceFromEngine maps a FireboltEngine spec change to its
// spec.instanceRef so an engine image pin change re-runs the
// instance-id canonicalize gate.
func enqueueInstanceFromEngine(_ context.Context, obj client.Object) []reconcile.Request {
	eng, ok := obj.(*computev1alpha1.FireboltEngine)
	if !ok || eng.Spec.InstanceRef == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: eng.Spec.InstanceRef, Namespace: eng.Namespace},
	}}
}
