#!/usr/bin/env python3
# Inject `x-kubernetes-preserve-unknown-fields: true` on every embedded
# ObjectMeta sub-schema in our CRDs.
#
# controller-gen renders the ObjectMeta from any embedded `metav1.ObjectMeta`
# (e.g. inside `corev1.PodTemplateSpec`) as a bare `{type: object}` with no
# declared properties. Structural-schema pruning then strips every key
# under `<parent>.metadata` (including `labels` and `annotations`) at
# apiserver write time, so a CR applied with pod-template labels /
# annotations round-trips as `metadata: {}` and an ArgoCD/Flux re-apply
# loop never settles.
#
# The +kubebuilder:pruning:PreserveUnknownFields marker on the field can
# only land at the field-level schema (the parent `template`), and the
# Kubernetes structural-schema rule does NOT propagate that marker into a
# child that has its own typed sub-schema like `metadata: {type:
# object}`. This script walks each CRD post-controller-gen and pushes
# the marker down to every embedded ObjectMeta so labels / annotations
# survive admission.
#
# Discriminator: the controller-gen-emitted description "Standard object's
# metadata." only appears on schemas generated from an embedded
# `metav1.ObjectMeta`. The top-level CR `metadata` field is left blank
# (no description) by controller-gen and is handled by the apiserver
# directly, so it is not affected by this script.
#
# Why line-oriented (not YAML-AST): PyYAML rewrites block-scalar
# descriptions to inline `\n`-escaped strings, which would produce a
# multi-thousand-line diff against controller-gen's output and obscure
# every future regen. The line-oriented patch is fragile to a
# controller-gen formatting change but produces a 1-line-per-injection
# diff that's easy to review.

from __future__ import annotations

import pathlib
import re
import sys

# Matches the controller-gen stub for an embedded metav1.ObjectMeta:
#
#                       metadata:
#                         description: |-
#                           Standard object's metadata.
#                           More info: https://.../sig-architecture/api-conventions.md#metadata
#                         type: object
#
# Captures the indentation of `description:` (group 1) to mirror it on
# the inserted `x-kubernetes-preserve-unknown-fields:` line so the YAML
# stays well-formed.
OBJECT_META_STUB = re.compile(
    r"""
    ^(?P<indent>[ ]+)description:[ ]\|-\n
    (?P=indent)[ ]{2}Standard[ ]object's[ ]metadata\.\n
    (?P=indent)[ ]{2}More[ ]info:[ ]https://git\.k8s\.io/community/contributors/devel/sig-architecture/api-conventions\.md\#metadata\n
    (?P=indent)type:[ ]object\n
    """,
    re.MULTILINE | re.VERBOSE,
)


def patch(text: str) -> tuple[str, int]:
    def repl(match: re.Match[str]) -> str:
        indent = match.group("indent")
        marker = f"{indent}x-kubernetes-preserve-unknown-fields: true\n"
        return match.group(0) + marker

    new_text, count = OBJECT_META_STUB.subn(repl, text)
    return new_text, count


def main(paths: list[str]) -> int:
    for path_str in paths:
        path = pathlib.Path(path_str)
        text = path.read_text()
        new_text, count = patch(text)
        if count:
            path.write_text(new_text)
        print(f"{path}: patched {count} embedded ObjectMeta sub-schema(s)")
    return 0


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("usage: patch-crd-template-metadata.py <crd.yaml> [<crd.yaml> ...]", file=sys.stderr)
        sys.exit(2)
    sys.exit(main(sys.argv[1:]))
