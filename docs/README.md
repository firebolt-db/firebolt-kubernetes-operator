# Firebolt Kubernetes Operator documentation

Published user-facing docs for this project are aggregated into [docs.firebolt.io](https://docs.firebolt.io) under **Self-Managed → Firebolt Operator**.

## Layout

| Path | Purpose |
| --- | --- |
| `docs.json` | Mintlify navigation + `redirects` for renamed/removed pages (edit when adding, regrouping, renaming, or removing pages) |
| `**/*.mdx` | Published documentation pages |
| `crd-reference/` | CRD reference pages (nested navigation group) |
| `scripts/` | Navigation + lost-redirect validation (`make -C docs check`) |
| `known_pages.json` | Baseline of published URLs; the lost-redirect guard fails if one disappears without a redirect |
| `Makefile` | Local doc checks |

Path depth is validated against packdb's [`check_group_structure.py`](https://github.com/firebolt-analytics/packdb/blob/master/docs/scripts/check_group_structure.py) rules **after** aggregation (`self-managed/firebolt-operator/` prefix, nested under **Self-Managed**). Run `make docs-check` from the repo root before opening a PR.

## Workflow

1. Edit or add `.mdx` files in this directory.
2. Update [`docs.json`](docs.json) navigation when adding, removing, or regrouping pages.
3. When **renaming or removing** a page, add a redirect to the [`docs.json`](docs.json) `redirects` array (source slug → new slug, leading slash, no prefix) and run `make -C docs check-lost-redirects-regenerate` to refresh [`known_pages.json`](known_pages.json). packdb prefixes and propagates these redirects into the published site, so old URLs keep working. Skipping this fails `make docs-check`.
4. Validate locally: `make docs-check`.
5. Open a **same-repo** pull request. [docs-sync.yml](../.github/workflows/docs-sync.yml) dispatches to [`firebolt-analytics/packdb`](https://github.com/firebolt-analytics/packdb), which keeps a **draft packdb PR** in sync with your branch and posts Mintlify preview progress on **both** PRs (in progress, then preview URL when ready).

Fork PRs do not receive aggregation (the workflow requires `head.repo == base.repo`).

When this PR **merges**, packdb marks its aggregation PR ready for review. Squash-merge that packdb PR into `master` to publish the docs (urgent fixes can be cherry-picked to `release/packdb-*` like any other docs change).

When this PR is **closed without merge**, packdb closes its aggregation PR and deletes the aggregate branch.

## Required secrets

Add the **firebolt-analytics** GitHub App credentials to this repository (same secret names as in packdb):

- `GH_APP_FBA_DOCS_INTEGRATION_CLIENT_ID`
- `GH_APP_FBA_DOCS_INTEGRATION_APP_KEY_PEM`

The app must be allowed to trigger workflows on `firebolt-analytics/packdb`. Mintlify API credentials live in packdb only; see [packdb docs README](https://github.com/firebolt-analytics/packdb/blob/master/docs/README.md#required-github-apps-and-secrets).

## MDX frontmatter

Each published page needs YAML frontmatter:

```yaml
---
title: Page title
description: One-line summary for search and SEO.
sidebarTitle: Short sidebar label
---
```

See [architecture.mdx](architecture.mdx) for an example.
