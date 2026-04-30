#!/usr/bin/env python3
# Copyright 2026 Firebolt Analytics.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
"""Generate the TLA+ state-cover test fixture for the FireboltEngine reconciler.

Phase 3 of the formal-verification plan (docs/formal-verification.md): for each
reachable state in the TLC state graph, the generated Go test materialises the
state in the in-memory engineSim, calls computeEngineReconcile, and verifies
that the resulting state is one the TLA+ model considers reachable from the
starting state via reconciler-only transitions.

Input  : formal/FireboltEngine.dot  (produced by `make formal-dump`)
Output : internal/controller/engine_tla_states_data_test.go (Go fixture)

The fixture lists every reachable TLA+ state (excluding uninitialised states,
which the operator's Go code handles via a single early-return in the outer
Reconcile method) plus, for each state, the set of TLA+ states reachable from
it via zero or more consecutive reconciler-only edges. That set is the
"reconciler closure": after one m.Reconcile() call against the engineSim, the
resulting state must belong to it.
"""

import argparse
import re
import sys
from collections import defaultdict
from pathlib import Path
from typing import Dict, FrozenSet, Iterable, List, Set, Tuple

# Action names that represent environment changes (user edits, pod readiness,
# instance readiness, drain completion). These are NOT applied by Reconcile.
# Anything else in the spec is a reconciler action.
ENV_ACTIONS = frozenset(
    [
        "EnvChangeSpec",
        "EnvPodsReady",
        "EnvPodsDrained",
        "EnvSetInstanceReady(TRUE)",
        "EnvSetInstanceReady(FALSE)",
    ]
)

# DOT node and edge lines. The label is a quoted string in which DOT escapes
# backslash as \\, quote as \", and newline as \n. The regex must accept those
# escape sequences inside the label.
LABEL_BODY = r'(?:[^"\\]|\\.)*'
NODE_RE = re.compile(r'^(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')
EDGE_RE = re.compile(r'^(-?\d+)\s+->\s+(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')

# stsSpecVer = (0 :> -1 @@ 1 :> -1 @@ 2 :> -1)
FN_ENTRY_RE = re.compile(r"(-?\d+)\s*:>\s*(-?\d+)")


def unescape_dot(s: str) -> str:
    """Decode DOT label escapes (\\, \", \n) into the corresponding chars."""
    return s.encode("utf-8").decode("unicode_escape")


def parse_var_value(name: str, raw: str) -> object:
    """Decode a TLA+ value string into a Python value."""
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


def decode_label(label: str) -> Dict[str, object]:
    """Decode a DOT node label into a dict of TLA+ variables."""
    state: Dict[str, object] = {}
    body = unescape_dot(label)
    # After unescaping, var rows are separated by real newlines and each looks
    # like "/\ name = value".
    for part in body.split("\n"):
        m = re.match(r"\s*/\\\s*([a-zA-Z]+)\s*=\s*(.+?)\s*$", part)
        if not m:
            continue
        state[m.group(1)] = parse_var_value(m.group(1), m.group(2))
    return state


def parse_dot(path: Path) -> Tuple[Dict[int, Dict[str, object]], List[Tuple[int, int, str]]]:
    nodes: Dict[int, Dict[str, object]] = {}
    edges: List[Tuple[int, int, str]] = []
    with path.open() as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            m_edge = EDGE_RE.match(line)
            if m_edge:
                src, dst, action = m_edge.group(1), m_edge.group(2), m_edge.group(3)
                edges.append((int(src), int(dst), unescape_dot(action)))
                continue
            m_node = NODE_RE.match(line)
            if m_node:
                nid = int(m_node.group(1))
                if nid not in nodes:
                    nodes[nid] = decode_label(m_node.group(2))
    return nodes, edges


def reconciler_closure(
    start: int,
    reconciler_edges: Dict[int, List[int]],
) -> FrozenSet[int]:
    """Set of nodes reachable from `start` via 0+ reconciler-only edges.

    The starting node is included (represents a reconciler stutter — useful
    when no reconciler edge is enabled or one Reconcile call performs a no-op).
    """
    seen: Set[int] = {start}
    stack = [start]
    while stack:
        cur = stack.pop()
        for nxt in reconciler_edges.get(cur, ()):
            if nxt not in seen:
                seen.add(nxt)
                stack.append(nxt)
    return frozenset(seen)


# The TLA+ state has 11 variables. We project to a reduced "observable" tuple
# that the engineSim can faithfully reproduce and assert against. specVer is
# baked into the engine spec image tag; specWantsStop into spec.replicas;
# stsSpecVer[g] into the per-gen STS image tag.
def state_key(state: Dict[str, object]) -> Tuple[object, ...]:
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
    )


GO_HEADER = """// Code generated by scripts/gen-tla-state-tests.py from formal/FireboltEngine.dot. DO NOT EDIT.
//
// Run `make formal-gen` to regenerate. The CI guard `make formal-verify` fails
// if this file is out of date relative to the TLA+ spec.

package controller

// tlaState is one reachable TLA+ state of the FireboltEngine spec, projected
// to the variables the engineSim can materialise and observe.
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
}}

// tlaTestCase pairs a TLA+ state with the set of states the model considers
// reachable from it via 0+ consecutive reconciler-only transitions. After
// engineSim.Reconcile, the resulting state must lie in this closure.
type tlaTestCase struct {{
	Start    tlaState
	Closure  []tlaState // includes Start (stutter)
}}

// tlaMaxGen and tlaMaxSpec are the TLC bounds the fixture was generated with.
const (
	tlaMaxGen  = {max_gen}
	tlaMaxSpec = {max_spec}
)

"""


def go_bool(v: bool) -> str:
    return "true" if v else "false"


def go_state_lit(s: Dict[str, object], max_gen: int) -> str:
    sts = s["stsSpecVer"]
    assert isinstance(sts, dict)
    sts_lit = ", ".join(str(sts[g]) for g in range(max_gen + 1))
    return (
        "tlaState{"
        f'Phase: "{s["phase"]}", '
        f'CurrentGen: {s["currentGen"]}, '
        f'ActiveGen: {s["activeGen"]}, '
        f'DrainingGen: {s["drainingGen"]}, '
        f'SpecVer: {s["specVer"]}, '
        f'SpecWantsStop: {go_bool(bool(s["specWantsStop"]))}, '
        f'StsSpecVer: [{max_gen + 1}]int{{{sts_lit}}}, '
        f'SvcTargetGen: {s["svcTargetGen"]}, '
        f'PodsReady: {go_bool(bool(s["podsReady"]))}, '
        f'PodsDrained: {go_bool(bool(s["podsDrained"]))}, '
        f'InstanceReady: {go_bool(bool(s["instanceReady"]))}'
        "}"
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dot", required=True, type=Path, help="TLC DOT dump")
    parser.add_argument("--out", required=True, type=Path, help="Go fixture output path")
    parser.add_argument(
        "--include-uninitialized",
        action="store_true",
        help="Include phase=uninitialized states (default: skip; the real code handles "
        "them via a single early-return in the outer Reconcile method)",
    )
    args = parser.parse_args()

    nodes, edges = parse_dot(args.dot)
    if not nodes:
        print(f"error: no states parsed from {args.dot}", file=sys.stderr)
        return 1

    # Partition edges into reconciler vs environment.
    reconciler_edges: Dict[int, List[int]] = defaultdict(list)
    for src, dst, action in edges:
        if action not in ENV_ACTIONS:
            reconciler_edges[src].append(dst)

    # Determine MaxGen / MaxSpec by inspecting the data (so the generated file
    # documents the bounds it was made under).
    max_gen = 0
    max_spec = 0
    for s in nodes.values():
        max_gen = max(max_gen, int(s["currentGen"]))
        max_spec = max(max_spec, int(s["specVer"]))
        sts = s["stsSpecVer"]
        if isinstance(sts, dict):
            for k in sts:
                max_gen = max(max_gen, k)

    # Filter starting states.
    start_ids: List[int] = []
    for nid, state in nodes.items():
        if not args.include_uninitialized and state["phase"] == "uninitialized":
            continue
        start_ids.append(nid)
    start_ids.sort()  # deterministic output

    # Compute closures and emit the fixture.
    out_lines: List[str] = [
        GO_HEADER.format(
            max_gen=max_gen, max_spec=max_spec, max_gen_plus_one=max_gen + 1
        )
    ]
    out_lines.append(f"// {len(start_ids)} reachable states (uninitialised excluded)")
    out_lines.append("var tlaEngineStateCases = []tlaTestCase{")

    for nid in start_ids:
        start = nodes[nid]
        closure_ids = reconciler_closure(nid, reconciler_edges)
        # Deduplicate closure entries by their projected key (same observable
        # state may correspond to multiple TLC node ids in rare cases).
        seen_keys: Set[Tuple[object, ...]] = set()
        closure_states: List[Dict[str, object]] = []
        for cid in sorted(closure_ids):
            cstate = nodes[cid]
            key = state_key(cstate)
            if key in seen_keys:
                continue
            seen_keys.add(key)
            closure_states.append(cstate)

        out_lines.append("\t{")
        out_lines.append(f"\t\tStart: {go_state_lit(start, max_gen)},")
        out_lines.append("\t\tClosure: []tlaState{")
        for cstate in closure_states:
            out_lines.append(f"\t\t\t{go_state_lit(cstate, max_gen)},")
        out_lines.append("\t\t},")
        out_lines.append("\t},")

    out_lines.append("}")
    out_lines.append("")

    args.out.write_text("\n".join(out_lines))
    print(
        f"wrote {args.out}: {len(start_ids)} test cases "
        f"(MaxGen={max_gen}, MaxSpec={max_spec})"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
