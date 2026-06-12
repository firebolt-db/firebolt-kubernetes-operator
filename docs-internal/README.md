# Internal documentation

Design notes, release process docs, and presentations that are **not** published to [docs.firebolt.io](https://docs.firebolt.io).

Published user-facing operator docs live in [`docs/`](../docs/) (Mintlify MDX + `docs.json`).

| Path | Purpose |
| --- | --- |
| [documentation-workflow.md](documentation-workflow.md) | End-to-end docs pipeline (operator → packdb → Mintlify), CI dispatch model, and call diagrams |
| [SDLC.md](SDLC.md) | Release lifecycle and default image bump rules |
| [operator-based-scaling.md](operator-based-scaling.md) | Zero-downtime engine scaling architecture with the Firebolt Operator |
| [option-b-per-engine-envoy-clusters.md](option-b-per-engine-envoy-clusters.md) | Per-engine Envoy cluster model (proposal, not implemented) |
| [formal-verification.md](formal-verification.md) | TLA+ model-checking strategy for operator state machines |
| [webhook-hardening-plan.md](webhook-hardening-plan.md) | Per-invariant plan to close webhook-on / webhook-off divergences |
| [slides/](slides/) | Presentations |

This directory is outside the Mintlify multirepo aggregation path (`docs/`), so nothing here is copied to the public docs site.
