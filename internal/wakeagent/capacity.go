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
	"strconv"
	"strings"
	"sync"
	"time"
)

// envoyAllocatedStat is the Envoy admin statistic carrying the server's
// current heap allocation in bytes.
const envoyAllocatedStat = "server.memory_allocated"

// memoryBudgetFraction is the share of Envoy's container memory limit the
// agent is willing to see consumed by held requests. The remainder is left
// for Envoy's steady-state working set: connection pools, the DFP
// sub-cluster table, stats, and the buffers of requests that are actively
// proxying rather than parked.
const memoryBudgetFraction = 0.25

// capacityLimiter answers "how many more requests may we hold right now?".
//
// Two inputs, deliberately:
//
//   - A hard ceiling from Envoy's container memory limit, handed to the
//     agent through the downward API. This bounds the worst case even if
//     the live reading is unavailable.
//   - A live reading of Envoy's actual allocation from its admin stats
//     listener, so the cap tightens under real pressure instead of
//     trusting arithmetic about a worst case that may never materialize.
//
// The memory at risk belongs to Envoy, not to the agent, which is why
// neither input is the agent's own cgroup.
type capacityLimiter struct {
	// envoyMemoryLimitBytes is Envoy's container limit. Zero means the
	// limit was not exposed (no limits set on the container), in which
	// case only the static fallback applies.
	envoyMemoryLimitBytes int64
	// perHoldBytes is the worst-case memory one held request pins. This is
	// per_connection_buffer_limit_bytes: we never read the body, but frames
	// the client already sent accumulate in the filter chain until the
	// watermark trips and backpressure stalls the sender.
	perHoldBytes int64
	// fallbackCap applies when no memory limit is available.
	fallbackCap int

	adminURL string
	client   *http.Client

	mu        sync.RWMutex
	allocated int64
	lastRead  time.Time
}

func newCapacityLimiter(envoyMemoryLimitBytes, perHoldBytes int64, fallbackCap int, adminURL string) *capacityLimiter {
	return &capacityLimiter{
		envoyMemoryLimitBytes: envoyMemoryLimitBytes,
		perHoldBytes:          perHoldBytes,
		fallbackCap:           fallbackCap,
		adminURL:              adminURL,
		client:                &http.Client{Timeout: 2 * time.Second},
	}
}

// Cap returns the current maximum number of concurrent holds.
func (c *capacityLimiter) Cap() int {
	if c.envoyMemoryLimitBytes <= 0 || c.perHoldBytes <= 0 {
		return c.fallbackCap
	}

	budget := int64(float64(c.envoyMemoryLimitBytes) * memoryBudgetFraction)

	c.mu.RLock()
	allocated, lastRead := c.allocated, c.lastRead
	c.mu.RUnlock()

	// Only trust a live reading we actually managed to take. Before the
	// first successful scrape (or after the admin listener goes away) fall
	// back to the static budget rather than to zero — a stale or missing
	// reading must not silently disable holding altogether.
	if !lastRead.IsZero() {
		headroom := budget - (allocated - c.steadyStateEstimate())
		if headroom < budget {
			budget = headroom
		}
	}

	if budget <= 0 {
		return 0
	}
	holds := budget / c.perHoldBytes
	if holds > int64(maxIntCap) {
		return maxIntCap
	}
	return int(holds)
}

// maxIntCap bounds the computed cap so a very large memory limit cannot
// produce a nonsensical hold count. 100k parked connections would exhaust
// file descriptors long before memory.
const maxIntCap = 100_000

// steadyStateEstimate is what we assume Envoy uses when idle, subtracted
// from the live allocation so the headroom calculation measures growth
// rather than baseline. Deliberately crude: the goal is to avoid the cap
// collapsing to zero just because Envoy's resident baseline is a
// meaningful fraction of a small limit.
func (c *capacityLimiter) steadyStateEstimate() int64 {
	est := int64(float64(c.envoyMemoryLimitBytes) * 0.25)
	return est
}

// Refresh reads Envoy's current allocation from the admin stats listener.
// A failure leaves the previous reading in place: the admin listener being
// briefly unavailable is not a reason to change the cap.
func (c *capacityLimiter) Refresh(ctx context.Context) error {
	if c.adminURL == "" {
		return nil
	}
	url := fmt.Sprintf("%s/stats?filter=%s", strings.TrimSuffix(c.adminURL, "/"), envoyAllocatedStat)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("envoy admin stats: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return err
	}
	allocated, ok := parseEnvoyStat(string(body), envoyAllocatedStat)
	if !ok {
		return fmt.Errorf("envoy admin stats: %s not present in response", envoyAllocatedStat)
	}
	c.mu.Lock()
	c.allocated = allocated
	c.lastRead = time.Now()
	c.mu.Unlock()
	return nil
}

// parseEnvoyStat pulls one counter out of Envoy's plain-text admin stats
// output, whose lines have the form "name: value".
func parseEnvoyStat(body, name string) (int64, bool) {
	for _, line := range strings.Split(body, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || strings.TrimSpace(key) != name {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
