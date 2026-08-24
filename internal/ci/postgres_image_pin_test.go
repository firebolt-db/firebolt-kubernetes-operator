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

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/distribution/reference"
)

// TestPostgresImageIsDigestPinned keeps the compiled internal-Postgres
// default on a tagged, digested ref. The digest must be the multi-arch
// manifest list (refresh with `docker buildx imagetools inspect
// postgres:16.15-alpine --format '{{.Manifest.Digest}}'`), not a
// single-platform blob. Both build variants share the pin so a
// IMAGE_VARIANT=dev operator does not silently float.
func TestPostgresImageIsDigestPinned(t *testing.T) {
	t.Parallel()

	files := []string{
		"config/images/defaults.latest.env",
		"config/images/defaults.dev.env",
	}
	var refs []string
	for _, rel := range files {
		val := postgresImageFromDefaults(t, rel)
		named, err := reference.ParseNormalizedNamed(val)
		if err != nil {
			t.Errorf("%s POSTGRES_IMAGE=%q: %v", rel, val, err)
			continue
		}
		tagged, isTagged := named.(reference.NamedTagged)
		digested, isDigested := named.(reference.Digested)
		if !isTagged || tagged.Tag() != "16.15-alpine" {
			t.Errorf("%s POSTGRES_IMAGE=%q: want tag 16.15-alpine", rel, val)
		}
		if !isDigested {
			t.Errorf("%s POSTGRES_IMAGE=%q: want a sha256 digest pin", rel, val)
			continue
		}
		if alg, enc := digested.Digest().Algorithm(), digested.Digest().Encoded(); alg != "sha256" || len(enc) != 64 {
			t.Errorf("%s POSTGRES_IMAGE=%q: digest %s:%s is not sha256 with 64 hex chars", rel, val, alg, enc)
		}
		refs = append(refs, val)
	}
	if len(refs) == 2 && refs[0] != refs[1] {
		t.Errorf("POSTGRES_IMAGE differs across variants: latest=%q dev=%q", refs[0], refs[1])
	}

	bootstrap := filepath.Join(repoRoot(t), "scripts", "ci", "bootstrap-postgres-firebolt-namespace.yaml")
	raw, err := os.ReadFile(bootstrap)
	if err != nil {
		t.Fatalf("read %s: %v", bootstrap, err)
	}
	if len(refs) > 0 && !strings.Contains(string(raw), refs[0]) {
		t.Errorf("%s does not pin the same POSTGRES_IMAGE %q", bootstrap, refs[0])
	}
}

func postgresImageFromDefaults(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "POSTGRES_IMAGE" {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("%s has no POSTGRES_IMAGE", rel)
	return ""
}
