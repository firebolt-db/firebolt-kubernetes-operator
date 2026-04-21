//go:build e2e

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

package controller

import (
	"sync"
	"time"
)

// CrashPoint identifies a specific point in the reconciliation loop where a crash can be triggered.
type CrashPoint string

const (
	// PhaseCreating crash points
	CrashAfterEngineConfigMapCreated CrashPoint = "after_engine_configmap_created"
	CrashAfterHeadlessServiceCreated CrashPoint = "after_headless_service_created"
	CrashAfterStatefulSetCreated     CrashPoint = "after_statefulset_created"
	CrashAfterClusterServiceEnsured  CrashPoint = "after_cluster_service_ensured"
	CrashBeforeCreatingToSwitching   CrashPoint = "before_creating_to_switching"

	// PhaseSwitching crash points
	CrashAfterServiceSelectorUpdate  CrashPoint = "after_service_selector_update"
	CrashBeforeSwitchingStatusUpdate CrashPoint = "before_switching_status_update"

	// PhaseCleaning crash points
	CrashAfterStatefulSetDeleted  CrashPoint = "after_statefulset_deleted"
	CrashBeforeCleaningToTerminal CrashPoint = "before_cleaning_to_terminal"
)

// CrashPointManager manages crash points for testing
type CrashPointManager struct {
	mu           sync.Mutex
	crashPoints  map[string]chan struct{} // key -> signal channel
	hitCallbacks map[string]func()        // key -> callback when hit
}

var globalCrashPointManager = &CrashPointManager{
	crashPoints:  make(map[string]chan struct{}),
	hitCallbacks: make(map[string]func()),
}

// CrashPointKey generates a key for the crash point map
func CrashPointKey(clusterName string, point CrashPoint) string {
	return clusterName + ":" + string(point)
}

// SetCrashPoint enables a crash point for a specific cluster.
// When hit, the callback is called and then MaybeCrash blocks until the channel is closed.
// Returns a channel that should be closed to "restart" the operator.
func SetCrashPoint(clusterName string, point CrashPoint, onHit func()) chan struct{} {
	key := CrashPointKey(clusterName, point)
	ch := make(chan struct{})

	globalCrashPointManager.mu.Lock()
	defer globalCrashPointManager.mu.Unlock()

	globalCrashPointManager.crashPoints[key] = ch
	globalCrashPointManager.hitCallbacks[key] = onHit
	return ch
}

// ClearCrashPoint disables a crash point for a specific cluster
func ClearCrashPoint(clusterName string, point CrashPoint) {
	key := CrashPointKey(clusterName, point)

	globalCrashPointManager.mu.Lock()
	defer globalCrashPointManager.mu.Unlock()

	if ch, ok := globalCrashPointManager.crashPoints[key]; ok {
		close(ch)
		delete(globalCrashPointManager.crashPoints, key)
	}
	delete(globalCrashPointManager.hitCallbacks, key)
}

// ClearAllCrashPoints removes all crash points
func ClearAllCrashPoints() {
	globalCrashPointManager.mu.Lock()
	defer globalCrashPointManager.mu.Unlock()

	for key, ch := range globalCrashPointManager.crashPoints {
		close(ch)
		delete(globalCrashPointManager.crashPoints, key)
	}
	for key := range globalCrashPointManager.hitCallbacks {
		delete(globalCrashPointManager.hitCallbacks, key)
	}
}

// MaybeCrash checks if a crash point is active and simulates a crash.
// When a crash point is set, this function:
// 1. Calls the registered callback (to signal the test that the point was hit)
// 2. Blocks until the restart channel is closed (simulating the operator being stopped)
func MaybeCrash(clusterName string, point CrashPoint) {
	key := CrashPointKey(clusterName, point)

	globalCrashPointManager.mu.Lock()
	ch, exists := globalCrashPointManager.crashPoints[key]
	callback := globalCrashPointManager.hitCallbacks[key]
	if exists {
		// Remove so it doesn't trigger again
		delete(globalCrashPointManager.crashPoints, key)
		delete(globalCrashPointManager.hitCallbacks, key)
	}
	globalCrashPointManager.mu.Unlock()

	if !exists {
		return
	}

	// Call callback to signal the test that we hit the crash point
	if callback != nil {
		callback()
	}

	// Block until the channel is closed (simulating that we're "crashed")
	// Use a timeout to prevent tests from hanging forever
	select {
	case <-ch:
		// Channel closed, we can "restart"
	case <-time.After(5 * time.Minute):
		// Safety timeout - shouldn't happen in tests
	}
}
