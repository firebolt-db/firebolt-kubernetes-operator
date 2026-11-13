#!/usr/bin/env python3
"""Exercise release selection against packaged charts in temporary repositories."""

import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import yaml


ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("gate", Path(__file__).with_name("crd-release-needed.py"))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)

CRD = {
    "apiVersion": "apiextensions.k8s.io/v1",
    "kind": "CustomResourceDefinition",
    "metadata": {"name": "examples.example.com"},
    "spec": {
        "group": "example.com",
        "scope": "Namespaced",
        "names": {"kind": "Example", "plural": "examples"},
        "versions": [{
            "name": "v1", "served": True, "storage": True,
            "schema": {"openAPIV3Schema": {
                "type": "object", "description": "Original docs",
                "properties": {
                    "description": {"type": "string", "description": "A user field"},
                    "count": {"type": "integer", "default": 1},
                    "options": {"type": "object", "default": {"description": "value"}},
                    "entries": {"type": "array", "items": {
                        "type": "object", "description": "An entry",
                        "additionalProperties": {"type": "string", "description": "A value"},
                    }},
                },
            }},
        }],
    },
}


class ReleaseComparisonTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.previous = Path.cwd()
        os.chdir(self.tmp.name)
        self.addCleanup(os.chdir, self.previous)
        self.git("init", "-q")
        self.git("config", "user.name", "Test")
        self.git("config", "user.email", "test@example.com")
        self.write("Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.0\n")
        self.write("values.yaml", "{}\n")
        self.write(".helmignore", "json-schema/\n*.tmp\n")
        self.write("templates/crd.yaml", yaml.safe_dump(CRD))
        self.commit()
        self.git("tag", gate.TAG_PREFIX + "0.1.0")

    def git(self, *args):
        return subprocess.check_output(["git", *args], stderr=subprocess.STDOUT)

    def write(self, name, content):
        path = Path(gate.CHART) / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        self.git("add", str(path))

    def commit(self):
        self.git("commit", "-qm", "fix: update chart")

    def test_documentation_formatting_and_excluded_files_do_not_release(self):
        crd = copy.deepcopy(CRD)
        schema = crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"]
        schema["description"] = "Updated docs"
        schema["properties"]["description"]["description"] = "Updated field docs"
        items = schema["properties"]["entries"]["items"]
        items["description"] = "Updated entry docs"
        items["additionalProperties"]["description"] = "Updated value docs"
        self.write("templates/crd.yaml", "# formatting\n" + yaml.safe_dump(crd, sort_keys=False))
        self.write("Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.1\ndescription: Docs\n")
        self.write("README.md", "Documentation\n")
        self.write("CHANGELOG.md", "Release notes\n")
        self.write("json-schema/generated.json", '{"changed": true}')
        self.write("scratch.tmp", "excluded")
        self.commit()
        self.assertFalse(gate.release_needed())

    def test_schema_changes_release(self):
        changes = [
            lambda s: s["properties"].update({"newField": {"type": "string"}}),
            lambda s: s["properties"].pop("description"),
            lambda s: s["properties"]["description"].update(type="integer"),
            lambda s: s["properties"]["count"].update(default=2),
            lambda s: s["properties"]["count"].update(default=True),
            lambda s: s["properties"]["options"]["default"].update(description="new value"),
            lambda s: s.update(required=["count"]),
            lambda s: s.update({"x-kubernetes-validations": [{"rule": "self.count > 0"}]}),
            lambda s: s["properties"]["entries"]["items"]["additionalProperties"].update(type="integer"),
        ]
        for change in changes:
            with self.subTest(change=change):
                crd = copy.deepcopy(CRD)
                change(crd["spec"]["versions"][0]["schema"]["openAPIV3Schema"])
                self.write("templates/crd.yaml", yaml.safe_dump(crd))
                self.commit()
                self.assertTrue(gate.release_needed())

    def test_conversion_and_version_changes_release(self):
        for change in (
            {"conversion": {"strategy": "Webhook"}},
            {"scope": "Cluster"},
            {"versions": [dict(CRD["spec"]["versions"][0], name="v2")]},
        ):
            with self.subTest(change=change):
                crd = copy.deepcopy(CRD)
                crd["spec"].update(change)
                self.write("templates/crd.yaml", yaml.safe_dump(crd))
                self.commit()
                self.assertTrue(gate.release_needed())

    def test_change_before_latest_commit_still_releases(self):
        self.write("values.yaml", "enabled: true\n")
        self.commit()
        self.write("README.md", "Later docs\n")
        self.commit()
        self.assertTrue(gate.release_needed())
        self.git("tag", gate.TAG_PREFIX + "0.2.0")
        self.assertFalse(gate.release_needed())

    def test_chart_installation_change_releases(self):
        self.write("Chart.yaml", "apiVersion: v2\nname: example-crds\nversion: 0.1.1\nkubeVersion: '>=1.30.0'\n")
        self.commit()
        self.assertTrue(gate.release_needed())

    def test_helmignore_change_uses_each_revisions_package(self):
        self.write("json-schema/generated.json", '{"changed": true}')
        self.commit()
        self.assertFalse(gate.release_needed())
        self.write(".helmignore", "*.tmp\n")
        self.commit()
        self.assertTrue(gate.release_needed())

    def test_new_and_removed_manifests_release(self):
        self.write("templates/extra.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: extra}\n")
        self.commit()
        self.assertTrue(gate.release_needed())
        self.git("tag", gate.TAG_PREFIX + "0.2.0")
        self.git("rm", gate.CHART + "/templates/extra.yaml")
        self.commit()
        self.assertTrue(gate.release_needed())

    def test_helm_template_change_releases(self):
        self.write("templates/helper.tpl", '{{ define "name" }}example{{ end }}')
        self.commit()
        self.assertTrue(gate.release_needed())

    def test_invalid_yaml_fails(self):
        self.write("templates/crd.yaml", "broken: [\n")
        self.commit()
        with self.assertRaises(yaml.YAMLError):
            gate.release_needed()

    def test_missing_release_tag_fails(self):
        self.git("tag", "-d", gate.TAG_PREFIX + "0.1.0")
        with self.assertRaises(subprocess.CalledProcessError):
            gate.release_needed()


class WorkflowTests(unittest.TestCase):
    def test_component_split_preserves_release_policy(self):
        main = json.loads((ROOT / "release-please-config.json").read_text())
        crds = json.loads((ROOT / "release-please-crds-config.json").read_text())
        self.assertEqual(set(main.pop("packages")), {".", "helm/firebolt-operator"})
        self.assertEqual(set(crds.pop("packages")), {gate.CHART})
        self.assertEqual(main, crds)

    def test_gate_does_not_skip_tagging_or_publication(self):
        workflow = yaml.safe_load((ROOT / ".github/workflows/release-main.yaml").read_text())
        job = workflow["jobs"]["release-please"]
        steps = {step["id"]: step for step in job["steps"] if "id" in step}
        release = steps["crds-release"]
        self.assertNotIn("if", release)
        self.assertNotIn("skip-github-release", release["with"])
        self.assertEqual(release["with"]["skip-github-pull-request"], "${{ steps.crds.outputs.needed != 'true' }}")
        self.assertIn("steps.crds-release.outputs[", job["outputs"]["helm_crds_release_created"])
        self.assertEqual(steps["versions"]["env"]["CRDS_PRS_JSON"], "${{ steps.crds-release.outputs.prs || '[]' }}")


if __name__ == "__main__":
    unittest.main()
