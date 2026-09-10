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
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// DefaultRouteProbeURL is where the gateway's Envoy serves its loopback
// routing-probe listener: a GET here is forwarded to the dynamic-forward-proxy
// cluster at whatever authority the request's Host header names, so a 200 is
// the engine answering /health/ready through the exact path a released query
// will take. The operator renders that listener into the gateway Envoy config
// whenever the wake agent is enabled; the port is asserted against the
// operator's constant by a test in the controller package.
const DefaultRouteProbeURL = "http://127.0.0.1:9905/health/ready"

// EngineHTTPQueryPort mirrors the controller package's constant of the same
// name: the engine's http-query listener port, and therefore the port in the
// :authority the gateway's Lua filter rewrites every engine request to.
// Duplicated rather than imported because the agent must not pull the
// controller package (and its controller-runtime dependency graph) into the
// sidecar's code path; the two are asserted equal by a test in the controller
// package.
const EngineHTTPQueryPort = 3473

const (
	// routeProbeInterval is how often a woken hold re-asks Envoy whether the
	// engine is routable. Short, because every tick of it is added latency
	// on a query that already waited out a cold start.
	routeProbeInterval = 100 * time.Millisecond

	// routeProbeTimeout bounds a single probe attempt. It covers the probe
	// listener's own route timeout and sub-cluster init timeout (1s each),
	// so a hung attempt cannot pin a hold much past its deadline.
	routeProbeTimeout = time.Second
)

// EngineAuthority is the :authority the gateway routes an engine's queries
// to — the same string the Lua filter rewrites into every request, which is
// what keys Envoy's dynamic-forward-proxy sub-cluster for that engine. The
// routing probe must present exactly this authority or it would warm (and
// observe) a different sub-cluster than the one the released query uses.
// A test in the controller package pins it against the Lua rewrite.
func EngineAuthority(engine, namespace string) string {
	return fmt.Sprintf("%s%s.%s.svc.cluster.local:%d", engine, ServiceSuffix, namespace, EngineHTTPQueryPort)
}

// routeProber asks the pod's own Envoy whether it can currently route to an
// authority.
//
// Concurrent probes for the same authority are coalesced into one in-flight
// request. Holds poll independently, so without coalescing a wake herd of N
// parked queries would multiply into N probe streams against an engine that
// just cold-started — each one a fresh TCP+TLS connection, because the
// dynamic-forward-proxy cluster runs one request per connection.
type routeProber struct {
	// url is the probe listener's address, fixed at construction. Empty
	// means probing is disabled and every answer is "routable".
	url string

	mu     sync.Mutex
	flight map[string]*probeFlight
}

// probeFlight is one in-flight probe request; ok is valid once done closes.
type probeFlight struct {
	done chan struct{}
	ok   bool
}

func newRouteProber(url string) *routeProber {
	return &routeProber{url: url, flight: make(map[string]*probeFlight)}
}

// enabled reports whether a probe target is configured at all.
func (p *routeProber) enabled() bool {
	return p.url != ""
}

// probe reports whether Envoy answered 200 for the authority, joining an
// already in-flight probe for it instead of adding another. A joiner whose
// context ends first stops waiting and reports false; the flight itself
// carries on for the callers still parked on it.
func (p *routeProber) probe(ctx context.Context, authority string) bool {
	p.mu.Lock()
	if f, exists := p.flight[authority]; exists {
		p.mu.Unlock()
		select {
		case <-f.done:
			return f.ok
		case <-ctx.Done():
			return false
		}
	}
	f := &probeFlight{done: make(chan struct{})}
	p.flight[authority] = f
	p.mu.Unlock()

	f.ok = p.probeOnce(ctx, authority)

	p.mu.Lock()
	delete(p.flight, authority)
	p.mu.Unlock()
	close(f.done)
	return f.ok
}

// routeProbeClient never consults proxy environment variables: the probe
// target is a loopback listener, and a proxy without a loopback exclusion
// would fail every probe and silently turn each hold into a deadline wait.
var routeProbeClient = &http.Client{Transport: &http.Transport{Proxy: nil}}

// probeOnce performs one probe: GET the probe listener with the engine's
// authority as Host and accept only a 200 — which can only be the engine
// itself answering /health/ready, since Envoy synthesizes non-200s for every
// local failure (unresolved name, no healthy host, failed handshake). A
// non-200 is deliberately ambiguous the other way: the engine's own 503
// (draining, not yet healthy) also holds, so 200/non-200 is a conservative
// "releasable now" signal, not a pure statement about Envoy's cache.
// Resolving DNS in this process instead would prove nothing about Envoy's
// own DNS cache, transport configuration, or health view.
func (p *routeProber) probeOnce(ctx context.Context, authority string) bool {
	ctx, cancel := context.WithTimeout(ctx, routeProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, http.NoBody)
	if err != nil {
		return false
	}
	req.Host = authority
	resp, err := routeProbeClient.Do(req)
	if err != nil {
		return false
	}
	// The verdict is the status code alone; the body is drained (bounded)
	// only so the connection can be reused, and errors doing so change
	// nothing.
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == http.StatusOK
}

// routeWait is awaitRoutable's outcome.
type routeWait int

const (
	// routeWaitRoutable: Envoy answered 200 for the engine's authority (or
	// probing is disabled), so releasing the hold now routes.
	routeWaitRoutable routeWait = iota
	// routeWaitDeadline: the hold deadline fired first. The caller answers
	// exactly as it does when probing is disabled.
	routeWaitDeadline
	// routeWaitClientGone: the client hung up while waiting.
	routeWaitClientGone
)

// awaitRoutable blocks until Envoy itself reports the engine reachable, the
// hold deadline fires, or the client gives up.
//
// Endpoint readiness says the pods are there; it says nothing about whether
// THIS Envoy can route to them yet. The dynamic-forward-proxy sub-cluster
// for the engine's authority resolves DNS and runs active health checks on
// its own schedule, so at the readiness edge Envoy may still be a DNS
// refresh or a first health-check pass away from having a healthy host — and
// a request released into that window dies as a local 503 that no retry
// policy can see. Probing through Envoy's loopback routing-probe listener
// exercises the same sub-cluster the released request will use, so a 200
// here is the engine answering through the exact path the query takes.
//
// The probe gates only releases the agent already decided to make, and only
// inside the hold window: on deadline the caller answers exactly as it would
// have without the probe, and an empty RouteProbeURL disables the gate
// entirely (the escape hatch when no probe listener is serving).
func (a *Agent) awaitRoutable(ctx context.Context, engine string, deadline <-chan time.Time) routeWait {
	if !a.prober.enabled() {
		return routeWaitRoutable
	}
	authority := EngineAuthority(engine, a.cfg.Namespace)
	ticker := time.NewTicker(routeProbeInterval)
	defer ticker.Stop()
	for {
		if a.prober.probe(ctx, authority) {
			return routeWaitRoutable
		}
		select {
		case <-ctx.Done():
			return routeWaitClientGone
		case <-deadline:
			return routeWaitDeadline
		case <-ticker.C:
		}
	}
}
