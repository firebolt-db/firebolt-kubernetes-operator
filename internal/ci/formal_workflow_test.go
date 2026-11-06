/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestFormalWorkflowRunsForToolInputs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), ".github", "workflows", "formal-verification.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ ID, Run string }
		}
	}
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, step := range workflow.Jobs["detect"].Steps {
		if step.ID == "detect" {
			script = step.Run
		}
	}
	if script == "" {
		t.Fatal("formal workflow detection script is missing")
	}
	for _, tc := range []struct {
		path              string
		relevant, mutants bool
	}{
		{"scripts/ci/pinned-tools.tsv", true, false},
		{"scripts/ci/fetch-verified.sh", true, false},
		{"Makefile", true, true},
		{".github/workflows/formal-verification.yaml", true, true},
		{"scripts/ci/check-counterexamples.sh", true, false},
		{"scripts/gen-tla-state-tests.py", true, false},
		{"formal/EngineWake.tla", true, true},
		{"internal/controller/wake_tla_states_data_test.go", true, true},
		{"internal/controller/engine_reconcile.go", false, true},
		{"docs/architecture.mdx", false, false},
		{"", true, true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "output")
			// Exercise the workflow's script using a changed-file response without
			// contacting GitHub or depending on credentials.
			cmd := exec.CommandContext(t.Context(), "bash", "-e", "-c",
				"gh() { printf '%s\\n' \"$CHANGED_FILES\"; }\n"+script)
			cmd.Env = append(os.Environ(), "EVENT_NAME=pull_request", "HEAD_REF=test",
				"HEAD_REPO=example/repo", "REPO=example/repo", "PR_AUTHOR=contributor",
				"BASE_SHA=base", "HEAD_SHA=head", "CHANGED_FILES="+tc.path,
				"GITHUB_OUTPUT="+outputPath)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("detection script: %v: %s", err, output)
			}
			output, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]bool{"relevant": tc.relevant, "mutants": tc.mutants} {
				if !strings.Contains(string(output), key+"="+strconv.FormatBool(want)+"\n") {
					t.Errorf("%s=%v not found in workflow outputs: %s", key, want, output)
				}
			}
		})
	}
}
