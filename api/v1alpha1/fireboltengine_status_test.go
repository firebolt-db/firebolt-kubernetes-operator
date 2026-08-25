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
	"encoding/json"
	"strings"
	"testing"
)

// A zero readyReplicas must reach the wire. Zero is an observation here —
// stopped, or nothing serving yet — and a client has to be able to tell it
// from the field being absent, which means an operator too old to report it.
// `omitempty` would collapse those two into one, so this guards the tag.
func TestReadyReplicasSerializesZero(t *testing.T) {
	encoded, err := json.Marshal(FireboltEngineStatus{ReadyReplicas: 0})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"readyReplicas":0`) {
		t.Errorf("zero readyReplicas was omitted from %s", encoded)
	}
}
