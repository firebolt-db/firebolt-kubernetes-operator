// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package wakeagent

import (
	"strconv"
	"testing"
)

func TestReadinessHistoryBoundedByCurrentServices(t *testing.T) {
	tracker := newReadinessTracker()
	tracker.MarkSynced()
	tracker.setReady("service:steady", true)
	_, steadyVersion := tracker.readinessVersion("service:steady")
	var previousVersion uint64
	for generation := range 100 {
		service := "service:generation-" + strconv.Itoa(generation)
		tracker.setReady(service, true)
		ready, version := tracker.readinessVersion(service)
		if !ready || version <= previousVersion {
			t.Fatalf("new generation reused a readiness identity: ready=%v version=%d previous=%d", ready, version, previousVersion)
		}
		previousVersion = version
		tracker.setReady(service, false)
		tracker.setReady(service, false) // Repeated empty-slice/delete events retain no history.
		if len(tracker.ready) != 1 || len(tracker.versions) != 1 {
			t.Fatalf("retired generation history leaked: ready=%d versions=%d", len(tracker.ready), len(tracker.versions))
		}
		tracker.setReady("service:steady", true)
		if ready, version := tracker.readinessVersion("service:steady"); !ready || version != steadyVersion {
			t.Fatalf("unrelated rollout invalidated stable readiness: ready=%v version=%d", ready, version)
		}
	}
	tracker.setReady("service:steady", false)
	tracker.setReady("service:steady", true)
	if ready, version := tracker.readinessVersion("service:steady"); !ready || version <= previousVersion {
		t.Fatalf("recreated Service reused a removed readiness identity: ready=%v version=%d previous=%d", ready, version, previousVersion)
	}
}
