#!/usr/bin/env python3
"""Compare the packaged CRD chart with its last release."""

import argparse
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

import yaml


CHART = "helm/firebolt-operator-crds"
TAG_PREFIX = "firebolt-operator-crds-chart-"
LOADER = yaml.CSafeLoader


def run(*args):
    return subprocess.check_output(args)


def without_schema_descriptions(schema):
    """Visit schema nodes, never property names or literal defaults/examples."""
    if not isinstance(schema, dict):
        return schema
    result = copy.deepcopy(schema)
    result.pop("description", None)
    for key in ("properties", "patternProperties", "definitions", "dependencies"):
        if isinstance(result.get(key), dict):
            result[key] = {
                name: without_schema_descriptions(value)
                for name, value in result[key].items()
            }
    for key in ("items", "additionalItems", "additionalProperties", "not"):
        if key in result:
            value = result[key]
            result[key] = (
                [without_schema_descriptions(item) for item in value]
                if isinstance(value, list)
                else without_schema_descriptions(value)
            )
    for key in ("allOf", "anyOf", "oneOf"):
        if key in result:
            result[key] = [without_schema_descriptions(item) for item in result[key]]
    return result


def normalize_resource(resource):
    if not isinstance(resource, dict) or resource.get("kind") != "CustomResourceDefinition":
        return resource
    resource = copy.deepcopy(resource)
    spec = resource["spec"]
    schemas = [version["schema"] for version in spec["versions"] if "schema" in version]
    for schema in schemas:
        schema["openAPIV3Schema"] = without_schema_descriptions(schema["openAPIV3Schema"])
    return resource


def normalize_file(name, content):
    if name == "Chart.yaml":
        chart = yaml.load(content, Loader=LOADER)
        del chart["version"]
        chart.pop("description", None)
        return json.dumps(chart, sort_keys=True)
    if name.endswith((".yaml", ".yml")) and b"{{" not in content:
        return json.dumps(
            [normalize_resource(doc) for doc in yaml.load_all(content, Loader=LOADER)],
            sort_keys=True,
        )
    return content


def packaged_chart(ref):
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        archive = run("git", "archive", ref, CHART)
        with tarfile.open(fileobj=io.BytesIO(archive)) as source:
            source.extractall(root, filter="data")
        output = root / "packages"
        output.mkdir()
        run("helm", "package", str(root / CHART), "--destination", str(output))
        packages = list(output.glob("*.tgz"))
        if len(packages) != 1:
            raise ValueError("expected one packaged CRD chart")
        result = {}
        with tarfile.open(packages[0]) as package:
            for member in package.getmembers():
                if not member.isfile():
                    continue
                name = member.name.split("/", 1)[1]
                if name in ("README.md", "CHANGELOG.md"):
                    continue
                with package.extractfile(member) as content:
                    result[name] = normalize_file(name, content.read())
        return result


def release_needed():
    if run("git", "rev-parse", "--is-shallow-repository").strip() != b"false":
        raise ValueError("CRD release comparison requires full git history")
    baseline = run(
        "git", "describe", "--tags", "--abbrev=0", "--match", TAG_PREFIX + "*", "HEAD"
    ).decode().strip()
    changed = packaged_chart(baseline) != packaged_chart("HEAD")
    print(f"CRD chart differs from {baseline}: {str(changed).lower()}")
    return changed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default=os.environ.get("GITHUB_OUTPUT"))
    args = parser.parse_args()
    changed = release_needed()
    if args.output:
        with open(args.output, "a", encoding="utf-8") as output:
            output.write(f"needed={str(changed).lower()}\n")


if __name__ == "__main__":
    main()
