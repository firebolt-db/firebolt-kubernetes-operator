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

import (
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestMintInstanceID_UppercaseWhileFloorEmpty(t *testing.T) {
	orig := CanonicalInstanceIDImageFloor
	CanonicalInstanceIDImageFloor = ""
	t.Cleanup(func() { CanonicalInstanceIDImageFloor = orig })

	id := MintInstanceID()
	if len(id) != 26 {
		t.Fatalf("MintInstanceID length = %d, want 26: %q", len(id), id)
	}
	if id != strings.ToUpper(id) {
		t.Errorf("MintInstanceID %q is not uppercase while the canonicalize floor is empty", id)
	}
	if _, err := ulid.Parse(id); err != nil {
		t.Errorf("MintInstanceID %q is not a ULID: %v", id, err)
	}
}

func TestMintInstanceID_LowercaseWhenFloorSet(t *testing.T) {
	orig := CanonicalInstanceIDImageFloor
	CanonicalInstanceIDImageFloor = "release-5.1.0-pre.0.20260828000000.deadbeef"
	t.Cleanup(func() { CanonicalInstanceIDImageFloor = orig })

	id := MintInstanceID()
	if len(id) != 26 {
		t.Fatalf("MintInstanceID length = %d, want 26: %q", len(id), id)
	}
	if id != strings.ToLower(id) {
		t.Errorf("MintInstanceID %q is not lowercase once the canonicalize floor is set", id)
	}
	if _, err := ulid.Parse(id); err != nil {
		t.Errorf("MintInstanceID %q is not a ULID: %v", id, err)
	}
}
