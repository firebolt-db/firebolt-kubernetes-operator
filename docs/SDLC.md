# SDLC: image publishing and distribution

This document captures the current publishing flow for the operator-related
images, plus the conventions we have agreed on for package naming and tag
semantics.

## Current state

- **Source of builds.** Firebolt Kubernetes Operator images and Helm chart releases are built from `main`. Version, release and package creation happens automatically on merge to `main` via GitHub actions.
- **Distribution to design partners.** Design partners get access through
  **private GHCR packages** linked to the `firebolt-db/packdb`, `firebolt-db/firebolt-kubernetes-operator` and `firebolt-db/firebolt-instance-helm` repositories.
  External design partners are added to the "Guest Triage Access" GitHub group, which is granted Read/Triage access on these repositories. Access to packages inherited from those repositories' collaborator lists. So granting a partner access to `firebolt-db/packdb` is what grants them image pull access.
- **Mirroring.** Partners that mirror images into their own registry should
  pin by **digest (sha256)**, not by mutable tag. The mutable aliases
  (`latest`, `dev`) exist to make discovery and ad-hoc testing easy; they
  are not a stable contract.

## Package naming

The git repository for the engine code stays as `firebolt-db/firebolt-db` (temporarily `firebolt-db/packdb`).
The published GHCR packages from the `firebolt-db/firebolt-db` and `firebolt-db/firebolt-kubernetes-operator` repositories use clear, role-specific names:

| Package | Purpose | Source |
|---------|---------|--------|
| `ghcr.io/firebolt-db/kubernetes-operator` | This operator | `firebolt-db/firebolt-kubernetes-operator` |
| `ghcr.io/firebolt-db/engine` | Firebolt engine (compute node) image | `firebolt-db/firebolt-db` |
| `ghcr.io/firebolt-db/metadata` | Metadata service | `firebolt-db/firebolt-db` |

These names appear in `README.md`, `examples/`, `config/images/defaults.env`,
`helm/` values, and the operator's CD workflow.

## Tag semantics

Two mutable aliases, plus immutable build tags:

| Tag | Points to | Audience |
|-----|-----------|----------|
| `latest` | Latest **stable / LTS** release | Design partners, POCs, README examples |
| `dev` | Latest **development / pre-release** build (typically `main`) | Internal testing, early-access validation. A `dev` tag only exists on projects that work with release branches for stable releases. For projects like `firebolt-db/firebolt-kubernetes-operator` that do continuous releases from main this is not applicable. |
| `<version>` / `<build-sha>` | Immutable build | Production deployments, anything that needs reproducibility |

Rules:

- `latest` MUST NOT be advanced to a pre-release build. If there is no
  stable release yet for a package, `latest` should be absent rather than
  pointing at a development build.
- `dev` MAY move at any time and MAY regress (e.g. revert).
- Anything that pins for reproducibility — partner mirrors, customer
  deployments, the operator's `config/images/defaults.env` — should pin to
  an immutable tag or digest, never to `latest` or `dev`.
- **The Firebolt Operator Helm chart and the Firebolt Instance Helm chart MUST NOT ship `latest` or `dev`** in any of their
  versioned fields. `Chart.yaml`'s `appVersion`, `values.yaml`'s
  `image.tag` default, and any image tag the chart embeds at render time
  must all reference immutable build tags. The chart is something users
  install and re-install at known versions; a chart whose meaning shifts
  under it because a mutable alias moved is not a chart we want to ship.
  The mutable aliases are for ad-hoc `kubectl`/`docker pull` discovery,
  not for release artifacts.
- Git tags that represent Semantic Versions like `1.2.3` MUST have a leading `v` like `v1.2.3`

## Helm Chart Versioning
Every chart must have a version number. A version should follow the SemVer 2 standard but it is not strictly enforced. Unlike Helm Classic, Helm v2 and later uses version numbers as release markers. Packages in repositories are identified by name plus version.

For example, an nginx chart whose version field is set to version: `1.2.3` will be named:
```
nginx-1.2.3.tgz
```
More complex SemVer 2 names are also supported, such as version: `1.2.3-alpha.1+ef365`. But non-SemVer names are explicitly disallowed by the system. Subject to exception are versions in format x or x.y. For example, if there is a leading v or a version listed without all 3 parts (e.g. `v1.2`) it will attempt to coerce it into a valid semantic version (e.g., `v1.2.0`).

The Helm chart `Chart.yaml` `version` field is the chart's own semver and MUST be **bare** (e.g. `0.1.25`) — no leading `v`. This is the value that becomes the OCI tag at `helm push` time, so a `v` prefix would land in the registry as `kubernetes-operator:v0.1.25`, which is non-idiomatic for Helm and confuses chart resolvers.

Source: [Helm Charts and Versioning](https://helm.sh/docs/topics/charts/#charts-and-versioning)

## Container Image Versioning
Container image versions for a major, minor or patch release MUST use a leading `v`.

Example: If we build version `1.2.3` the container image will be tagged `myimage:v1.2.3`.

### Chart vs image tag prefix — important asymmetry

Because Helm and container-registry conventions diverge, the same release ships under two different tag spellings. This is intentional and not a bug:

| Artifact | Pushed coordinate | Source of truth |
|----------|-------------------|-----------------|
| Operator image | `ghcr.io/firebolt-db/kubernetes-operator:v1.17.0` (**`v` prefix**) | git tag from semantic-release |
| Helm chart | `oci://ghcr.io/firebolt-db/helm-charts/kubernetes-operator:0.1.25` (**no prefix**) | `Chart.yaml` `version` |

Inside `Chart.yaml` itself, both spellings co-exist:
- `version: 0.1.25` — the chart's own semver, bare. Used as the OCI tag.
- `appVersion: "v1.17.0"` — mirrors the operator git tag, with the `v` prefix. Metadata only; not used as the OCI tag.

When telling a partner "we shipped 1.17", point them at `kubernetes-operator:v1.17.0` (image) **and** at the chart version that carries `appVersion: v1.17.0`, not at a chart whose own `version` happens to be `1.17.0`.

## GitHub Releases
We do GitHub releases for major, minor and patch versions. A GitHub release always references a git tag (see [tag semantics](#tag-semantics)). A GitHub release name MUST also use a leading `v` to match the git tag.

Example: Release `v1.2.3` MUST reference git tag `v1.2.3`.

## Quickstart and README guidance

Wherever README or `examples/` show an image reference, the example MUST:

1. Use a **valid, currently-published tag** (not a placeholder).
2. Explain **when to use stable vs development tags**: `latest` for trying
   the operator out and for partner POCs; `dev` for following the bleeding
   edge; pinned tags for anything that needs to be reproducible.

This applies to `README.md`, `examples/*.yaml`, and any `helm` values shown
inline in docs.

## License and provenance checks

These checks are easy to forget once images are flowing, so they are
listed here explicitly.

**Can be skipped for private GHCR distribution:**

- Public-registry license-scan gating (the package is not publicly listed).
- Public attestation / Sigstore transparency-log enforcement on pull
  (partners pulling from private GHCR rely on GHCR ACLs, not on
  signature verification, for trust).

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
