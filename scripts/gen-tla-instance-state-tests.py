#!/usr/bin/env python3
# Copyright 2026 Firebolt Analytics.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
"""Generate the TLA+ state-cover test fixture for the FireboltInstance reconciler.

Phase 5 of the formal-verification plan (docs/formal-verification.mdx): for each
reachable state in the TLC state graph of FireboltInstance.tla, the generated Go
test materialises the state in the in-memory instanceSim, calls the same
setInstanceReadyRollup + computePhase functions the production code calls, and
verifies that the resulting state lies in the model's reconciler closure of the
starting state.

Input  : formal/FireboltInstance.dot                       (from `make formal-dump`)
Output : internal/controller/instance_tla_states_data_test.go (Go fixture)

The fixture lists every reachable TLA+ state plus, for each state, the set of
TLA+ states reachable from it via zero or more consecutive reconciler-only
edges. After one Reconcile call against the instanceSim the resulting state
must belong to that closure.

A sibling generator (gen-tla-state-tests.py) handles FireboltEngine. The two
share the DOT line shapes but differ in projection, env-action filter, and
Go output, so they are kept as separate scripts to avoid coupling fixture
regeneration of one spec to changes in the other.
"""

import argparse
import re
import sys
from collections import defaultdict
from pathlib import Path
from typing import Dict, FrozenSet, List, Set, Tuple

# Env-action label *prefixes* (the action label includes the component arg,
# e.g. EnvComponentReady("postgres")). Any action label whose start matches
# one of these is treated as environment, not reconciler.
ENV_ACTION_PREFIXES = ("EnvComponentReady(", "EnvComponentDegrades(")

# DOT node and edge lines. The label is a quoted string in which DOT escapes
# backslash as \\, quote as \", and newline as \n. The regex must accept those
# escape sequences inside the label.
LABEL_BODY = r'(?:[^"\\]|\\.)*'
NODE_RE = re.compile(r'^(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')
EDGE_RE = re.compile(r'^(-?\d+)\s+->\s+(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')

# compAvail = [postgres |-> FALSE, metadata |-> TRUE, gateway |-> FALSE]
RECORD_ENTRY_RE = re.compile(r"([a-zA-Z]+)\s*\|->\s*(TRUE|FALSE)")

COMPONENTS = ("postgres", "metadata", "gateway")


def unescape_dot(s: str) -> str:
    """Decode DOT label escapes (\\, \", \n) into the corresponding chars."""
    return s.encode("utf-8").decode("unicode_escape")


def parse_var_value(raw: str) -> object:
    """Decode a TLA+ value string into a Python value."""
    raw = raw.strip()
    if raw.startswith('"') and raw.endswith('"'):
        return raw[1:-1]
    if raw == "TRUE":
        return True
    if raw == "FALSE":
        return False
    if raw.startswith("["):
        # Record literal like [postgres |-> FALSE, metadata |-> TRUE, gateway |-> FALSE]
        return {k: (v == "TRUE") for k, v in RECORD_ENTRY_RE.findall(raw)}
    return raw


def decode_label(label: str) -> Dict[str, object]:
    """Decode a DOT node label into a dict of TLA+ variables."""
    state: Dict[str, object] = {}
    body = unescape_dot(label)
    for part in body.split("\n"):
        m = re.match(r"\s*/\\\s*([a-zA-Z]+)\s*=\s*(.+?)\s*$", part)
        if not m:
            continue
        state[m.group(1)] = parse_var_value(m.group(2))
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


def is_env_action(label: str) -> bool:
    return label.startswith(ENV_ACTION_PREFIXES)


def reconciler_closure(
    start: int,
    reconciler_edges: Dict[int, List[int]],
) -> FrozenSet[int]:
    """States reachable from `start` via 1+ reconciler edges, plus `start`
    itself iff a legitimate stutter is permitted there.

    Go's compute layer can fire several TLA actions in one Reconcile call
    (e.g. ReconcileInit + ReconcileRun on the first reconcile from
    uninitialized). The closure therefore tracks the transitive set of
    states reachable via reconciler-only edges.

    A stutter at `start` is legitimate iff `start` has no outgoing
    reconciler edges or has a self-loop reconciler edge. Excluding
    `start` otherwise forces the test to assert that Reconcile advances
    to a model-valid successor — without this, a buggy `computePhase`
    that fails to advance from Provisioning to Ready when all components
    are available would project to `actual == start` and the closure
    check would trivially accept it.
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
    while stack:
        cur = stack.pop()
        for nxt in reconciler_edges.get(cur, ()):
            if nxt not in seen:
                seen.add(nxt)
                stack.append(nxt)
    return frozenset(seen)


def state_key(state: Dict[str, object]) -> Tuple[object, ...]:
    """Project a TLA+ state to a hashable tuple ordered for stable output."""
    avail = state["compAvail"]
    assert isinstance(avail, dict)
    return (
        state["phase"],
        avail["postgres"],
        avail["metadata"],
        avail["gateway"],
    )


GO_HEADER = """// Code generated by scripts/gen-tla-instance-state-tests.py from formal/FireboltInstance.dot. DO NOT EDIT.
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


def go_bool(v: bool) -> str:
    return "true" if v else "false"


def go_state_lit(s: Dict[str, object]) -> str:
    """Positional tlaInstanceState composite literal. Outer type is elided
    because the literal sits inside `[]tlaInstanceState{ … }` (the pool).
    Field order MUST match the tlaInstanceState struct in GO_HEADER."""
    avail = s["compAvail"]
    assert isinstance(avail, dict)
    return (
        "{"
        f'"{s["phase"]}", '
        f'{go_bool(bool(avail["postgres"]))}, '
        f'{go_bool(bool(avail["metadata"]))}, '
        f'{go_bool(bool(avail["gateway"]))}'
        "}"
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dot", required=True, type=Path, help="TLC DOT dump for FireboltInstance")
    parser.add_argument("--out", required=True, type=Path, help="Go fixture output path")
    args = parser.parse_args()

    nodes, edges = parse_dot(args.dot)
    if not nodes:
        print(f"error: no states parsed from {args.dot}", file=sys.stderr)
        return 1

    # Partition edges into reconciler vs environment.
    reconciler_edges: Dict[int, List[int]] = defaultdict(list)
    for src, dst, action in edges:
        if not is_env_action(action):
            reconciler_edges[src].append(dst)

    # Order starting states by content (TLC node IDs are not stable across runs).
    start_ids: List[int] = sorted(nodes.keys(), key=lambda nid: state_key(nodes[nid]))

    # Build the state pool, deduped by projected state_key.
    key_to_pool_idx: Dict[Tuple[object, ...], int] = {}
    pool_states: List[Dict[str, object]] = []

    def pool_idx(state: Dict[str, object]) -> int:
        key = state_key(state)
        idx = key_to_pool_idx.get(key)
        if idx is None:
            idx = len(pool_states)
            key_to_pool_idx[key] = idx
            pool_states.append(state)
        return idx

    for nid in start_ids:
        pool_idx(nodes[nid])

    # Build cases: dedupe starts by state_key; closure entries are pool indices.
    emitted_starts: Set[Tuple[object, ...]] = set()
    cases: List[Tuple[int, List[int]]] = []
    for nid in start_ids:
        start_key = state_key(nodes[nid])
        if start_key in emitted_starts:
            continue
        emitted_starts.add(start_key)
        closure_node_ids = reconciler_closure(nid, reconciler_edges)
        closure_pool: Set[int] = set()
        for cid in closure_node_ids:
            closure_pool.add(pool_idx(nodes[cid]))
        cases.append((pool_idx(nodes[nid]), sorted(closure_pool)))

    out_lines: List[str] = [GO_HEADER]
    out_lines.append(f"// {len(pool_states)} unique reachable TLA+ states.")
    out_lines.append("var tlaInstanceStatePool = []tlaInstanceState{")
    for s in pool_states:
        out_lines.append(f"\t{go_state_lit(s)},")
    out_lines.append("}")
    out_lines.append("")
    out_lines.append(f"// {len(cases)} test cases referencing tlaInstanceStatePool by index.")
    out_lines.append("var tlaInstanceStateCases = []tlaInstanceTestCase{")
    for start_idx, closure_indices in cases:
        closure_str = ", ".join(str(i) for i in closure_indices)
        out_lines.append(f"\t{{{start_idx}, []int{{{closure_str}}}}},")
    out_lines.append("}")
    out_lines.append("")

    args.out.write_text("\n".join(out_lines))
    print(
        f"wrote {args.out}: {len(cases)} test cases over "
        f"{len(pool_states)} pooled states"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
