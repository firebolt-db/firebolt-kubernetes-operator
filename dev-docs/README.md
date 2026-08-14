# Development documentation

This directory contains contributor-facing documentation for developing, testing, releasing, and maintaining the Firebolt Operator. It is not published to the Firebolt documentation site.

Published user documentation lives in [`docs/`](../docs/). The documentation synchronization workflow selects changes under `docs/`; it does not select this directory.

## Source hierarchy

Use these sources in descending order of authority:

1. Code, tests, manifests, and workflow configuration define actual behavior.
2. [`docs/`](../docs/) defines current public behavior and product terminology.
3. `dev-docs/` explains contributor workflows and implementation rationale that is not appropriate for public documentation.

If a development page disagrees with code or published documentation, update or remove the development page. Do not restore an older behavior or name solely to match this directory.

## Layout

| Path | Purpose |
| --- | --- |
| [`architecture/engine-scaling.md`](architecture/engine-scaling.md) | Blue-green scaling rationale, reconciler code map, resources, and change-impact guide |
| [`architecture/admission-and-controller-validation.md`](architecture/admission-and-controller-validation.md) | Admission, CEL, and controller-side invariant enforcement |
| [`process/release-process.md`](process/release-process.md) | Release component ownership and coupling invariants |
| [`testing/e2e-testing.md`](testing/e2e-testing.md) | Kind-based E2E architecture, lifecycle helpers, focus, timeouts, and diagnostics |
| [`testing/formal-verification.md`](testing/formal-verification.md) | TLA+ models, state-cover fixtures, property tests, and negative controls |
| [`testing/local-image-registry.md`](testing/local-image-registry.md) | Local registry topology, lifecycle, and common failure modes |

Add subdirectories only when a topic needs them. Use `proposals/` for forward-looking designs and state clearly that they are not implemented.

## Writing conventions

- Use standard Markdown files with the `.md` extension. Do not use MDX components or Mintlify frontmatter.
- Describe current behavior rather than the history of a change.
- Do not include ticket references or commit IDs.
- Prefer relative links to source files and published documentation.
- Keep commands at repository-root scope unless a page says otherwise.
- Update the corresponding page when a workflow, command, state machine, or source-of-truth file changes.
