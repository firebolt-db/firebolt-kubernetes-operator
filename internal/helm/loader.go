/*
Copyright 2025.

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

package helm

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
)

const evictionTTL = 7 * 24 * time.Hour

type cacheEntry struct {
	chart    *chart.Chart
	lastUsed time.Time
}

// ChartCache manages loaded Helm charts with version-keyed caching and
// time-based eviction of unused entries.
type ChartCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

// NewChartCache creates an empty chart cache.
func NewChartCache() *ChartCache {
	return &ChartCache{
		entries: make(map[string]cacheEntry),
	}
}

func cacheKey(source, version string) string {
	return source + ":" + version
}

// GetOrLoad returns a cached chart or loads it from the given source. For OCI
// sources (oci://...) the specified version is appended to the pull request.
// For local filesystem paths the version parameter is informational only.
// Stale entries unused for 7+ days are evicted whenever a new chart is loaded.
func (c *ChartCache) GetOrLoad(source, version string) (*chart.Chart, error) {
	key := cacheKey(source, version)

	c.mu.Lock()
	if entry, ok := c.entries[key]; ok {
		entry.lastUsed = time.Now()
		c.entries[key] = entry
		c.mu.Unlock()
		return entry.chart, nil
	}
	c.mu.Unlock()

	ch, err := loadChart(source, version)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry{
		chart:    ch,
		lastUsed: time.Now(),
	}

	c.evictStaleLocked()
	return ch, nil
}

func (c *ChartCache) evictStaleLocked() {
	cutoff := time.Now().Add(-evictionTTL)
	for k, entry := range c.entries {
		if entry.lastUsed.Before(cutoff) {
			delete(c.entries, k)
		}
	}
}

func loadChart(source, version string) (*chart.Chart, error) {
	if strings.HasPrefix(source, "oci://") {
		return pullOCIChart(source, version)
	}
	return loader.Load(source)
}

func pullOCIChart(ref, version string) (*chart.Chart, error) {
	settings := cli.New()

	regClient, err := registry.NewClient()
	if err != nil {
		return nil, fmt.Errorf("creating OCI registry client: %w", err)
	}

	pull := action.NewPullWithOpts(action.WithConfig(&action.Configuration{}))
	pull.SetRegistryClient(regClient)
	pull.Settings = settings
	pull.Version = version

	tmpDir, err := os.MkdirTemp("", "helm-pull-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir for chart pull: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	pull.DestDir = tmpDir
	pull.Untar = true
	pull.UntarDir = tmpDir

	_, err = pull.Run(ref)
	if err != nil {
		return nil, fmt.Errorf("pulling chart %s version %s: %w", ref, version, err)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("reading pulled chart dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return loader.Load(tmpDir + "/" + entry.Name())
		}
	}

	return nil, fmt.Errorf("no chart directory found after pulling %s", ref)
}
