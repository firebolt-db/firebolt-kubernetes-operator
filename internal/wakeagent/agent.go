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

// Package wakeagent implements the gateway's wake-on-zero sidecar.
//
// The agent holds a query that arrived for an auto-stopped engine while the
// operator scales that engine back up, and reports the demand that makes
// the operator do so. It deliberately holds no write credential: its entire
// Kubernetes grant is get/list/watch on EndpointSlices. The operator is the
// only component that writes, which is what keeps a compromise of the
// gateway — the one component terminating untrusted traffic — from reaching
// the FireboltEngine API at all.
//
// Request flow:
//
//  1. Envoy's Lua filter calls /hold?engine=X on loopback before routing.
//  2. The agent stamps demand for X and, if X has no ready endpoints,
//     parks the request.
//  3. The operator polls /demand, sees fresh demand against an engine it
//     knows is at zero replicas, and scales it.
//  4. The agent's EndpointSlice watch observes endpoints appear and
//     releases the parked request; Envoy routes it.
//
// Step 4 is exact rather than approximate: the engine Service is headless
// with PublishNotReadyAddresses false, so the endpoints the agent watches
// are precisely the A records kube-dns will serve to Envoy's resolver.
package wakeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Default ports. The hold listener is bound to loopback only — it is
// reachable from Envoy in the same pod and from nothing else. The demand
// listener binds all interfaces because the operator polls it across the
// pod network.
const (
	DefaultHoldPort   = 9902
	DefaultDemandPort = 9903

	// DefaultHoldTimeout bounds how long a single request is parked before
	// the agent gives up and returns 503. Sized to cover an engine cold
	// start (image pull on a fresh node plus packdb startup) without
	// pinning a connection indefinitely when the engine never arrives.
	DefaultHoldTimeout = 120 * time.Second

	// DefaultDemandRetention bounds how long an engine's demand stamp
	// survives without refresh. Longer than the operator's wake TTL so the
	// operator, not the agent, decides when a stamp is too old to act on.
	DefaultDemandRetention = 10 * time.Minute

	// DefaultFallbackHoldCap applies when Envoy's memory limit was not
	// exposed through the downward API, so the memory-derived cap cannot
	// be computed.
	DefaultFallbackHoldCap = 256

	// envoyStatsRefreshInterval is how often the live allocation reading
	// behind the dynamic hold cap is refreshed.
	envoyStatsRefreshInterval = 5 * time.Second
)

// DecisionHeader is set on every response the agent itself produces, and is
// how Envoy's Lua filter tells "the agent answered" from "the agent could
// not be reached".
//
// That distinction cannot be made from the status code alone. Envoy's Lua
// httpCall does NOT return nil when the call fails at the transport layer:
// StreamHandleWrapper::onFailure synthesizes a 503 and delivers it through
// the success path. So a crashed or absent agent is indistinguishable from
// an agent that deliberately shed the request — and treating both as "do
// not route" turns any agent outage into a total gateway outage, which is
// the opposite of the intended failure mode.
//
// Envoy's synthesized response carries no headers of ours, so requiring
// this one before honoring a non-200 makes the filter fail open by
// construction.
const DecisionHeader = "x-firebolt-wake-decision"

// Values for DecisionHeader.
const (
	DecisionReady    = "ready"    // engine was already routable
	DecisionReleased = "released" // held, then released when endpoints appeared
	DecisionShed     = "shed"     // hold capacity reached
	DecisionTimeout  = "timeout"  // engine did not arrive within the hold window
	DecisionRejected = "rejected" // malformed engine name
	DecisionUnsynced = "unsynced" // EndpointSlice cache not yet usable
)

// Config is the agent's runtime configuration, assembled from flags and
// the downward API by the wake-agent subcommand.
type Config struct {
	// Namespace is the namespace whose EndpointSlices the agent watches.
	// Always the agent's own: engine Services live alongside the gateway.
	Namespace string

	HoldAddr   string
	DemandAddr string

	// EnvoyAdminURL is the base URL of Envoy's admin listener on loopback,
	// used only to read memory statistics for the dynamic hold cap. Empty
	// disables the live reading and falls back to the static budget.
	EnvoyAdminURL string

	// EnvoyMemoryLimitBytes is Envoy's container memory limit, supplied via
	// the downward API's resourceFieldRef against the envoy container.
	// Zero means no limit was set, and the cap falls back to FallbackCap.
	EnvoyMemoryLimitBytes int64

	// PerHoldBytes is the worst-case memory a single held request pins,
	// i.e. Envoy's per_connection_buffer_limit_bytes.
	PerHoldBytes int64

	FallbackCap     int
	HoldTimeout     time.Duration
	DemandRetention time.Duration
}

func (c *Config) applyDefaults() {
	if c.HoldAddr == "" {
		c.HoldAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(DefaultHoldPort))
	}
	if c.DemandAddr == "" {
		c.DemandAddr = net.JoinHostPort("0.0.0.0", strconv.Itoa(DefaultDemandPort))
	}
	if c.HoldTimeout == 0 {
		c.HoldTimeout = DefaultHoldTimeout
	}
	if c.DemandRetention == 0 {
		c.DemandRetention = DefaultDemandRetention
	}
	if c.FallbackCap == 0 {
		c.FallbackCap = DefaultFallbackHoldCap
	}
}

// Agent is the assembled sidecar.
type Agent struct {
	cfg       Config
	demand    *demandTracker
	readiness *readinessTracker
	capacity  *capacityLimiter
}

// New builds an Agent with its collaborators wired but nothing started.
func New(cfg Config) *Agent {
	cfg.applyDefaults()
	return &Agent{
		cfg:       cfg,
		demand:    newDemandTracker(cfg.DemandRetention, time.Now),
		readiness: newReadinessTracker(),
		capacity: newCapacityLimiter(
			cfg.EnvoyMemoryLimitBytes,
			cfg.PerHoldBytes,
			cfg.FallbackCap,
			cfg.EnvoyAdminURL,
		),
	}
}

// Run starts the informer and both HTTP servers, blocking until ctx is
// canceled or a server fails.
func (a *Agent) Run(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("wake-agent")

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("building in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("building clientset: %w", err)
	}

	// Started in the background, and deliberately not waited on. A missing
	// EndpointSlice grant makes the reflector retry forever without ever
	// syncing; blocking here would leave the listeners unbound, the
	// readiness probe failing, and the whole gateway pod out of its
	// Service — turning a lost wake capability into a lost data path.
	go func() {
		logger.Info("syncing EndpointSlice cache", "namespace", a.cfg.Namespace)
		if err := startReadinessInformer(ctx, clientset, a.cfg.Namespace, 10*time.Minute, a.readiness); err != nil {
			logger.Error(err, "EndpointSlice informer never synced; "+
				"wake-on-zero is disabled and queries will route straight through. "+
				"Check that the gateway ServiceAccount can watch endpointslices.")
			return
		}
		a.readiness.MarkSynced()
		logger.Info("EndpointSlice cache synced; wake-on-zero active")
	}()

	go a.refreshEnvoyStats(ctx)
	go a.demand.pruneLoop(ctx, a.cfg.DemandRetention)

	holdSrv := &http.Server{
		Addr:              a.cfg.HoldAddr,
		Handler:           a.holdMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	demandSrv := &http.Server{
		Addr:              a.cfg.DemandAddr,
		Handler:           a.demandMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	serve := func(name string, srv *http.Server) {
		logger.Info("listening", "server", name, "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s server: %w", name, err)
			return
		}
		errCh <- nil
	}
	go serve("hold", holdSrv)
	go serve("demand", demandSrv)

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			shutdown(holdSrv, demandSrv)
			return err
		}
	}
	shutdown(holdSrv, demandSrv)
	return nil
}

func shutdown(servers ...*http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
}

// refreshEnvoyStats keeps the dynamic hold cap's live input current.
func (a *Agent) refreshEnvoyStats(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("wake-agent")
	ticker := time.NewTicker(envoyStatsRefreshInterval)
	defer ticker.Stop()
	for {
		if err := a.capacity.Refresh(ctx); err != nil {
			logger.V(1).Info("refreshing Envoy memory stats failed, keeping previous reading",
				"error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *Agent) holdMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/hold", a.handleHold)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (a *Agent) demandMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/demand", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(a.demand.Render()))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

// handleHold is the request-path hook Envoy calls before routing.
//
// Responses are deliberately coarse: 200 means "route it", anything else
// means "do not". Envoy is configured to fail open, so an unreachable or
// erroring agent lets the query through to the same 503 it would have got
// without the wake feature at all — the agent is a convenience, not a
// security control, and must never be able to take the data path down.
func (a *Agent) handleHold(w http.ResponseWriter, r *http.Request) {
	engine := r.URL.Query().Get("engine")
	if !isValidEngineName(engine) {
		w.Header().Set(DecisionHeader, DecisionRejected)
		http.Error(w, "invalid engine name", http.StatusBadRequest)
		return
	}

	// Before the initial cache sync every engine looks not-ready, which
	// would park every query for the full hold timeout. An agent that
	// cannot watch EndpointSlices — a missing RBAC grant is the likely
	// cause — must degrade to "route it" rather than to "hold everything".
	if !a.readiness.Synced() {
		w.Header().Set(DecisionHeader, DecisionUnsynced)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Fast path: the engine is up, so there is nothing to wake and nothing
	// to record. This is every request in steady state, so it must not
	// touch the demand map or take a write lock.
	if a.readiness.IsReady(engine) {
		w.Header().Set(DecisionHeader, DecisionReady)
		w.WriteHeader(http.StatusOK)
		return
	}

	// Stamp before consulting the cap. A shed request still proves someone
	// asked for this engine, and that is the signal the operator acts on.
	a.demand.Stamp(engine)

	if !a.demand.AcquireHold(engine, a.capacity.Cap()) {
		w.Header().Set(DecisionHeader, DecisionShed)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "gateway wake capacity reached", http.StatusServiceUnavailable)
		return
	}
	defer a.demand.ReleaseHold(engine)

	ready := a.readiness.WaitChan(engine)
	defer a.readiness.DoneWaiting(engine)
	timer := time.NewTimer(a.cfg.HoldTimeout)
	defer timer.Stop()

	select {
	case <-ready:
		// Re-check rather than trusting the edge: an engine can flap back
		// to not-ready between the close and this wakeup, and releasing
		// into a name that no longer resolves just converts the wait into
		// a confusing upstream error.
		if !a.readiness.IsReady(engine) {
			w.Header().Set(DecisionHeader, DecisionTimeout)
			w.Header().Set("Retry-After", "5")
			http.Error(w, "engine became ready then went away", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set(DecisionHeader, DecisionReleased)
		w.WriteHeader(http.StatusOK)
	case <-timer.C:
		w.Header().Set(DecisionHeader, DecisionTimeout)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "engine did not become ready", http.StatusServiceUnavailable)
	case <-r.Context().Done():
		// Client hung up. Nothing to write; the demand stamp already
		// recorded that they asked, which is why the timestamp rather
		// than a pending-count is what the operator reads.
	}
}

// isValidEngineName mirrors the Lua filter's is_valid_engine: a single
// RFC 1123 DNS label, lowercase alphanumerics and hyphens only, no dots,
// max 63 characters, no leading or trailing hyphen. Revalidated here
// rather than trusted from Envoy so the agent's contract holds on its own
// terms — it is an independent process with its own listener.
func isValidEngineName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	if s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '-':
		default:
			return false
		}
	}
	return true
}
