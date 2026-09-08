// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustChange(t *testing.T, changed bool, err error) {
	t.Helper()
	if err != nil || !changed {
		t.Fatalf("mutation: changed=%v error=%v", changed, err)
	}
}

func testRoute(generation int) Route {
	return Route{EngineUID: "engine-uid", Generation: generation,
		Authority: GenerationServiceName("engine-uid", generation), Enabled: true}
}

func TestRetirementCapturesRegistrationOrder(t *testing.T) {
	state := NewState("instance")
	changed, err := SetRoute(&state, "query", testRoute(1))
	mustChange(t, changed, err)
	changed, err = RegisterSession(&state, "before", "boot-a")
	mustChange(t, changed, err)
	old := state.Routes["query"]
	changed, err = SetRoute(&state, "query", testRoute(2))
	mustChange(t, changed, err)
	fence := state.Revision
	changed, err = RegisterSession(&state, "after", "boot-b")
	mustChange(t, changed, err)
	retirement := state.Retirements[old.Key()]
	if len(retirement.Holders) != 1 || retirement.Holders["before"] != "boot-a" || retirement.RequiredRevision != fence {
		t.Fatalf("holder snapshot was changed by later registration: %+v", retirement)
	}
	if CanRetire(retirement, nil, nil) {
		t.Fatal("unreachable holder must block retirement")
	}
	reports := map[string]Report{"before": {PodUID: "before", SessionID: "boot-a", Registered: true, AppliedRevision: fence, Outstanding: map[string]Count{}}}
	if !CanRetire(retirement, reports, nil) {
		t.Fatal("post-fence zero report should retire without a later session's report")
	}
}

func TestRetirementRejectsUncertainAcknowledgements(t *testing.T) {
	old := testRoute(1)
	old.Epoch = 2
	retirement := Retirement{Route: old, RequiredRevision: 4, Holders: map[string]string{"pod": "boot"}}
	valid := Report{PodUID: "pod", SessionID: "boot", AppliedRevision: 4, Registered: true, Outstanding: map[string]Count{}}
	tests := []struct {
		name string
		edit func(*Report)
	}{
		{"stale revision", func(r *Report) { r.AppliedRevision = 3 }},
		{"restarted agent", func(r *Report) { r.SessionID = "other-boot" }},
		{"wrong pod", func(r *Report) { r.PodUID = "other-pod" }},
		{"not registered", func(r *Report) { r.Registered = false }},
		{"missing counts", func(r *Report) { r.Outstanding = nil }},
		{"active request", func(r *Report) { r.Outstanding = map[string]Count{old.Key(): {Active: 1}} }},
		{"lost accounting", func(r *Report) { r.Outstanding = map[string]Count{old.Key(): {Unknown: 1}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := valid
			tt.edit(&report)
			if CanRetire(retirement, map[string]Report{"pod": report}, nil) {
				t.Fatal("uncertain holder must block retirement")
			}
			if !CanRetire(retirement, map[string]Report{"pod": report}, map[string]bool{"pod": true}) {
				t.Fatal("positive process termination must resolve that holder")
			}
		})
	}
}

func TestGarbageCollectionRequiresPositiveEvidence(t *testing.T) {
	state := NewState("instance")
	changed, err := SetRoute(&state, "query", testRoute(1))
	mustChange(t, changed, err)
	changed, err = RegisterSession(&state, "pod", "boot")
	mustChange(t, changed, err)
	old := state.Routes["query"]
	changed, err = Withdraw(&state, "query")
	mustChange(t, changed, err)
	if changed, err := ForgetStoppedSession(&state, "pod", false); changed || err == nil {
		t.Fatal("missing stop evidence must not clear obligations")
	}
	if changed, err := CompleteRetirement(&state, old.Key(), nil, nil); changed || err == nil {
		t.Fatal("unacknowledged retirement must not be forgotten")
	}
	if changed, err := ForgetEngine(&state, "query"); changed || err == nil {
		t.Fatal("outstanding retirement must retain engine route")
	}
	if changed, err := SetRoute(&state, "query", testRoute(1)); changed || err == nil {
		t.Fatal("withdrawn generation must not be reauthorized")
	}
	changed, err = ForgetStoppedSession(&state, "pod", true)
	mustChange(t, changed, err)
	if len(state.Sessions) != 0 || len(state.Retirements[old.Key()].Holders) != 0 {
		t.Fatal("stop evidence must durably clear all obligations")
	}
	changed, err = CompleteRetirement(&state, old.Key(), nil, nil)
	mustChange(t, changed, err)
	changed, err = ForgetEngine(&state, "query")
	mustChange(t, changed, err)
	if len(state.Routes) != 0 || len(state.Retirements) != 0 {
		t.Fatal("completed engine must be garbage collected")
	}
}

func TestStaleControllerCannotRestoreOlderGeneration(t *testing.T) {
	state := NewState("instance")
	changed, err := SetRoute(&state, "query", testRoute(2))
	mustChange(t, changed, err)
	revision := state.Revision
	if changed, err := SetRoute(&state, "query", testRoute(1)); changed || err == nil || state.Revision != revision {
		t.Fatal("stale reconciliation must not roll routing backwards")
	}
}

func TestInitialGenerationZeroHasRoutingAuthority(t *testing.T) {
	state := NewState("instance")
	changed, err := SetRoute(&state, "query", testRoute(0))
	mustChange(t, changed, err)
	if state.Routes["query"].Generation != 0 || state.Routes["query"].Epoch == 0 {
		t.Fatal("initial generation zero must receive a nonzero admission epoch")
	}
	if _, err := Encode(state); err != nil {
		t.Fatalf("initial generation cannot be persisted: %v", err)
	}
	if changed, err := SetRoute(&state, "query", testRoute(-1)); changed || err == nil {
		t.Fatal("uninitialized negative generation must not receive authority")
	}
}

func TestInstanceClosureRejectsConcurrentPublicationAndSurvivesRestart(t *testing.T) {
	state := NewState("instance")
	changed, err := SetRoute(&state, "query", testRoute(0))
	mustChange(t, changed, err)
	changed, err = RegisterSession(&state, "pod", "boot")
	mustChange(t, changed, err)
	old := state.Routes["query"]
	changed, err = CloseInstance(&state)
	mustChange(t, changed, err)
	if state.Routes["query"].Enabled || !state.Closed || state.Retirements[old.Key()].Holders["pod"] != "boot" {
		t.Fatal("instance closure must atomically capture holders and disable admission")
	}
	data, err := Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	state, err = Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"query", "another-engine"} {
		if changed, err := SetRoute(&state, name, testRoute(100)); changed || err == nil {
			t.Fatal("stale engine controller must not publish any generation after instance closure")
		}
	}
	if changed, err := CloseInstance(&state); changed || err != nil {
		t.Fatal("instance closure must be idempotent")
	}
	changed, err = RegisterSession(&state, "late-pod", "late-boot")
	mustChange(t, changed, err)
	if state.Routes["query"].Enabled {
		t.Fatal("late registration reopened the instance")
	}
}

func TestRestartAndWithdraw(t *testing.T) {
	state := NewState("instance")
	changed, err := RegisterSession(&state, "pod", "boot")
	mustChange(t, changed, err)
	changed, err = SetRoute(&state, "query", testRoute(1))
	mustChange(t, changed, err)
	old := state.Routes["query"]
	encoded, err := Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := RegisterSession(&restored, "pod", "other-boot"); changed || err == nil {
		t.Fatal("agent restart must not replace its predecessor's identity")
	}
	if changed, err := RegisterSession(&restored, "pod", "boot"); changed || err != nil {
		t.Fatal("same session registration must be idempotent")
	}
	changed, err = Withdraw(&restored, "query")
	mustChange(t, changed, err)
	if restored.Routes["query"].Enabled || restored.Routes["query"].Epoch <= old.Epoch {
		t.Fatal("withdrawal must fence the old epoch")
	}
	if _, exists := restored.Retirements[old.Key()]; !exists {
		t.Fatal("withdrawal lost holders across operator restart")
	}
	revision := restored.Revision
	if changed, err := Withdraw(&restored, "query"); changed || err != nil || restored.Revision != revision {
		t.Fatal("repeated withdrawal must not create additional epochs")
	}
}

func TestMalformedStateFailsClosed(t *testing.T) {
	for _, input := range []string{"", "null", "{}", `{"revision":1,"instanceUID":"i"}`} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Fatalf("accepted malformed state %q", input)
		}
	}
	state := NewState("instance")
	changed, err := SetRoute(&state, "query", testRoute(1))
	mustChange(t, changed, err)
	route := state.Routes["query"]
	state.Retirements[route.Key()] = Retirement{Route: route, RequiredRevision: state.Revision + 1, Holders: map[string]string{}}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(encoded); err == nil {
		t.Fatal("accepted inconsistent fence and current route")
	}
	if CanRetire(Retirement{}, nil, nil) {
		t.Fatal("empty retirement must fail closed")
	}
}

func TestResourceNamesAreStableAndBounded(t *testing.T) {
	name := ConfigMapName(strings.Repeat("instance.", 40))
	if len(name) > 63 || strings.Contains(name, ".") || name != ConfigMapName(strings.Repeat("instance.", 40)) {
		t.Fatalf("invalid stable resource name: %q", name)
	}
	if GenerationServiceName("old-uid", 1) == GenerationServiceName("new-uid", 1) ||
		GenerationServiceName("old-uid", 1) == GenerationServiceName("old-uid", 2) {
		t.Fatal("generation destinations must not alias recreated engines or different generations")
	}
}
