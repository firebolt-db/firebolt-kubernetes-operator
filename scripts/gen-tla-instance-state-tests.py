#!/usr/bin/env python3
# Copyright 2026 Firebolt Analytics.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
"""Generate the TLA+ state-cover test fixture for the FireboltInstance reconciler.

Phase 5 of the formal-verification plan (docs/formal-verification.md): for each
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
    itself iff the reconciler legitimately stutters at `start`.

    Stuttering is legitimate when:
      - `start` has no outgoing reconciler edges, OR
      - `start` has a self-loop reconciler edge.

    Including `start` unconditionally would let buggy implementations pass:
    if `computePhase` failed to advance from Provisioning to Ready when
    all components are available, the projection of the post-Reconcile
    state would equal `start` and the closure check would trivially
    accept it, hiding the regression. Excluding `start` forces the test
    to assert that `Reconcile` advances to a model-valid successor.

    Cycles back to `start` via 2+ edges are still respected.
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
type tlaInstanceState struct {
\tPhase         string
\tPostgresAvail bool
\tMetadataAvail bool
\tGatewayAvail  bool
}

// tlaInstanceTestCase pairs a TLA+ state with the set of states the model
// considers reachable from it via 0+ consecutive reconciler-only transitions.
// After instanceSim.Reconcile, the resulting state must lie in this closure.
type tlaInstanceTestCase struct {
\tStart   tlaInstanceState
\tClosure []tlaInstanceState // includes Start (stutter)
}

"""


def go_bool(v: bool) -> str:
    return "true" if v else "false"


def go_state_lit(s: Dict[str, object]) -> str:
    avail = s["compAvail"]
    assert isinstance(avail, dict)
    return (
        "tlaInstanceState{"
        f'Phase: "{s["phase"]}", '
        f'PostgresAvail: {go_bool(bool(avail["postgres"]))}, '
        f'MetadataAvail: {go_bool(bool(avail["metadata"]))}, '
        f'GatewayAvail: {go_bool(bool(avail["gateway"]))}'
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

    out_lines: List[str] = [GO_HEADER]
    out_lines.append(f"// {len(start_ids)} reachable states")
    out_lines.append("var tlaInstanceStateCases = []tlaInstanceTestCase{")

    for nid in start_ids:
        start = nodes[nid]
        closure_ids = reconciler_closure(nid, reconciler_edges)
        # Deduplicate closure entries by projected key, ordered for stability.
        seen_keys: Set[Tuple[object, ...]] = set()
        closure_states: List[Dict[str, object]] = []
        for cid in sorted(closure_ids, key=lambda c: state_key(nodes[c])):
            cstate = nodes[cid]
            key = state_key(cstate)
            if key in seen_keys:
                continue
            seen_keys.add(key)
            closure_states.append(cstate)

        out_lines.append("\t{")
        out_lines.append(f"\t\tStart: {go_state_lit(start)},")
        out_lines.append("\t\tClosure: []tlaInstanceState{")
        for cstate in closure_states:
            out_lines.append(f"\t\t\t{go_state_lit(cstate)},")
        out_lines.append("\t\t},")
        out_lines.append("\t},")

    out_lines.append("}")
    out_lines.append("")

    args.out.write_text("\n".join(out_lines))
    print(f"wrote {args.out}: {len(start_ids)} test cases")
    return 0


if __name__ == "__main__":
    sys.exit(main())
