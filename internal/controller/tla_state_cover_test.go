/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// One definition of the three operations every TLA+ state-cover harness needs:
// compare a projected state, test closure membership, and render a closure for a
// failure message. The engine, instance and rotation harnesses each carried
// their own copy, and the copies had drifted — the instance harness compared
// projections with `==` while the other two used reflect.DeepEqual behind a
// named helper documenting why. `==` happened to be equivalent for a struct of
// four comparable fields, so nothing was broken, but "equivalent by accident"
// is the state that produces the real gap next time a projection grows a field.
//
// Generic over the projected state type because that type is what the generated
// fixtures define per spec (tlaState, tlaInstanceState, tlaRotationState) and it
// is the only thing that differs.

import (
	"fmt"
	"reflect"
)

// tlaProjectionEqual compares two projected TLA+ states by reflection
// deliberately.
//
// A hand-written field-by-field comparison is weakenable in a way nothing
// catches: drop a field and the closure check silently accepts states the model
// distinguishes (ClassReady was in fact missing from the engine comparison until
// this was written). `make formal-verify` cannot catch it either — it asserts
// the committed fixture matches the generator's output, not that the fixture is
// still being compared in full. reflect.DeepEqual covers every field, including
// arrays and slices, so narrowing the comparison now requires deleting a field
// from the projection struct — which the generator owns, so it shows up as a
// generator change and a fixture diff.
func tlaProjectionEqual[S any](a, b S) bool {
	return reflect.DeepEqual(a, b)
}

// tlaClosureContains reports whether `actual` is one of the TLA+ states the
// model considers reachable from a test case's starting state. A real Reconcile
// may perform several model sub-steps in one shot (the spec models reconciles
// atomically per sub-action; the implementation batches), so the resulting state
// is checked for closure membership rather than equality with any single
// specific successor. The closure includes the starting state itself only when
// the model permits a stutter there (no reconciler action enabled or a self-loop
// edge); otherwise a no-op Reconcile is rejected.
//
// closureIDs are indices into `pool`, the fixture's state pool.
func tlaClosureContains[S any](pool []S, closureIDs []int, actual S) bool {
	for _, id := range closureIDs {
		if tlaProjectionEqual(pool[id], actual) {
			return true
		}
	}
	return false
}

// tlaClosureFormatLimit caps how many closure entries a failure message renders.
// Engine closures reach into the hundreds; the first few plus a count is enough
// to see what the model expected without burying the rest of the failure.
const tlaClosureFormatLimit = 8

// tlaFormatClosure renders the first few entries of a closure index list for
// inclusion in a Fatalf message. Each entry is prefixed by its pool index so
// errors point straight back into the fixture's state pool.
func tlaFormatClosure[S any](pool []S, closureIDs []int) string {
	out := ""
	for i, id := range closureIDs {
		if i >= tlaClosureFormatLimit {
			out += fmt.Sprintf("    ... (%d more)\n", len(closureIDs)-tlaClosureFormatLimit)
			break
		}
		out += fmt.Sprintf("    [pool %d] %+v\n", id, pool[id])
	}
	return out
}
