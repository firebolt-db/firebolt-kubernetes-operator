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

// operatorMountedSecretNames returns the Secret names reachable through the
// operator-built volumes of one pod.
//
// Derived from the rendered volumes rather than a hand-kept list, so a newly
// mounted operator Secret is protected the moment it is mounted, with no second
// place to remember to update.
func operatorMountedSecretNames(operator []corev1.Volume) map[string]struct{} {
	out := make(map[string]struct{}, len(operator))
	for i := range operator {
		for _, name := range computev1alpha1.VolumeSecretRefs(&operator[i]) {
			out[name] = struct{}{}
		}
	}
	return out
}

// aliasesOperatorSecret reports whether a user-supplied volume reaches one of
// the operator's own Secrets under a volume name of the author's choosing —
// which the reserved-name check cannot detect, since the name is arbitrary.
func aliasesOperatorSecret(v *corev1.Volume, operatorSecrets map[string]struct{}) bool {
	for _, name := range computev1alpha1.VolumeSecretRefs(v) {
		if _, hit := operatorSecrets[name]; hit {
			return true
		}
	}
	return false
}
