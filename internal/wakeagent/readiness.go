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
	"strings"
	"sync"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// ServiceSuffix mirrors controller.SuffixService. Duplicated rather than
// imported because the agent must not pull the controller package (and its
// controller-runtime dependency graph) into the sidecar's code path; the
// two are asserted equal by a test in the controller package.
const ServiceSuffix = "-service"

// serviceNameLabel is the well-known label every EndpointSlice carries,
// naming the Service it backs. Engine Services are `<engine>-service`, so
// this label is how the agent maps a slice back to an engine name.
const serviceNameLabel = "kubernetes.io/service-name"

// readinessTracker answers one question: does engine <name> currently have
// at least one ready endpoint?
//
// It is backed by an EndpointSlice informer rather than by DNS lookups
// because the engine Service is headless with PublishNotReadyAddresses
// false — so the set of ready endpoints IS the set of A records kube-dns
// will serve. Watching the slices therefore observes exactly the condition
// that makes a held request routable, with no polling and no DNS caching
// in between.
type readinessTracker struct {
	mu sync.RWMutex
	// ready holds the engines with >= 1 ready endpoint. Presence in the
	// map is the whole signal; the value is unused.
	ready map[string]struct{}
	// waiters holds one channel per engine that some in-flight hold is
	// blocked on. Closed (and removed) when the engine becomes ready, so
	// every hold for that engine wakes at once.
	waiters map[string]chan struct{}
	// waiterRefs counts the holds currently parked on each channel.
	//
	// Without it the map grows forever: engine names come from an
	// untrusted header and are never checked against engines that exist,
	// so a client that opens a request for a random name and hangs up
	// leaks an entry per request. The hold cap bounds concurrency, not
	// rate, so it does not help here.
	waiterRefs map[string]int
	// synced reports whether the EndpointSlice cache has completed its
	// initial sync. Until it has, readiness answers are meaningless.
	synced bool
}

func newReadinessTracker() *readinessTracker {
	return &readinessTracker{
		ready:      make(map[string]struct{}),
		waiters:    make(map[string]chan struct{}),
		waiterRefs: make(map[string]int),
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
// Before the initial sync every engine looks not-ready, which would park
// every query for the full hold timeout. Callers treat unsynced as "let it
// through" instead — the same direction as every other degradation here.
func (t *readinessTracker) Synced() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.synced
}

// IsReady reports whether the engine currently has a ready endpoint.
func (t *readinessTracker) IsReady(engine string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.ready[engine]
	return ok
}

// WaitChan returns a channel that is closed once the engine has at least
// one ready endpoint. Callers must re-check IsReady after receiving, since
// an engine can flap back to not-ready between the close and the wakeup.
//
// Returns a closed channel immediately when the engine is already ready,
// which collapses the common "engine is up" path to a non-blocking select.
func (t *readinessTracker) WaitChan(engine string) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.ready[engine]; ok {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	if ch, ok := t.waiters[engine]; ok {
		t.waiterRefs[engine]++
		return ch
	}
	ch := make(chan struct{})
	t.waiters[engine] = ch
	t.waiterRefs[engine] = 1
	return ch
}

// DoneWaiting releases one reference taken by WaitChan, dropping the
// channel once the last waiter has gone. Safe to call for a waiter whose
// channel was already closed by setReady.
//
// The caller passes back the channel WaitChan gave it, and the release is
// keyed on that identity rather than on the engine name alone. Refcounts
// are per channel generation: once setReady retires a channel, a fresh
// hold can register a new one under the same engine name, and a stale
// release from the previous generation must not decrement — let alone
// delete — the new waiter's registration, or that hold would be stranded
// until its timeout.
func (t *readinessTracker) DoneWaiting(engine string, ch <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if current, ok := t.waiters[engine]; !ok || current != ch {
		return
	}
	n, ok := t.waiterRefs[engine]
	if !ok {
		return
	}
	if n <= 1 {
		delete(t.waiterRefs, engine)
		delete(t.waiters, engine)
		return
	}
	t.waiterRefs[engine] = n - 1
}

// setReady records the engine's readiness, releasing any waiters on the
// not-ready → ready edge.
func (t *readinessTracker) setReady(engine string, ready bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !ready {
		delete(t.ready, engine)
		return
	}
	t.ready[engine] = struct{}{}
	if ch, ok := t.waiters[engine]; ok {
		close(ch)
		delete(t.waiters, engine)
		delete(t.waiterRefs, engine)
	}
}

// engineFromSliceLabels extracts the engine name from an EndpointSlice's
// service-name label, or "" when the slice does not back an engine Service.
func engineFromSliceLabels(labels map[string]string) string {
	svc := labels[serviceNameLabel]
	if svc == "" || !strings.HasSuffix(svc, ServiceSuffix) {
		return ""
	}
	return strings.TrimSuffix(svc, ServiceSuffix)
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
// exists so recomputeEngine can be unit-tested without an API server.
type sliceStore interface {
	List() []interface{}
}

// recomputeEngine re-derives one engine's readiness from every slice
// currently in the store.
//
// Recomputing from the full set rather than applying the delta matters:
// a Service can be backed by several EndpointSlices, so "this slice went
// empty" does not imply "the engine has no endpoints". Deriving from the
// whole store keeps the answer correct regardless of how the API server
// chose to shard the endpoints.
func recomputeEngine(store sliceStore, engine string) bool {
	for _, obj := range store.List() {
		slice, ok := obj.(*discoveryv1.EndpointSlice)
		if !ok {
			continue
		}
		if engineFromSliceLabels(slice.Labels) != engine {
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
		engine := engineFromSliceLabels(slice.Labels)
		if engine == "" {
			return
		}
		tracker.setReady(engine, recomputeEngine(store, engine))
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
