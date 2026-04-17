#!/usr/bin/env python3
"""Emit a short Markdown summary of failed Ginkgo JUnit cases for GitHub Actions step summary."""
from __future__ import annotations

import html
import os
import sys
import xml.etree.ElementTree as ET

# Keep reasons short for the Actions UI.
MAX_REASON_LEN = 280

REPORT_PATH = sys.argv[1] if len(sys.argv) > 1 else "e2e-report.xml"
MATRIX_NAME = os.environ.get("E2E_MATRIX_NAME", "")

# Ginkgo often puts only a label in the failure @message; the real assertion text is in the element body.
_GENERIC_FAILURE_OPENERS = (
    "unexpected error:",
    "expected success, but got an error:",
)


def _truncate(s: str, max_len: int) -> str:
    if len(s) <= max_len:
        return s
    cut = s[: max_len - 3]
    sp = cut.rfind(" ")
    if sp > max_len // 3:
        cut = cut[:sp]
    return cut.rstrip() + "..."


def _failure_body_text(elem: ET.Element) -> str:
    """Full failure/error text: attribute + body (Ginkgo duplicates the label in body sometimes)."""
    msg = elem.get("message")
    body = "".join(elem.itertext()).strip()
    msg_d = html.unescape(msg.strip()) if msg else ""
    body_d = html.unescape(body)
    if not body_d:
        return msg_d
    if msg_d and body_d.startswith(msg_d.rstrip()):
        return body_d
    if msg_d:
        return msg_d + "\n" + body_d
    return body_d


def _reason_from_failure_text(text: str) -> str:
    """Drop Ginkgo location line; prefer substance after generic one-line openers."""
    lines: list[str] = []
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        if line.startswith("In [") and " at:" in line:
            break
        if line.startswith("Full Stack Trace"):
            break
        lines.append(line)

    if not lines:
        return "(no message)"

    def is_generic_opener(s: str) -> bool:
        low = s.lower()
        return any(low.startswith(p) for p in _GENERIC_FAILURE_OPENERS)

    # If the first line is only a generic label and more detail follows, skip it.
    if len(lines) >= 2 and is_generic_opener(lines[0]):
        lines = lines[1:]

    # Join a few lines (Gomega matchers often span multiple lines).
    condensed = " ".join(lines[:6]).strip()
    return _truncate(condensed, MAX_REASON_LEN)


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
                raw = _failure_body_text(child)
                failures.append((name, _reason_from_failure_text(raw)))

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
