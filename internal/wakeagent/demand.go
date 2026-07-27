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
	"slices"
	"strings"
	"sync"
	"time"
)

// Metric names exposed on the agent's demand endpoint. The operator parses
// MetricLastDemand; the other two are for humans and dashboards.
const (
	// MetricLastDemand is a per-engine gauge carrying the Unix timestamp
	// (seconds, float) of the most recent request the gateway received for
	// that engine while it had no ready endpoints.
	MetricLastDemand = "firebolt_gateway_wake_last_demand_timestamp_seconds"
	// MetricPendingHolds is a per-engine gauge of requests currently held
	// waiting for the engine to come up.
	MetricPendingHolds = "firebolt_gateway_wake_pending_holds"
	// MetricSheddedTotal counts requests rejected because the hold cap was
	// already reached.
	MetricSheddedTotal = "firebolt_gateway_wake_shed_total"
)

// demandTracker records, per engine, when the gateway last saw a request
// for a stopped engine.
//
// A timestamp rather than a pending-request gauge is deliberate. A gauge of
// currently-held requests drops to zero the moment the client hangs up, so
// an impatient caller that disconnects between the hold starting and the
// operator's next poll would produce no observed demand at all and the
// engine would never wake — making wake dependent on poll timing. The
// timestamp survives the client leaving, and it is stamped on arrival
// (before the hold cap is consulted), so a shed request still registers
// demand.
type demandTracker struct {
	mu      sync.RWMutex
	last    map[string]time.Time
	pending map[string]int
	shed    map[string]int64
	// retention bounds how long an engine's entry survives without being
	// refreshed. Entries are dropped on read so a gateway that has seen
	// thousands of engines over its lifetime does not grow without bound,
	// and so the operator never acts on demand older than it would honor
	// anyway.
	retention time.Duration
	now       func() time.Time
}

func newDemandTracker(retention time.Duration, now func() time.Time) *demandTracker {
	if now == nil {
		now = time.Now
	}
	return &demandTracker{
		last:      make(map[string]time.Time),
		pending:   make(map[string]int),
		shed:      make(map[string]int64),
		retention: retention,
		now:       now,
	}
}

// Stamp records demand for the engine as of now.
func (d *demandTracker) Stamp(engine string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last[engine] = d.now()
}

// AcquireHold registers an in-flight hold when the agent is below its cap.
// Returns false when the cap is already reached, in which case the caller
// must shed the request — the demand stamp has already been recorded.
//
// A negative limit means unlimited. Zero means shed everything, and that
// distinction matters: the capacity limiter returns zero precisely when
// Envoy is under memory pressure, so treating it as "no limit" would
// disable admission control exactly when it is needed and let a flood of
// parked requests amplify the pressure that triggered it.
func (d *demandTracker) AcquireHold(engine string, limit int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	total := 0
	for _, n := range d.pending {
		total += n
	}
	if limit >= 0 && total >= limit {
		d.shed[engine]++
		return false
	}
	d.pending[engine]++
	return true
}

// ReleaseHold drops an in-flight hold registered by AcquireHold.
func (d *demandTracker) ReleaseHold(engine string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pending[engine] <= 1 {
		delete(d.pending, engine)
		return
	}
	d.pending[engine]--
}

// PendingTotal is the number of held requests across all engines.
func (d *demandTracker) PendingTotal() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	total := 0
	for _, n := range d.pending {
		total += n
	}
	return total
}

// snapshot is one engine's row in the rendered metric output.
type snapshot struct {
	engine  string
	last    time.Time
	pending int
	shed    int64
}

// Snapshot returns the current per-engine state, evicting entries whose
// last demand is older than the retention window.
func (d *demandTracker) Snapshot() []snapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	cutoff := d.now().Add(-d.retention)
	out := make([]snapshot, 0, len(d.last))
	for engine, ts := range d.last {
		if ts.Before(cutoff) && d.pending[engine] == 0 {
			delete(d.last, engine)
			delete(d.shed, engine)
			continue
		}
		out = append(out, snapshot{
			engine:  engine,
			last:    ts,
			pending: d.pending[engine],
			shed:    d.shed[engine],
		})
	}
	slices.SortFunc(out, func(a, b snapshot) int { return strings.Compare(a.engine, b.engine) })
	return out
}

// pruneLoop evicts stale entries on a timer.
//
// Eviction used to happen only inside Snapshot, i.e. only when the operator
// scraped — and the operator skips namespaces with no stopped engine, so a
// gateway in a healthy namespace was never scraped and its maps grew without
// bound on an attacker-supplied key space.
func (d *demandTracker) pruneLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.Snapshot()
		}
	}
}

// Render writes the Prometheus text-format exposition the operator polls.
// Hand-rolled rather than routed through prometheus/client_golang because
// the label set is unbounded (one series per engine ever seen) and needs
// the retention-based eviction above, which the registry's collector model
// does not give for free.
func (d *demandTracker) Render() string {
	var b strings.Builder
	b.WriteString("# HELP " + MetricLastDemand +
		" Unix timestamp of the most recent gateway request for a stopped engine.\n")
	b.WriteString("# TYPE " + MetricLastDemand + " gauge\n")
	rows := d.Snapshot()
	for _, r := range rows {
		fmt.Fprintf(&b, "%s{engine=%q} %d\n", MetricLastDemand, r.engine, r.last.Unix())
	}
	b.WriteString("# HELP " + MetricPendingHolds +
		" Requests currently held waiting for an engine to become ready.\n")
	b.WriteString("# TYPE " + MetricPendingHolds + " gauge\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s{engine=%q} %d\n", MetricPendingHolds, r.engine, r.pending)
	}
	b.WriteString("# HELP " + MetricSheddedTotal +
		" Requests rejected because the gateway's hold capacity was reached.\n")
	b.WriteString("# TYPE " + MetricSheddedTotal + " counter\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "%s{engine=%q} %d\n", MetricSheddedTotal, r.engine, r.shed)
	}
	return b.String()
}
