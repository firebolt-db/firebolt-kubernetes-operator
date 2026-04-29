# SDLC: image publishing and distribution

This document captures the current publishing flow for the operator-related
images, plus the conventions we have agreed on for package naming and tag
semantics.

## Current state

- **Source of builds.** Images are built from `master`. Until a formal
  release flow exists, builds are produced via **manual workflow runs**.
  There is no scheduled or tag-driven release pipeline yet.
- **Mirroring.** Partners that mirror images into their own registry should
  pin by **digest (sha256)**, not by mutable tag. The mutable aliases
  (`latest`, `dev`) exist to make discovery and ad-hoc testing easy; they
  are not a stable contract.

## Package naming

The published GHCR packages use clearer, role-specific names:

| Package | Purpose |
|---------|---------|
| `ghcr.io/firebolt-db/engine` | Firebolt engine (compute node) image |
| `ghcr.io/firebolt-db/kubernetes-operator` | This operator |
| `ghcr.io/firebolt-db/metadata` | Pensieve metadata service |

These names appear in `README.md`, `examples/`, `config/images/defaults.env`,
`helm/` values, and the operator's CD workflow.

## Tag semantics

Two mutable aliases, plus immutable build tags:

| Tag | Points to | Audience |
|-----|-----------|----------|
| `latest` | Latest **stable / LTS** release | Design partners, POCs, README examples |
| `dev` | Latest **development / pre-release** build (typically `master`) | Internal testing, early-access validation |
| `<version>` / `<build-sha>` | Immutable build | Production deployments, anything that needs reproducibility |

Rules:

- `latest` MUST NOT be advanced to a pre-release build. If there is no
  stable release yet for a package, `latest` should be absent rather than
  pointing at a development build.
- `dev` MAY move at any time and MAY regress (e.g. revert).
- Anything that pins for reproducibility — partner mirrors, customer
  deployments, the operator's `config/images/defaults.env` — should pin to
  an immutable tag or digest, never to `latest` or `dev`.
- **The Helm chart MUST NOT ship `latest` or `dev`** in any of its
  versioned fields. `Chart.yaml`'s `appVersion`, `values.yaml`'s
  `image.tag` default, and any image tag the chart embeds at render time
  must all reference immutable build tags. The chart is something users
  install and re-install at known versions; a chart whose meaning shifts
  under it because a mutable alias moved is not a chart we want to ship.
  The mutable aliases are for ad-hoc `kubectl`/`docker pull` discovery,
  not for release artifacts.

## Quickstart and README guidance

Wherever README or `examples/` show an image reference, the example must:

1. Use a **valid, currently-published tag** (not a placeholder).
2. Explain **when to use stable vs development tags**: `latest` for trying
   the operator out and for partner POCs; `dev` for following the bleeding
   edge; pinned tags for anything that needs to be reproducible.

This applies to `README.md`, `examples/*.yaml`, and any `helm` values shown
inline in docs.

## License and provenance checks

These checks are easy to forget once images are flowing, so they are
listed here explicitly.

**Still needs recurring verification, regardless of distribution channel:**

- **Third-party license inventory** for every image we publish (engine,
  metadata, operator). Vendored Go modules and base-image OS packages
  must be re-scanned on each release-candidate build.
- **SBOM generation** for each published image, archived alongside the
  build artifact.
- **Provenance / build attestation** (`docker buildx --attest` or
  equivalent) so that even within private GHCR we can prove which commit
  and workflow run produced a given digest. This is the audit trail we
  rely on if a partner asks "what is in this image".
- **Base image CVE scan** on each manual workflow run. A manual release
  flow does not exempt us from re-scanning before tagging `latest`.

These should be wired into whatever release workflow replaces the current
manual-run setup.

## Intended workflow for design partners and POCs

1. Partner is granted access to `firebolt-db/packdb` (and therefore to
   the linked GHCR packages).
2. For initial evaluation, the partner pulls `…/engine:latest`,
   `…/kubernetes-operator:latest`, `…/metadata:latest` — the mutable
   aliases keep the quickstart short.
3. For anything beyond evaluation (mirroring into their registry, pinning
   in their GitOps repo, running in their staging or production), the
   partner resolves `latest` / `dev` to a **digest** at the time of mirror
   and pins to that digest. The mutable alias is only an entry point.
4. When we cut a new stable release, `latest` advances. Partners pick up
   the new digest on their next mirror; existing pinned deployments are
   not affected.

## Out of scope (for this document)

- Branch / release-branch strategy.
- Versioning scheme for the operator vs the engine vs metadata.
- Customer-facing changelog format.
- Public (non-GHCR) distribution.

These are deliberately deferred until the manual workflow is replaced by
a formal release flow.
