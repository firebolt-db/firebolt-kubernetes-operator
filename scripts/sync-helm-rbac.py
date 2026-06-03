#!/usr/bin/env python3
"""Regenerate the operator Helm chart's ClusterRole template from the canonical
kubebuilder output.

The canonical source is `config/rbac/role.yaml`, produced by `controller-gen`
from the `// +kubebuilder:rbac:` markers on the reconcilers. The Helm chart at
`helm/firebolt-operator/templates/clusterrole.yaml` needs the SAME rule set
but with Helm-templated metadata (release-derived name, labels, annotations,
`.Values.rbac.create` toggle).

Historical drift between these two files has caused real outages — the
file used to be hand-maintained, and changes to the kubebuilder markers
were applied to `config/rbac/role.yaml` without a matching edit to the
chart. Re-generating from the canonical source removes that drift entirely.

The script is idempotent: re-running with no marker changes produces no
diff. It is invoked from `make manifests` so RBAC edits in the controllers
propagate to the chart in the same workflow that regenerates
`config/rbac/role.yaml` itself.
"""

from __future__ import annotations

import pathlib
import sys

import yaml


REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = REPO_ROOT / "config" / "rbac" / "role.yaml"
DST = REPO_ROOT / "helm" / "firebolt-operator" / "templates" / "clusterrole.yaml"


HEADER = """\
{{- /*
GENERATED FILE. DO NOT EDIT.
Regenerated from config/rbac/role.yaml (the canonical controller-gen
output) by scripts/sync-helm-rbac.py. `make manifests` runs both steps
in order, so changes to `// +kubebuilder:rbac:` markers in the
reconcilers propagate to the chart automatically. A hand-edit here will
be overwritten on the next `make manifests`.
*/ -}}
{{- if .Values.rbac.create -}}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "firebolt-operator.fullname" . }}-manager
  labels:
    {{- include "firebolt-operator.labels" . | nindent 4 }}
  {{- with .Values.extraAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
rules:
"""


FOOTER = "{{- end }}\n"


def _quote_apigroup(group: str) -> str:
    """Match the project's `apiGroups: - ""` style for the core API group
    rather than PyYAML's default `apiGroups: - ''`. Other groups stay bare."""
    if group == "":
        return '""'
    return group


def render_rules(rules: list[dict]) -> str:
    """Emit the chart's `rules:` body with a fixed shape:

        - apiGroups:
            - "<group>"
          resources:
            - <res>
          verbs:
            - <verb>

    PyYAML's safe_dump doesn't produce this exact style — it indents lists
    flat with their parent key and uses single quotes for empty strings —
    so the helper hand-formats each rule to match the chart's house style
    and keep diffs against the prior hand-maintained file minimal."""
    out: list[str] = []
    for rule in rules:
        out.append("  - apiGroups:")
        for group in rule.get("apiGroups", []):
            out.append(f"      - {_quote_apigroup(group)}")
        if "resources" in rule:
            out.append("    resources:")
            for resource in rule["resources"]:
                out.append(f"      - {resource}")
        if "resourceNames" in rule:
            out.append("    resourceNames:")
            for name in rule["resourceNames"]:
                out.append(f"      - {name}")
        if "verbs" in rule:
            out.append("    verbs:")
            for verb in rule["verbs"]:
                out.append(f"      - {verb}")
    return "\n".join(out) + "\n"


def main() -> int:
    if not SRC.exists():
        print(f"error: {SRC} not found (run `make manifests` first)", file=sys.stderr)
        return 1

    canonical = yaml.safe_load(SRC.read_text())
    if not canonical or canonical.get("kind") != "ClusterRole":
        print(f"error: {SRC} is not a ClusterRole document", file=sys.stderr)
        return 1
    rules = canonical.get("rules") or []

    rendered = HEADER + render_rules(rules) + FOOTER
    DST.parent.mkdir(parents=True, exist_ok=True)
    DST.write_text(rendered)
    return 0


if __name__ == "__main__":
    sys.exit(main())
