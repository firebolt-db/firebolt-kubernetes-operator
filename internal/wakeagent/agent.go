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

// Package wakeagent implements gateway request admission and wake-on-zero.
// The operator publishes routing assignments; the agent watches them, accounts
// for local Envoy requests, and reports fences and demand without API writes.
package wakeagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	extpb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Default ports. Local health and admission listeners bind loopback only.
// The demand and routing report listener binds all interfaces for the operator.
const (
	DefaultHoldPort    = 9902
	DefaultDemandPort  = 9903
	DefaultExtProcPort = 9904

	// DefaultHoldTimeout bounds how long a single request is parked before
	// the agent gives up and returns 503. Sized to cover an engine cold
	// start (image pull on a fresh node plus engine startup) without
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

// Config is the agent's runtime configuration, assembled from flags and
// the downward API by the wake-agent subcommand.
type Config struct {
	// Namespace is the namespace whose EndpointSlices the agent watches.
	// Always the agent's own: engine Services live alongside the gateway.
	Namespace     string
	InstanceName  string
	InstanceUID   string
	PodUID        string
	ExtProcAddr   string
	RouteProbeURL string

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
	if c.ExtProcAddr == "" {
		c.ExtProcAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(DefaultExtProcPort))
	}
	if c.RouteProbeURL == "" {
		c.RouteProbeURL = "http://127.0.0.1:9905/health/ready"
	}
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
	extpb.UnimplementedExternalProcessorServer
	routes    *routeLedger
	cfg       Config
	demand    *demandTracker
	readiness *readinessTracker
	capacity  *capacityLimiter
}

// New builds an Agent with its collaborators wired but nothing started.
func New(cfg Config) *Agent { //nolint:gocritic // Snapshot configuration so callers cannot mutate a running agent.
	cfg.applyDefaults()
	return &Agent{
		cfg:       cfg,
		routes:    newRouteLedger(cfg.PodUID, cfg.InstanceUID),
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

// Run starts the informers, HTTP observers and gRPC processor, blocking until ctx is
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

	// Bind listeners while caches synchronize; admission remains closed until
	// registration and generation readiness have been observed.
	go func() {
		logger.Info("syncing EndpointSlice cache", "namespace", a.cfg.Namespace)
		if err := startReadinessInformer(ctx, clientset, a.cfg.Namespace, 10*time.Minute, a.readiness); err != nil {
			logger.Error(err, "EndpointSlice informer never synced; "+
				"gateway admission remains unavailable. "+
				"Check that the gateway ServiceAccount can watch endpointslices.")
			return
		}
		a.readiness.MarkSynced()
		logger.Info("EndpointSlice cache synced; wake-on-zero active")
	}()

	go func() {
		if err := a.startRoutingInformer(ctx, clientset); err != nil {
			logger.Error(err, "routing informer failed")
		}
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

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", a.cfg.ExtProcAddr)
	if err != nil {
		return fmt.Errorf("routing processor listener: %w", err)
	}
	processor := grpc.NewServer()
	extpb.RegisterExternalProcessorServer(processor, a)
	defer processor.Stop()
	errCh := make(chan error, 3)
	go func() { errCh <- processor.Serve(listener) }()
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (a *Agent) demandMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/routing", a.handleRouting)
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !a.RoutingReport().Registered || !a.readiness.Synced() {
			http.Error(w, "routing session or endpoint cache is not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/demand", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(a.demand.Render()))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return mux
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
