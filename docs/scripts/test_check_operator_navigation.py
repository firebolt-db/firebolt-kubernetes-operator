#!/usr/bin/env python3

from __future__ import annotations

import unittest

from check_operator_navigation import AGGREGATE_PREFIX, build_aggregated_docs_tabs, check_operator_navigation


class OperatorNavigationTest(unittest.TestCase):
    def test_check_operator_navigation_allows_href_and_openapi_entries(self) -> None:
        docs_json = {
            "navigation": {
                "groups": [
                    {
                        "group": "Firebolt Operator",
                        "pages": [
                            "installation",
                            {"href": "https://example.com", "label": "External docs"},
                            {"openapi": "openapi.json"},
                            "quickstart",
                        ],
                    }
                ]
            }
        }

        check_operator_navigation(docs_json)

    def test_build_aggregated_docs_tabs_leaves_href_and_openapi_entries_unprefixed(self) -> None:
        href_entry = {"href": "https://example.com", "label": "External docs"}
        openapi_entry = {"openapi": "openapi.json"}
        operator_group = {
            "group": "Firebolt Operator",
            "pages": [
                "installation",
                href_entry,
                openapi_entry,
            ],
        }

        tabs = build_aggregated_docs_tabs(operator_group)
        operator_pages = tabs[0]["groups"][0]["pages"][1]["pages"]

        self.assertEqual(f"{AGGREGATE_PREFIX}/installation", operator_pages[0])
        self.assertIs(href_entry, operator_pages[1])
        self.assertIs(openapi_entry, operator_pages[2])


if __name__ == "__main__":
    unittest.main()
