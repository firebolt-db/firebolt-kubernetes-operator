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

package wakeagent

import (
	"context"
	"sync"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// serviceNameLabel identifies the exact generation Service backing a slice.
const serviceNameLabel = "kubernetes.io/service-name"

// readinessTracker answers one question: does a generation Service have
// at least one ready endpoint?
//
// Its EndpointSlice informer observes the ready endpoints that the headless
// Service publishes. DNS publication and Envoy's independently refreshed
// subcluster can lag that observation. True therefore means Kubernetes has
// a ready endpoint; it does not prove that Envoy can dispatch a request yet.
type readinessTracker struct {
	mu sync.RWMutex
	// ready holds the Services with >= 1 ready endpoint. Presence in the
	// map is the whole signal; the value is unused.
	ready    map[string]struct{}
	versions map[string]uint64
	// Versions are allocated across Services, so deleting a history entry
	// cannot make a recreated Service match an old successful route probe.
	nextVersion uint64
	// synced reports whether the EndpointSlice cache has completed its
	// initial sync. Until it has, readiness answers are meaningless.
	synced bool
}

func newReadinessTracker() *readinessTracker {
	return &readinessTracker{
		ready:    make(map[string]struct{}),
		versions: make(map[string]uint64),
	}
}

// MarkSynced records that the informer's initial cache sync completed.
func (t *readinessTracker) MarkSynced() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.synced = true
}

// Synced reports whether readiness answers can be trusted yet.
//
// Admission remains closed until this observation is usable.
func (t *readinessTracker) Synced() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.synced
}

// setReady records readiness and invalidates probes on transitions.
func (t *readinessTracker) setReady(engine string, ready bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, wasReady := t.ready[engine]
	if !ready {
		delete(t.ready, engine)
		delete(t.versions, engine)
		return
	}
	if !wasReady {
		t.nextVersion++
		t.versions[engine] = t.nextVersion
	}
	t.ready[engine] = struct{}{}
}

// sliceHasReadyEndpoint reports whether the slice contains at least one
// endpoint whose Ready condition is true. A nil Ready condition means
// "ready" per the EndpointSlice API contract.
func sliceHasReadyEndpoint(slice *discoveryv1.EndpointSlice) bool {
	for i := range slice.Endpoints {
		cond := slice.Endpoints[i].Conditions.Ready
		if cond == nil || *cond {
			return true
		}
	}
	return false
}

// sliceStore is the subset of the informer's indexer the tracker needs. It
// exists so recomputeService can be unit-tested without an API server.
type sliceStore interface {
	List() []interface{}
}

// recomputeService re-derives one Service's readiness from every slice
// currently in the store.
//
// Recomputing from the full set rather than applying the delta matters:
// a Service can be backed by several EndpointSlices, so "this slice went
// empty" does not imply "the engine has no endpoints". Deriving from the
// whole store keeps the answer correct regardless of how the API server
// chose to shard the endpoints.
func recomputeService(store sliceStore, service string) bool {
	for _, obj := range store.List() {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			continue
		}
		if slice.Labels[serviceNameLabel] != service {
			continue
		}
		if sliceHasReadyEndpoint(slice) {
			return true
		}
	}
	return false
}

// startReadinessInformer wires an EndpointSlice informer scoped to the
// agent's own namespace and keeps the tracker in sync with it. Blocks
// until the initial cache sync completes so the first held request is
// evaluated against real state rather than an empty map.
func startReadinessInformer(
	ctx context.Context,
	clientset kubernetes.Interface,
	namespace string,
	resync time.Duration,
	tracker *readinessTracker,
) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		clientset, resync, informers.WithNamespace(namespace),
	)
	informer := factory.Discovery().V1().EndpointSlices().Informer()
	store := informer.GetStore()

	onChange := func(obj interface{}) {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			// Tombstone from a delete the watch missed; unwrap it so the
			// engine still gets recomputed rather than going stale-ready.
			tombstone, isTombstone := obj.(cache.DeletedFinalStateUnknown)
			if !isTombstone {
				return
			}
			slice, ok = tombstone.Obj.(*discoveryv1.EndpointSlice)
			if !ok {
				return
			}
		}
		service := slice.Labels[serviceNameLabel]
		if service != "" {
			tracker.setReady("service:"+service, recomputeService(store, service))
		}
	}

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    onChange,
		UpdateFunc: func(_, newObj interface{}) { onChange(newObj) },
		DeleteFunc: onChange,
	}); err != nil {
		return err
	}

	factory.Start(ctx.Done())
	for _, synced := range factory.WaitForCacheSync(ctx.Done()) {
		if !synced {
			return context.Cause(ctx)
		}
	}
	return nil
}

// readinessVersion invalidates successful route probes whenever a Service loses
// and regains its ready endpoints, including between consecutive admissions.
func (t *readinessTracker) readinessVersion(service string) (bool, uint64) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ready := t.ready[service]
	return t.synced && ready, t.versions[service]
}
