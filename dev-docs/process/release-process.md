# Release process

Release Please manages three independently versioned components from `main`:

| Component | Configuration path | Version owner |
| --- | --- | --- |
| Firebolt Operator | `.` | `version.txt` |
| Firebolt Operator Helm chart | `helm/firebolt-operator` | chart `version` |
| CRD Helm chart | `helm/firebolt-operator-crds` | chart `version` |

The authoritative configuration is
[`release-please-config.json`](../../release-please-config.json),
[`release-please-crds-config.json`](../../release-please-crds-config.json),
[`.release-please-manifest.json`](../../.release-please-manifest.json), and
[`.github/workflows/release-main.yaml`](../../.github/workflows/release-main.yaml).

## Coupling invariants

- Release Please owns every component version and release tag.
- The operator chart's `appVersion` must reference an existing immutable
  operator image. An operator release may update `appVersion`, but must not edit
  the chart's own `version` directly.
- The CRD chart has no `appVersion` and releases independently from the
  operator image.
- Official chart publication is driven by the component's release-created
  output, not merely by a changed-file check.
- Build-only prerelease jobs validate artifacts without pushing them.
- Non-shipping root paths, including `docs/`, `dev-docs/`, `examples/`,
  `formal/`, `helm/`, and `scripts/`, stay excluded from the root application
  release component. Helm paths belong to their own components.

Conventional commit subjects determine release eligibility and version impact;
follow the repository commit rules in [`AGENTS.md`](../../AGENTS.md).

## CRD release selection

The CRD component uses a separate Release Please invocation. Before creating
its release PR, CI compares the packaged chart with the last CRD release tag.
Schema descriptions, YAML formatting, chart version/description, README, and
changelog changes do not trigger a release. Helm excludes files according to
each revision's `.helmignore`. All other packaged changes remain eligible.

Deferred descriptions ship with the next substantive release, so `kubectl
explain` may lag documentation-only corrections. Release tagging and chart
publication always run for merged release PRs. Missing history, packaging
errors, and invalid YAML fail the check. A pending release PR whose substantive
changes were reverted must be closed before merging it.

## Development image variants

[`config/images/defaults.latest.env`](../../config/images/defaults.latest.env)
contains release-oriented workload defaults, while
[`config/images/defaults.dev.env`](../../config/images/defaults.dev.env) contains
development defaults. Repository Make targets use the `latest` build tag by
default; raw Go commands without build tags embed the development variant.

Keep the operator build variant aligned with the images prepared for E2E tests.

## Change checklist

When changing release behavior:

1. Update workflow logic, Release Please configuration, and component metadata together.
2. Preserve independent component version ownership and chart `appVersion` coupling.
3. Verify build-only jobs cannot publish and official jobs publish only released components.
4. Keep root non-shipping exclusions aligned with repository structure.
5. Update this page and the release rules in [`AGENTS.md`](../../AGENTS.md).
