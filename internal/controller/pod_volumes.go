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
	corev1 "k8s.io/api/core/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// aliasesProtectedSecret reports whether a user-supplied volume reaches an
// operator-managed Secret under a volume name of the author's choosing — which
// the reserved-name check cannot detect, since the name is arbitrary.
//
// isProtected is the Instance-wide predicate (instanceProtectedSecret). It
// deliberately replaced a set derived from the component's OWN rendered volumes:
// that made protection a function of which pod was being rendered, so the gateway
// pod dropped only gateway Secrets and each engine only its own, leaving every
// cross-component route — a gateway sidecar reading the JWT signing key, one
// engine reading a sibling's serving key — open at render time. A nil predicate
// protects nothing.
func aliasesProtectedSecret(v *corev1.Volume, isProtected func(string) bool) bool {
	if isProtected == nil {
		return false
	}
	for _, name := range computev1alpha1.VolumeSecretRefs(v) {
		if isProtected(name) {
			return true
		}
	}
	return false
}
