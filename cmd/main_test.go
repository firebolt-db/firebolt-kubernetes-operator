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

package main

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseEngineResourceBounds_AllEmptyIsZeroValue(t *testing.T) {
	got, err := parseEngineResourceBounds("", "", "")
	if err != nil {
		t.Fatalf("parseEngineResourceBounds: empty flags should not error, got %v", err)
	}
	if !got.IsEmpty() {
		t.Fatalf("parseEngineResourceBounds: all-empty should yield IsEmpty(); got %+v", got)
	}
}

func TestParseEngineResourceBounds_ParsesValidQuantities(t *testing.T) {
	got, err := parseEngineResourceBounds("32", "256Gi", "10Ti")
	if err != nil {
		t.Fatalf("parseEngineResourceBounds: %v", err)
	}
	if got.MaxCPU.Cmp(resource.MustParse("32")) != 0 {
		t.Errorf("MaxCPU = %s, want 32", got.MaxCPU.String())
	}
	if got.MaxMemory.Cmp(resource.MustParse("256Gi")) != 0 {
		t.Errorf("MaxMemory = %s, want 256Gi", got.MaxMemory.String())
	}
	if got.MaxEphemeralStorage.Cmp(resource.MustParse("10Ti")) != 0 {
		t.Errorf("MaxEphemeralStorage = %s, want 10Ti", got.MaxEphemeralStorage.String())
	}
}

// TestParseEngineResourceBounds_MalformedFails locks in the
// fail-fast semantic: a typo in a flag must surface at process start
// rather than silently leaving that dimension unbounded. The first
// failing flag name appears in the error so the operator can be
// debugged from the manager log alone.
func TestParseEngineResourceBounds_MalformedFails(t *testing.T) {
	cases := []struct {
		name          string
		cpu, mem, eph string
		wantFragment  string
	}{
		{"cpu", "not-a-quantity", "", "", "--engine-max-cpu"},
		{"memory", "", "16GB", "", "--engine-max-memory"}, // GB (not Gi) is not a valid quantity
		{"ephemeral", "", "", "garbage", "--engine-max-ephemeral-storage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseEngineResourceBounds(tc.cpu, tc.mem, tc.eph)
			if err == nil {
				t.Fatalf("parseEngineResourceBounds: expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantFragment) {
				t.Errorf("error %q does not name the offending flag (%s)", err.Error(), tc.wantFragment)
			}
		})
	}
}

func TestParseNamespaces(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "alpha", []string{"alpha"}},
		{"two", "alpha,beta", []string{"alpha", "beta"}},
		{"three with whitespace", " alpha , beta , gamma ", []string{"alpha", "beta", "gamma"}},
		{"empty entries dropped", "alpha,,beta,", []string{"alpha", "beta"}},
		{"only commas yields nil-equivalent", ",,,", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseNamespaces(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("parseNamespaces(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseNamespaces(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
				}
			}
		})
	}
}
