//go:build integration

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitCommand(t *testing.T, args ...string) string {
	t.Helper()
	data, err := run("git", args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func writeChartFile(t *testing.T, name, data string) {
	t.Helper()
	path := filepath.Join(chartPath, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, "add", path)
}

func commit(t *testing.T) string {
	t.Helper()
	gitCommand(t, "commit", "-qm", "fix: update chart")
	return gitCommand(t, "rev-parse", "HEAD")
}

func fixture(t *testing.T) string {
	t.Helper()
	t.Chdir(t.TempDir())
	gitCommand(t, "init", "-q")
	gitCommand(t, "config", "user.name", "Test")
	gitCommand(t, "config", "user.email", "test@example.com")
	writeChartFile(t, "Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.0\n")
	writeChartFile(t, "templates/crd.yaml", fmt.Sprintf(crdTemplate, "1e3"))
	writeChartFile(t, ".helmignore", "json-schema/\n*.tmp\n")
	base := commit(t)
	gitCommand(t, "tag", tagPrefix+"0.1.0")
	return base
}

func TestReleaseCandidate(t *testing.T) {
	base := fixture(t)
	writeChartFile(t, "README.md", "Updated docs\n")
	docs := commit(t)
	if err := check(base, docs); err != nil {
		t.Fatalf("ordinary PR must pass: %v", err)
	}
	writeChartFile(t, "Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.1\ndescription: Updated docs\n")
	writeChartFile(t, "templates/crd.yaml", strings.ReplaceAll(fmt.Sprintf(crdTemplate, "1e3"), "docs", "documentation"))
	writeChartFile(t, "CHANGELOG.md", "Release notes\n")
	writeChartFile(t, "json-schema/generated.json", "{}")
	writeChartFile(t, "scratch.tmp", "ignored")
	cosmetic := commit(t)
	if err := check(base, cosmetic); err == nil || !strings.Contains(err.Error(), "no substantive change") {
		t.Fatalf("cosmetic release must be rejected, got %v", err)
	}

	// A real default change makes the same release candidate eligible.
	writeChartFile(t, "templates/crd.yaml", fmt.Sprintf(crdTemplate, "'1e3'"))
	substantive := commit(t)
	if err := check(base, substantive); err != nil {
		t.Fatalf("default type change must pass: %v", err)
	}
	writeChartFile(t, "templates/crd.yaml", fmt.Sprintf(crdTemplate, "2026-11-13"))
	date := commit(t)
	if err := check(base, date); err != nil {
		t.Fatalf("date string default must pass: %v", err)
	}

	// Restore the released content, then add another commit: use the candidate's
	// content, not commit messages, the working tree, or a moving branch name.
	writeChartFile(t, "templates/crd.yaml", fmt.Sprintf(crdTemplate, "1e3"))
	reverted := commit(t)
	writeChartFile(t, "README.md", "Later documentation\n")
	commit(t)
	if err := check(base, reverted); err == nil {
		t.Fatal("reverted candidate must be rejected")
	}
	if err := check(base, substantive); err != nil {
		t.Fatalf("fixed substantive candidate must remain valid after HEAD moves: %v", err)
	}
	if err := check(substantive, "HEAD"); err != nil {
		t.Fatalf("PR with unchanged chart version must pass: %v", err)
	}
}

func TestPackagedChanges(t *testing.T) {
	for _, tc := range []struct{ name, path, contents string }{
		{"installation", "Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.1\nkubeVersion: '>=1.30.0'\n"},
		{"manifest addition", "templates/config.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: example}\n"},
		{"included file", ".helmignore", "*.tmp\n"},
		{"values", "values.yaml", "enabled: true\n"},
		{"Helm helper", "templates/helper.tpl", "{{ define \"name\" }}example{{ end }}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := fixture(t)
			writeChartFile(t, "json-schema/generated.json", "{}")
			writeChartFile(t, "Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.1\n")
			writeChartFile(t, tc.path, tc.contents)
			candidate := commit(t)
			if err := check(base, candidate); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReleaseErrors(t *testing.T) {
	base := fixture(t)
	if err := check("", "HEAD"); err == nil {
		t.Fatal("missing base must fail")
	}
	writeChartFile(t, "Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.1\n")
	writeChartFile(t, "templates/crd.yaml", "broken: [")
	candidate := commit(t)
	if err := check(base, candidate); err == nil || !strings.Contains(err.Error(), "yaml:") {
		t.Fatalf("invalid candidate YAML must fail parsing, got %v", err)
	}
	gitCommand(t, "tag", "-d", tagPrefix+"0.1.0")
	if err := check(base, candidate); err == nil {
		t.Fatal("missing release history must fail")
	}
}

func TestNextReleaseUsesLatestTag(t *testing.T) {
	fixture(t)
	writeChartFile(t, "Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.2.0\n")
	writeChartFile(t, "templates/crd.yaml", fmt.Sprintf(crdTemplate, "'1e3'"))
	released := commit(t)
	gitCommand(t, "tag", tagPrefix+"0.2.0")
	writeChartFile(t, "Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.2.1\n")
	candidate := commit(t)
	if err := check(released, candidate); err == nil || !strings.Contains(err.Error(), tagPrefix+"0.2.0") {
		t.Fatalf("candidate must be compared with the new release, got %v", err)
	}
}
