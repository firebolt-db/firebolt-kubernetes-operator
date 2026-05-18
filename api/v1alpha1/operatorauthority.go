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

package v1alpha1

// This file is the single source of truth for fields the operator owns
// across user-supplied templating surfaces (spec.customEngineConfig today;
// EngineClass tomorrow). Consumers reference these declarations directly
// instead of restating the path lists, so a future addition lands in one
// place and propagates to every strip / reject site.

// EngineConfigOwnedSection enumerates one operator-owned section of the
// rendered engine config.yaml. Section is the top-level key (empty string
// for the document root). Keys lists the immediate child keys under Section
// that the operator manages exclusively.
//
// When Section is non-empty and the user-supplied value at that section is
// not a JSON object, the entire section is dropped from user input: a deep
// merge would otherwise replace the operator-built section wholesale with
// the user's scalar, losing every authoritative key.
type EngineConfigOwnedSection struct {
	// Section is the top-level key in the rendered config document, or "" for
	// the document root.
	Section string

	// Keys are the immediate children of Section managed exclusively by the
	// operator. User input at any of these paths is silently stripped.
	Keys []string
}

// OperatorOwnedEngineConfigPaths declares every path in the rendered engine
// config.yaml that the operator manages exclusively. It is consumed by
// stripProtectedEngineConfigPaths (internal/controller/engine_reconcile.go),
// which removes these paths from spec.customEngineConfig before the deep
// merge into the canonical document.
//
// Stripping is silent so that the same FireboltEngine spec stays portable
// across operator releases even when this list grows: users do not need to
// chase the protected set in their CRs to keep them applying cleanly.
var OperatorOwnedEngineConfigPaths = []EngineConfigOwnedSection{
	{Section: "", Keys: []string{"schema_version"}},
	{Section: "instance", Keys: []string{"id", "type", "multi_engine"}},
	{Section: "engine", Keys: []string{"id", "nodes", "termination_grace_period"}},
}
