#!/usr/bin/env python3
# Copyright 2026 Firebolt Analytics.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
"""Generate the TLA+ state-cover test fixture for the signing-key rotation state machine.

For each reachable TLA+ state of SigningKeyRotation.tla, the generated Go test
materializes a FireboltInstance whose Status.Auth matches the state (plus fake
engines standing in for the fleet's convergence), calls the production
stepSigningKeyRotation, and verifies that the resulting state lies in the
model's reconciler closure of the starting state.

Input  : formal/SigningKeyRotation.dot                        (from `make formal-dump`)
Output : internal/controller/rotation_tla_states_data_test.go  (Go fixture)

Two projections lose information on purpose, because the Go side genuinely
cannot see the difference:

  * `minting` collapses into `absent`. A key whose Certificate is applied but
    whose Secret is not ready yet is not in Status.Auth.SigningKeys and does not
    bump SigningKeyGeneration, so no field distinguishes it from a key that does
    not exist. The corresponding reconciler edges still appear in the closure, so
    a Reconcile that performs MintStart and MintReady in one call is accepted.

  * The per-engine `observed` mapping collapses into a single Converged flag.
    stepSigningKeyRotation reads only "has the whole fleet converged", so that is
    all the harness can drive. The richer mapping exists in the model to express
    NoValidationGap, which is a property of the fleet rather than of one call.

Sibling generators (gen-tla-state-tests.py, gen-tla-instance-state-tests.py)
handle FireboltEngine and FireboltInstance. All three share the DOT line shapes
but differ in projection and Go output, so they stay separate scripts rather
than coupling one spec's fixture regeneration to another's changes.
"""
import argparse
import re
import sys
from collections import defaultdict
from pathlib import Path
from typing import Dict, FrozenSet, List, Set, Tuple

# Action label prefixes that belong to the environment rather than the
# reconciler. The label carries its argument (e.g. EngineRolls(e1)), so these
# match on the start of the label.
ENV_ACTION_PREFIXES = ("EngineRolls(", "RetainElapses(")

LABEL_BODY = r'(?:[^"\\]|\\.)*'
NODE_RE = re.compile(r'^(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')
EDGE_RE = re.compile(r'^(-?\d+)\s+->\s+(-?\d+)\s+\[label="(' + LABEL_BODY + r')"')

# keyPhase = <<"active", "validationOnly", "absent">>
TUPLE_ENTRY_RE = re.compile(r'"([a-zA-Z]+)"')
# demoted = {1, 3}   /   anchored = {}
SET_INT_RE = re.compile(r"\d+")
# observed = (e1 :> [active |-> 1, keys |-> {1, 2}] @@ e2 :> [...])
OBSERVED_RE = re.compile(r"\[active\s*\|->\s*(\d+),\s*keys\s*\|->\s*\{([^}]*)\}\]")

# Go-visible phase for each modeled phase. "minting" is not representable in
# Status.Auth.SigningKeys, so it projects the same as "absent".
PHASE_PROJECTION = {
    "absent": "absent",
    "minting": "absent",
    "validationOnly": "validationOnly",
    "active": "active",
    "removing": "removing",
}


def unescape_dot(s: str) -> str:
    """Decode DOT label escapes (\\, \", \n) into the corresponding chars."""
    return s.encode("utf-8").decode("unicode_escape")


def decode_label(label: str) -> Dict[str, object]:
    """Decode a DOT node label into a dict of raw TLA+ variable strings."""
    state: Dict[str, object] = {}
    for part in unescape_dot(label).split("\n"):
        m = re.match(r"\s*/\\\s*([a-zA-Z]+)\s*=\s*(.+?)\s*$", part)
        if m:
            state[m.group(1)] = m.group(2).strip()
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
                edges.append((int(m_edge.group(1)), int(m_edge.group(2)), unescape_dot(m_edge.group(3))))
                continue
            m_node = NODE_RE.match(line)
            if m_node:
                nid = int(m_node.group(1))
                if nid not in nodes:
                    nodes[nid] = decode_label(m_node.group(2))
    return nodes, edges


def is_env_action(label: str) -> bool:
    return label.startswith(ENV_ACTION_PREFIXES)


def int_set(raw: str) -> Set[int]:
    return {int(n) for n in SET_INT_RE.findall(raw)}


def converged(state: Dict[str, object]) -> bool:
    """Recompute the model's Converged predicate from the dumped variables.

    TLC dumps only VARIABLES, so the derived RenderedConfig has to be rebuilt
    here: the active kid plus every rendered (active or validate-only) kid,
    compared against what each engine reports observing.
    """
    phases = TUPLE_ENTRY_RE.findall(str(state["keyPhase"]))
    rendered = {i + 1 for i, p in enumerate(phases) if p in ("active", "validationOnly")}
    actives = [i + 1 for i, p in enumerate(phases) if p == "active"]
    assert len(actives) == 1, f"expected exactly one active key, got {actives}"
    active = actives[0]
    seen = OBSERVED_RE.findall(str(state["observed"]))
    assert seen, f"could not parse observed from {state['observed']!r}"
    return all(int(a) == active and int_set(keys) == rendered for a, keys in seen)


def state_key(state: Dict[str, object]) -> Tuple[object, ...]:
    """Project a TLA+ state onto what the Go harness can materialize and observe."""
    phases = TUPLE_ENTRY_RE.findall(str(state["keyPhase"]))
    demoted = int_set(str(state["demoted"]))
    anchored = int_set(str(state["anchored"]))
    retain_done = int_set(str(state["retainDone"]))
    keys: List[Tuple[str, bool, bool, bool]] = []
    for i, phase in enumerate(phases):
        kid = i + 1
        keys.append((
            PHASE_PROJECTION[phase],
            kid in demoted,
            kid in anchored,
            kid in retain_done,
        ))
    return (tuple(keys), int(str(state["gen"])), converged(state))


def reconciler_closure(start: int, reconciler_edges: Dict[int, List[int]]) -> FrozenSet[int]:
    """States reachable from `start` via 1+ reconciler edges, plus `start` itself
    iff a legitimate stutter is permitted there.

    One Reconcile can fire several model actions (MintStart then MintReady in a
    single call, for instance), so the closure is transitive. `start` is included
    only when it has no outgoing reconciler edge or a self-loop; otherwise the
    test would accept a stepSigningKeyRotation that failed to advance at all.
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


GO_HEADER = """// Code generated by scripts/gen-tla-rotation-state-tests.py from formal/SigningKeyRotation.dot. DO NOT EDIT.
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


def go_bool(v: bool) -> str:
    return "true" if v else "false"


def go_state_lit(key: Tuple[object, ...]) -> str:
    """Positional tlaRotationState composite literal. The outer type is elided
    because the literal sits inside `[]tlaRotationState{ … }`."""
    keys, gen, conv = key
    assert isinstance(keys, tuple)
    parts = []
    for phase, demoted, anchored, retain in keys:  # type: ignore[misc]
        parts.append(f'{{"{phase}", {go_bool(demoted)}, {go_bool(anchored)}, {go_bool(retain)}}}')
    return "{[]tlaRotationKey{" + ", ".join(parts) + "}, " + f"{gen}, {go_bool(bool(conv))}" + "}"


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dot", required=True, type=Path, help="TLC DOT dump for SigningKeyRotation")
    parser.add_argument("--out", required=True, type=Path, help="Go fixture output path")
    args = parser.parse_args()

    nodes, edges = parse_dot(args.dot)
    if not nodes:
        print(f"error: no states parsed from {args.dot}", file=sys.stderr)
        return 1

    reconciler_edges: Dict[int, List[int]] = defaultdict(list)
    for src, dst, action in edges:
        if not is_env_action(action):
            reconciler_edges[src].append(dst)

    # TLC node IDs are not stable across runs, so order by projected content.
    start_ids = sorted(nodes.keys(), key=lambda nid: str(state_key(nodes[nid])))

    key_to_pool_idx: Dict[Tuple[object, ...], int] = {}
    pool_keys: List[Tuple[object, ...]] = []

    def pool_idx(state: Dict[str, object]) -> int:
        key = state_key(state)
        idx = key_to_pool_idx.get(key)
        if idx is None:
            idx = len(pool_keys)
            key_to_pool_idx[key] = idx
            pool_keys.append(key)
        return idx

    for nid in start_ids:
        pool_idx(nodes[nid])

    emitted: Set[Tuple[object, ...]] = set()
    cases: List[Tuple[int, List[int]]] = []
    for nid in start_ids:
        key = state_key(nodes[nid])
        if key in emitted:
            continue
        emitted.add(key)
        closure: Set[int] = set()
        for cid in reconciler_closure(nid, reconciler_edges):
            closure.add(pool_idx(nodes[cid]))
        cases.append((pool_idx(nodes[nid]), sorted(closure)))

    out_lines: List[str] = [GO_HEADER]
    out_lines.append(f"// {len(pool_keys)} unique reachable TLA+ states, projected.")
    out_lines.append("var tlaRotationStatePool = []tlaRotationState{")
    for key in pool_keys:
        out_lines.append(f"\t{go_state_lit(key)},")
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
    print(f"wrote {args.out}: {len(cases)} test cases over {len(pool_keys)} pooled states")
    return 0


if __name__ == "__main__":
    sys.exit(main())
