#!/usr/bin/env python3
# Drop `containers` from the `required` list on every embedded pod-template
# `.spec` in our CRDs.
#
# Each CRD embeds a corev1.PodTemplateSpec (FireboltEngine.spec.template,
# FireboltEngineClass.spec.template, and FireboltInstance.spec.{gateway,
# metadata}.template). controller-gen faithfully renders PodSpec's `containers`
# as a required field, emitting `required: [containers]` under each
# `template.spec`. But these templates are *fragments*: the operator injects
# the engine / gateway / metadata container at StatefulSet / Deployment build
# time, so a CR legitimately sets only pod-level fields (serviceAccountName,
# nodeSelector, tolerations, pod labels, an optional sidecar) and no
# `containers`. Kubernetes 1.36 enforces the embedded `required: [containers]`
# at write time, so such a CR is rejected with
# `spec.template.spec.containers: Required value` (older apiservers did not
# enforce it on these embedded sub-schemas). This script removes the
# requirement after controller-gen so the fragment model keeps working; the
# operator's own pod-template validation (ValidatePodTemplate / the
# operator-owned-path rules) is unaffected.
#
# Line-oriented (not YAML-AST) for the same reason as
# patch-crd-template-metadata.py: a PyYAML round-trip rewrites every block
# scalar and would bury this change in a multi-thousand-line diff.
#
# PodSpec's only required field is `containers`, so the target is always a
# two-line `required:\n- containers` block whose item is the sole entry.
# controller-gen emits list items at the same indentation as their key. The
# negative lookahead refuses to touch any `required` list that carries
# additional siblings (none exist today — it just keeps the patch conservative).

from __future__ import annotations

import pathlib
import re
import sys

CONTAINERS_REQUIRED = re.compile(
    r"^(?P<indent>[ ]+)required:\n(?P=indent)- containers\n(?!(?P=indent)- )",
    re.MULTILINE,
)


def patch(text: str) -> tuple[str, int]:
    return CONTAINERS_REQUIRED.subn("", text)


def main(paths: list[str]) -> int:
    for path_str in paths:
        path = pathlib.Path(path_str)
        new_text, count = patch(path.read_text())
        if count:
            path.write_text(new_text)
        print(f"{path}: removed `containers` from {count} pod-template required list(s)")
    return 0


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("usage: patch-crd-template-required.py <crd.yaml> [<crd.yaml> ...]", file=sys.stderr)
        sys.exit(2)
    sys.exit(main(sys.argv[1:]))
