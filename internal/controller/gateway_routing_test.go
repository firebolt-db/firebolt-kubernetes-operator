// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type routingReportTransport func(*http.Request) (*http.Response, error)

func (f routingReportTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func useRoutingReport(t *testing.T, report *routing.Report) {
	t.Helper()
	previous := gatewayReportClient
	gatewayReportClient = &http.Client{Transport: routingReportTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/routing" {
			return nil, errors.New("unexpected gateway endpoint")
		}
		if report == nil {
			return nil, errors.New("gateway unreachable")
		}
		body, err := json.Marshal(report)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
	})}
	t.Cleanup(func() { gatewayReportClient = previous })
}

type routingTestFixture struct {
	client   client.Client
	instance *computev1alpha1.FireboltInstance
	engine   *computev1alpha1.FireboltEngine
	pod      *corev1.Pod
	state    routing.State
	oldRoute routing.Route
	scheme   *runtime.Scheme
}

func newRoutingTestFixture(t *testing.T, includePod bool, resources ...client.Object) *routingTestFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, computev1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	instance := &computev1alpha1.FireboltInstance{ObjectMeta: metav1.ObjectMeta{Name: "instance", Namespace: "routing-test", UID: "instance-uid"}}
	engine := &computev1alpha1.FireboltEngine{ObjectMeta: metav1.ObjectMeta{Name: "query", Namespace: instance.Namespace, UID: "engine-uid"}, Spec: computev1alpha1.FireboltEngineSpec{InstanceRef: instance.Name}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: instance.Namespace, UID: "pod-uid", Labels: instanceLabels(instance.Name, "gateway")},
		Status: corev1.PodStatus{PodIP: "192.0.2.10"}}
	state := routing.NewState(string(instance.UID))
	route := routing.Route{EngineUID: string(engine.UID), Generation: 1, Authority: "old.routing-test.svc.cluster.local:3473", Enabled: true}
	if _, err := routing.SetRoute(&state, engine.Name, route); err != nil {
		t.Fatal(err)
	}
	if _, err := routing.RegisterSession(&state, string(pod.UID), "boot-old"); err != nil {
		t.Fatal(err)
	}
	oldRoute := state.Routes[engine.Name]
	route.Generation = 2
	route.Authority = "new.routing-test.svc.cluster.local:3473"
	if _, err := routing.SetRoute(&state, engine.Name, route); err != nil {
		t.Fatal(err)
	}
	data, err := routing.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: routing.ConfigMapName(instance.Name), Namespace: instance.Namespace}, Data: map[string]string{routing.DataKey: string(data)}}
	objects := []client.Object{instance, engine, cm}
	if includePod {
		objects = append(objects, pod)
	}
	objects = append(objects, resources...)
	return &routingTestFixture{client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		instance: instance, engine: engine, pod: pod, state: state, oldRoute: oldRoute, scheme: scheme}
}

func (f *routingTestFixture) report() routing.Report {
	return routing.Report{PodUID: string(f.pod.UID), SessionID: "boot-old", Registered: true,
		AppliedRevision: f.state.Revision, Outstanding: map[string]routing.Count{}}
}

func TestGatewayRoutingRemovalRequiresMatchingFence(t *testing.T) {
	tests := []struct {
		name        string
		includePod  bool
		unreachable bool
		edit        func(*routing.Report, routing.Route)
		allowed     bool
	}{
		{name: "post-fence zero", includePod: true, allowed: true},
		{name: "Pod absent"},
		{name: "gateway unreachable", includePod: true, unreachable: true},
		{name: "stale fence", includePod: true, edit: func(r *routing.Report, _ routing.Route) { r.AppliedRevision-- }},
		{name: "wrong Pod UID", includePod: true, edit: func(r *routing.Report, _ routing.Route) { r.PodUID = "another-pod" }},
		{name: "restarted agent", includePod: true, edit: func(r *routing.Report, _ routing.Route) { r.SessionID = "new-boot" }},
		{name: "unregistered agent", includePod: true, edit: func(r *routing.Report, _ routing.Route) { r.Registered = false }},
		{name: "unknown permit", includePod: true, edit: func(r *routing.Report, old routing.Route) { r.Outstanding[old.Key()] = routing.Count{Unknown: 1} }},
		{name: "active permit", includePod: true, edit: func(r *routing.Report, old routing.Route) { r.Outstanding[old.Key()] = routing.Count{Active: 1} }},
		{name: "missing accounting", includePod: true, edit: func(r *routing.Report, _ routing.Route) { r.Outstanding = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newRoutingTestFixture(t, tt.includePod)
			report := fixture.report()
			if tt.edit != nil {
				tt.edit(&report, fixture.oldRoute)
			}
			if tt.unreachable {
				useRoutingReport(t, nil)
			} else {
				useRoutingReport(t, &report)
			}
			r := FireboltEngineReconciler{Client: fixture.client, APIReader: fixture.client}
			err := r.authorizeGenerationRemoval(context.Background(), fixture.engine, 1)
			if (err == nil) != tt.allowed {
				t.Fatalf("allowed=%v, authorization error=%v", tt.allowed, err)
			}
			if err := r.authorizeGenerationRemoval(context.Background(), fixture.engine, 2); err == nil {
				t.Fatal("current enabled generation must never be removed")
			}
		})
	}
}

func TestGatewayRoutingPositiveTerminationEvidence(t *testing.T) {
	now := metav1.Now()
	terminated := func(p *corev1.Pod) {
		p.DeletionTimestamp = &now
		p.Status.Phase = corev1.PodSucceeded
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: computev1alpha1.GatewayContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: now}}}}
	}
	tests := []struct {
		name    string
		edit    func(*corev1.Pod)
		stopped bool
	}{
		{name: "running gateway"},
		{name: "deletion alone", edit: func(p *corev1.Pod) { p.DeletionTimestamp = &now }},
		{name: "restartable terminated container", edit: func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: computev1alpha1.GatewayContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		}},
		{name: "last termination is insufficient", edit: func(p *corev1.Pod) {
			p.DeletionTimestamp = &now
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: computev1alpha1.GatewayContainerName, LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		}},
		{name: "agent termination is insufficient", edit: func(p *corev1.Pod) {
			p.DeletionTimestamp = &now
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "wake-agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}
		}},
		{name: "deleting gateway terminated", stopped: true, edit: terminated},
		{name: "crashed gateway still in Running Pod", edit: func(p *corev1.Pod) { terminated(p); p.Status.Phase = corev1.PodRunning }},
		{name: "lost node synthetic Pod failure", edit: func(p *corev1.Pod) { terminated(p); p.Status.Phase = corev1.PodFailed; p.Status.Reason = "NodeLost" }},
		{name: "PodGC failure preserves stale container termination", edit: func(p *corev1.Pod) {
			terminated(p)
			p.Status.Phase = corev1.PodFailed
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "DeletionByPodGC"}}
		}},
		{name: "unknown synthetic container status", edit: func(p *corev1.Pod) {
			terminated(p)
			p.Status.ContainerStatuses[0].State.Terminated.Reason = "ContainerStatusUnknown"
		}},
		{name: "lost node container status", edit: func(p *corev1.Pod) { terminated(p); p.Status.ContainerStatuses[0].State.Terminated.Reason = "NodeLost" }},
		{name: "missing completion timestamp", edit: func(p *corev1.Pod) {
			terminated(p)
			p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{}
			if tt.edit != nil {
				tt.edit(pod)
			}
			if got := gatewayEnvoyStopped(pod); got != tt.stopped {
				t.Fatalf("stopped=%v, want %v", got, tt.stopped)
			}
		})
	}
}

func TestGatewayRoutingRestartDoesNotReplaceRegisteredSession(t *testing.T) {
	fixture := newRoutingTestFixture(t, true)
	report := fixture.report()
	report.SessionID = "new-boot"
	useRoutingReport(t, &report)
	r := FireboltInstanceReconciler{Client: fixture.client, APIReader: fixture.client, Scheme: fixture.scheme}
	if err := r.registerGatewaySessions(context.Background(), fixture.instance); err != nil {
		t.Fatal(err)
	}
	_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if state.Sessions[string(fixture.pod.UID)].ID != "boot-old" {
		t.Fatal("new agent replaced unresolved predecessor")
	}
	pod := &corev1.Pod{}
	if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(fixture.pod), pod); err != nil {
		t.Fatal(err)
	}
	if pod.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(pod, gatewayRoutingFinalizer) {
		t.Fatal("restarted gateway must terminate with its evidence finalizer retained")
	}
}

func TestGatewayRoutingNewSessionWaitsForEnvoyProcess(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  *corev1.ContainerStatus
		started bool
	}{
		{name: "no container status"},
		{name: "image pull failure", status: &corev1.ContainerStatus{Name: computev1alpha1.GatewayContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ErrImageNeverPull"}}}},
		{name: "running unrelated container", status: &corev1.ContainerStatus{Name: "other",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}},
		{name: "running Envoy", started: true, status: &corev1.ContainerStatus{Name: computev1alpha1.GatewayContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newRoutingTestFixture(t, false)
			cm, _, err := readRoutingState(t.Context(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			data, err := routing.Encode(routing.NewState(string(fixture.instance.UID)))
			if err != nil {
				t.Fatal(err)
			}
			cm.Data[routing.DataKey] = string(data)
			if err := fixture.client.Update(t.Context(), cm); err != nil {
				t.Fatal(err)
			}
			if tt.status != nil {
				fixture.pod.Status.ContainerStatuses = []corev1.ContainerStatus{*tt.status}
			}
			if err := fixture.client.Create(t.Context(), fixture.pod); err != nil {
				t.Fatal(err)
			}
			report := fixture.report()
			report.Registered = false
			useRoutingReport(t, &report)
			r := FireboltInstanceReconciler{Client: fixture.client, APIReader: fixture.client, Scheme: fixture.scheme}
			if err := r.registerGatewaySessions(t.Context(), fixture.instance); err != nil {
				t.Fatal(err)
			}
			_, state, err := readRoutingState(t.Context(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			_, registered := state.Sessions[string(fixture.pod.UID)]
			pod := &corev1.Pod{}
			if err := fixture.client.Get(t.Context(), client.ObjectKeyFromObject(fixture.pod), pod); err != nil {
				t.Fatal(err)
			}
			if registered != tt.started || controllerutil.ContainsFinalizer(pod, gatewayRoutingFinalizer) != tt.started {
				t.Fatalf("registered=%v finalizers=%v, want authority only after Envoy starts: %v", registered, pod.Finalizers, tt.started)
			}
		})
	}
}

func TestGatewayRoutingRejectsReportFromReusedPodIP(t *testing.T) {
	fixture := newRoutingTestFixture(t, true)
	report := fixture.report()
	report.PodUID = "different-pod"
	useRoutingReport(t, &report)
	if _, err := fetchGatewayReport(context.Background(), fixture.pod); err == nil {
		t.Fatal("Pod IP must not substitute for Pod UID")
	}
}

func TestGatewayRoutingProtectsGenerationResourcesUntilFence(t *testing.T) {
	factories := map[string]func(metav1.ObjectMeta) client.Object{
		"StatefulSet": func(m metav1.ObjectMeta) client.Object { return &appsv1.StatefulSet{ObjectMeta: m} },
		"Service":     func(m metav1.ObjectMeta) client.Object { return &corev1.Service{ObjectMeta: m} },
		"Secret":      func(m metav1.ObjectMeta) client.Object { return &corev1.Secret{ObjectMeta: m} },
		"ConfigMap":   func(m metav1.ObjectMeta) client.Object { return &corev1.ConfigMap{ObjectMeta: m} },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			resource := factory(metav1.ObjectMeta{Name: "old-generation", Namespace: "routing-test", Labels: map[string]string{LabelEngine: "query", LabelGeneration: "1"}})
			fixture := newRoutingTestFixture(t, true, resource)
			report := fixture.report()
			report.Outstanding[fixture.oldRoute.Key()] = routing.Count{Active: 1}
			useRoutingReport(t, &report)
			r := FireboltEngineReconciler{Client: fixture.client, APIReader: fixture.client}
			if err := r.deleteIfExists(context.Background(), resource); err == nil {
				t.Fatal("active permit must block resource deletion")
			}
			if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(resource), resource); err != nil {
				t.Fatalf("blocked deletion changed resource: %v", err)
			}
			delete(report.Outstanding, fixture.oldRoute.Key())
			if err := r.deleteIfExists(context.Background(), resource); err != nil {
				t.Fatal(err)
			}
			if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(resource), resource); !apierrors.IsNotFound(err) {
				t.Fatalf("acknowledged resource not deleted: %v", err)
			}
		})
	}
}

func TestGatewayRoutingMissingStateCannotForgetExistingGateway(t *testing.T) {
	fixture := newRoutingTestFixture(t, true)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: routing.ConfigMapName(fixture.instance.Name), Namespace: fixture.instance.Namespace}}
	if err := fixture.client.Delete(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	r := FireboltInstanceReconciler{Client: fixture.client, APIReader: fixture.client, Scheme: fixture.scheme}
	if err := r.ensureGatewayRouting(context.Background(), fixture.instance); err == nil {
		t.Fatal("missing coordination must not erase an existing gateway's authority")
	}
	if err := fixture.client.Get(context.Background(), types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, cm); !apierrors.IsNotFound(err) {
		t.Fatalf("missing record was recreated: %v", err)
	}
}

func TestGatewayRoutingRejectsAnotherInstanceIdentity(t *testing.T) {
	fixture := newRoutingTestFixture(t, false)
	fixture.instance.UID = "replacement-instance-uid"
	r := FireboltInstanceReconciler{Client: fixture.client, APIReader: fixture.client, Scheme: fixture.scheme}
	if err := r.ensureGatewayRouting(context.Background(), fixture.instance); err == nil {
		t.Fatal("another Instance UID's coordination must not be adopted")
	}
}

func TestGatewayRoutingTerminatedProcessResolvesLostAccounting(t *testing.T) {
	fixture := newRoutingTestFixture(t, true)
	stopRoutingGateway(t, fixture)
	useRoutingReport(t, nil)
	r := FireboltEngineReconciler{Client: fixture.client, APIReader: fixture.client}
	if err := r.authorizeGenerationRemoval(context.Background(), fixture.engine, 1); err != nil {
		t.Fatalf("positive process termination must resolve even unreachable accounting: %v", err)
	}
}

func stopRoutingGateway(t *testing.T, fixture *routingTestFixture) {
	t.Helper()
	pod := &corev1.Pod{}
	key := client.ObjectKeyFromObject(fixture.pod)
	if err := fixture.client.Get(context.Background(), key, pod); err != nil {
		t.Fatal(err)
	}
	controllerutil.AddFinalizer(pod, gatewayRoutingFinalizer)
	if err := fixture.client.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := fixture.client.Get(context.Background(), key, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: computev1alpha1.GatewayContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.Now()}}}}
	pod.Status.Phase = corev1.PodSucceeded
	if err := fixture.client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

type routingRejectConfigMapUpdate struct{ client.Client }

func (c routingRejectConfigMapUpdate) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		return errors.New("injected coordination persistence failure")
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestGatewayRoutingPersistsStopEvidenceBeforeRemovingFinalizer(t *testing.T) {
	for _, persistenceFailure := range []bool{true, false} {
		name := "persisted"
		if persistenceFailure {
			name = "persistence failed"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newRoutingTestFixture(t, true)
			stopRoutingGateway(t, fixture)
			writer := fixture.client
			if persistenceFailure {
				writer = routingRejectConfigMapUpdate{Client: fixture.client}
			}
			r := FireboltInstanceReconciler{Client: writer, APIReader: fixture.client, Scheme: fixture.scheme}
			err := r.reclaimStoppedGatewaySessions(context.Background(), fixture.instance)
			if (err != nil) != persistenceFailure {
				t.Fatalf("persistence failure=%v, error=%v", persistenceFailure, err)
			}
			pod := &corev1.Pod{}
			podErr := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(fixture.pod), pod)
			_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			_, registered := state.Sessions[string(fixture.pod.UID)]
			if persistenceFailure {
				if podErr != nil || !controllerutil.ContainsFinalizer(pod, gatewayRoutingFinalizer) || !registered {
					t.Fatal("failed persistence must retain Pod evidence and durable obligations")
				}
			} else {
				if !apierrors.IsNotFound(podErr) || registered {
					t.Fatal("successful persistence should release the Pod finalizer and registration")
				}
				if !routing.CanRetire(state.Retirements[fixture.oldRoute.Key()], nil, nil) {
					t.Fatal("persisted stop evidence must survive removal of the Pod object")
				}
			}
		})
	}
}

func TestGatewayRoutingInitialGenerationZeroCannotBeRemoved(t *testing.T) {
	fixture := newRoutingTestFixture(t, false)
	state := routing.NewState(string(fixture.instance.UID))
	if _, err := routing.SetRoute(&state, fixture.engine.Name, routing.Route{
		EngineUID: string(fixture.engine.UID), Generation: 0, Enabled: true,
		Authority: "initial.routing-test.svc.cluster.local:3473",
	}); err != nil {
		t.Fatal(err)
	}
	cm, _, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := routing.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[routing.DataKey] = string(data)
	if err := fixture.client.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	r := FireboltEngineReconciler{Client: fixture.client, APIReader: fixture.client}
	if err := r.authorizeGenerationRemoval(context.Background(), fixture.engine, 0); err == nil {
		t.Fatal("generation zero is a live generation and requires withdrawal")
	}
}

func TestGatewayRoutingStaleEngineCannotWithdrawReplacement(t *testing.T) {
	fixture := newRoutingTestFixture(t, false)
	stale := fixture.engine.DeepCopy()
	stale.UID = "predecessor-engine-uid"
	r := FireboltEngineReconciler{Client: fixture.client, APIReader: fixture.client}
	if err := r.withdrawEngineRoute(context.Background(), stale); err == nil {
		t.Fatal("a stale engine reconciliation must not withdraw its same-name replacement")
	}
	_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Routes[fixture.engine.Name].Enabled || state.Revision != fixture.state.Revision {
		t.Fatal("stale reconciliation changed replacement routing authority")
	}
}

func TestGatewayRoutingRemovalRejectsMismatchedInstanceRecord(t *testing.T) {
	fixture := newRoutingTestFixture(t, true)
	report := fixture.report()
	useRoutingReport(t, &report)
	cm, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	state.InstanceUID = "predecessor-instance-uid"
	data, err := routing.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[routing.DataKey] = string(data)
	if err := fixture.client.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	r := FireboltEngineReconciler{Client: fixture.client, APIReader: fixture.client}
	if err := r.authorizeGenerationRemoval(context.Background(), fixture.engine, 1); err == nil {
		t.Fatal("retirement cannot rely on another Instance's coordination record")
	}
}

type routingConcurrentRegistration struct {
	client.Client
	injected bool
}

func (c *routingConcurrentRegistration) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok && !c.injected {
		c.injected = true
		live := &corev1.ConfigMap{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
			return err
		}
		state, err := routing.Decode([]byte(live.Data[routing.DataKey]))
		if err != nil {
			return err
		}
		if _, err := routing.RegisterSession(&state, "late-pod", "late-session"); err != nil {
			return err
		}
		data, err := routing.Encode(state)
		if err != nil {
			return err
		}
		live.Data[routing.DataKey] = string(data)
		if err := c.Client.Update(ctx, live); err != nil {
			return err
		}
		return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("concurrent registration"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestGatewayRoutingCASIncludesConcurrentRegistrationInFence(t *testing.T) {
	fixture := newRoutingTestFixture(t, false)
	writer := &routingConcurrentRegistration{Client: fixture.client}
	closed := fixture.state.Routes[fixture.engine.Name]
	if err := mutateRoutingState(context.Background(), writer, fixture.client, fixture.instance.Namespace, fixture.instance.Name,
		func(state *routing.State) (bool, error) {
			route := state.Routes[fixture.engine.Name]
			route.Generation++
			route.Authority = "next.routing-test.svc.cluster.local:3473"
			return routing.SetRoute(state, fixture.engine.Name, route)
		}); err != nil {
		t.Fatal(err)
	}
	_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	retirement := state.Retirements[closed.Key()]
	if retirement.Holders["late-pod"] != "late-session" || retirement.Holders[string(fixture.pod.UID)] != "boot-old" {
		t.Fatalf("registration committed before fence was omitted: %+v", retirement.Holders)
	}
	if retirement.RequiredRevision != state.Revision || state.Revision <= fixture.state.Revision+1 {
		t.Fatal("registration and route fence must each advance the durable revision")
	}
}

func TestGatewayRoutingMissingActiveStatefulSetAllowsRecovery(t *testing.T) {
	fixture := newRoutingTestFixture(t, false)
	engine := &computev1alpha1.FireboltEngine{}
	if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(fixture.engine), engine); err != nil {
		t.Fatal(err)
	}
	engine.Status = computev1alpha1.FireboltEngineStatus{Phase: computev1alpha1.PhaseStable, CurrentGeneration: 2, ActiveGeneration: 2}
	cm, _, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture.client = fake.NewClientBuilder().WithScheme(fixture.scheme).WithObjects(engine, fixture.instance, cm).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}).Build()
	result := computeEngineReconcile(&engine.Spec, &engine.Status, EngineState{}, engine.Name, engine.Namespace, engine.Generation, InstanceInfo{}, nil)
	r := engineRefTestReconciler(fixture.client, fixture.scheme)
	r.APIReader = fixture.client
	if err := r.applyEngineState(context.Background(), engine, &result); err != nil {
		t.Fatalf("missing active StatefulSet prevented recovery intent: %v", err)
	}
	if err := fixture.client.Get(context.Background(), client.ObjectKeyFromObject(engine), engine); err != nil {
		t.Fatal(err)
	}
	if engine.Status.Phase != computev1alpha1.PhaseCreating || engine.Status.CurrentGeneration != 3 {
		t.Fatalf("missing StatefulSet recovery was not persisted: %+v", engine.Status)
	}
	_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != fixture.state.Revision || state.Routes[engine.Name] != fixture.state.Routes[engine.Name] {
		t.Fatal("missing target must not invent or alter routing authority")
	}
}

func TestGatewayRoutingPublicationRejectsPredecessorStatefulSet(t *testing.T) {
	for _, tt := range []struct {
		name     string
		ownerUID types.UID
		deleting bool
		valid    bool
	}{
		{name: "unowned ready generation"},
		{name: "predecessor ready generation", ownerUID: "predecessor-engine"},
		{name: "deleting current generation", ownerUID: "engine-uid", deleting: true},
		{name: "current ready generation", ownerUID: "engine-uid", valid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newRoutingTestFixture(t, false)
			engine := fixture.engine.DeepCopy()
			engine.Status = computev1alpha1.FireboltEngineStatus{Phase: computev1alpha1.PhaseSwitching, CurrentGeneration: 0, ActiveGeneration: -1}
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: genResourceName(engine.Name, 0, ""), Namespace: engine.Namespace},
				Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(1))}, Status: appsv1.StatefulSetStatus{ReadyReplicas: 1}}
			if tt.ownerUID != "" {
				sts.OwnerReferences = []metav1.OwnerReference{{APIVersion: computev1alpha1.GroupVersion.String(), Kind: "FireboltEngine", Name: engine.Name, UID: tt.ownerUID, Controller: ptr(true)}}
			}
			if tt.deleting {
				now := metav1.Now()
				sts.DeletionTimestamp = &now
				sts.Finalizers = []string{"foregroundDeletion"}
			}
			data, err := routing.Encode(routing.NewState(string(fixture.instance.UID)))
			if err != nil {
				t.Fatal(err)
			}
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: routing.ConfigMapName(fixture.instance.Name), Namespace: fixture.instance.Namespace}, Data: map[string]string{routing.DataKey: string(data)}}
			serviceApplies := 0
			cli := fake.NewClientBuilder().WithScheme(fixture.scheme).WithObjects(engine, fixture.instance, sts, cm).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
					serviceApplies++
					return nil
				},
			}).Build()
			if err := cli.Get(t.Context(), client.ObjectKeyFromObject(engine), engine); err != nil {
				t.Fatal(err)
			}
			r := FireboltEngineReconciler{Client: cli, APIReader: cli, Scheme: fixture.scheme}
			err = r.publishEngineRoute(t.Context(), engine)
			if tt.valid && err != nil {
				t.Fatal(err)
			}
			if !tt.valid && !errors.Is(err, errGatewayWithdrawalPending) {
				t.Fatalf("invalid target should wait without publication: %v", err)
			}
			_, state, err := readRoutingState(t.Context(), cli, engine.Namespace, fixture.instance.Name)
			if err != nil {
				t.Fatal(err)
			}
			if tt.valid {
				if route, ok := state.Routes[engine.Name]; !ok || !route.Enabled || route.EngineUID != string(engine.UID) || serviceApplies != 1 {
					t.Fatalf("valid generation did not publish: route=%+v serviceApplies=%d", route, serviceApplies)
				}
			} else if len(state.Routes) != 0 || state.Revision != 1 || serviceApplies != 0 {
				t.Fatalf("predecessor resources acquired fresh authority: routes=%+v revision=%d serviceApplies=%d", state.Routes, state.Revision, serviceApplies)
			}
		})
	}
}

func TestGatewayRoutingDeletedEngineCleanupWaitsForHolders(t *testing.T) {
	fixture := newRoutingTestFixture(t, true)
	if err := fixture.client.Delete(context.Background(), fixture.engine); err != nil {
		t.Fatal(err)
	}
	report := fixture.report()
	report.Outstanding[fixture.oldRoute.Key()] = routing.Count{Active: 1}
	useRoutingReport(t, &report)
	r := FireboltInstanceReconciler{Client: fixture.client, APIReader: fixture.client, Scheme: fixture.scheme}
	if err := r.reclaimDeletedEngineRoutes(context.Background(), fixture.instance); err != nil {
		t.Fatal(err)
	}
	_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if state.Routes[fixture.engine.Name].Enabled || len(state.Retirements) == 0 {
		t.Fatal("deleted engine must be fenced while retaining outstanding obligations")
	}
	report.AppliedRevision = state.Revision
	if err := r.reclaimDeletedEngineRoutes(context.Background(), fixture.instance); err != nil {
		t.Fatal(err)
	}
	_, state, err = readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := state.Retirements[fixture.oldRoute.Key()]; !exists {
		t.Fatal("active old permit was forgotten")
	}
	delete(report.Outstanding, fixture.oldRoute.Key())
	if err := r.reclaimDeletedEngineRoutes(context.Background(), fixture.instance); err != nil {
		t.Fatal(err)
	}
	_, state, err = readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Routes) != 0 || len(state.Retirements) != 0 {
		t.Fatalf("completed deleted engine was not reclaimed: %+v", state)
	}
}

func TestGatewayRoutingDeletedEngineCleanupPreservesSameNameReplacement(t *testing.T) {
	fixture := newRoutingTestFixture(t, true)
	if err := fixture.client.Delete(context.Background(), fixture.engine); err != nil {
		t.Fatal(err)
	}
	replacement := fixture.engine.DeepCopy()
	replacement.UID = "replacement-engine-uid"
	replacement.ResourceVersion = ""
	if err := fixture.client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := mutateRoutingState(context.Background(), fixture.client, fixture.client, fixture.instance.Namespace, fixture.instance.Name,
		func(state *routing.State) (bool, error) {
			return routing.SetRoute(state, replacement.Name, routing.Route{
				EngineUID: string(replacement.UID), Generation: 0, Authority: "replacement.routing-test.svc.cluster.local:3473", Enabled: true,
			})
		}); err != nil {
		t.Fatal(err)
	}
	_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	report := fixture.report()
	report.AppliedRevision = state.Revision
	useRoutingReport(t, &report)
	r := FireboltInstanceReconciler{Client: fixture.client, APIReader: fixture.client, Scheme: fixture.scheme}
	if err := r.reclaimDeletedEngineRoutes(context.Background(), fixture.instance); err != nil {
		t.Fatal(err)
	}
	_, state, err = readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	route := state.Routes[replacement.Name]
	if !route.Enabled || route.EngineUID != string(replacement.UID) || route.Generation != 0 {
		t.Fatal("cleanup withdrew the live same-name replacement")
	}
	if len(state.Retirements) != 0 {
		t.Fatal("satisfied predecessor obligations were not reclaimed")
	}
}

type routingPublishDuringReadClient struct {
	client.Client
	replacement  *computev1alpha1.FireboltEngine
	instanceName string
	injected     bool
}

func (c *routingPublishDuringReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok && !c.injected {
		c.injected = true
		if err := c.Create(ctx, c.replacement); err != nil {
			return err
		}
		if err := mutateRoutingState(ctx, c.Client, c.Client, c.replacement.Namespace, c.instanceName,
			func(state *routing.State) (bool, error) {
				return routing.SetRoute(state, c.replacement.Name, routing.Route{
					EngineUID: string(c.replacement.UID), Generation: 0, Enabled: true,
					Authority: "replacement.routing-test.svc.cluster.local:3473",
				})
			}); err != nil {
			return err
		}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestGatewayRoutingCleanupRevalidatesEngineUIDsAfterSnapshot(t *testing.T) {
	fixture := newRoutingTestFixture(t, false)
	if err := fixture.client.Delete(context.Background(), fixture.engine); err != nil {
		t.Fatal(err)
	}
	replacement := fixture.engine.DeepCopy()
	replacement.UID = "concurrent-replacement-uid"
	replacement.ResourceVersion = ""
	reader := &routingPublishDuringReadClient{Client: fixture.client, replacement: replacement, instanceName: fixture.instance.Name}
	r := FireboltInstanceReconciler{Client: fixture.client, APIReader: reader, Scheme: fixture.scheme}
	if err := r.reclaimDeletedEngineRoutes(context.Background(), fixture.instance); err != nil {
		t.Fatal(err)
	}
	_, state, err := readRoutingState(context.Background(), fixture.client, fixture.instance.Namespace, fixture.instance.Name)
	if err != nil {
		t.Fatal(err)
	}
	route := state.Routes[replacement.Name]
	if !reader.injected || !route.Enabled || route.EngineUID != string(replacement.UID) {
		t.Fatal("a stale Engine list withdrew the route published before the CAS snapshot")
	}
}
