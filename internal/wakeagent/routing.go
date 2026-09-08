// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package wakeagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// routeLedger serializes publication of fences with admission and reports. A
// permit remains charged to its original epoch even after a new route is applied.
type routeLedger struct {
	probed                         map[string]uint64
	mu                             sync.Mutex
	podUID, sessionID, instanceUID string
	revision                       uint64
	registered                     bool
	routes                         map[string]routing.Route
	outstanding                    map[string]routing.Count
	changed                        chan struct{}
}

func newRouteLedger(podUID, instanceUID string) *routeLedger {
	var boot [32]byte
	if _, err := rand.Read(boot[:]); err != nil {
		panic(fmt.Sprintf("generate gateway session: %v", err))
	}
	return &routeLedger{podUID: podUID, sessionID: hex.EncodeToString(boot[:]), instanceUID: instanceUID,
		routes: map[string]routing.Route{}, probed: map[string]uint64{}, outstanding: map[string]routing.Count{}, changed: make(chan struct{})}
}

// ApplyRoutingState installs a validated operator assignment atomically. Stale
// snapshots cannot reopen a fenced epoch. Failed validation retains the last
// authorized table; retirement then waits for this session's acknowledgement.
func (a *Agent) ApplyRoutingState(state routing.State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	l := a.routes
	l.mu.Lock()
	defer l.mu.Unlock()
	if state.InstanceUID != l.instanceUID {
		return errors.New("routing instance UID does not match")
	}
	if state.Revision <= l.revision {
		return nil
	}
	l.registered = l.podUID != "" && state.Sessions[l.podUID].ID == l.sessionID
	l.routes = make(map[string]routing.Route, len(state.Routes))
	probed := make(map[string]uint64, len(state.Routes))
	for engine, route := range state.Routes {
		l.routes[engine] = route
		if version, ok := l.probed[route.Key()]; ok {
			probed[route.Key()] = version
		}
	}
	l.probed = probed
	l.revision = state.Revision
	close(l.changed)
	l.changed = make(chan struct{})
	return nil
}

// RoutingReport is a consistent fence-and-permit snapshot, including grants
// whose stream transport failed and therefore cannot safely be released.
func (a *Agent) RoutingReport() routing.Report {
	l := a.routes
	l.mu.Lock()
	defer l.mu.Unlock()
	report := routing.Report{PodUID: l.podUID, SessionID: l.sessionID, AppliedRevision: l.revision,
		Registered: l.registered, Outstanding: make(map[string]routing.Count, len(l.outstanding))}
	for key, count := range l.outstanding {
		report.Outstanding[key] = count
	}
	return report
}

func (a *Agent) finishPermit(key string, clean bool) {
	l := a.routes
	l.mu.Lock()
	defer l.mu.Unlock()
	count := l.outstanding[key]
	count.Active--
	if !clean {
		count.Unknown++
	}
	if count.Active == 0 && count.Unknown == 0 {
		delete(l.outstanding, key)
	} else {
		l.outstanding[key] = count
	}
}

// acquireRoute records the permit before returning its immutable destination.
// The readiness observation aids availability; only the ledger fence establishes
// withdrawal safety. An EndpointSlice observation is never a withdrawal ACK.
func (a *Agent) acquireRoute(ctx context.Context, engine string) (routing.Route, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.HoldTimeout)
	defer cancel()
	held := false
	defer func() {
		if held {
			a.demand.ReleaseHold(engine)
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		l := a.routes
		l.mu.Lock()
		route, exists := l.routes[engine]
		endpointReady, endpointVersion := a.readiness.readinessVersion(serviceReadinessKey(route.Authority))
		ready := l.registered && exists && route.Enabled && endpointReady
		probedVersion, probed := l.probed[route.Key()]
		if ready && probed && probedVersion == endpointVersion {
			if err := ctx.Err(); err != nil {
				l.mu.Unlock()
				return routing.Route{}, err
			}
			count := l.outstanding[route.Key()]
			count.Active++
			l.outstanding[route.Key()] = count
			l.mu.Unlock()
			return route, nil
		}
		if ready {
			l.mu.Unlock()
			if !held {
				a.demand.Stamp(engine)
				if !a.demand.AcquireHold(engine, a.capacity.Cap()) {
					return routing.Route{}, errors.New("gateway wake capacity reached")
				}
				held = true
			}
			if a.probeRoute(ctx, route.Authority) {
				l.mu.Lock()
				current, ok := l.routes[engine]
				stillReady, version := a.readiness.readinessVersion(serviceReadinessKey(route.Authority))
				if l.registered && ok && current == route && stillReady && version == endpointVersion {
					l.probed[route.Key()] = version
				}
				l.mu.Unlock()
				continue
			}
			l.mu.Lock()
		}
		changed := l.changed
		l.mu.Unlock()
		a.demand.Stamp(engine)
		if !held {
			if !a.demand.AcquireHold(engine, a.capacity.Cap()) {
				return routing.Route{}, errors.New("gateway wake capacity reached")
			}
			held = true
		}
		select {
		case <-ctx.Done():
			return routing.Route{}, ctx.Err()
		case <-changed:
		case <-ticker.C:
		}
	}
}

func serviceReadinessKey(authority string) string {
	host, _, err := net.SplitHostPort(authority)
	if err != nil {
		host = authority
	}
	return "service:" + strings.SplitN(host, ".", 2)[0]
}

func (a *Agent) handleRouting(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(a.RoutingReport())
}

func (a *Agent) startRoutingInformer(ctx context.Context, clientset kubernetes.Interface) error {
	name := routing.ConfigMapName(a.cfg.InstanceName)
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, 10*time.Minute,
		informers.WithNamespace(a.cfg.Namespace), informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
		}))
	informer := factory.Core().V1().ConfigMaps().Informer()
	apply := func(obj interface{}) {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok || cm.Name != name {
			return
		}
		state, err := routing.Decode([]byte(cm.Data[routing.DataKey]))
		if err == nil {
			err = a.ApplyRoutingState(state)
		}
		if err != nil {
			log.FromContext(ctx).Error(err, "rejecting gateway routing assignment")
		}
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: apply, UpdateFunc: func(_, obj interface{}) { apply(obj) }}); err != nil {
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

// probeRoute warms the same Envoy DFP subcluster that serves queries. Checking
// DNS in this process would not establish that Envoy can use that destination.
func (a *Agent) probeRoute(ctx context.Context, authority string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.RouteProbeURL, http.NoBody)
	if err != nil {
		return false
	}
	request.Host = authority
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode == http.StatusOK
}
