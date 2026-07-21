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
	"encoding/json"
	"fmt"
	"os"
	"testing"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// testdata/packdb-application-config.schema.json is a point-in-time copy of
// packdb's src/Core/Application/application-config.schema.json, captured at
// packdb commit 2127bc79a9dc3238c0a1c04edcc830eca06da596 (2026-07-01). This
// file is FB-896's most exhaustive check on rendered auth config: it is
// additionalProperties:false at every level, so validating buildConfigMap's
// full output against it catches any unknown/misplaced key — the class of
// bug that crashes every engine at startup — deterministically, without
// booting the engine binary.
//
// Two caveats, both discovered reading packdb source directly (not just
// this schema) and worth keeping in mind when this file is next updated:
//
//  1. This schema is generated FROM packdb's compiled config-entry struct
//     tree (src/Common/Configuration/DocGen.cpp) for documentation
//     purposes; the running engine does NOT load or evaluate this JSON
//     Schema file itself. The underlying guarantee this test cares about
//     ("an unknown key is rejected") is separately, actually enforced by
//     hand-written traversal code in
//     src/Common/Configuration/ChangeParser.cpp, which walks the same
//     struct tree this schema was generated from — so a conformance pass
//     here is strong evidence the config loads, but it is not literally
//     the code path the engine executes.
//  2. This schema says nothing about packdb's own semantic (non-shape)
//     validation — e.g. "admin requires exactly one of
//     password_value/password_env/password_file", "OIDC provider names
//     must not start with _", "preferred_authorization_server must name a
//     configured server". Those live in AuthConfig::Validate()
//     (src/PackDB/Auth/AuthConfig.cpp) and are separately mirrored by
//     ValidateAuth (api/v1alpha1/fireboltinstance_webhook.go) — read and
//     cross-checked against that C++ function directly while building
//     this test, not derived from the schema.
const packdbSchemaPath = "testdata/packdb-application-config.schema.json"

// packdbSchemaKeywords is every JSON-Schema keyword the vendored file
// actually uses (confirmed by walking it and collecting every keyword seen
// at a schema-node position). packdbSchemaNode below implements exactly
// this subset — not the general JSON Schema specification — because that's
// all this file needs. TestPackdbSchemaOnlyUsesSupportedKeywords guards the
// assumption: if a future re-vendor introduces something like "oneOf" or
// "pattern", that test fails loudly instead of this validator silently
// under-validating.
var packdbSchemaKeywords = map[string]bool{
	"$id": true, "$schema": true, "title": true, "description": true, "default": true,
	"type": true, "properties": true, "additionalProperties": true, "required": true, "items": true,
}

// packdbSchemaNode is a minimal JSON-Schema-subset representation covering
// exactly the keywords packdbSchemaKeywords lists that affect validation
// (type/properties/additionalProperties/required/items — the rest are
// metadata this validator doesn't need to interpret). additionalProperties
// is typed as *bool, not schema-or-bool, because the vendored schema never
// uses the object-valued form (confirmed while building this test).
type packdbSchemaNode struct {
	Type                 string                      `json:"type"`
	Properties           map[string]packdbSchemaNode `json:"properties"`
	AdditionalProperties *bool                       `json:"additionalProperties"`
	Required             []string                    `json:"required"`
	Items                *packdbSchemaNode           `json:"items"`
}

// validate checks value against n, appending one human-readable message per
// violation to violations. path is the dotted/indexed location for
// error messages (e.g. "instance.auth.oidc.providers[0].name").
func (n packdbSchemaNode) validate(path string, value interface{}, violations *[]string) {
	switch n.Type {
	case "object":
		obj, ok := value.(map[string]interface{})
		if !ok {
			*violations = append(*violations, fmt.Sprintf("%s: expected object, got %T", path, value))
			return
		}
		for _, req := range n.Required {
			if _, present := obj[req]; !present {
				*violations = append(*violations, fmt.Sprintf("%s: missing required property %q", path, req))
			}
		}
		for k, v := range obj {
			sub, known := n.Properties[k]
			if !known {
				if n.AdditionalProperties != nil && !*n.AdditionalProperties {
					*violations = append(*violations, fmt.Sprintf("%s: additional property %q is not allowed", path, k))
				}
				continue
			}
			sub.validate(path+"."+k, v, violations)
		}
	case "array":
		arr, ok := value.([]interface{})
		if !ok {
			*violations = append(*violations, fmt.Sprintf("%s: expected array, got %T", path, value))
			return
		}
		if n.Items != nil {
			for i, item := range arr {
				n.Items.validate(fmt.Sprintf("%s[%d]", path, i), item, violations)
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			*violations = append(*violations, fmt.Sprintf("%s: expected string, got %T", path, value))
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			*violations = append(*violations, fmt.Sprintf("%s: expected boolean, got %T", path, value))
		}
	case "integer", "number":
		switch value.(type) {
		case float64, int, int64:
		default:
			*violations = append(*violations, fmt.Sprintf("%s: expected number, got %T", path, value))
		}
	case "":
		// No type constraint at this schema node.
	default:
		*violations = append(*violations, fmt.Sprintf("%s: unsupported schema type %q (packdbSchemaNode gap, "+
			"not a real violation — extend the validator)", path, n.Type))
	}
}

// loadPackdbSchema reads and parses the vendored schema file.
func loadPackdbSchema(t *testing.T) packdbSchemaNode {
	t.Helper()
	raw, err := os.ReadFile(packdbSchemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", packdbSchemaPath, err)
	}
	var root packdbSchemaNode
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parsing %s: %v", packdbSchemaPath, err)
	}
	return root
}

// TestPackdbSchemaOnlyUsesSupportedKeywords guards packdbSchemaNode's
// central assumption: that the vendored schema uses only the keyword
// subset it implements. If a future re-vendor of
// testdata/packdb-application-config.schema.json introduces a keyword
// like "oneOf", "pattern", or "enum", packdbSchemaNode would silently
// ignore it rather than validate it — this test turns that into a loud,
// specific failure instead.
func TestPackdbSchemaOnlyUsesSupportedKeywords(t *testing.T) {
	raw, err := os.ReadFile(packdbSchemaPath)
	if err != nil {
		t.Fatalf("reading %s: %v", packdbSchemaPath, err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parsing %s: %v", packdbSchemaPath, err)
	}

	// walkSchemaNode visits a schema-node position (a dict whose OWN keys
	// are JSON-Schema keywords, e.g. the document root or any entry under
	// "properties"/"items"). It must not be applied uniformly to every
	// dict in the file: "properties" and "required" hold user-defined
	// config field names as VALUES/array-entries, not as schema keywords,
	// so those are handled specially rather than recursed into generically.
	var walkSchemaNode func(node map[string]interface{})
	walkSchemaNode = func(node map[string]interface{}) {
		for k, v := range node {
			if !packdbSchemaKeywords[k] {
				t.Errorf("schema uses unsupported keyword %q; extend packdbSchemaNode/packdbSchemaKeywords "+
					"to cover it before trusting conformance results", k)
				continue
			}
			switch k {
			case "properties":
				props, ok := v.(map[string]interface{})
				if !ok {
					continue
				}
				for _, sub := range props {
					if subNode, ok := sub.(map[string]interface{}); ok {
						walkSchemaNode(subNode)
					}
				}
			case "items":
				if subNode, ok := v.(map[string]interface{}); ok {
					walkSchemaNode(subNode)
				}
				// required/type/additionalProperties/default/description/title/
				// $id/$schema are terminal at this level: their values are
				// plain data (strings/bools/arrays-of-strings), not nested
				// schema nodes, so no further recursion is needed.
			}
		}
	}
	walkSchemaNode(root)
}

// renderedConfigDoc renders a full config.yaml via buildConfigMap and
// decodes it back to a generic map, the same shape packdbSchemaNode.validate
// walks.
func renderedConfigDoc(t *testing.T, instanceInfo InstanceInfo) map[string]interface{} {
	t.Helper()
	cm := buildConfigMap(testSpec(), testEngineName, testNamespace, 0, instanceInfo, nil)
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data[ConfigFileName]), &doc); err != nil {
		t.Fatalf("rendered config.yaml is not valid YAML: %v", err)
	}
	return doc
}

// TestBuildConfigMap_ConformsToPackdbSchema validates buildConfigMap's
// rendered config.yaml — disabled, native (local) auth, and OIDC auth —
// against the vendored packdb schema. See this file's top-level doc
// comment for what a pass here does and does not prove.
func TestBuildConfigMap_ConformsToPackdbSchema(t *testing.T) {
	schema := loadPackdbSchema(t)

	oidcInfo := testInstanceInfoWithAuth()
	oidcInfo.Auth.Spec.PreferredAuthorizationServer = "okta"
	oidcInfo.Auth.Spec.OIDC = &computev1alpha1.OIDCAuthSpec{
		JWT: &computev1alpha1.OIDCJWTSpec{ClockSkewTolerance: "45s", MaxTokenAge: "12h"},
		Providers: []computev1alpha1.OIDCProviderSpec{
			{
				Name:            "okta",
				Title:           "Okta SSO",
				DiscoveryURL:    "https://okta.example.com/.well-known/openid-configuration",
				Audience:        "firebolt-prod",
				UsernameMapping: "{{ email }}",
				JITProvisioning: &computev1alpha1.JITProvisioningSpec{Enabled: true, DefaultRoles: []string{"public", "analyst"}},
				JWKS:            &computev1alpha1.OIDCJWKSSpec{CacheTTL: "2h"},
				Discovery:       &computev1alpha1.OIDCDiscoverySpec{RefreshInterval: "6h"},
			},
			{
				// Minimal shape: only the three required provider fields.
				Name:            "azuread",
				DiscoveryURL:    "https://login.microsoftonline.com/common/v2.0/.well-known/openid-configuration",
				UsernameMapping: "{{ sub }}",
			},
		},
	}

	authAndTLSInfo := testInstanceInfoWithAuth()
	authAndTLSInfo.TLS = testInstanceInfoWithTLS().TLS

	cases := []struct {
		name string
		info InstanceInfo
	}{
		{name: "disabled", info: testInstanceInfo()},
		{name: "native", info: testInstanceInfoWithAuth()},
		{name: "oidc", info: oidcInfo},
		{name: "engine-tls", info: testInstanceInfoWithTLS()},
		{name: "auth-and-engine-tls", info: authAndTLSInfo},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := renderedConfigDoc(t, tc.info)
			var violations []string
			schema.validate("<root>", doc, &violations)
			for _, v := range violations {
				t.Error(v)
			}
		})
	}
}

// TestPackdbSchemaValidator_CatchesUnknownField is a discriminating check
// on the validator itself: it must actually reject a config that violates
// additionalProperties:false, or TestBuildConfigMap_ConformsToPackdbSchema
// passing would prove nothing.
func TestPackdbSchemaValidator_CatchesUnknownField(t *testing.T) {
	schema := loadPackdbSchema(t)
	doc := renderedConfigDoc(t, testInstanceInfoWithAuth())

	instance, ok := doc["instance"].(map[string]interface{})
	if !ok {
		t.Fatalf("rendered doc missing instance object: %v", doc)
	}
	auth, ok := instance["auth"].(map[string]interface{})
	if !ok {
		t.Fatalf("rendered doc missing instance.auth object: %v", instance)
	}
	auth["unknown_field"] = "should be rejected"

	var violations []string
	schema.validate("<root>", doc, &violations)
	if len(violations) == 0 {
		t.Fatal("expected at least one violation for an injected unknown field, got none — " +
			"the validator is not discriminating")
	}
	found := false
	for _, v := range violations {
		if v == `<root>.instance.auth: additional property "unknown_field" is not allowed` {
			found = true
		}
	}
	if !found {
		t.Errorf("violations = %v, want one flagging instance.auth.unknown_field", violations)
	}
}
