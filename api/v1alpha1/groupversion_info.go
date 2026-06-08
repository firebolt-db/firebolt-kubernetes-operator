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

// Package v1alpha1 contains API Schema definitions for the compute v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=compute.firebolt.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "compute.firebolt.io", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionResource scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// addKnownTypes registers every type in the compute.firebolt.io/v1alpha1 group
// with the scheme. Registration is centralized here (rather than per-type init
// functions) so the api package depends only on k8s.io/apimachinery, not on
// controller-runtime's pkg/scheme helper.
func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&FireboltInstance{}, &FireboltInstanceList{},
		&FireboltEngine{}, &FireboltEngineList{},
		&FireboltEngineClass{}, &FireboltEngineClassList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
