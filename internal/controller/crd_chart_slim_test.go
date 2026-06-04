/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

// Both Helm charts ship description-slimmed copies of the canonical CRDs in
// config/crd/bases so each Helm release Secret stays under Kubernetes' 1 MiB
// cap (scripts/strip-crd-descriptions.py, run by `make manifests`; full-fat
// CRDs overflow the Secret and `helm install` fails). These specs pin the
// contract that slimming removes ONLY descriptions: every structural element
// — types, CEL x-kubernetes-validations, the x-kubernetes-preserve-unknown-fields
// markers injected by patch-crd-template-metadata.py, defaults, enums, required
// lists — must survive intact. A regression in the strip script that drops a
// marker or mangles the schema fails here instead of in a user's cluster.
//
// Pure file comparison (no envtest); the canonical bases are exercised for
// real apiserver behavior by crd_pod_template_metadata_test.go.

// base CRD filename -> the chart copies generated from it.
var crdChartCopies = map[string][]string{
	"compute.firebolt.io_fireboltinstances.yaml": {
		"firebolt-operator-crds/templates/fireboltinstances.yaml",
		"firebolt-operator/crds/fireboltinstances.yaml",
	},
	"compute.firebolt.io_fireboltengines.yaml": {
		"firebolt-operator-crds/templates/fireboltengines.yaml",
		"firebolt-operator/crds/fireboltengines.yaml",
	},
	"compute.firebolt.io_fireboltengineclasses.yaml": {
		"firebolt-operator-crds/templates/fireboltengineclasses.yaml",
		"firebolt-operator/crds/fireboltengineclasses.yaml",
	},
}

func loadCRDMap(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse %s as YAML: %v", path, err)
	}
	return m
}

// stripDescriptions recursively removes every "description" key so two CRDs
// can be compared for structural equality independent of doc text. Mutates
// and returns v.
func stripDescriptions(v any) any {
	switch node := v.(type) {
	case map[string]any:
		delete(node, "description")
		for k, child := range node {
			node[k] = stripDescriptions(child)
		}
		return node
	case []any:
		for i, child := range node {
			node[i] = stripDescriptions(child)
		}
		return node
	default:
		return v
	}
}

func countDescriptions(v any) int {
	n := 0
	switch node := v.(type) {
	case map[string]any:
		if _, ok := node["description"]; ok {
			n++
		}
		for _, child := range node {
			n += countDescriptions(child)
		}
	case []any:
		for _, child := range node {
			n += countDescriptions(child)
		}
	}
	return n
}

func TestChartCRDsAreBasesMinusDescriptions(t *testing.T) {
	basesDir := filepath.Join("..", "..", "config", "crd", "bases")
	helmDir := filepath.Join("..", "..", "helm")

	for baseName, copies := range crdChartCopies {
		basePath := filepath.Join(basesDir, baseName)
		baseDescr := countDescriptions(loadCRDMap(t, basePath))
		baseStripped := stripDescriptions(loadCRDMap(t, basePath))

		for _, rel := range copies {
			chartPath := filepath.Join(helmDir, rel)

			// Slimming ran: the chart copy carries far fewer descriptions
			// than the base, but is not stripped bare — under-template mode
			// keeps Firebolt's own field docs (and all status docs).
			chartDescr := countDescriptions(loadCRDMap(t, chartPath))
			if chartDescr >= baseDescr {
				t.Errorf("%s: has %d descriptions vs base %d — strip-crd-descriptions.py did not run; rerun `make manifests`",
					rel, chartDescr, baseDescr)
			}
			if chartDescr == 0 {
				t.Errorf("%s: every description stripped — under-template mode must keep Firebolt's own field docs", rel)
			}

			// Nothing structural changed: chart == base once every
			// description is removed from both.
			chartStripped := stripDescriptions(loadCRDMap(t, chartPath))
			if !reflect.DeepEqual(baseStripped, chartStripped) {
				t.Errorf("%s: schema differs from canonical %s beyond descriptions — the strip altered CRD structure (markers/CEL/types). Compare against config/crd/bases.",
					rel, baseName)
			}
		}
	}
}
