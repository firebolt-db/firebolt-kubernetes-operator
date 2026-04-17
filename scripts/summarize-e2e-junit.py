#!/usr/bin/env python3
"""Emit a short Markdown summary of failed Ginkgo JUnit cases for GitHub Actions step summary."""
from __future__ import annotations

import html
import os
import sys
import xml.etree.ElementTree as ET

# Keep reasons short for the Actions UI.
MAX_REASON_LEN = 220

REPORT_PATH = sys.argv[1] if len(sys.argv) > 1 else "e2e-report.xml"
MATRIX_NAME = os.environ.get("E2E_MATRIX_NAME", "")


def _truncate(s: str, max_len: int) -> str:
    if len(s) <= max_len:
        return s
    cut = s[: max_len - 3]
    sp = cut.rfind(" ")
    if sp > max_len // 3:
        cut = cut[:sp]
    return cut.rstrip() + "..."


def _first_line_reason(msg: str) -> str:
    if not msg:
        return "(no message)"
    text = html.unescape(msg)
    line = text.split("\n", 1)[0].strip()
    return _truncate(line, MAX_REASON_LEN)


def main() -> int:
    title = "### E2E"
    if MATRIX_NAME:
        title += f" — `{MATRIX_NAME}`"

    try:
        tree = ET.parse(REPORT_PATH)
    except FileNotFoundError:
        print(title)
        print()
        print("No `e2e-report.xml` found (tests may not have completed).")
        return 0
    except ET.ParseError as exc:
        print(title)
        print()
        print(f"Could not parse JUnit report: {exc}")
        return 0

    root = tree.getroot()
    failures: list[tuple[str, str]] = []
    for tc in root.iter("testcase"):
        name = tc.get("name", "?")
        for child in tc:
            if child.tag in ("failure", "error"):
                raw = child.get("message") or (child.text or "").strip()
                failures.append((name, _first_line_reason(raw)))

    print(title)
    print()
    if not failures:
        print("JUnit: no failing test cases.")
        return 0

    print(f"**Failed ({len(failures)})**")
    for name, reason in failures:
        print(f"- `{name}` — {reason}")
    print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
