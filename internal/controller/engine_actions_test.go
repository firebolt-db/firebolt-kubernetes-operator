/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Relates what the rapid harness can DO to what the spec says CAN HAPPEN.
//
// The invariant side of this is already single-sourced: the generator emits the
// spec's `Safety ==` conjuncts and engine_invariants_test.go fails when one has
// no Go counterpart. Nothing did the same for the *actions*. The state cover
// asserts that a reconcile lands somewhere the model allows, and the rapid walk
// now starts from model-reachable states, but the transitions the walk takes
// were unconstrained relative to the spec: a disjunct could be added to
// `Next ==` and no harness action would ever attempt it, or the harness could
// grow an action the model says nothing about, and every formal target stayed
// green either way.
//
// So the correspondence is declared here, in both directions, and asserted
// against the generated tlaEngineSpecActions and against the action set rapid
// itself will walk. It has to be declared rather than inferred from names: the
// two vocabularies deliberately differ (EnvPodsReady vs PodsBecomesReady), and
// the granularity does too -- the spec models each reconciler sub-step as its
// own atomic action while the harness exposes one whole-pass Reconcile.

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// tlaEngineActionMap declares which engineSim actions realize each disjunct of
// formal/FireboltEngine.tla's `Next ==`.
//
// Many-to-one is expected and fine. Every reconciler disjunct maps to the single
// Reconcile action because the spec splits one reconcile pass into the sub-steps
// it can perform atomically (EnsureSTS, EnsureService, Advance) while
// engineSim.Reconcile runs the whole pass; a pass that fires several sub-steps
// at once is exactly what the state cover's transitive closure allows for. One
// spec action mapping to several harness actions happens too: EnvChangeSpec
// bumps specVer and may flip specWantsStop, and different harness actions drive
// those two halves.
var tlaEngineActionMap = map[string][]string{
	// Environment: a user edits the engine. EnvChangeSpec's `specVer' =
	// specVer + 1` is the pod-template carriers, its `specWantsStop' \in
	// BOOLEAN` is the replica count.
	"EnvChangeSpec": {
		"ApplySpecChange",
		"ScaleReplicas",
		// Also a spec-template edit (ServiceAccountName), on top of the
		// class/engine merge arbitration that is its real reason to exist. The
		// merge layer itself has no model at all -- see formal/model-scope.tsv.
		"ApplyConflictingClassAndEngine",
		// Preset overlay identity (PresetHash) is another input to
		// the spec-content hash specVer abstracts over. The merge layer
		// itself stays UNMODELLED — see formal/model-scope.tsv.
		"ApplyPresetChange",
	},
	"EnvPodsReady":   {"PodsBecomesReady"},
	"EnvPodsDrained": {"DrainCompletes"},

	// The class-Ready gate. The compute layer cannot see a class's Ready
	// condition, only whether a resolved class was handed to it, so the harness
	// drives the gate by supplying or withholding classInfo -- which is what
	// resolveFireboltEngineClassInfo does in production.
	"EnvSetClassReady(TRUE)":  {"ApplyClassChange"},
	"EnvSetClassReady(FALSE)": {"ApplyClassUnready"},

	// The Preset fail-closed gate. Same shape as the class-Ready gate:
	// the compute layer cannot see Preset Ready / required / ambiguous,
	// only whether an overlay was handed to it.
	"EnvSetDefaultsReady(TRUE)":  {"ApplyPresetChange"},
	"EnvSetDefaultsReady(FALSE)": {"ApplyPresetUnready"},

	// GC of generations that are neither current, active, nor draining.
	// engineSim.Reconcile runs gcStaleResources on every pass, mirroring the
	// real controller, which runs the sweep in every phase.
	//
	// Worth knowing: GCOrphans is enabled in NO reachable state at the
	// configured bounds (grep the DOT dump -- it labels zero edges), because the
	// model's reconciler actions already drop the STSes they supersede. So the
	// state cover says nothing about the Go GC step; Inv_NoOrphanedResources in
	// the shared invariant registry is what actually holds a line under it, and
	// it is listed in goOnlyEngineInvariants for that reason.
	"GCOrphans": {"Reconcile"},

	// The reconciler proper. One pass, every sub-step.
	"ReconcileTerminal_Drift":           {"Reconcile"},
	"ReconcileCreating_SpecDrift":       {"Reconcile"},
	"ReconcileCreating_SpecDrift_AtMax": {"Reconcile"},
	"ReconcileCreating_EnsureSTS":       {"Reconcile"},
	"ReconcileCreating_EnsureService":   {"Reconcile"},
	"ReconcileCreating_Advance":         {"Reconcile"},
	"ReconcileSwitching_UpdateService":  {"Reconcile"},
	"ReconcileSwitching_Complete":       {"Reconcile"},
	"ReconcileDraining_Complete":        {"Reconcile"},
	"ReconcileCleaning":                 {"Reconcile"},
}

// tlaEngineSpecOnlyActions are `Next ==` disjuncts with no engineSim
// counterpart, and why. Every one of them is a transition the compute layer this
// harness drives cannot perform, not a gap in the harness.
var tlaEngineSpecOnlyActions = map[string]string{
	"ReconcileInit": "seeds phase=\"\" -> creating, which happens in the outer " +
		"Reconcile (engine_controller.go), not in computeEngineReconcile: the " +
		"compute layer routes \"\" into computeStable instead. The generator drops " +
		"uninitialized states from the fixture for the same reason, and " +
		"engine_outer_property_test.go (build tag outerharness) is what walks " +
		"the outer loop",

	"EnvSetInstanceReady(TRUE)": "the instance gate lives in the outer Reconcile; " +
		"engineSim has no instanceReady field at all -- projectEngineSim takes it " +
		"as a parameter and carries it through unchanged, and tlaShouldGateOut " +
		"skips the states where the gate would be shut",
	"EnvSetInstanceReady(FALSE)": "same as EnvSetInstanceReady(TRUE): the gate is " +
		"enforced above the compute layer",

	"EnvSetGatesOpen": "liveness scaffolding with no safety content. It drives " +
		"instanceReady, classReady, and defaultsReady TRUE in one step purely so " +
		"weak fairness can force a moment when all three gates are open at once; " +
		"independent fairness on the per-flag actions only makes each TRUE " +
		"infinitely often. There is no reconciler behavior to reproduce",
}

// tlaEngineHarnessOnlyActions are engineSim actions with no `Next ==` disjunct,
// and why. Each is a hazard the model deliberately does not represent, so the
// rapid harness is the only thing exercising it.
var tlaEngineHarnessOnlyActions = map[string]string{
	"CrashReconcile": "applies the resource writes but not the status update. The " +
		"spec models every reconciler action atomically, so it has no notion of a " +
		"partially-applied pass and nothing to map this to",
	"CrashAtPrefix": "same hazard as CrashReconcile at the four earlier side-effect " +
		"prefixes (the CrashAfter* points in crash_points.go)",
	"CacheCatchesUp": "informer-cache lag. The spec has one view of the cluster; " +
		"the api/cache split is a Go-side concern with no modeled variable",
	"DeleteEngine": "the CR being deleted mid-flight (reconcileDelete stripping " +
		"generation-scoped objects and then the finalizer). The spec models an " +
		"engine that exists for the whole behavior and has no deletion action",
}

// engineHarnessActions returns the action names rapid will actually walk, asked
// of rapid itself rather than reflected over by hand: rapid.StateMachineActions
// is what TestEngineStateMachine calls, so this cannot disagree with the walk.
// The empty key is rapid's slot for the Check invariant, not an action.
func engineHarnessActions(t *testing.T) []string {
	t.Helper()
	names := make([]string, 0, 16)
	for name := range rapid.StateMachineActions(&engineSim{}) {
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// TestEngineActionsMatchSpec is the anti-drift guard for the action sets: the
// spec's Next disjuncts and engineSim's rapid actions must account for each
// other, modulo the two explicitly-reasoned exemption lists.
func TestEngineActionsMatchSpec(t *testing.T) {
	if len(tlaEngineSpecActions) == 0 {
		t.Fatal("tlaEngineSpecActions is empty: the generator stopped parsing the spec's Next relation")
	}

	harness := engineHarnessActions(t)
	known := make(map[string]bool, len(harness))
	for _, name := range harness {
		known[name] = true
	}

	// Forward: every spec action is mapped or exempted, and never both.
	for _, action := range tlaEngineSpecActions {
		targets, isMapped := tlaEngineActionMap[action]
		reason, isExempt := tlaEngineSpecOnlyActions[action]
		switch {
		case isMapped && isExempt:
			t.Errorf("%s is both mapped to %v and listed in tlaEngineSpecOnlyActions. Pick one: "+
				"a spec action the harness performs is not spec-only.", action, targets)
		case isExempt:
			if strings.TrimSpace(reason) == "" {
				t.Errorf("tlaEngineSpecOnlyActions[%q] has an empty reason. The point of the list is the reason.", action)
			}
		case isMapped:
			if len(targets) == 0 {
				t.Errorf("tlaEngineActionMap[%q] maps to no harness action. Use tlaEngineSpecOnlyActions "+
					"with a reason instead of an empty mapping.", action)
			}
			for _, target := range targets {
				if !known[target] {
					t.Errorf("formal/FireboltEngine.tla's %s is mapped to harness action %q, which engineSim "+
						"does not expose (rapid walks %v). Either the action was renamed or the mapping is stale.",
						action, target, harness)
				}
			}
		default:
			t.Errorf("formal/FireboltEngine.tla disjoins %s into Next but nothing on the Go side claims it.\n"+
				"Add it to tlaEngineActionMap if a harness action performs it, or to tlaEngineSpecOnlyActions "+
				"with the reason the harness cannot. Do not leave it unclaimed: an unattempted transition is "+
				"state space the rapid walk silently never enters.", action)
		}
	}

	// Reverse: every harness action maps back to a spec action or is declared
	// harness-only. Without this, the harness can grow behavior the model says
	// nothing about and nothing points that out.
	targeted := make(map[string]bool, len(harness))
	for _, targets := range tlaEngineActionMap {
		for _, target := range targets {
			targeted[target] = true
		}
	}
	for _, action := range harness {
		reason, isExempt := tlaEngineHarnessOnlyActions[action]
		switch {
		case targeted[action] && isExempt:
			t.Errorf("engineSim.%s is both a mapping target and listed in tlaEngineHarnessOnlyActions. Pick one.", action)
		case isExempt:
			if strings.TrimSpace(reason) == "" {
				t.Errorf("tlaEngineHarnessOnlyActions[%q] has an empty reason. The point of the list is the reason.", action)
			}
		case targeted[action]:
			// mapped; nothing to assert beyond the forward direction
		default:
			t.Errorf("engineSim exposes the rapid action %q, which no spec action maps to and which is not "+
				"listed in tlaEngineHarnessOnlyActions.\nEither map it to the `Next ==` disjunct it realizes, "+
				"or record why the model does not represent it.", action)
		}
	}

	// Stale keys in either direction: an action renamed on one side leaves a map
	// entry that would otherwise sit there looking like coverage.
	specActions := make(map[string]bool, len(tlaEngineSpecActions))
	for _, action := range tlaEngineSpecActions {
		specActions[action] = true
	}
	for _, action := range mapKeys(tlaEngineActionMap) {
		if !specActions[action] {
			t.Errorf("tlaEngineActionMap has an entry for %q, which is not a disjunct of the spec's Next. "+
				"It was probably renamed in formal/FireboltEngine.tla; fix the key rather than leaving "+
				"dead coverage.", action)
		}
	}
	for _, action := range mapKeys(tlaEngineSpecOnlyActions) {
		if !specActions[action] {
			t.Errorf("tlaEngineSpecOnlyActions has an entry for %q, which is not a disjunct of the spec's "+
				"Next. It was probably renamed in formal/FireboltEngine.tla; fix the key rather than "+
				"leaving dead coverage.", action)
		}
	}
	for _, action := range mapKeys(tlaEngineHarnessOnlyActions) {
		if !known[action] {
			t.Errorf("tlaEngineHarnessOnlyActions has an entry for %q, which engineSim does not expose. "+
				"It was probably renamed; fix the key rather than leaving dead coverage.", action)
		}
	}

	t.Logf("%d spec actions, %d harness actions (%d spec-only, %d harness-only)",
		len(tlaEngineSpecActions), len(harness),
		len(tlaEngineSpecOnlyActions), len(tlaEngineHarnessOnlyActions))
}

// tlaActionCoverage records, per modeled machine, whether anything checks that
// its action set corresponds to a harness's -- and when nothing does, why.
//
// The list exists so an uncovered spec is a visible decision rather than a
// silence. TestTLAActionCoverageIsDeclared reads formal/ and fails on a spec
// that is not named here, so adding a machine forces the decision to be made.
type tlaActionCoverage struct {
	Spec string
	// Empty when no harness action set is checked against this spec.
	CheckedBy string
	Reason    string
}

var tlaActionCoverageLedger = []tlaActionCoverage{
	{
		Spec:      "FireboltEngine.tla",
		CheckedBy: "TestEngineActionsMatchSpec (engineSim, engine_property_test.go)",
		Reason:    "both directions asserted against the generated tlaEngineSpecActions",
	},
	{
		Spec: "FireboltInstance.tla",
		Reason: "out of scope by decision, not by omission. Its Next has three " +
			"disjuncts, one of them existentially quantified over Components, and " +
			"naming the instantiations of a quantified action means deciding a " +
			"convention for no real gain over the three-action instanceSim the " +
			"state cover already drives. The generator's Next parser rejects the " +
			"shape outright rather than emitting a list that quietly omits it",
	},
	{
		Spec: "EngineWake.tla",
		Reason: "the Go side of this spec is one pure function, not a harness with " +
			"an action vocabulary: wake_tla_state_test.go calls " +
			"computeAutoStopDecision directly against a materialized state. Its " +
			"eight Reconcile* disjuncts are the arms of that one function and are " +
			"covered by the state cover collectively; its environment disjuncts " +
			"(the clock, the poller, the agent) are things no Go code in this " +
			"package performs. Covering the action set would mean writing a rapid " +
			"sim for the wake protocol, which is worth doing when the agent side " +
			"is bound too -- see formal/model-scope.tsv",
	},
	{
		Spec: "WakeAgentHold.tla",
		Reason: "no Go binding at all, by decision: its subject is unexported " +
			"in-memory bookkeeping in internal/wakeagent, which cannot import this " +
			"package. formal/model-scope.tsv carries the reason and what unblocking " +
			"it would take. Its Next also uses the existentially-quantified shape " +
			"the generator's parser rejects",
	},
	{
		Spec: "SigningKeyRotation.tla",
		Reason: "no rapid/sim harness exists to correspond to. " +
			"rotation_tla_state_test.go calls production stepSigningKeyRotation " +
			"directly against a materialized state, so there is no harness action " +
			"vocabulary on the Go side. Writing one is the prerequisite for " +
			"covering this spec, and its Next also uses the quantified shape the " +
			"parser rejects",
	},
}

// TestTLAActionCoverageIsDeclared fails when formal/ holds a spec the ledger
// above does not mention, so a new modeled machine cannot be added without
// someone deciding whether its actions are checked.
func TestTLAActionCoverageIsDeclared(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed, cannot locate formal/")
	}
	formalDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "formal")
	entries, err := os.ReadDir(formalDir)
	if err != nil {
		t.Fatalf("reading %s: %v", formalDir, err)
	}

	declared := make(map[string]bool, len(tlaActionCoverageLedger))
	for _, entry := range tlaActionCoverageLedger {
		if entry.CheckedBy == "" && strings.TrimSpace(entry.Reason) == "" {
			t.Errorf("%s is listed with neither a checker nor a reason, which is the silence this "+
				"ledger exists to prevent", entry.Spec)
		}
		declared[entry.Spec] = true
	}

	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".tla") {
			continue
		}
		// TLC writes a trace-exploration spec beside a spec whenever a run
		// reports a violation (formal-check-counterexample provokes six of
		// them). They are gitignored scratch, not modeled machines.
		if strings.Contains(name, "_TTrace_") {
			continue
		}
		found++
		if !declared[name] {
			t.Errorf("formal/%s has no entry in tlaActionCoverageLedger. Decide whether its `Next ==` "+
				"disjuncts are checked against a harness's action set, and record the decision -- "+
				"including a decision not to.", name)
		}
	}
	if found == 0 {
		t.Fatalf("no .tla specs found under %s: the check would pass vacuously", formalDir)
	}
	for _, entry := range tlaActionCoverageLedger {
		if _, err := os.Stat(filepath.Join(formalDir, entry.Spec)); err != nil {
			t.Errorf("tlaActionCoverageLedger names formal/%s, which does not exist: %v", entry.Spec, err)
		}
	}
	t.Logf("%d specs, %d with their action set checked", found, countChecked())
}

func countChecked() int {
	n := 0
	for _, entry := range tlaActionCoverageLedger {
		if entry.CheckedBy != "" {
			n++
		}
	}
	return n
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
