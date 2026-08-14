# Development documentation rules

These rules apply to every file under `dev-docs/`.

- Write standard Markdown (`.md`), not MDX. Do not add Mintlify frontmatter or navigation entries.
- Describe the repository as it behaves now. Do not preserve historical names, migration narratives, commit IDs, or tracker references.
- Verify implementation details against code and configuration. For public behavior and terminology, treat `docs/` as the documentation source of truth.
- Keep operational instructions executable. Use repository targets and scripts instead of reproducing their implementation inline.
- Mark forward-looking designs explicitly and keep them under `proposals/`. Do not mix proposed behavior into current-state documentation.
- Link to source files instead of duplicating long constants, schemas, or generated configuration.
- Update the relevant page when its source code, workflow, command, or invariant changes.

The `dev-docs/` tree is outside the Mintlify synchronization path. Published user documentation belongs in `docs/`.
