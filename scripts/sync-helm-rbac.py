#!/usr/bin/env python3
"""Regenerate the operator Helm chart's manager RBAC template from the
canonical kubebuilder output.

The canonical source is `config/rbac/role.yaml`, produced by `controller-gen`
from the `// +kubebuilder:rbac:` markers on the reconcilers. The Helm chart
needs the SAME rule set in two shapes:

  - cluster-wide install (`watchNamespaces=[]`): one `ClusterRole` +
    one `ClusterRoleBinding`.
  - namespaced install (`watchNamespaces=[ns1, ns2, …]`): one `Role`
    plus one `RoleBinding` in each listed namespace. Same rules, just
    a namespaced envelope.

Both shapes live in a single generated template
`helm/firebolt-operator/templates/manager-rbac.yaml`. The chart toggle
(`.Values.watchNamespaces`) picks the shape at install time. The script
is invoked from `make manifests` so any RBAC marker edit propagates to
the chart in the same workflow that regenerates `config/rbac/role.yaml`.
"""

from __future__ import annotations

import pathlib
import sys

import yaml


REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = REPO_ROOT / "config" / "rbac" / "role.yaml"
DST = REPO_ROOT / "helm" / "firebolt-operator" / "templates" / "manager-rbac.yaml"


HEADER = """\
{{- /*
GENERATED FILE. DO NOT EDIT.
Regenerated from config/rbac/role.yaml (the canonical controller-gen
output) by scripts/sync-helm-rbac.py. `make manifests` runs both steps
in order, so changes to `// +kubebuilder:rbac:` markers in the
reconcilers propagate to the chart automatically. A hand-edit here will
be overwritten on the next `make manifests`.

Empty `watchNamespaces` renders a cluster-wide ClusterRole +
ClusterRoleBinding. A non-empty list renders a Role + RoleBinding in
each listed namespace with the same rule set.
*/ -}}
{{- if .Values.rbac.create -}}
{{- if empty .Values.watchNamespaces }}
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


CLUSTER_BINDING = """\
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "firebolt-operator.fullname" . }}-manager
  labels:
    {{- include "firebolt-operator.labels" . | nindent 4 }}
  {{- with .Values.extraAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ include "firebolt-operator.fullname" . }}-manager
subjects:
  - kind: ServiceAccount
    name: {{ include "firebolt-operator.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
"""


NS_LOOP_HEADER = """\
{{- else }}
{{- range $ns := .Values.watchNamespaces }}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "firebolt-operator.fullname" $ }}-manager
  namespace: {{ $ns }}
  labels:
    {{- include "firebolt-operator.labels" $ | nindent 4 }}
  {{- with $.Values.extraAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
rules:
"""


NS_BINDING = """\
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "firebolt-operator.fullname" $ }}-manager
  namespace: {{ $ns }}
  labels:
    {{- include "firebolt-operator.labels" $ | nindent 4 }}
  {{- with $.Values.extraAnnotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "firebolt-operator.fullname" $ }}-manager
subjects:
  - kind: ServiceAccount
    name: {{ include "firebolt-operator.serviceAccountName" $ }}
    namespace: {{ $.Release.Namespace }}
{{- end }}
{{- end }}
"""


FOOTER = "{{- end }}\n"


def _quote_apigroup(group: str) -> str:
    """Match the project's `apiGroups: - ""` style for the core API group
    rather than PyYAML's default `apiGroups: - ''`. Other groups stay bare."""
    if group == "":
        return '""'
    return group


def render_rules(rules: list[dict]) -> str:
    """Emit the rules body with the chart's house style (double-quoted
    core API group, two-space rule indent, six-space list-item indent)."""
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
    rules_body = render_rules(rules)

    rendered = (
        HEADER
        + rules_body
        + CLUSTER_BINDING
        + NS_LOOP_HEADER
        + rules_body
        + NS_BINDING
        + FOOTER
    )
    DST.parent.mkdir(parents=True, exist_ok=True)
    DST.write_text(rendered)

    # Drop the old two files now superseded by manager-rbac.yaml. Idempotent.
    for legacy in ("clusterrole.yaml", "clusterrolebinding.yaml"):
        legacy_path = DST.parent / legacy
        if legacy_path.exists():
            legacy_path.unlink()

    return 0


if __name__ == "__main__":
    sys.exit(main())
