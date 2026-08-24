#!/usr/bin/env python3
# Copyright 2026 Firebolt Analytics.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
"""Generate a TLA+ state-cover test fixture for one modelled state machine.

For each reachable state in a spec's TLC state graph, the generated Go fixture
records the state (projected to what the Go harness can materialise and observe)
plus the set of states reachable from it via zero or more consecutive
reconciler-only edges -- the "reconciler closure". The Go harness materialises
each state, runs one reconcile, and asserts the result lies in that closure.

Usage:

    gen-tla-state-tests.py --model engine \\
        --dot formal/FireboltEngine.dot \\
        --spec formal/FireboltEngine.tla \\
        --out internal/controller/engine_tla_states_data_test.go

`make formal-gen` runs one invocation per model; `make formal-verify` is the CI
guard that regenerates every fixture and fails on a diff.

One generator, one config per model
-----------------------------------

Every modelled machine needs the same six steps -- parse the DOT dump, split
edges into environment vs reconciler, compute closures, project states onto what
Go can see, order the output by content (TLC node IDs are not stable across
runs), and emit Go. Only the *projection* differs: which TLA+ variables to read,
how to decode them, which action labels belong to the environment, and what the
emitted types and variables are called.

So that is what MODELS holds: one SpecConfig per machine, and the pipeline below
is shared. It used to be three near-identical scripts sharing eight of ten
functions, which meant a fix to the closure or the ordering rule landed in one
copy and not the others.

Adding a machine is a new MODELS entry plus a `formal-gen` line in the Makefile.
Every config is handed the .tla path as well as the .dot, because some of them
read the spec text directly (`emit_invariants` parses `Safety ==`), and making
that per-model would put the decision in the Makefile instead of here.
"""

import argparse
import re
import sys
from collections import defaultdict
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Dict, FrozenSet, List, Set, Tuple

# A decoded DOT node: TLA+ variable name -> decoded value. What "decoded" means
# is per-model (SpecConfig.decode_value).
State = Dict[str, object]
# A state projected onto what the Go harness can observe. Hashable, so it doubles
# as the pool's dedupe key, and orderable, so it doubles as the output order.
StateKey = Tuple[object, ...]
# (source node, destination node, action label)
Edge = Tuple[int, int, str]
# Values interpolated into the Go header and the summary line (e.g. TLC bounds).
Ctx = Dict[str, object]


# ---------------------------------------------------------------------------
# DOT parsing
# ---------------------------------------------------------------------------

# DOT node and edge lines. The label is a quoted string in which DOT escapes
# backslash as \\, quote as \", and newline as \n. The regex must accept those
# escape sequences inside the label.
LABEL_BODY = r'(?:[^"\\]|\\.)*'
NODE_RE = re.compile(r'^(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')
EDGE_RE = re.compile(r'^(-?\d+)\s+->\s+(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')

# A variable row inside a node label, after unescaping: "/\ name = value".
VAR_ROW_RE = re.compile(r"\s*/\\\s*([a-zA-Z]+)\s*=\s*(.+?)\s*$")


def unescape_dot(s: str) -> str:
    """Decode DOT label escapes (\\, \", \n) into the corresponding chars."""
    return s.encode("utf-8").decode("unicode_escape")


def decode_label(label: str, decode_value: Callable[[str], object]) -> State:
    """Decode a DOT node label into a dict of TLA+ variables."""
    state: State = {}
    for part in unescape_dot(label).split("\n"):
        m = VAR_ROW_RE.match(part)
        if m:
            state[m.group(1)] = decode_value(m.group(2))
    return state


def parse_dot(
    path: Path, decode_value: Callable[[str], object]
) -> Tuple[Dict[int, State], List[Edge]]:
    """Read a TLC `-dump dot,actionlabels` graph into nodes and labelled edges.

    Insertion order of `nodes` is file order, which the output ordering relies on
    only as a stable tie-break: states are sorted by projected content because
    TLC's node IDs depend on worker count and exploration order.
    """
    nodes: Dict[int, State] = {}
    edges: List[Edge] = []
    with path.open() as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            m_edge = EDGE_RE.match(line)
            if m_edge:
                edges.append(
                    (
                        int(m_edge.group(1)),
                        int(m_edge.group(2)),
                        unescape_dot(m_edge.group(3)),
                    )
                )
                continue
            m_node = NODE_RE.match(line)
            if m_node:
                nid = int(m_node.group(1))
                if nid not in nodes:
                    nodes[nid] = decode_label(m_node.group(2), decode_value)
    return nodes, edges


# ---------------------------------------------------------------------------
# TLA+ value decoders (one per model, see SpecConfig.decode_value)
# ---------------------------------------------------------------------------

# stsSpecVer = (0 :> -1 @@ 1 :> -1 @@ 2 :> -1)
FN_ENTRY_RE = re.compile(r"(-?\d+)\s*:>\s*(-?\d+)")
# compAvail = [postgres |-> FALSE, metadata |-> TRUE, gateway |-> FALSE]
RECORD_ENTRY_RE = re.compile(r"([a-zA-Z]+)\s*\|->\s*(TRUE|FALSE)")


def decode_int_or_function(raw: str) -> object:
    """Strings, booleans, int-to-int function literals, and integers."""
    raw = raw.strip()
    if raw.startswith('"') and raw.endswith('"'):
        return raw[1:-1]
    if raw == "TRUE":
        return True
    if raw == "FALSE":
        return False
    if raw.startswith("("):
        # Function literal like (0 :> -1 @@ 1 :> -1 @@ 2 :> -1)
        return {int(k): int(v) for k, v in FN_ENTRY_RE.findall(raw)}
    return int(raw)


def decode_bool_record(raw: str) -> object:
    """Strings, booleans, and name-to-boolean record literals.

    Anything else stays a raw string: this model's non-record variables are all
    strings, so there is nothing to widen for.
    """
    raw = raw.strip()
    if raw.startswith('"') and raw.endswith('"'):
        return raw[1:-1]
    if raw == "TRUE":
        return True
    if raw == "FALSE":
        return False
    if raw.startswith("["):
        # Record literal like [postgres |-> FALSE, metadata |-> TRUE]
        return {k: (v == "TRUE") for k, v in RECORD_ENTRY_RE.findall(raw)}
    return raw


def decode_verbatim(raw: str) -> object:
    """Keep the TLA+ text as written.

    The rotation projection re-parses tuples, sets and records itself (it has to
    recompute a derived predicate TLC does not dump), so decoding twice would
    only add a lossy intermediate representation.
    """
    return raw.strip()


# ---------------------------------------------------------------------------
# Spec-text parsing
# ---------------------------------------------------------------------------

# Conjuncts of the spec's `Safety ==` definition:
#     Safety ==
#         /\ TypeOK
#         /\ Inv_TerminalConsistency
SAFETY_START_RE = re.compile(r"^Safety\s*==\s*$")
CONJUNCT_RE = re.compile(r"^\s*/\\\s*([A-Za-z_][A-Za-z0-9_]*)\s*$")


def parse_safety_conjuncts(spec_path: Path) -> List[str]:
    """Return the names conjoined into the spec's Safety predicate.

    These are emitted into the fixture so the Go side can assert it implements
    every one of them. The invariants are otherwise transcribed by hand into two
    harnesses, and nothing notices when the spec grows a conjunct that neither
    of them checks.
    """
    names: List[str] = []
    in_safety = False
    for line in spec_path.read_text().splitlines():
        if not in_safety:
            if SAFETY_START_RE.match(line):
                in_safety = True
            continue
        m = CONJUNCT_RE.match(line)
        if m:
            names.append(m.group(1))
            continue
        # Blank lines and comments inside the definition are fine; anything else
        # (the next definition, or a conjunct that is not a bare name) ends it.
        if line.strip() == "" or line.lstrip().startswith("\\*"):
            continue
        break
    if not names:
        raise SystemExit(f"error: no Safety conjuncts parsed from {spec_path}")
    return names


# Disjuncts of the spec's `Next ==` definition:
#     Next ==
#         \/ EnvChangeSpec
#         \/ EnvSetInstanceReady(TRUE)
#         \/ ReconcileCleaning
NEXT_START_RE = re.compile(r"^Next\s*==\s*$")
DISJUNCT_RE = re.compile(
    r"^\s*\\/\s*([A-Za-z_][A-Za-z0-9_]*(?:\([^()]*\))?)\s*$"
)


def parse_next_actions(spec_path: Path) -> List[str]:
    """Return the action names disjoined into the spec's Next relation, in spec
    order and including the argument as written (`EnvSetClassReady(TRUE)`).

    These are emitted into the fixture so the Go side can assert that the set of
    things its harness can DO corresponds to the set of things the spec says can
    happen. Nothing else relates the two: a spec action with no harness
    counterpart is a transition the random walk never attempts, and a harness
    action with no spec counterpart is behaviour the model says nothing about.
    Neither shows up as a fixture diff or a failing test.

    Only bare `\\/ Name` and `\\/ Name(args)` disjuncts are understood. An
    existentially-quantified disjunct (`\\/ \\E c \\in Components : ...`, which
    FireboltInstance.tla and SigningKeyRotation.tla both use) is rejected rather
    than guessed at: the action's identity there is the quantified name, and
    inferring it means deciding how to name each instantiation. A caller that
    needs those specs covered should teach this function the shape deliberately.
    """
    names: List[str] = []
    in_next = False
    for line in spec_path.read_text().splitlines():
        if not in_next:
            if NEXT_START_RE.match(line):
                in_next = True
            continue
        m = DISJUNCT_RE.match(line)
        if m:
            names.append(m.group(1))
            continue
        if line.strip() == "" or line.lstrip().startswith("\\*"):
            continue
        if line.lstrip().startswith("\\/"):
            raise SystemExit(
                f"error: {spec_path} has a Next disjunct this script cannot name:\n"
                f"           {line.strip()}\n"
                "       Emitting a spec-action list that silently omits it would "
                "make the\n       Go-side correspondence check pass while covering "
                "less than it claims."
            )
        # The next definition (or anything else at column zero) ends Next.
        break
    if not names:
        raise SystemExit(f"error: no Next disjuncts parsed from {spec_path}")
    return names


def check_labels_are_spec_actions(
    edges: List[Edge], spec_actions: List[str], spec_path: Path
) -> None:
    """Fail if the state graph is labelled with an action the Next parse missed.

    This is what makes the emitted list trustworthy rather than merely
    plausible: every edge TLC produced is attributed to a disjunct we named. The
    reverse does not hold and is not checked -- a disjunct can be enabled in no
    reachable state at the configured bounds, which is a fact about the model
    worth stating on the Go side rather than a generation error.
    """
    known = set(spec_actions)
    unknown = sorted({action for _, _, action in edges if action not in known})
    if unknown:
        raise SystemExit(
            "error: the state graph is labelled with action(s) absent from "
            f"{spec_path}'s Next:\n           "
            + ", ".join(unknown)
            + "\n       The Next parser and TLC disagree about what the actions "
            "are, so the\n       emitted list cannot be used to check the harness "
            "against the spec."
        )


# ---------------------------------------------------------------------------
# Reconciler closure
# ---------------------------------------------------------------------------


def reconciler_closure(
    start: int,
    reconciler_edges: Dict[int, List[int]],
    transitive: bool = True,
) -> FrozenSet[int]:
    """States one Go call may legitimately land in, starting from `start`.

    With `transitive` (the default), states reachable via 1+ reconciler edges;
    otherwise only the direct reconciler successors. Either way `start` itself is
    included iff a legitimate stutter is permitted there.

    The spec models each reconciler action atomically, but Go's compute layer
    legitimately fires several TLA actions in one Reconcile when their
    preconditions are simultaneously satisfied (e.g. from `(creating, sts ok,
    svc absent, podsReady=true)` Go does EnsureService + Advance in one shot,
    landing in `(switching, …)`; a rotation reconcile does MintStart and
    MintReady together). The closure therefore tracks the transitive set of
    states reachable via reconciler-only edges -- the upper bound on what one
    Reconcile can produce without touching environment state.

    That upper bound is only sound where the Go entry point really can batch.
    Where one call is one model action -- a pure decision function whose arms are
    mutually exclusive -- the transitive set is strictly WEAKER than what the
    model says, because it accepts the successor's successor as well as the
    successor. When the difference is projection-visible that is a hole: the wake
    model's scale-down step goes `Idle` then `Stopped` and its first quiet
    observation goes `Initializing` then `ActivityObserved`, so a transitive
    check would accept either reason token for a step the model pins to one.
    Hence `transitive=False`, selected per model in MODELS.

    A stutter at `start` is legitimate iff `start` has no outgoing reconciler
    edges or has a self-loop reconciler edge. Including `start` unconditionally
    would let a buggy implementation that fails to advance from a state where
    the model says progress is mandatory pass silently -- `actual == start`
    would trivially lie in the closure. Excluding `start` in those cases forces
    the test to assert that Reconcile advances to a model-valid successor.

    Cycles back to `start` via 2+ edges are still respected -- they are
    discovered during BFS and `start` re-enters `seen` via the cycle.

    The remaining gap (a known limitation): a reconciler that takes a *valid*
    multi-step path but skips an intermediate step that has no observable
    downstream effect on the projection would slip through. The closure check
    pairs with the explicit safety invariants the harnesses assert to catch bugs
    at the level the projection observes.
    """
    out = reconciler_edges.get(start, ())
    seen: Set[int] = set()
    if not out or start in out:
        seen.add(start)
    stack: List[int] = []
    for n in out:
        if n not in seen:
            seen.add(n)
            stack.append(n)
    if not transitive:
        return frozenset(seen)
    while stack:
        cur = stack.pop()
        for nxt in reconciler_edges.get(cur, ()):
            if nxt not in seen:
                seen.add(nxt)
                stack.append(nxt)
    return frozenset(seen)


# ---------------------------------------------------------------------------
# Go emission helpers
# ---------------------------------------------------------------------------


def go_bool(v: bool) -> str:
    return "true" if v else "false"


def go_str(v: object) -> str:
    return f'"{v}"'


def go_named_const(v: object, consts: Dict[str, str], what: str) -> str:
    """Render a projected string as the existing Go constant that carries it.

    Two reasons a model's emit hook reaches for this instead of go_str. It
    single-sources the token's spelling: rename the constant or change its value
    and the fixture either stops compiling or regenerates differently, rather
    than agreeing with production by coincidence. And it keeps a fixture with
    hundreds of cases from dominating the package's duplicate-literal count --
    goconst tallies occurrences package-wide and reports them against whichever
    line is not excluded, so a generated file can make a lint failure appear in
    production code it has nothing to do with.

    A missing entry fails generation rather than falling back to a quoted
    literal, which would keep the fixture compiling while quietly giving up both
    properties.

    The mapping belongs to the model (see MODELS) because the vocabularies do
    not match: a spec names a decision, Go names a status token, and the two
    differ on purpose in at least one place.
    """
    ident = consts.get(str(v))
    if ident is None:
        raise SystemExit(
            f"error: {what} value {v!r} has no Go constant in this model's "
            "mapping.\n       Add it, or the fixture would restate the literal and "
            "drift from production\n       the next time the constant changes."
        )
    return ident


# ---------------------------------------------------------------------------
# Per-model configuration
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class SpecConfig:
    """Everything that differs between the modelled state machines.

    The pipeline in `generate` is otherwise identical for all of them.
    """

    # How to decode one TLA+ variable value out of a DOT node label.
    decode_value: Callable[[str], object]
    # True for action labels the *environment* performs (a user edit, pods
    # becoming ready, an engine rolling). Everything else counts as a reconciler
    # edge and therefore widens the closure, so a mis-classified environment
    # action weakens the whole cover -- see `validate_actions`.
    is_env_action: Callable[[str], bool]
    # Project a decoded state onto what the Go harness can materialise and
    # observe. The result is the pool's dedupe key AND the output order, so it
    # must be hashable and orderable.
    project: Callable[[State], StateKey]
    # Render one projected state as a positional Go composite literal. Positional
    # (not keyed) to keep the engine fixture at ~700 KB instead of ~2 MB, which
    # is why field order in the emitted struct is load-bearing.
    go_state_lit: Callable[[StateKey, Ctx], str]
    # The generated file's header: package clause, struct definitions, consts.
    header: Callable[[Ctx], str]
    # Emitted Go identifiers.
    state_type: str
    case_type: str
    pool_var: str
    cases_var: str
    # Comment above the pool. Interpolated with {n} = number of pooled states.
    pool_comment: str
    # Whether one call of the Go entry point may realize SEVERAL consecutive
    # reconciler actions of the spec. True for a whole reconcile pass, which
    # batches every sub-step whose preconditions hold at once; False for an entry
    # point that is one model action by construction, where accepting the
    # transitive set would accept a successor the model does not permit. See
    # reconciler_closure.
    transitive_closure: bool = True
    # Drop states the Go harness deliberately never exercises.
    skip_state: Callable[[State], bool] = lambda _state: False
    # Output order. Identity means "order by the projected tuple itself".
    sort_key: Callable[[StateKey], object] = lambda key: key
    # Values for the header and the summary line, derived from the graph (TLC
    # bounds, for instance, so the fixture documents what it was made under).
    bounds: Callable[[Dict[int, State]], Ctx] = lambda _nodes: {}
    # Reject a graph whose action labels this config cannot classify. Runs before
    # anything else, because the failure mode it guards is silent.
    validate_actions: Callable[[List[Edge]], None] = lambda _edges: None
    # Emit the spec's `Safety ==` conjuncts so the Go side can assert it
    # implements every one of them.
    emit_invariants: bool = False
    # Go identifier for the emitted conjunct list, and the test file that
    # asserts against it. Both required when emit_invariants: two models
    # emitting the same identifier into the same package would not compile, and
    # the comment has to point at whichever test does the asserting.
    invariants_var: str = ""
    invariants_test_file: str = ""
    # Emit the spec's `Next ==` disjuncts so the Go side can assert its harness's
    # action set corresponds to them. Also turns on the check that every action
    # label TLC produced was named by the Next parse.
    emit_actions: bool = False
    # Go identifier for the emitted action list. Required when emit_actions.
    actions_var: str = ""
    # Appended to the stdout summary, interpolated with the `bounds` ctx.
    summary_suffix: str = ""

    def __post_init__(self) -> None:
        if self.emit_actions and not self.actions_var:
            raise SystemExit("error: emit_actions needs actions_var")
        if self.emit_invariants and not (
            self.invariants_var and self.invariants_test_file
        ):
            raise SystemExit(
                "error: emit_invariants needs invariants_var and invariants_test_file"
            )


# ---------------------------------------------------------------------------
# FireboltEngine
# ---------------------------------------------------------------------------

ENGINE_HEADER = """// Code generated by scripts/gen-tla-state-tests.py from formal/FireboltEngine.dot. DO NOT EDIT.
//
// Run `make formal-gen` to regenerate. The CI guard `make formal-verify` fails
// if this file is out of date relative to the TLA+ spec.

package controller

// tlaState is one reachable TLA+ state of the FireboltEngine spec, projected
// to the variables the engineSim can materialise and observe. Field order is
// load-bearing: tlaStatePool below uses positional composite literals to keep
// the generated file compact, so adding/reordering/removing fields here must
// be done in lockstep with the generator's go_state_lit.
type tlaState struct {{
	Phase          string
	CurrentGen     int
	ActiveGen      int
	DrainingGen    int // -1 means no draining generation
	SpecVer        int
	SpecWantsStop  bool
	StsSpecVer     [{max_gen_plus_one}]int // -1 at index g means no STS for generation g
	SvcTargetGen   int // -1 means cluster Service absent
	PodsReady      bool
	PodsDrained    bool
	InstanceReady  bool
	ClassReady     bool
	DefaultsReady  bool
}}

// tlaTestCase references tlaStatePool by index. Start is the index of the
// starting state; Closure is the set of indices the model considers reachable
// from Start via 1+ reconciler-only transitions (plus Start itself when a
// stutter is legitimate). After engineSim.Reconcile, the resulting state must
// lie in this closure.
//
// The indirection keeps the fixture compact: every state appears once in
// tlaStatePool, and every closure entry is a 2–4 byte int rather than a
// fully-qualified composite literal. Without this, the file is ~2 MB; with
// it, ~300 KB.
type tlaTestCase struct {{
	Start    int
	Closure  []int
}}

// tlaMaxGen and tlaMaxSpec are the TLC bounds the fixture was generated with.
const (
	tlaMaxGen  = {max_gen}
	tlaMaxSpec = {max_spec}
)

"""

# Action names that represent environment changes (user edits, pod readiness,
# instance readiness, drain completion). These are NOT applied by Reconcile.
# Anything else in the spec is a reconciler action.
ENGINE_ENV_ACTIONS = frozenset(
    [
        "EnvChangeSpec",
        "EnvPodsReady",
        "EnvPodsDrained",
        "EnvSetInstanceReady(TRUE)",
        "EnvSetInstanceReady(FALSE)",
        "EnvSetClassReady(TRUE)",
        "EnvSetClassReady(FALSE)",
        "EnvSetDefaultsReady(TRUE)",
        "EnvSetDefaultsReady(FALSE)",
        "EnvSetGatesOpen",
    ]
)


def engine_check_env_actions(edges: List[Edge]) -> None:
    """Fail if the state graph has an `Env*` action this script does not know.

    Everything not in ENGINE_ENV_ACTIONS counts as a *reconciler* edge, so an
    unrecognised environment action silently widens every reconciler closure and
    weakens the whole state cover -- with no fixture staleness and no failing
    test to show for it. Fail generation instead.
    """
    unknown = sorted(
        {
            action
            for _, _, action in edges
            if action.startswith("Env") and action not in ENGINE_ENV_ACTIONS
        }
    )
    if unknown:
        raise SystemExit(
            "error: unrecognised environment action(s) in the state graph: "
            + ", ".join(unknown)
            + "\n       Add them to ENGINE_ENV_ACTIONS in this script. Until then "
            "they count as\n       reconciler transitions, which widens every "
            "closure and weakens the cover."
        )


# The TLA+ state has 13 variables. We project to a reduced "observable" tuple
# that the engineSim can faithfully reproduce and assert against. specVer is
# carried by the spec template's ServiceAccountName; specWantsStop by
# spec.replicas; stsSpecVer[g] by the per-gen STS pod template.
def engine_project(state: State) -> StateKey:
    sts = state["stsSpecVer"]
    assert isinstance(sts, dict)
    sts_tuple = tuple(sts[k] for k in sorted(sts))
    return (
        state["phase"],
        state["currentGen"],
        state["activeGen"],
        state["drainingGen"],
        state["specVer"],
        state["specWantsStop"],
        sts_tuple,
        state["svcTargetGen"],
        state["podsReady"],
        state["podsDrained"],
        state["instanceReady"],
        state["classReady"],
        state["defaultsReady"],
    )


def engine_go_state_lit(key: StateKey, ctx: Ctx) -> str:
    """Positional tlaState composite literal. The outer `tlaState` type is
    elided because the literal sits inside `[]tlaState{ … }` (the pool); Go
    infers it. Field order MUST match the tlaState struct in ENGINE_HEADER --
    this is the price of dropping field names to shrink the fixture."""
    (
        phase,
        current_gen,
        active_gen,
        draining_gen,
        spec_ver,
        spec_wants_stop,
        sts,
        svc_target_gen,
        pods_ready,
        pods_drained,
        instance_ready,
        class_ready,
        defaults_ready,
    ) = key
    assert isinstance(sts, tuple)
    gens = int(ctx["max_gen_plus_one"])
    assert len(sts) == gens, f"stsSpecVer has {len(sts)} entries, expected {gens}"
    sts_lit = ", ".join(str(v) for v in sts)
    return (
        "{"
        f"{go_str(phase)}, "
        f"{current_gen}, "
        f"{active_gen}, "
        f"{draining_gen}, "
        f"{spec_ver}, "
        f"{go_bool(bool(spec_wants_stop))}, "
        f"[{gens}]int{{{sts_lit}}}, "
        f"{svc_target_gen}, "
        f"{go_bool(bool(pods_ready))}, "
        f"{go_bool(bool(pods_drained))}, "
        f"{go_bool(bool(instance_ready))}, "
        f"{go_bool(bool(class_ready))}, "
        f"{go_bool(bool(defaults_ready))}"
        "}"
    )


def engine_bounds(nodes: Dict[int, State]) -> Ctx:
    """Recover MaxGen / MaxSpec from the data, so the generated file documents
    the TLC bounds it was made under rather than restating them by hand."""
    max_gen = 0
    max_spec = 0
    for s in nodes.values():
        max_gen = max(max_gen, int(str(s["currentGen"])))
        max_spec = max(max_spec, int(str(s["specVer"])))
        sts = s["stsSpecVer"]
        if isinstance(sts, dict):
            for k in sts:
                max_gen = max(max_gen, k)
    return {
        "max_gen": max_gen,
        "max_spec": max_spec,
        "max_gen_plus_one": max_gen + 1,
    }


# ---------------------------------------------------------------------------
# FireboltInstance
# ---------------------------------------------------------------------------

INSTANCE_HEADER = """// Code generated by scripts/gen-tla-state-tests.py from formal/FireboltInstance.dot. DO NOT EDIT.
//
// Run `make formal-gen` to regenerate. The CI guard `make formal-verify` fails
// if this file is out of date relative to the TLA+ spec.

package controller

// tlaInstanceState is one reachable TLA+ state of the FireboltInstance spec,
// projected to the variables the instanceSim can materialise and observe.
// Field order is load-bearing: tlaInstanceStatePool below uses positional
// composite literals; adding/reordering/removing fields here must be done
// in lockstep with the generator's go_state_lit.
type tlaInstanceState struct {
\tPhase         string
\tPostgresAvail bool
\tMetadataAvail bool
\tGatewayAvail  bool
}

// tlaInstanceTestCase references tlaInstanceStatePool by index. Start is the
// index of the starting state; Closure is the set of indices the model
// considers reachable from Start via 1+ reconciler-only transitions (plus
// Start itself when a stutter is legitimate). The indirection keeps the
// fixture compact and matches the engine fixture's shape.
type tlaInstanceTestCase struct {
\tStart   int
\tClosure []int
}

"""

# Env-action label *prefixes* (the action label includes the component arg,
# e.g. EnvComponentReady("postgres")). Any action label whose start matches
# one of these is treated as environment, not reconciler.
INSTANCE_ENV_ACTION_PREFIXES = ("EnvComponentReady(", "EnvComponentDegrades(")


def instance_project(state: State) -> StateKey:
    """Project a TLA+ state to a hashable tuple ordered for stable output."""
    avail = state["compAvail"]
    assert isinstance(avail, dict)
    return (
        state["phase"],
        avail["postgres"],
        avail["metadata"],
        avail["gateway"],
    )


def instance_go_state_lit(key: StateKey, _ctx: Ctx) -> str:
    """Positional tlaInstanceState composite literal. Outer type is elided
    because the literal sits inside `[]tlaInstanceState{ … }` (the pool).
    Field order MUST match the tlaInstanceState struct in INSTANCE_HEADER."""
    phase, postgres, metadata, gateway = key
    return (
        "{"
        f"{go_str(phase)}, "
        f"{go_bool(bool(postgres))}, "
        f"{go_bool(bool(metadata))}, "
        f"{go_bool(bool(gateway))}"
        "}"
    )


# ---------------------------------------------------------------------------
# SigningKeyRotation
# ---------------------------------------------------------------------------

ROTATION_HEADER = """// Code generated by scripts/gen-tla-state-tests.py from formal/SigningKeyRotation.dot. DO NOT EDIT.
//
// Run `make formal-gen` to regenerate. The CI guard `make formal-verify` fails
// if this file is out of date relative to the TLA+ spec.

package controller

// tlaRotationKey is one signing key's modeled state, projected to what
// Status.Auth.SigningKeys can represent. Index in tlaRotationState.Keys is the
// kid number minus one.
//
// The model's "minting" phase projects to "absent": a key whose Certificate is
// applied but whose Secret is not ready yet is not in status and does not bump
// the generation counter, so nothing on the Go side distinguishes the two.
type tlaRotationKey struct {
\tPhase      string
\tDemoted    bool
\tAnchored   bool
\tRetainDone bool
}

// tlaRotationState is one reachable TLA+ state of SigningKeyRotation.tla.
// Field order is load-bearing: tlaRotationStatePool below uses positional
// composite literals, so changing these fields requires changing the
// generator's go_state_lit in lockstep.
type tlaRotationState struct {
\tKeys      []tlaRotationKey
\tGen       int
\tConverged bool
}

// tlaRotationTestCase references tlaRotationStatePool by index. Start is the
// starting state; Closure is the set of indices the model considers reachable
// from Start via 1+ reconciler-only transitions (plus Start itself when a
// stutter is legitimate).
type tlaRotationTestCase struct {
\tStart   int
\tClosure []int
}

"""

# Action label prefixes that belong to the environment rather than the
# reconciler. The label carries its argument (e.g. EngineRolls(e1)), so these
# match on the start of the label.
ROTATION_ENV_ACTION_PREFIXES = ("EngineRolls(", "RetainElapses(")

# keyPhase = <<"active", "validationOnly", "absent">>
TUPLE_ENTRY_RE = re.compile(r'"([a-zA-Z]+)"')
# demoted = {1, 3}   /   anchored = {}
SET_INT_RE = re.compile(r"\d+")
# observed = (e1 :> [active |-> 1, keys |-> {1, 2}] @@ e2 :> [...])
OBSERVED_RE = re.compile(r"\[active\s*\|->\s*(\d+),\s*keys\s*\|->\s*\{([^}]*)\}\]")

# Go-visible phase for each modeled phase. "minting" is not representable in
# Status.Auth.SigningKeys, so it projects the same as "absent". The
# corresponding reconciler edges still appear in the closure, so a Reconcile
# that performs MintStart and MintReady in one call is accepted.
ROTATION_PHASE_PROJECTION = {
    "absent": "absent",
    "minting": "absent",
    "validationOnly": "validationOnly",
    "active": "active",
    "removing": "removing",
}


def rotation_int_set(raw: str) -> Set[int]:
    return {int(n) for n in SET_INT_RE.findall(raw)}


def rotation_converged(state: State) -> bool:
    """Recompute the model's Converged predicate from the dumped variables.

    TLC dumps only VARIABLES, so the derived RenderedConfig has to be rebuilt
    here: the active kid plus every rendered (active or validate-only) kid,
    compared against what each engine reports observing.
    """
    phases = TUPLE_ENTRY_RE.findall(str(state["keyPhase"]))
    rendered = {
        i + 1 for i, p in enumerate(phases) if p in ("active", "validationOnly")
    }
    actives = [i + 1 for i, p in enumerate(phases) if p == "active"]
    assert len(actives) == 1, f"expected exactly one active key, got {actives}"
    active = actives[0]
    seen = OBSERVED_RE.findall(str(state["observed"]))
    assert seen, f"could not parse observed from {state['observed']!r}"
    return all(
        int(a) == active and rotation_int_set(keys) == rendered for a, keys in seen
    )


def rotation_project(state: State) -> StateKey:
    """Project a TLA+ state onto what the Go harness can materialize and observe.

    The per-engine `observed` mapping collapses into a single Converged flag:
    stepSigningKeyRotation reads only "has the whole fleet converged", so that is
    all the harness can drive. The richer mapping exists in the model to express
    NoValidationGap, which is a property of the fleet rather than of one call.
    """
    phases = TUPLE_ENTRY_RE.findall(str(state["keyPhase"]))
    demoted = rotation_int_set(str(state["demoted"]))
    anchored = rotation_int_set(str(state["anchored"]))
    retain_done = rotation_int_set(str(state["retainDone"]))
    keys: List[Tuple[str, bool, bool, bool]] = []
    for i, phase in enumerate(phases):
        kid = i + 1
        keys.append(
            (
                ROTATION_PHASE_PROJECTION[phase],
                kid in demoted,
                kid in anchored,
                kid in retain_done,
            )
        )
    return (tuple(keys), int(str(state["gen"])), rotation_converged(state))


def rotation_go_state_lit(key: StateKey, _ctx: Ctx) -> str:
    """Positional tlaRotationState composite literal. The outer type is elided
    because the literal sits inside `[]tlaRotationState{ … }`."""
    keys, gen, conv = key
    assert isinstance(keys, tuple)
    parts = []
    for phase, demoted, anchored, retain in keys:  # type: ignore[misc]
        parts.append(
            f'{{"{phase}", {go_bool(demoted)}, {go_bool(anchored)}, {go_bool(retain)}}}'
        )
    return (
        "{[]tlaRotationKey{"
        + ", ".join(parts)
        + "}, "
        + f"{gen}, {go_bool(bool(conv))}"
        + "}"
    )


# ---------------------------------------------------------------------------
# EngineWake
# ---------------------------------------------------------------------------

WAKE_HEADER = """// Code generated by scripts/gen-tla-state-tests.py from formal/EngineWake.dot. DO NOT EDIT.
//
// Run `make formal-gen` to regenerate. The CI guard `make formal-verify` fails
// if this file is out of date relative to the TLA+ spec.

package controller

// tlaWakeState is one reachable TLA+ state of the EngineWake spec, projected to
// the inputs and outputs computeAutoStopDecision can be driven through and
// observed by.
//
// The two timestamps are projected as AGES in model ticks rather than as
// instants, because the decision function only ever reads them as an age
// against `now`: the wake TTL and the idle timeout are both `now - stamp`
// comparisons. That collapses every (now, stamp) pair with the same difference
// into one case, and it is what makes the projection sound -- see the
// generator's wake_project for the argument.
//
// Field order is load-bearing: tlaWakeStatePool below uses positional composite
// literals, so adding/reordering/removing fields here must be done in lockstep
// with the generator's go_state_lit.
type tlaWakeState struct {
\tReplicas int
\tWakeAge  int // -1: the operator holds no demand for this engine
\tIdleAge  int // -1: status.lastActivityTime is unset
\tActivity string
\t// Reason is emitted as the production constant that carries the token
\t// (AutoStopReasonIdle and friends), so the fixture and status.autoStopReason
\t// cannot drift to two spellings of the same decision.
\tReason string
}

// tlaWakeTestCase references tlaWakeStatePool by index. Start is the index of
// the starting state; Successors is the set of indices the model reaches from
// Start in EXACTLY ONE reconciler transition (plus Start itself when a stutter
// is legitimate, i.e. when the enabled arm changes nothing).
//
// One step, not the transitive closure the other fixtures carry, because one
// call of computeAutoStopDecision is one reconciler action: its arms are
// mutually exclusive. Accepting the transitive set would accept the successor's
// successor too, and here that is projection-visible -- a scale-down would pass
// while reporting Stopped instead of Idle, and a first quiet observation while
// reporting ActivityObserved instead of Initializing.
type tlaWakeTestCase struct {
\tStart      int
\tSuccessors []int
}

"""

# Action labels the ENVIRONMENT performs, relative to the function under test.
# The two Poll* actions are the operator's own demand poller and not the
# environment in any deployment sense, but they are environment to
# computeAutoStopDecision: it reads the cache they maintain and cannot write it.
# Treating them as reconciler edges would let the state cover accept a decision
# that only a poll could have produced.
WAKE_ENV_ACTIONS = frozenset(
    [
        "EnvTick",
        "DemandArrives",
        "AgentPrunesDemand",
        "AgentRestarts",
        "PollObserves",
        "PollLosesCache",
    ]
)

# Env actions whose label carries an argument, e.g. EnvScrapeObserves("busy").
WAKE_ENV_ACTION_PREFIXES = ("EnvScrapeObserves(",)


def wake_is_env_action(label: str) -> bool:
    return label in WAKE_ENV_ACTIONS or label.startswith(WAKE_ENV_ACTION_PREFIXES)


def wake_check_actions(edges: List[Edge]) -> None:
    """Fail on any action label that is neither a known environment action nor a
    Reconcile* one.

    Stronger than the engine model's equivalent, which only validates labels
    beginning with `Env`. Here EVERY label is accounted for, in both directions:
    an unrecognised environment action would silently widen every reconciler
    closure, and a reconciler action misspelled into looking like an environment
    one would silently narrow it. Neither shows up as a fixture diff.
    """
    unknown = sorted(
        {
            action
            for _, _, action in edges
            if not wake_is_env_action(action) and not action.startswith("Reconcile")
        }
    )
    if unknown:
        raise SystemExit(
            "error: unclassifiable action(s) in the state graph: "
            + ", ".join(unknown)
            + "\n       Every EngineWake action is either listed in WAKE_ENV_ACTIONS / "
            "WAKE_ENV_ACTION_PREFIXES\n       or named Reconcile*. Classify it in this "
            "script before regenerating."
        )


def wake_project(state: State) -> StateKey:
    """Project a TLA+ state onto what the decision function reads and writes.

    Dropped: `now`, which only ever appears inside an age comparison, and
    `stamp`, which is the AGENT's view -- the reconciler cannot see it and reads
    the operator's cached copy instead. The model keeps them apart because the
    drift between the two is the whole subject; the Go decision function is
    downstream of it.

    Soundness of the ages: every reconciler guard reads `now` only via
    `now - cache < WakeTTL` and `now - lastActivity >= IdleTimeout`, and every
    reconciler write is either a replica level, a reason token, or
    `lastActivity' = now` (age 0). So the projected successor of a state is a
    function of its projection alone, which is what the generator's dedupe
    assumes: it keeps ONE case per projected key and uses that node's closure
    for it.
    """
    now = int(str(state["now"]))

    def age(raw: object) -> int:
        v = int(str(raw))
        return -1 if v < 0 else now - v

    return (
        int(str(state["replicas"])),
        age(state["cache"]),
        age(state["lastActivity"]),
        state["activity"],
        state["reason"],
    )


# The model's reason tokens, mapped to the constants production writes to
# status.autoStopReason. The fixture emits the IDENTIFIER, so the spelling of
# every token is single-sourced from engine_autostop.go -- note that the
# vocabularies already differ once ("ActivityObserved" is AutoStopReasonActivity),
# which is exactly the kind of drift a quoted literal would hide.
#
# Activity is deliberately absent from this treatment: "quiet" / "busy" /
# "scrapeFailed" are model abstractions of a metric scrape with no production
# constant to point at, so a literal is the honest rendering.
WAKE_REASON_CONSTS = {
    "Stopped": "AutoStopReasonStopped",
    "WakeRequested": "AutoStopReasonWakeRequested",
    "ScrapeFailed": "AutoStopReasonScrapeFailed",
    "ActivityObserved": "AutoStopReasonActivity",
    "Idle": "AutoStopReasonIdle",
    "Initializing": "AutoStopReasonInitializing",
}


def wake_go_state_lit(key: StateKey, _ctx: Ctx) -> str:
    """Positional tlaWakeState composite literal. The outer type is elided
    because the literal sits inside `[]tlaWakeState{ … }` (the pool). Field
    order MUST match the tlaWakeState struct in WAKE_HEADER."""
    replicas, wake_age, idle_age, activity, reason = key
    return (
        "{"
        f"{replicas}, "
        f"{wake_age}, "
        f"{idle_age}, "
        f"{go_str(activity)}, "
        f"{go_named_const(reason, WAKE_REASON_CONSTS, 'reason')}"
        "}"
    )


# ---------------------------------------------------------------------------
# The registry
# ---------------------------------------------------------------------------

MODELS: Dict[str, SpecConfig] = {
    "engine": SpecConfig(
        decode_value=decode_int_or_function,
        is_env_action=lambda label: label in ENGINE_ENV_ACTIONS,
        project=engine_project,
        go_state_lit=engine_go_state_lit,
        header=lambda ctx: ENGINE_HEADER.format(**ctx),
        state_type="tlaState",
        case_type="tlaTestCase",
        pool_var="tlaStatePool",
        cases_var="tlaEngineStateCases",
        pool_comment="// {n} unique reachable TLA+ states (uninitialised excluded).",
        # The operator's Go code handles phase="" via a single early-return in
        # the outer Reconcile method, so the compute layer never sees it.
        skip_state=lambda state: state["phase"] == "uninitialized",
        bounds=engine_bounds,
        validate_actions=engine_check_env_actions,
        emit_invariants=True,
        invariants_var="tlaRequiredInvariants",
        invariants_test_file="engine_invariants_test.go",
        emit_actions=True,
        actions_var="tlaEngineSpecActions",
        summary_suffix=" (MaxGen={max_gen}, MaxSpec={max_spec})",
    ),
    "instance": SpecConfig(
        decode_value=decode_bool_record,
        is_env_action=lambda label: label.startswith(INSTANCE_ENV_ACTION_PREFIXES),
        project=instance_project,
        go_state_lit=instance_go_state_lit,
        header=lambda _ctx: INSTANCE_HEADER,
        state_type="tlaInstanceState",
        case_type="tlaInstanceTestCase",
        pool_var="tlaInstanceStatePool",
        cases_var="tlaInstanceStateCases",
        pool_comment="// {n} unique reachable TLA+ states.",
    ),
    "rotation": SpecConfig(
        decode_value=decode_verbatim,
        is_env_action=lambda label: label.startswith(ROTATION_ENV_ACTION_PREFIXES),
        project=rotation_project,
        go_state_lit=rotation_go_state_lit,
        header=lambda _ctx: ROTATION_HEADER,
        state_type="tlaRotationState",
        case_type="tlaRotationTestCase",
        pool_var="tlaRotationStatePool",
        cases_var="tlaRotationStateCases",
        pool_comment="// {n} unique reachable TLA+ states, projected.",
        # The projection nests a tuple of per-key tuples, which orders fine but
        # reads better -- and diffs more stably -- rendered to text first.
        sort_key=str,
    ),
    "wake": SpecConfig(
        decode_value=decode_int_or_function,
        is_env_action=wake_is_env_action,
        project=wake_project,
        go_state_lit=wake_go_state_lit,
        header=lambda _ctx: WAKE_HEADER,
        state_type="tlaWakeState",
        case_type="tlaWakeTestCase",
        pool_var="tlaWakeStatePool",
        cases_var="tlaWakeStateCases",
        pool_comment="// {n} unique reachable TLA+ states, projected to the "
        "decision function's inputs and outputs.",
        validate_actions=wake_check_actions,
        # One call of computeAutoStopDecision is exactly one reconciler action:
        # its arms are mutually exclusive and exactly one is enabled per state,
        # so the model's ONE-STEP successor is what the Go result must equal.
        # The other three models keep the transitive default because their Go
        # entry point is a whole pass that genuinely batches: the engine's does
        # EnsureService+Advance together, a rotation reconcile does
        # MintStart+MintReady, and an instance reconcile initializes the phase
        # and then computes the rollup in the same call.
        transitive_closure=False,
        # go_state_lit emits the Reason field as the production constant that
        # carries the token (WAKE_REASON_CONSTS) rather than as a quoted string.
        emit_invariants=True,
        invariants_var="tlaWakeRequiredInvariants",
        invariants_test_file="wake_tla_state_test.go",
    ),
}


INVARIANTS_COMMENT = (
    "// {var} are the conjuncts of the spec's `Safety ==`\n"
    "// predicate, in spec order. {test} asserts that every\n"
    "// one of them has a Go counterpart in the shared invariant registry, so a\n"
    "// conjunct added to the spec fails the build until it is implemented -- the\n"
    "// invariants are hand-transcribed into the harnesses, and nothing else\n"
    "// notices when the spec grows one they do not check."
)

ACTIONS_COMMENT = (
    "// {var} are the disjuncts of the spec's `Next ==` relation, in spec order\n"
    "// and with arguments as written. engine_actions_test.go asserts that each\n"
    "// one is either mapped to a harness action by an explicit declaration or\n"
    "// listed as spec-only with a reason -- and the same in reverse for the\n"
    "// harness's own actions.\n"
    "//\n"
    "// The correspondence has to be declared rather than inferred: the names\n"
    "// deliberately differ on the two sides, and several spec actions map to the\n"
    "// one whole-pass Reconcile the harness exposes."
)


# ---------------------------------------------------------------------------
# The shared pipeline
# ---------------------------------------------------------------------------


def generate(cfg: SpecConfig, dot: Path, spec: Path, out: Path) -> str:
    """Read the state graph, build the fixture, write it, return a summary."""
    nodes, edges = parse_dot(dot, cfg.decode_value)
    if not nodes:
        raise SystemExit(f"error: no states parsed from {dot}")

    cfg.validate_actions(edges)
    ctx = cfg.bounds(nodes)

    # Partition edges into reconciler vs environment.
    reconciler_edges: Dict[int, List[int]] = defaultdict(list)
    for src, dst, action in edges:
        if not cfg.is_env_action(action):
            reconciler_edges[src].append(dst)

    # TLC's node IDs are NOT stable across runs (they depend on worker count,
    # exploration order, and other run-specific factors), so the output must be
    # ordered by *state content* -- the projected tuple -- rather than by node ID.
    start_ids = [nid for nid, state in nodes.items() if not cfg.skip_state(state)]
    start_ids.sort(key=lambda nid: cfg.sort_key(cfg.project(nodes[nid])))

    # Build the state pool, deduped by projected key: TLC's node IDs are not
    # stable across runs, and two distinct nodes can share one projection. Pool
    # order follows start_ids, which is already content-sorted, so the pool is
    # content-stable too.
    key_to_pool_idx: Dict[StateKey, int] = {}
    pool: List[StateKey] = []

    def pool_idx(state: State) -> int:
        key = cfg.project(state)
        idx = key_to_pool_idx.get(key)
        if idx is None:
            idx = len(pool)
            key_to_pool_idx[key] = idx
            pool.append(key)
        return idx

    for nid in start_ids:
        pool_idx(nodes[nid])

    # Build cases: dedupe starts by projected key (so two TLC nodes with the same
    # projection produce one test case, not two). Closure entries are pool
    # indices, sorted for stable diffs.
    emitted: Set[StateKey] = set()
    cases: List[Tuple[int, List[int]]] = []
    for nid in start_ids:
        key = cfg.project(nodes[nid])
        if key in emitted:
            continue
        emitted.add(key)
        closure: Set[int] = set()
        for cid in reconciler_closure(nid, reconciler_edges, cfg.transitive_closure):
            closure.add(pool_idx(nodes[cid]))
        cases.append((pool_idx(nodes[nid]), sorted(closure)))

    lines: List[str] = [cfg.header(ctx)]

    if cfg.emit_invariants:
        lines.append(
            INVARIANTS_COMMENT.format(
                var=cfg.invariants_var, test=cfg.invariants_test_file
            )
        )
        lines.append(f"var {cfg.invariants_var} = []string{{")
        for name in parse_safety_conjuncts(spec):
            lines.append(f"\t{go_str(name)},")
        lines.append("}")
        lines.append("")

    if cfg.emit_actions:
        spec_actions = parse_next_actions(spec)
        check_labels_are_spec_actions(edges, spec_actions, spec)
        lines.append(ACTIONS_COMMENT.format(var=cfg.actions_var))
        lines.append(f"var {cfg.actions_var} = []string{{")
        for name in spec_actions:
            lines.append(f"\t{go_str(name)},")
        lines.append("}")
        lines.append("")

    lines.append(cfg.pool_comment.format(n=len(pool)))
    lines.append(f"var {cfg.pool_var} = []{cfg.state_type}{{")
    for key in pool:
        lines.append(f"\t{cfg.go_state_lit(key, ctx)},")
    lines.append("}")
    lines.append("")

    lines.append(f"// {len(cases)} test cases referencing {cfg.pool_var} by index.")
    lines.append(f"var {cfg.cases_var} = []{cfg.case_type}{{")
    for start_idx, closure_indices in cases:
        closure_str = ", ".join(str(i) for i in closure_indices)
        lines.append(f"\t{{{start_idx}, []int{{{closure_str}}}}},")
    lines.append("}")
    lines.append("")

    out.write_text("\n".join(lines))
    return (
        f"wrote {out}: {len(cases)} test cases over {len(pool)} pooled states"
        + cfg.summary_suffix.format(**ctx)
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--model",
        required=True,
        choices=sorted(MODELS),
        help="which modelled state machine to generate for (see MODELS)",
    )
    parser.add_argument("--dot", required=True, type=Path, help="TLC DOT dump")
    parser.add_argument(
        "--spec",
        required=True,
        type=Path,
        help="TLA+ spec the DOT dump came from; read directly by the models that "
        "emit spec-derived lists (Safety conjuncts)",
    )
    parser.add_argument("--out", required=True, type=Path, help="Go fixture output path")
    args = parser.parse_args()

    print(generate(MODELS[args.model], args.dot, args.spec, args.out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
