# Firebolt Operator documentation

Published user-facing docs for this project appear on [docs.firebolt.io](https://docs.firebolt.io) under **Self-Managed → Firebolt Operator**.

## Layout

| Path | Purpose |
| --- | --- |
| `docs.json` | Mintlify navigation + `redirects` for renamed/removed pages (edit when adding, regrouping, renaming, or removing pages) |
| `**/*.mdx` | Published documentation pages |
| `crd-reference/` | CRD reference pages (nested navigation group) |
| `scripts/` | Navigation + lost-redirect validation (`make -C docs check`) |
| `known_pages.json` | Baseline of published URLs; the lost-redirect guard fails if one disappears without a redirect |
| `Makefile` | Local doc checks |

Path depth is validated for the published `self-managed/firebolt-operator/` location under **Self-Managed**. Run `make docs-check` from the repo root before opening a PR.

## Workflow

1. Edit or add `.mdx` files in this directory.
2. Update [`docs.json`](docs.json) navigation when adding, removing, or regrouping pages.
3. When **renaming or removing** a page, add a redirect to the [`docs.json`](docs.json) `redirects` array (source slug → new slug, leading slash, no prefix) and run `make -C docs check-lost-redirects-regenerate` to refresh [`known_pages.json`](known_pages.json). The publishing pipeline applies these redirects so old URLs keep working. Skipping this fails `make docs-check`.
4. Validate locally: `make docs-check`.
5. Open a **same-repo** pull request. [docs-sync.yml](../.github/workflows/docs-sync.yml) requests a documentation preview and reports its progress on the pull request.

Fork PRs do not receive previews because the workflow requires `head.repo == base.repo`.

When the pull request is merged or closed, the publishing workflow performs the corresponding publication or cleanup action.

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
