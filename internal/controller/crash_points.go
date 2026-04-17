//go:build !e2e

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

// CrashPoint identifies a specific point in the reconciliation loop where a crash can be triggered.
// This is the production stub - crash points are no-ops when not built with e2e tag.
type CrashPoint string

// Crash-point constants used by the e2e test harness to inject failures at
// deterministic locations in the reconciliation loop. In production builds
// MaybeCrash is inlined as a no-op.
const (
	// CrashAfterEngineConfigMapCreated fires after the engine ConfigMap is written.
	CrashAfterEngineConfigMapCreated CrashPoint = "after_engine_configmap_created"
	// CrashAfterHeadlessServiceCreated fires after the headless Service is written.
	CrashAfterHeadlessServiceCreated CrashPoint = "after_headless_service_created"
	// CrashAfterStatefulSetCreated fires after the StatefulSet is written.
	CrashAfterStatefulSetCreated CrashPoint = "after_statefulset_created"
	// CrashAfterClusterServiceEnsured fires after the cluster Service is ensured.
	CrashAfterClusterServiceEnsured CrashPoint = "after_cluster_service_ensured"
	// CrashBeforeCreatingToSwitching fires before transitioning from creating to switching.
	CrashBeforeCreatingToSwitching CrashPoint = "before_creating_to_switching"

	// CrashAfterServiceSelectorUpdate fires after the Service selector is updated.
	CrashAfterServiceSelectorUpdate CrashPoint = "after_service_selector_update"
	// CrashBeforeSwitchingStatusUpdate fires before the switching status write.
	CrashBeforeSwitchingStatusUpdate CrashPoint = "before_switching_status_update"

	// CrashAfterStatefulSetDeleted fires after the old StatefulSet is deleted.
	CrashAfterStatefulSetDeleted CrashPoint = "after_statefulset_deleted"
	// CrashBeforeCleaningToStable fires before transitioning from cleaning to stable.
	CrashBeforeCleaningToStable CrashPoint = "before_cleaning_to_stable"
)

// MaybeCrash is a no-op in production builds.
// When built with the e2e tag, this function checks for active crash points.
//
//go:inline
func MaybeCrash(_ string, _ CrashPoint) {
	// No-op in production builds
}
