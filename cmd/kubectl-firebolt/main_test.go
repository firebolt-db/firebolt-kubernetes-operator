package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

func TestVersionCommand(t *testing.T) {
	prev := version
	version = "v9.9.9-test"
	t.Cleanup(func() { version = prev })

	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version command: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "v9.9.9-test" {
		t.Errorf("version output = %q, want %q", got, "v9.9.9-test")
	}
}

func TestMarshalObjectListIsKubectlStyle(t *testing.T) {
	engines := []v1alpha1.FireboltEngine{
		{ObjectMeta: metav1.ObjectMeta{Name: "e1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "e2"}},
	}
	out, err := marshalObjectList(outJSON, engines)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got["kind"] != "List" || got["apiVersion"] != "v1" {
		t.Errorf("want kind=List apiVersion=v1, got kind=%v apiVersion=%v", got["kind"], got["apiVersion"])
	}
	if items, ok := got["items"].([]any); !ok || len(items) != 2 {
		t.Fatalf("items = %v, want a 2-element array", got["items"])
	}

	// An empty result must marshal items as [], not null, so `.items[]` works.
	out, err = marshalObjectList(outJSON, []v1alpha1.FireboltEngine{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"items": []`) {
		t.Errorf("empty list should render items: [], got:\n%s", out)
	}
}

func TestValidateOutput(t *testing.T) {
	for _, ok := range []string{outTable, outWide, outJSON, outYAML, outName, ""} {
		if err := validateOutput(ok); err != nil {
			t.Errorf("validateOutput(%q) = %v, want nil", ok, err)
		}
	}
	if err := validateOutput("xml"); err == nil {
		t.Error("validateOutput(\"xml\") = nil, want error")
	}
}
