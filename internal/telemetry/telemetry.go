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

// Package telemetry sends one anonymous, aggregate usage event to Scarf per
// day. The payload contains coarse deployment information and no names, stable
// identifiers, query data, schemas, or configuration. As with any network
// request, the source IP is visible to Scarf; Scarf may use it to infer the
// company and does not store it.
package telemetry

import (
	"context"
	"math/rand/v2"
	"runtime"
	"sort"
	"strings"
	"time"

	dockerref "github.com/distribution/reference"
	"github.com/go-logr/logr"
	"github.com/scarf-sh/scarf-go/scarf"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/config/images"
)

const (
	// DefaultEndpoint is the Firebolt-owned Scarf Event Collection endpoint.
	DefaultEndpoint = "https://telemetry.firebolt.io/firebolt-operator"

	reportInterval   = 24 * time.Hour
	sendTimeout      = 3 * time.Second
	maxStartupJitter = 5 * time.Minute
)

type eventLogger interface {
	Enabled() bool
	LogEvent(map[string]any) error
}

// Reporter is a leader-only controller-runtime runnable. Running on the
// elected leader ensures an HA operator reports once per cluster rather than
// once per operator replica.
type Reporter struct {
	Client          client.Client
	Config          *rest.Config
	OperatorVersion string
	Endpoint        string
	Enabled         bool

	// Interval overrides the daily interval in tests.
	Interval time.Duration
	logger   eventLogger
}

var _ manager.LeaderElectionRunnable = (*Reporter)(nil)

// NeedLeaderElection makes the reporter run only on the elected manager.
func (*Reporter) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable and blocks until ctx is canceled.
func (r *Reporter) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("telemetry")

	if !r.Enabled {
		log.Info("anonymous usage telemetry is disabled; not reporting")
		return nil
	}

	if r.logger == nil {
		endpoint := r.Endpoint
		if endpoint == "" {
			endpoint = DefaultEndpoint
		}
		r.logger = scarf.NewScarfEventLogger(endpoint, sendTimeout)
	}
	if !r.logger.Enabled() {
		log.Info("anonymous usage telemetry is disabled by environment; not reporting")
		return nil
	}

	log.Info("anonymous usage telemetry is enabled; disable with --telemetry=false, DO_NOT_TRACK=1, or SCARF_NO_ANALYTICS=1")

	interval := r.Interval
	if interval <= 0 {
		interval = reportInterval
	}

	// Wait a full reporting interval before the first event. This preserves the
	// at-most-once-per-day guarantee across pod restarts and leader failovers
	// without storing an identifier or last-send marker in the cluster. Jitter
	// spreads otherwise simultaneous reports and does not need cryptographic
	// randomness.
	initialDelay := interval + time.Duration(rand.Int64N(int64(maxStartupJitter))) //nolint:gosec // non-cryptographic jitter
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
	}
	r.report(ctx, log)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.report(ctx, log)
		}
	}
}

func (r *Reporter) report(ctx context.Context, log logr.Logger) {
	payload, err := r.collect(ctx)
	if err != nil {
		log.V(1).Info("telemetry collection failed; skipping report", "error", err)
		return
	}
	if err := r.logger.LogEvent(payload); err != nil {
		log.V(1).Info("telemetry send failed; ignoring", "error", err)
	}
}

func (r *Reporter) collect(ctx context.Context) (map[string]any, error) {
	var instances computev1alpha1.FireboltInstanceList
	if err := r.Client.List(ctx, &instances); err != nil {
		return nil, err
	}

	var engines computev1alpha1.FireboltEngineList
	if err := r.Client.List(ctx, &engines); err != nil {
		return nil, err
	}

	var classList computev1alpha1.FireboltEngineClassList
	if err := r.Client.List(ctx, &classList); err != nil {
		return nil, err
	}
	classes := make(map[string]*computev1alpha1.FireboltEngineClass, len(classList.Items))
	for i := range classList.Items {
		class := &classList.Items[i]
		classes[class.Namespace+"/"+class.Name] = class
	}

	payload := map[string]any{
		"event":                 "operator_daily_summary",
		"operator_version":      r.OperatorVersion,
		"os":                    runtime.GOOS,
		"arch":                  runtime.GOARCH,
		"instance_count_bucket": countBucket(len(instances.Items)),
		"engine_count_bucket":   countBucket(len(engines.Items)),
		"engine_versions":       engineVersions(engines.Items, classes),
		"replica_size_buckets":  replicaSizeBuckets(engines.Items),
	}
	if version := k8sMinor(r.Config); version != "" {
		payload["k8s_minor"] = version
	}
	return payload, nil
}

func countBucket(count int) string {
	switch {
	case count <= 0:
		return "0"
	case count == 1:
		return "1"
	case count <= 4:
		return "2-4"
	case count <= 16:
		return "5-16"
	default:
		return "17+"
	}
}

var bucketOrder = []string{"0", "1", "2-4", "5-16", "17+"}

func engineVersions(engines []computev1alpha1.FireboltEngine, classes map[string]*computev1alpha1.FireboltEngineClass) string {
	versions := make(map[string]struct{})
	for i := range engines {
		if tag := engineTag(&engines[i], classes); tag != "" {
			versions[tag] = struct{}{}
		}
	}

	result := make([]string, 0, len(versions))
	for version := range versions {
		result = append(result, version)
	}
	sort.Strings(result)
	return strings.Join(result, ",")
}

// engineTag mirrors the controller's effective engine image precedence:
// operator default, then class template, then engine template.
func engineTag(engine *computev1alpha1.FireboltEngine, classes map[string]*computev1alpha1.FireboltEngineClass) string {
	tag := images.EngineTag
	if ref := engine.Spec.EngineClassRef; ref != nil && *ref != "" {
		if class := classes[engine.Namespace+"/"+*ref]; class != nil {
			if container := computev1alpha1.EngineContainerInTemplate(&class.Spec.Template); container != nil && container.Image != "" {
				tag = tagOf(container.Image)
			}
		}
	}
	if container := computev1alpha1.EngineContainerInTemplate(engine.Spec.Template); container != nil && container.Image != "" {
		tag = tagOf(container.Image)
	}
	return tag
}

func tagOf(image string) string {
	named, err := dockerref.ParseNormalizedNamed(image)
	if err != nil {
		return ""
	}
	if tagged, ok := named.(dockerref.Tagged); ok {
		return tagged.Tag()
	}
	return ""
}

func replicaSizeBuckets(engines []computev1alpha1.FireboltEngine) string {
	buckets := make(map[string]struct{})
	for i := range engines {
		buckets[countBucket(int(engines[i].Spec.Replicas))] = struct{}{}
	}

	result := make([]string, 0, len(buckets))
	for _, bucket := range bucketOrder {
		if _, ok := buckets[bucket]; ok {
			result = append(result, bucket)
		}
	}
	return strings.Join(result, ",")
}

func k8sMinor(config *rest.Config) string {
	if config == nil {
		return ""
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return ""
	}
	version, err := discoveryClient.ServerVersion()
	if err != nil {
		return ""
	}
	major := strings.TrimFunc(version.Major, notDigit)
	minor := strings.TrimFunc(version.Minor, notDigit)
	if major == "" || minor == "" {
		return ""
	}
	return major + "." + minor
}

func notDigit(r rune) bool { return r < '0' || r > '9' }
