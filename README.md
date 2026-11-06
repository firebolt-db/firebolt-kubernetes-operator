# Archived CI tools

This branch preserves exact tool builds used by this repository. Downloads
must use a full commit SHA and verify the checksum in
`scripts/ci/pinned-tools.tsv` on the main branch. Do not force-push or delete
this branch: retain every archived commit. Updates are deliberate, not automatic.

## TLA tools

- File: `tla2tools/tla2tools.jar`
- SHA-256: `e20447644bfabd29d6bd0fc2c0eb1473fb301c1a0e4dfe1961938ce5a191edbf`
- Size: 4507038 bytes
- Upstream: https://github.com/tlaplus/tlaplus
- Downloaded from: https://github.com/tlaplus/tlaplus/releases/download/v1.8.0/tla2tools.jar
- Original release asset ID: `706603562`
- Upstream revision from the JAR manifest: `309e489a512e8017f1cdc8388fa6a5e8a01cf597`
- Build timestamp from the JAR manifest: `2026-11-03T18:09:53.565Z`
- GitHub's release-asset digest matched these bytes when validated on 2026-11-05.

This is an unmodified upstream JAR. `tla2tools/LICENSE` is copied verbatim
from its `License.txt`; bundled dependency licenses and notices remain inside
the JAR. The five model checks, expected counterexamples, and generated-fixture
verification passed with this build, without changing the generated fixtures.

To archive another tool build, verify its provenance and checksum, run the
relevant checks, and append a commit on this branch. Change the main branch's
commit URL and checksum through a pull request. Existing pins keep using their
original archived commits.
