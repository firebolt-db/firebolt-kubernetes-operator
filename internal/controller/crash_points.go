//go:build !e2e
// +build !e2e

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

package controller

// CrashPoint identifies a specific point in the reconciliation loop where a crash can be triggered.
// This is the production stub - crash points are no-ops when not built with e2e tag.
type CrashPoint string

const (
	// PhaseCreating crash points
	CrashAfterCoreConfigMapCreated   CrashPoint = "after_core_configmap_created"
	CrashAfterHeadlessServiceCreated CrashPoint = "after_headless_service_created"
	CrashAfterStatefulSetCreated     CrashPoint = "after_statefulset_created"
	CrashAfterClusterServiceEnsured  CrashPoint = "after_cluster_service_ensured"
	CrashBeforeCreatingToSwitching   CrashPoint = "before_creating_to_switching"

	// PhaseSwitching crash points
	CrashAfterServiceSelectorUpdate  CrashPoint = "after_service_selector_update"
	CrashBeforeSwitchingStatusUpdate CrashPoint = "before_switching_status_update"

	// PhaseDraining crash points
	CrashBeforeDrainingToCleaning CrashPoint = "before_draining_to_cleaning"

	// PhaseCleaning crash points
	CrashAfterStatefulSetDeleted     CrashPoint = "after_statefulset_deleted"
	CrashAfterHeadlessServiceDeleted CrashPoint = "after_headless_service_deleted"
	CrashAfterCoreConfigMapDeleted   CrashPoint = "after_core_configmap_deleted"
	CrashBeforeCleaningToStable      CrashPoint = "before_cleaning_to_stable"
)

// MaybeCrash is a no-op in production builds.
// When built with the e2e tag, this function checks for active crash points.
//
//go:inline
func MaybeCrash(clusterName string, point CrashPoint) {
	// No-op in production builds
}
