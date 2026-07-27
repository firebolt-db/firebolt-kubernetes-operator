#!/usr/bin/env python3
# Copyright 2026 Firebolt Analytics.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
"""Generate the TLA+ state-cover test fixture for the signing-key rotation
state machine.

For each reachable state in the TLC state graph of SigningKeyRotation.tla,
the generated Go test materialises the state against a fake client
(Status.Auth.SigningKeys, cert-manager Certificates/Secrets, and
FireboltEngines carrying the ObservedAuthHash of their observed render),
runs one ensureSigningKeys call, and verifies that the resulting state lies
in the model's reconciler closure of the starting state.

Input  : formal/SigningKeyRotation.dot                        (from `make formal-dump`)
Output : internal/controller/rotation_tla_states_data_test.go (Go fixture)

Sibling generators handle FireboltEngine (gen-tla-state-tests.py) and
FireboltInstance (gen-tla-instance-state-tests.py). The scripts share the
DOT line shapes but differ in projection, env-action filter, and Go output,
so they are kept separate to avoid coupling fixture regeneration of one
spec to changes in another.
"""

import argparse
import re
import sys
from collections import defaultdict
from pathlib import Path
from typing import Dict, FrozenSet, List, Set, Tuple

# Env-action label *prefixes*. Anything else in the spec is a reconciler
# action; the Quiesced bounded-model stutter counts as reconciler so the
# closure rule grants the quiesced fixed point its legitimate stutter.
ENV_ACTION_PREFIXES = (
    "EnvRotationDue",
    "EnvCertIssued",
    "EnvRetainElapsed",
    "EnvEngineSync(",
)

# DOT node and edge lines. The label is a quoted string in which DOT escapes
# backslash as \\, quote as \", and newline as \n. The regex must accept those
# escape sequences inside the label.
LABEL_BODY = r'(?:[^"\\]|\\.)*'
NODE_RE = re.compile(r'^(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')
EDGE_RE = re.compile(r'^(-?\d+)\s+->\s+(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')

# engineObs = (e1 :> [active |-> 3, other |-> 0] @@ e2 :> [active |-> 1, other |-> 0])
OBS_ENTRY_RE = re.compile(
    r"([a-zA-Z0-9_]+)\s*:>\s*\[\s*active\s*\|->\s*(\d+)\s*,\s*other\s*\|->\s*(\d+)\s*\]"
)


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
    if raw.startswith("("):
        # Function literal mapping engine model values to observation records.
        return {e: (int(a), int(o)) for e, a, o in OBS_ENTRY_RE.findall(raw)}
    return int(raw)


def decode_label(label: str) -> Dict[str, object]:
    """Decode a DOT node label into a dict of TLA+ variables.

    TLC pretty-prints long values (like the engineObs function) across
    several lines, so var rows cannot be parsed line-by-line: flatten the
    label first, then split on the `/\\` row separators.
    """
    state: Dict[str, object] = {}
    body = unescape_dot(label).replace("\n", " ")
    for m in re.finditer(r"/\\\s*([a-zA-Z]+)\s*=\s*(.*?)\s*(?=/\\|$)", body):
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

    One Go ensureSigningKeys call advances at most one rotation step, but a
    mint whose Secret is already issued legitimately covers MintStart and
    MintComplete in a single call, so the closure tracks the transitive set
    of states reachable via reconciler-only edges.

    A stutter at `start` is legitimate iff `start` has no outgoing
    reconciler edges or has a self-loop reconciler edge. Excluding `start`
    otherwise forces the test to assert that the reconciler advances a step
    the model says is due (e.g. a due mint or a converged promote).
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


def engine_names(state: Dict[str, object]) -> List[str]:
    obs = state["engineObs"]
    assert isinstance(obs, dict)
    return sorted(obs)


def state_key(state: Dict[str, object]) -> Tuple[object, ...]:
    """Project a TLA+ state to a hashable tuple ordered for stable output."""
    obs = state["engineObs"]
    assert isinstance(obs, dict)
    return (
        state["activeKey"],
        state["otherKey"],
        state["otherState"],
        state["rotationDue"],
        state["certReady"],
        state["retainElapsed"],
        tuple(obs[e] for e in sorted(obs)),
    )


GO_HEADER = """// Code generated by scripts/gen-tla-rotation-state-tests.py from formal/SigningKeyRotation.dot. DO NOT EDIT.
//
// Run `make formal-gen` to regenerate. The CI guard `make formal-verify` fails
// if this file is out of date relative to the TLA+ spec.

package controller

// tlaRotationObs is one engine's observed render: the key it signs with
// (Active) and the extra key it can validate (Other; 0 = none). It is the
// decomposed form of the engine's ObservedAuthHash.
type tlaRotationObs struct {
\tActive int
\tOther  int
}

// tlaRotationState is one reachable TLA+ state of the SigningKeyRotation
// spec, projected to the variables the rotation harness can materialise and
// observe. Field order is load-bearing: tlaRotationStatePool below uses
// positional composite literals; adding/reordering/removing fields here must
// be done in lockstep with the generator's go_state_lit.
type tlaRotationState struct {
\tActiveKey     int
\tOtherKey      int // 0 = no non-Active key outstanding
\tOtherState    string
\tRotationDue   bool
\tCertReady     bool
\tRetainElapsed bool
\tEngineObs     [@N_ENGINES@]tlaRotationObs // sorted by model engine name
}

// tlaRotationTestCase references tlaRotationStatePool by index. Start is the
// index of the starting state; Closure is the set of indices the model
// considers reachable from Start via 1+ reconciler-only transitions (plus
// Start itself when a stutter is legitimate). The indirection matches the
// engine and instance fixtures' shape.
type tlaRotationTestCase struct {
\tStart   int
\tClosure []int
}

"""


def go_bool(v: bool) -> str:
    return "true" if v else "false"


def go_state_lit(s: Dict[str, object]) -> str:
    """Positional tlaRotationState composite literal. Outer type is elided
    because the literal sits inside `[]tlaRotationState{ … }` (the pool).
    Field order MUST match the tlaRotationState struct in GO_HEADER."""
    obs = s["engineObs"]
    assert isinstance(obs, dict)
    n = len(obs)
    obs_lit = ", ".join(f"{{{obs[e][0]}, {obs[e][1]}}}" for e in sorted(obs))
    return (
        "{"
        f'{s["activeKey"]}, '
        f'{s["otherKey"]}, '
        f'"{s["otherState"]}", '
        f'{go_bool(bool(s["rotationDue"]))}, '
        f'{go_bool(bool(s["certReady"]))}, '
        f'{go_bool(bool(s["retainElapsed"]))}, '
        f"[{n}]tlaRotationObs{{{obs_lit}}}"
        "}"
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dot", required=True, type=Path, help="TLC DOT dump for SigningKeyRotation")
    parser.add_argument("--out", required=True, type=Path, help="Go fixture output path")
    args = parser.parse_args()

    nodes, edges = parse_dot(args.dot)
    if not nodes:
        print(f"error: no states parsed from {args.dot}", file=sys.stderr)
        return 1

    n_engines = len(engine_names(next(iter(nodes.values()))))

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

    out_lines: List[str] = [GO_HEADER.replace("@N_ENGINES@", str(n_engines))]
    out_lines.append(f"// {len(pool_states)} unique reachable TLA+ states.")
    out_lines.append("var tlaRotationStatePool = []tlaRotationState{")
    for s in pool_states:
        out_lines.append(f"\t{go_state_lit(s)},")
    out_lines.append("}")
    out_lines.append("")
    out_lines.append(f"// {len(cases)} test cases referencing tlaRotationStatePool by index.")
    out_lines.append("var tlaRotationStateCases = []tlaRotationTestCase{")
    for start_idx, closure_indices in cases:
        closure_str = ", ".join(str(i) for i in closure_indices)
        out_lines.append(f"\t{{{start_idx}, []int{{{closure_str}}}}},")
    out_lines.append("}")
    out_lines.append("")

    args.out.write_text("\n".join(out_lines))
    print(
        f"wrote {args.out}: {len(cases)} test cases over "
        f"{len(pool_states)} pooled states ({n_engines} engines)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
