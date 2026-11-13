# Release process

Release Please manages three independently versioned components from `main`:

| Component | Configuration path | Version owner |
| --- | --- | --- |
| Firebolt Operator | `.` | `version.txt` |
| Firebolt Operator Helm chart | `helm/firebolt-operator` | chart `version` |
| CRD Helm chart | `helm/firebolt-operator-crds` | chart `version` |

The authoritative configuration is
[`release-please-config.json`](../../release-please-config.json),
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

## CRD release validation

Release Please opens and updates all component release PRs through the normal
workflow. The required `Lint` check validates any PR that changes the CRD chart
version, using the checked-out merge candidate and the last CRD release tag
reachable from the PR base. Ordinary PRs that leave that version unchanged skip
this comparison.

The check packages both revisions with Helm and compares their contents using
Kubernetes YAML decoding. Schema descriptions, YAML formatting, chart
version/description, README, and changelog do not count as substantive changes.
Helm applies each revision's `.helmignore`. Schema defaults, validation rules,
resource versions, installation settings, and other packaged changes do count.

An unchanged candidate fails `Lint` and cannot merge through the normal required
checks. Leave that release PR open: Release Please updates it when further changes
land, and a substantive change makes it eligible. Deferred descriptions ship with
that release, so `kubectl explain` may lag documentation-only corrections.
Missing release history or packaging/parsing errors also fail the check.

The check reads fixed Git revisions; it does not create PRs, tag releases, or
publish charts. Publication after a release PR merges is unchanged. As with other
PR checks, its result applies to the tested candidate; it is not a guarantee about
subsequent edits or an administrative bypass of required checks.

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
