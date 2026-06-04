#!/usr/bin/env python3
# Write description-slimmed copies of the CRDs into the CRD Helm chart's
# templates/ directory.
#
# WHY THIS EXISTS
# ---------------
# Helm stores every release in a Secret (`sh.helm.release.v1.<name>.<rev>`)
# that Kubernetes caps at 1 MiB (1048576 bytes). The release embeds both the
# chart files AND the rendered manifest, gzipped + base64. Our CRDs each
# embed a full corev1.PodSpec (gateway.template, metadata.template, the
# engine template) whose OpenAPI schema is dominated by upstream Kubernetes
# Go-doc `description:` text -- ~63% of fireboltinstances is descriptions.
# Shipped verbatim, the release Secret is ~1.05 MB and `helm install` fails:
#
#   Error: INSTALLATION FAILED: Secret "sh.helm.release.v1.firebolt-crds.v1"
#   is invalid: data: Too long: may not be more than 1048576 bytes
#
# Stripping descriptions cuts each CRD ~68% (release Secret -> ~0.23 MB) with
# zero effect on validation: descriptions do not participate in structural
# schema validation, CEL rules, pruning, or defaulting.
#
# WHAT IT KEEPS
# -------------
# Only descriptions *strictly inside* a `template` sub-schema are removed --
# i.e. the embedded PodSpec docs, which are upstream Kubernetes fields fully
# documented at kubernetes.io. Firebolt's own API field docs (including the
# `template` field's own doc) are kept so `kubectl explain
# fireboltinstance.spec.gateway` still documents our fields.
#
# ORDERING CONTRACT (see Makefile `manifests`)
# --------------------------------------------
# This MUST run after scripts/patch-crd-template-metadata.py. That patch keys
# off the controller-gen description "Standard object's metadata." to inject
# x-kubernetes-preserve-unknown-fields on every embedded ObjectMeta (the fix
# that stops ArgoCD/Flux pruning pod-template labels/annotations). If we
# stripped first, the discriminator would be gone and the marker would land
# nowhere. We read the already-patched config/crd/bases (its full-fat,
# canonical form -- left untouched for kustomize, envtest, kubectl explain,
# and the json-schema artifacts) and emit slimmed copies into the chart.
#
# WHY LINE-ORIENTED (not YAML-AST)
# --------------------------------
# Same rationale as patch-crd-template-metadata.py: PyYAML rewrites block
# scalars to inline `\n`-escaped strings, exploding the diff. A line-oriented
# pass removes whole `description:` blocks and leaves every other byte intact,
# so the chart copy stays a readable, reviewable subset of the canonical CRD.

from __future__ import annotations

import pathlib
import re
import sys

# A mapping key: leading indent (group 1) + key name (group 2). The value, if
# any, is irrelevant here. Block-scalar / wrapped continuation lines of a
# scalar value never match (a word followed by a space, not a colon), so they
# never pollute the ancestor stack -- and we consume description blocks as a
# unit regardless, so an in-prose "http://..." can't be mistaken for a key.
KEY = re.compile(r"^(\s*)([\w.\-/]+):(?:\s|$)")


def _indent(line: str) -> int:
    return len(line) - len(line.lstrip(" "))


def _description_block_end(lines: list[str], start: int, key_indent: int) -> int:
    """Return the index one past the end of the description scalar that begins
    at `start` (the line after the `description:` key). Continuation lines are
    more-indented than the key; embedded blank lines (folded-scalar paragraph
    breaks) belong to the block iff a later non-blank line is still deeper."""
    j = start
    n = len(lines)
    while j < n:
        if lines[j].strip() == "":
            k = j
            while k < n and lines[k].strip() == "":
                k += 1
            if k < n and _indent(lines[k]) > key_indent:
                j = k
                continue
            break
        if _indent(lines[j]) > key_indent:
            j += 1
            continue
        break
    return j


def strip_template_descriptions(text: str) -> tuple[str, int]:
    """Remove `description:` blocks that sit strictly below a `template` key.
    Returns (new_text, removed_count)."""
    lines = text.split("\n")
    out: list[str] = []
    stack: list[tuple[int, str]] = []  # (indent, key) ancestor chain
    removed = 0
    i = 0
    n = len(lines)
    while i < n:
        line = lines[i]
        m = KEY.match(line)
        if not m:
            out.append(line)
            i += 1
            continue
        ind = len(m.group(1))
        key = m.group(2)
        while stack and stack[-1][0] >= ind:
            stack.pop()
        if key == "description":
            end = _description_block_end(lines, i + 1, ind)
            parent = stack[-1][1] if stack else None
            inside_template = any(k == "template" for _, k in stack)
            # Strip only the embedded PodSpec docs: a description nested below
            # `template`, but not `template`'s own description (parent ==
            # "template"), which is a Firebolt-authored doc worth keeping.
            if inside_template and parent != "template":
                removed += 1
            else:
                out.extend(lines[i:end])
            i = end
            continue
        stack.append((ind, key))
        out.append(line)
        i += 1
    return "\n".join(out), removed


# CRD metadata.name is "<plural>.<group>"; the chart template is named
# "<plural>.yaml" (matching the symlinks this generation step replaces).
CRD_NAME = re.compile(r"^\s{2}name:\s*([a-z0-9]+)\.", re.MULTILINE)


def chart_filename(text: str, src: pathlib.Path) -> str:
    m = CRD_NAME.search(text)
    if not m:
        raise SystemExit(f"{src}: could not find CRD metadata.name (line '  name: <plural>.<group>')")
    return f"{m.group(1)}.yaml"


def main(argv: list[str]) -> int:
    if len(argv) < 3 or argv[1] != "--out-dir":
        print("usage: strip-crd-descriptions.py --out-dir <dir> <crd.yaml> [<crd.yaml> ...]", file=sys.stderr)
        return 2
    out_dir = pathlib.Path(argv[2])
    out_dir.mkdir(parents=True, exist_ok=True)
    for src_str in argv[3:]:
        src = pathlib.Path(src_str)
        text = src.read_text()
        slim, removed = strip_template_descriptions(text)
        dst = out_dir / chart_filename(text, src)
        # Remove any pre-existing entry first. Earlier revisions shipped these
        # as symlinks into config/crd/bases; write_text() would follow the
        # link and clobber the canonical CRD. unlink() drops the link itself.
        dst.unlink(missing_ok=True)
        dst.write_text(slim)
        saved = len(text) - len(slim)
        print(f"{dst}: stripped {removed} pod-template description block(s), -{saved} bytes")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
