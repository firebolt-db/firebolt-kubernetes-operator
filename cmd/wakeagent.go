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

package main

import (
	"errors"
	"flag"
	"fmt"

	"net"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/controller"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/wakeagent"
)

// wakeAgentSubcommand is the argv[1] value that switches this binary from
// the controller manager to the gateway's wake-agent sidecar.
//
// The agent ships inside the operator image rather than one of its own so
// the two cannot drift: the operator polls the agent's demand endpoint, so
// they share a wire contract, and a separately versioned agent image could
// be pinned to an old release and silently break wake detection. One image
// makes that impossible.
const wakeAgentSubcommand = "wake-agent"

// runWakeAgent parses the wake-agent flag set and runs the sidecar. args is
// everything after the subcommand itself.
func runWakeAgent(args []string) error {
	fs := flag.NewFlagSet(wakeAgentSubcommand, flag.ExitOnError)

	var (
		namespace       string
		holdPort        int
		demandPort      int
		envoyAdminURL   string
		envoyMemLimit   int64
		perHoldBytes    int64
		fallbackCap     int
		holdTimeout     time.Duration
		demandRetention time.Duration
	)

	fs.StringVar(&namespace, "namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace whose EndpointSlices are watched. Defaults to $POD_NAMESPACE "+
			"(set from the downward API by the operator-rendered pod template).")
	fs.IntVar(&holdPort, "hold-port", wakeagent.DefaultHoldPort,
		"Port for the hold endpoint Envoy calls. Bound to loopback only.")
	fs.IntVar(&demandPort, "demand-port", wakeagent.DefaultDemandPort,
		"Port for the demand endpoint the operator polls. Bound to all interfaces.")
	fs.StringVar(&envoyAdminURL, "envoy-admin-url", "",
		"Base URL of Envoy's admin listener on loopback, read for memory statistics "+
			"that tighten the hold cap under pressure. Empty disables the live reading.")
	fs.Int64Var(&envoyMemLimit, "envoy-memory-limit-bytes", envInt64("ENVOY_MEMORY_LIMIT_BYTES"),
		"Envoy's container memory limit in bytes, supplied via the downward API. "+
			"Zero falls back to --fallback-hold-cap.")
	fs.Int64Var(&perHoldBytes, "per-hold-bytes", 0,
		"Worst-case memory one held request pins, i.e. Envoy's "+
			"per_connection_buffer_limit_bytes. Used with the memory limit to derive the hold cap.")
	fs.IntVar(&fallbackCap, "fallback-hold-cap", wakeagent.DefaultFallbackHoldCap,
		"Maximum concurrent holds when no Envoy memory limit is available.")
	fs.DurationVar(&holdTimeout, "hold-timeout", wakeagent.DefaultHoldTimeout,
		"How long a request is held before the agent gives up and returns 503.")
	fs.DurationVar(&demandRetention, "demand-retention", wakeagent.DefaultDemandRetention,
		"How long an engine's demand stamp survives without being refreshed.")

	zapOpts := zap.Options{Development: false}
	zapOpts.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if namespace == "" {
		return errors.New("--namespace is empty and $POD_NAMESPACE is unset; " +
			"the agent must know which namespace's EndpointSlices to watch")
	}

	ctrl.SetLogger(zap.New(zapLoggerOpts(zapOpts)...))
	logger := ctrl.Log.WithName("wake-agent")
	logger.Info("starting wake agent",
		"version", version,
		"namespace", namespace,
		"holdPort", holdPort,
		"demandPort", demandPort,
		"envoyMemoryLimitBytes", envoyMemLimit,
		"perHoldBytes", perHoldBytes,
	)

	agent := wakeagent.New(wakeagent.Config{
		Namespace:             namespace,
		HoldAddr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(holdPort)),
		DemandAddr:            net.JoinHostPort("0.0.0.0", strconv.Itoa(demandPort)),
		EnvoyAdminURL:         envoyAdminURL,
		EnvoyMemoryLimitBytes: envoyMemLimit,
		PerHoldBytes:          perHoldBytes,
		FallbackCap:           fallbackCap,
		HoldTimeout:           holdTimeout,
		DemandRetention:       demandRetention,
	})

	return agent.Run(ctrl.SetupSignalHandler())
}

// envInt64 reads a base-10 int64 from the environment, returning 0 when the
// variable is unset or unparseable. A malformed value is treated as absent
// rather than fatal: the downward API can legitimately produce an empty
// string when the referenced container declares no memory limit, and the
// agent has a working fallback for that case.
func envInt64(key string) int64 {
	v := os.Getenv(key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// isWakeAgentInvocation reports whether argv selects the wake-agent
// subcommand rather than the controller manager.
func isWakeAgentInvocation(args []string) bool {
	return len(args) > 1 && args[1] == wakeAgentSubcommand
}

// setupWakeDemand registers the wake demand tracker with the manager and
// returns the source the engine reconciler reads.
//
// The tracker polls each gateway's read-only wake agent and caches the
// per-engine demand it reports; the engine reconciler reads that cache and
// does the scaling. Nothing in a gateway pod ever writes to the API, which
// is the point of the arrangement. Without an agent image there is no agent
// to poll, so wake stays off and auto-stop behaves exactly as it did before
// the feature existed.
func setupWakeDemand(
	mgr ctrl.Manager,
	wakeAgentImage string,
	watchNamespaces []string,
) (controller.WakeDemandSource, error) {
	if wakeAgentImage == "" {
		return controller.NoWakeDemand{}, nil
	}
	// The clientset is what ApiserverProxy scrape mode needs. Without it
	// the tracker errors on every scrape in exactly the clusters that mode
	// exists for — operator off the pod network, or a NetworkPolicy denying
	// pod-to-pod ingress — and does so at V(1), so wake would simply never
	// fire and nothing would say why.
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return nil, fmt.Errorf("building clientset for the wake demand tracker: %w", err)
	}
	tracker := controller.NewWakeDemandTracker(mgr.GetClient(), clientset, watchNamespaces)
	if err := mgr.Add(tracker); err != nil {
		return nil, err
	}
	return tracker, nil
}
