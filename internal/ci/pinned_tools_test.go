/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package ci holds tests that pin repository-level CI invariants. It carries
// no production code.
package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// Guards the supply-chain posture of third-party binaries that CI executes.
// Runners that download kind / yq / helm-docs / tla2tools hold a GITHUB_TOKEN
// and run PR code, so a re-cut upstream release tag is arbitrary code
// execution unless the bytes are checked. scripts/ci/fetch-verified.sh is the
// only sanctioned way to fetch them; these tests keep the pin manifest
// well-formed and stop a raw `curl | tar` from creeping back in.

const (
	manifestRel = "scripts/ci/pinned-tools.tsv"
	helperRel   = "scripts/ci/fetch-verified.sh"

	// A download line may opt out of the helper when it does not fetch an
	// executable artifact (an API call, say). The reason is mandatory so the
	// exemption stays reviewable.
	exemptMarker = "fetch-verified-exempt:"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve caller for repo root")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

type pinnedTool struct {
	name   string
	arch   string
	sha256 string
	url    string
	line   int
}

func readManifest(t *testing.T) []pinnedTool {
	t.Helper()
	path := filepath.Join(repoRoot(t), manifestRel)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", manifestRel, err)
	}
	var tools []pinnedTool
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 4 {
			t.Fatalf("%s:%d: expected 4 whitespace-separated fields (name arch sha256 url), got %d: %q",
				manifestRel, i+1, len(fields), trimmed)
		}
		tools = append(tools, pinnedTool{
			name: fields[0], arch: fields[1], sha256: fields[2], url: fields[3], line: i + 1,
		})
	}
	if len(tools) == 0 {
		t.Fatalf("%s contains no pinned tools", manifestRel)
	}
	return tools
}

func TestPinnedToolsManifestIsWellFormed(t *testing.T) {
	sha256Pattern := regexp.MustCompile(`^[0-9a-f]{64}$`)
	validArch := map[string]bool{"amd64": true, "arm64": true, "any": true}

	seen := map[string]int{}
	archKinds := map[string]map[string]bool{}

	for _, tool := range readManifest(t) {
		if !validArch[tool.arch] {
			t.Errorf("%s:%d: %s has arch %q; want amd64, arm64 or any",
				manifestRel, tool.line, tool.name, tool.arch)
		}
		if !sha256Pattern.MatchString(tool.sha256) {
			t.Errorf("%s:%d: %s has digest %q; want 64 lowercase hex characters",
				manifestRel, tool.line, tool.name, tool.sha256)
		}
		// Plain HTTP would let a network position swap the artifact before the
		// digest check ever sees the real one.
		if !strings.HasPrefix(tool.url, "https://") {
			t.Errorf("%s:%d: %s downloads over %q; want an https:// URL",
				manifestRel, tool.line, tool.name, tool.url)
		}

		key := tool.name + "/" + tool.arch
		if prev, dup := seen[key]; dup {
			t.Errorf("%s:%d: duplicate entry for %s (already on line %d)",
				manifestRel, tool.line, key, prev)
		}
		seen[key] = tool.line

		if archKinds[tool.name] == nil {
			archKinds[tool.name] = map[string]bool{}
		}
		archKinds[tool.name][tool.arch] = true
	}

	// fetch-verified.sh selects a row by (name, arch-or-any) and refuses to
	// guess when both shapes match, so catch the ambiguity at test time
	// instead of mid-pipeline.
	for name, arches := range archKinds {
		if arches["any"] && len(arches) > 1 {
			t.Errorf("%s: %s mixes an 'any' row with architecture-specific rows; "+
				"fetch-verified.sh cannot disambiguate those", manifestRel, name)
		}
	}
}

func TestFetchVerifiedHelperIsExecutable(t *testing.T) {
	path := filepath.Join(repoRoot(t), helperRel)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", helperRel, err)
	}
	// Workflows invoke the script directly, so a lost exec bit breaks CI.
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is not executable (mode %v); run: chmod +x %s", helperRel, info.Mode().Perm(), helperRel)
	}
}

// scannedFiles returns the workflow, Makefile and shell-script paths whose
// download lines are subject to the helper rule.
func scannedFiles(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)

	files := []string{filepath.Join(root, "Makefile")}
	for _, pattern := range []string{
		filepath.Join(root, ".github", "workflows", "*.y*ml"),
		filepath.Join(root, "scripts", "*.sh"),
		filepath.Join(root, "scripts", "ci", "*.sh"),
		filepath.Join(root, "scripts", "ci", "lib", "*.sh"),
	} {
		matched, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		files = append(files, matched...)
	}
	return files
}

func TestNoUnverifiedBinaryDownloads(t *testing.T) {
	root := repoRoot(t)
	helperPath := filepath.Join(root, helperRel)
	downloader := regexp.MustCompile(`\b(curl|wget)\b`)

	for _, path := range scannedFiles(t) {
		// The helper is the one place allowed to reach out to the network.
		if path == helperPath {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if !downloader.MatchString(line) || !strings.Contains(line, "https://") {
				continue
			}
			if strings.Contains(line, exemptMarker) {
				continue
			}
			t.Errorf("%s:%d fetches over https with curl/wget outside the digest-verified helper:\n  %s\n"+
				"Pin the artifact in %s and fetch it with `%s <name> <dest>`, or annotate the line "+
				"with `# %s <reason>` if it is not an executable artifact.",
				rel, i+1, strings.TrimSpace(line), manifestRel, helperRel, exemptMarker)
		}
	}
}

func TestFetchVerifiedCallsResolveToPinnedTools(t *testing.T) {
	known := map[string]bool{}
	for _, tool := range readManifest(t) {
		known[tool.name] = true
	}

	root := repoRoot(t)
	helperPath := filepath.Join(root, helperRel)
	call := regexp.MustCompile(`fetch-verified\.sh\s+([A-Za-z0-9._-]+)`)

	calls := 0
	for _, path := range scannedFiles(t) {
		if path == helperPath {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for i, line := range strings.Split(string(raw), "\n") {
			match := call.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			calls++
			if !known[match[1]] {
				t.Errorf("%s:%d requests unpinned tool %q; add it to %s",
					rel, i+1, match[1], manifestRel)
			}
		}
	}

	// A zero count means the scan lost sight of the call sites, which would
	// also silently defang TestNoUnverifiedBinaryDownloads.
	if calls == 0 {
		t.Fatal("found no fetch-verified.sh call sites; scannedFiles is looking in the wrong place")
	}
}
