package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

const crdTemplate = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: examples.example.com
spec:
  group: example.com
  scope: Namespaced
  names: {kind: Example, plural: examples}
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        description: Original docs
        properties:
          choice:
            anyOf: [{type: integer}, {type: string}]
            x-kubernetes-int-or-string: true
            default: %s
          description:
            type: string
            description: Field docs
          options:
            type: object
            default: {description: value}
          entries:
            type: array
            items:
              type: object
              description: Entry docs
              additionalProperties:
                type: string
                description: Value docs
`

func TestNormalizeFile(t *testing.T) {
	original := fmt.Sprintf(crdTemplate, "1")
	for _, tc := range []struct {
		name, before, after string
		equal               bool
	}{
		{"descriptions", original, strings.ReplaceAll(original, "docs", "documentation"), true},
		{"comments", original, "# comment\n" + original + "\n---\n", true},
		{"defaults", original, fmt.Sprintf(crdTemplate, "2"), false},
		{"number versus string", fmt.Sprintf(crdTemplate, "1e3"), fmt.Sprintf(crdTemplate, "'1e3'"), false},
		{"colon scalar", fmt.Sprintf(crdTemplate, "1:20"), fmt.Sprintf(crdTemplate, "80"), false},
		{"date string", fmt.Sprintf(crdTemplate, "2026-11-13"), fmt.Sprintf(crdTemplate, "'2026-11-13'"), true},
		{"date change", fmt.Sprintf(crdTemplate, "2026-11-13"), fmt.Sprintf(crdTemplate, "2026-11-14"), false},
		{"large integers", fmt.Sprintf(crdTemplate, "9007199254740992"), fmt.Sprintf(crdTemplate, "9007199254740993"), false},
		{"boolean versus integer", fmt.Sprintf(crdTemplate, "true"), original, false},
		{"property named description", original, strings.Replace(original, "          description:", "          renamed:", 1), false},
		{"literal default", original, strings.Replace(original, "{description: value}", "{description: changed}", 1), false},
		{"type", original, strings.Replace(original, "type: string", "type: boolean", 1), false},
		{"required", original, strings.Replace(original, "        type: object", "        type: object\n        required: [choice]", 1), false},
		{"validation", original, strings.Replace(original, "        type: object", "        type: object\n        x-kubernetes-validations: [{rule: 'true'}]", 1), false},
		{"version", original, strings.Replace(original, "name: v1", "name: v2", 1), false},
		{"scope", original, strings.Replace(original, "scope: Namespaced", "scope: Cluster", 1), false},
		{"conversion", original, strings.Replace(original, "spec:\n", "spec:\n  conversion: {strategy: None}\n", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, err := normalizeFile("templates/crd.yaml", []byte(tc.before))
			if err != nil {
				t.Fatal(err)
			}
			after, err := normalizeFile("templates/crd.yaml", []byte(tc.after))
			if err != nil {
				t.Fatal(err)
			}
			if equal := bytes.Equal(before, after); equal != tc.equal {
				t.Fatalf("equal = %v, want %v", equal, tc.equal)
			}
		})
	}
}

func TestNormalizeFailures(t *testing.T) {
	for _, input := range []string{
		"broken: [",
		"kind: CustomResourceDefinition\nspec: invalid",
		"kind: CustomResourceDefinition\nspec: {versions: invalid}",
		"kind: CustomResourceDefinition\nspec: {versions: [{schema: {}}]}",
	} {
		if _, err := normalizeFile("templates/crd.yaml", []byte(input)); err == nil {
			t.Fatalf("expected failure for %q", input)
		}
	}
}

func TestHelmExpressionsRemainSignificant(t *testing.T) {
	input := []byte("value: {{ .Values.example }}")
	output, err := normalizeFile("templates/config.yaml", input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, input) {
		t.Fatal("Helm expressions must be compared verbatim")
	}
}
