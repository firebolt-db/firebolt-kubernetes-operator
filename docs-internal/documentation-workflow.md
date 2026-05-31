# Documentation workflow (operator → packdb → Mintlify)

Internal reference for how Kubernetes Operator docs reach [docs.firebolt.io](https://docs.firebolt.io). Published contributor docs live in [`docs/`](../docs/README.md); this file is **not** aggregated.

## At a glance

Operator documentation is authored next to the operator code, aggregated into the main Firebolt Mintlify site in [`firebolt-analytics/packdb`](https://github.com/firebolt-analytics/packdb), and published from packdb release branches. Mintlify only watches **one packdb Git branch** at a time (for example `release/packdb-4.31`); it does not read the operator repository directly.

```mermaid
flowchart LR
  subgraph operator ["firebolt-db/firebolt-kubernetes-operator"]
    DOCS["docs/*.mdx + docs.json"]
    INT["docs-internal/ (not published)"]
  end

  subgraph packdb ["firebolt-analytics/packdb"]
    MASTER["master"]
    AGG["aggregate/operator-docs-pr-N"]
    REL["release/packdb-X.Y"]
    MDX["docs/docs-mdx/"]
  end

  subgraph mintlify ["Mintlify"]
    PREVIEW["Preview *.mintlify.app"]
    PROD["docs.firebolt.io"]
  end

  DOCS -->|"multirepo-action (CI)"| AGG
  AGG -->|"draft / ready PR"| MASTER
  MASTER -->|"release cut (~3 weeks)"| REL
  AGG --> PREVIEW
  REL --> PROD
  MASTER --> MDX
  REL --> MDX
```

## Repository layout

| Location | Repo | Published? | Role |
| --- | --- | --- | --- |
| [`docs/`](../docs/) | operator | Yes | MDX pages + `docs.json` navigation fragment merged into the Firebolt site under **Documentation → Self-Managed → Firebolt Operator** |
| [`docs-internal/`](../docs-internal/) | operator | No | Design notes, SDLC, slides — outside the multirepo path |
| `docs/docs-mdx/` | packdb | Yes | Full Mintlify site (theme, tabs, SQL examples, redirects) |
| `docs/docs-mdx/self-managed/firebolt-operator/` | packdb (generated) | Yes | Copy of operator `docs/` produced by CI — do not edit by hand |

Operator `docs.json` defines **navigation only**. Global Mintlify settings (`docs/docs-mdx/docs.json` in packdb) own theme, tabs, redirects, and site metadata.

After aggregation, packdb runs [`merge_multirepo_navigation.py`](https://github.com/firebolt-analytics/packdb/blob/master/docs/scripts/merge_multirepo_navigation.py) to nest the **Firebolt Operator** group under **Documentation → Self-Managed** (required because packdb uses tabbed navigation).

## Workflows and entry points

| Workflow | Repository | Trigger |
| --- | --- | --- |
| [`.github/workflows/docs-sync.yml`](../.github/workflows/docs-sync.yml) | operator | Same-repo PR (opened, reopened, synchronize, closed). `sync` requires a `docs/**` or `.github/workflows/docs-sync.yml` change; `ready`/`cleanup` on close are gated on the existence of the packdb aggregate branch |
| [`.github/workflows/docs-multirepo-aggregate.yml`](https://github.com/firebolt-analytics/packdb/blob/master/.github/workflows/docs-multirepo-aggregate.yml) | packdb | `repository_dispatch` (`operator-docs-changed`) or manual `workflow_dispatch` |

An in-job relevance check (not a trigger-level `paths` filter) on the operator workflow means changes under `docs/`, or edits to the sync workflow file itself, trigger `sync` aggregation. Edits confined to `docs-internal/` do not. The relevance check is intentionally **not** used to gate the `closed` event so that cleanup/ready still run for a PR whose final diff no longer touches `docs/`; the close action is resolved from the packdb aggregate branch instead.

`docs/docs-mdx/` is not a separate deploy target — it is the Mintlify site root **on** whichever packdb branch is built (`aggregate/operator-docs-pr-{N}` for previews, `release/packdb-*` for production).

## Dispatch model

The operator workflow never pushes to packdb. It sends a `repository_dispatch` with a small, allowlisted payload:

```json
{
  "event_type": "operator-docs-changed",
  "client_payload": {
    "action": "sync | ready | cleanup",
    "operator_pr_number": 123
  }
}
```

Packdb resolves all Git refs itself (operator PR head, `main`, branch names). It does **not** trust client-supplied branch names.

On `opened` / `reopened` / `synchronize`, the operator workflow uses the PR's current file list to decide whether to `sync`. On `closed` it does **not** rely on the file list — the merged diff is an unreliable proxy for whether a `sync` ever ran. Instead it queries packdb for the aggregate branch `aggregate/operator-docs-pr-{N}` (created only by `sync`). If the branch exists, a merged PR is marked `ready` and an abandoned one is `cleanup`'d; if it does not exist, nothing is dispatched. This guarantees a merged docs PR is never accidentally torn down because its final diff no longer matched `docs/*`, and avoids dispatching no-op `cleanup` events for closed non-docs PRs.

```mermaid
flowchart TD
  OP_PR["Operator PR event"]
  RESOLVE{"PR closed?"}
  RELEVANT{"docs/* or sync\nworkflow changed?"}
  BRANCH{"aggregate branch\nexists in packdb?"}
  MERGED{"Merged?"}
  SYNC["action: sync"]
  READY["action: ready"]
  CLEAN["action: cleanup"]
  NOOP["no dispatch"]

  OP_PR --> RESOLVE
  RESOLVE -->|opened / synchronize| RELEVANT
  RELEVANT -->|yes| SYNC
  RELEVANT -->|no| NOOP
  RESOLVE -->|closed| BRANCH
  BRANCH -->|no| NOOP
  BRANCH -->|yes| MERGED
  MERGED -->|yes| READY
  MERGED -->|no| CLEAN

  SYNC --> JOB1["packdb: sync-aggregate"]
  READY --> JOB2["packdb: ready-aggregate"]
  CLEAN --> JOB3["packdb: cleanup-aggregate"]
```

| `action` | Operator trigger | Packdb job | Outcome |
| --- | --- | --- | --- |
| `sync` | PR opened or updated | `sync-aggregate` | Aggregate branch updated; **draft** packdb PR (labels `docs`, `docs-operator`); in-progress Mintlify comments on operator + packdb PRs; wait for preview (GitHub App check by default); comments updated with preview URL |
| `ready` | PR merged | `ready-aggregate` | Re-aggregate from operator `main`; packdb PR marked **ready for review**; comment on operator PR |
| `cleanup` | PR closed without merge | `cleanup-aggregate` | Close packdb PR; delete aggregate branch |

Fork PRs are ignored on the operator side (`head.repo == base.repo`). Packdb rejects aggregation if the operator PR head is not `firebolt-db/firebolt-kubernetes-operator`.

## Branches and pull requests

Naming is strict and allowlisted in packdb CI:

| Artifact | Pattern | Example |
| --- | --- | --- |
| Aggregate branch | `aggregate/operator-docs-pr-{N}` | `aggregate/operator-docs-pr-42` |
| Packdb integration PR | base `master`, head aggregate branch | `docs: aggregate Kubernetes Operator docs (operator #42)` |
| Operator docs source (after merge) | `main` | — |
| Packdb integration base | `master` | — |
| Live Mintlify deploy branch | `release/packdb-X.Y` | `release/packdb-4.32` |

**Nothing is pushed to `master` by automation.** The bot only force-pushes the aggregate branch. Humans squash-merge the packdb PR through normal review.

### PR state machine (packdb)

```mermaid
stateDiagram-v2
  [*] --> NoBranch: operator PR opened
  NoBranch --> DraftPR: sync (multirepo + gh pr create --draft)
  DraftPR --> DraftPR: sync on each operator push
  DraftPR --> ReadyPR: operator PR merged (ready job)
  ReadyPR --> OnMaster: human squash-merge packdb PR
  DraftPR --> [*]: operator PR abandoned (cleanup)
  ReadyPR --> OnMaster: human squash-merge packdb PR
  OnMaster --> [*]: optional manual delete of aggregate branch
```

While the operator PR is open, the packdb PR stays a **draft** and its head branch is updated on every `sync`. After the operator PR merges, the packdb PR becomes **ready for review** but remains open until a docs owner merges it.

**Packdb PR reuse:** While an open packdb PR exists for the aggregate branch, `sync` reuses it. If that packdb PR was **closed without merge** but the operator PR is still open, the next `sync` opens a **new** draft PR (it does not reopen the old one).

**Aggregate branch after success:** CI does not delete `aggregate/operator-docs-pr-{N}` when the packdb PR merges to `master`. Delete stale aggregate branches manually if desired.

## Call diagrams

### Actors and credentials

```mermaid
flowchart TB
  subgraph secrets ["Secrets / tokens"]
    FBA["FBA docs integration app\n(secrets in operator repo)"]
    FBDB["FB_DB docs integration app\n(secrets in packdb repo)"]
    GHT["GITHUB_TOKEN\n(packdb workflow)"]
  end

  subgraph repos ["Repositories"]
    OP["firebolt-kubernetes-operator"]
    PDB["packdb"]
  end

  OP -->|"repository_dispatch\n(FBA app token)"| PDB
  PDB -->|"gh pr view / gh pr comment\n(FB_DB app token)"| OP
  PDB -->|"multirepo clone + push aggregate branch,\nopen/update/close PRs\n(GITHUB_TOKEN today)"| PDB
  ML["Mintlify GitHub App"] -->|"Mintlify Deployment check\n(async on branch push)"| PDB
```

| Token | Installed / used on | Permissions needed |
| --- | --- | --- |
| FBA docs integration app | `packdb` (secrets stored in **operator** repo; token minted in operator workflow) | **Contents: Read and write** on `packdb`. GitHub maps [`POST /repos/.../dispatches`](https://docs.github.com/en/rest/overview/permissions-required-for-github-apps#repository-permissions-for-contents) to **Contents write** — read-only is not sufficient and returns HTTP 403. |
| FB_DB docs integration app | `firebolt-kubernetes-operator` (secrets stored in **packdb** repo; token minted in packdb workflow) | Pull requests: read + write on operator repo (verify PR, post comments). **Not** passed to multirepo today. |
| `GITHUB_TOKEN` | packdb workflow | `contents: write`, `pull-requests: write`, `issues: write`, `checks: read`; repo setting **Allow GitHub Actions to create pull requests** enabled. Used for multirepo push to packdb **and** as the multirepo-action `token` input. Polls Mintlify GitHub App check runs. |
| Mintlify GitHub App | packdb repo (dashboard install) | Automatic preview deployments on PRs targeting `master`; aggregation workflow reads `Mintlify Deployment` check `details_url` |

#### Multirepo token caveat

[`mintlify/multirepo-action`](https://github.com/mintlify/multirepo-action) accepts a **single** token for cloning sub-repositories and pushing the aggregate branch. The workflow currently passes packdb’s `GITHUB_TOKEN`. That token can write to packdb but **cannot read private repositories in another org** (`firebolt-db`).

If aggregation fails at clone time with an auth or 404 error against `firebolt-kubernetes-operator`, pass a token that has **Contents: read** on the operator repo (for example the FB_DB app token) to multirepo-action instead of, or in addition to, `GITHUB_TOKEN`. Until that wiring change is made, the FB_DB app’s Contents permission is required for comments/PR checks only — not for multirepo itself.

### Sequence: `sync` (operator PR open / update)

```mermaid
sequenceDiagram
  autonumber
  actor Author
  participant OP as Operator repo / docs-sync.yml
  participant FBA as FBA docs app
  participant PDB as packdb / docs-multirepo-aggregate.yml
  participant FBDB as FB_DB docs app
  participant GH as GitHub (packdb)
  participant ML as Mintlify GitHub App

  Author->>OP: Push to operator PR branch (docs/**)
  OP->>OP: action = sync
  OP->>FBA: Mint installation token (packdb)
  OP->>PDB: repository_dispatch(operator-docs-changed)
  PDB->>FBDB: Mint token (operator repo)
  PDB->>OP: gh pr view — verify OPEN, same-repo, read headRefName
  PDB->>GH: checkout packdb master
  PDB->>PDB: mintlify/multirepo-action<br/>master + operator PR head → aggregate/operator-docs-pr-N
  PDB->>PDB: merge_multirepo_navigation.py
  PDB->>GH: push aggregate branch (force)
  PDB->>GH: gh pr create or reuse draft PR → master<br/>labels docs, docs-operator
  PDB->>OP: upsert PR comment — Mintlify preview in progress
  PDB->>GH: upsert PR comment on packdb PR — preview in progress + lifecycle note
  ML-->>GH: async Mintlify Deployment check on head commit
  PDB->>GH: poll check-runs on packdb PR head SHA
  GH-->>PDB: check success details_url
  PDB->>OP: upsert PR comment — Mintlify preview URL + packdb draft PR link
  PDB->>GH: upsert PR comment on packdb PR — Mintlify preview URL
```

Operator ref for multirepo: **PR head branch**. Packdb base content: **`master`**.

### Sequence: `ready` (operator PR merged)

```mermaid
sequenceDiagram
  autonumber
  participant OP as Operator repo / docs-sync.yml
  participant FBA as FBA docs app
  participant PDB as packdb / ready-aggregate
  participant FBDB as FB_DB docs app
  participant GH as GitHub (packdb)

  OP->>OP: pull_request closed, merged = true → action = ready
  OP->>FBA: Mint token
  OP->>PDB: repository_dispatch(action=ready)
  PDB->>FBDB: Mint token
  PDB->>OP: gh pr view — verify merged, same-repo
  PDB->>GH: checkout packdb master
  PDB->>PDB: multirepo — master + operator main → aggregate branch
  PDB->>PDB: merge_multirepo_navigation.py + push aggregate branch
  PDB->>GH: gh pr ready (or create non-draft PR if missing)
  PDB->>OP: gh pr comment — packdb PR ready for review
```

Operator ref for multirepo: **`main`**. The aggregate branch name is still tied to the original operator PR number.

### Sequence: `cleanup` (operator PR closed without merge)

```mermaid
sequenceDiagram
  autonumber
  participant OP as Operator repo
  participant PDB as packdb / cleanup-aggregate
  participant GH as GitHub (packdb)

  OP->>PDB: repository_dispatch(action=cleanup)
  PDB->>GH: gh pr close (open packdb PR for aggregate branch)
  PDB->>GH: DELETE refs/heads/aggregate/operator-docs-pr-N
```

### Sequence: human path to production

```mermaid
sequenceDiagram
  autonumber
  actor DocsOwner as Docs owner
  participant PDB as packdb master
  participant REL as release/packdb-X.Y
  participant ML as Mintlify (Git integration)

  DocsOwner->>PDB: Squash-merge packdb aggregation PR
  Note over PDB: Operator MDX now on master under docs/docs-mdx/self-managed/firebolt-operator/

  alt Next scheduled release cut (~3 weeks)
    DocsOwner->>REL: Cut release/packdb-X.Y from master
    DocsOwner->>ML: Point deploy branch to release/packdb-X.Y (if promoting)
    ML->>ML: Production build → docs.firebolt.io
  else Urgent doc fix before cut
    DocsOwner->>REL: Cherry-pick packdb aggregation commit to active release branch
    ML->>ML: Production rebuild from release branch
  end
```

Production deploys use Mintlify’s **GitHub integration** on the configured release branch. Operator-docs aggregate previews use the **Mintlify GitHub App** by default (`DOCS_AGGREGATE_USE_MINTLIFY_PREVIEW_API=false` in the workflow). Optional Preview API path is gated by that env var or manual `workflow_dispatch` input `use_mintlify_preview_api`.

## Operational notes

### PR comments (Mintlify preview)

- **`sync`** upserts a single comment on each PR (operator and packdb) using an HTML marker, so repeated pushes update the same comment instead of spamming new ones.
- Comments appear **before** Mintlify finishes building (“in progress”), then update with the preview URL when the deployment completes (or a fallback URL if polling times out).
- The packdb PR comment and body explain the automated lifecycle: marked **ready for review** when the operator PR merges, **closed** when the operator PR is abandoned.
- **`ready`** posts a separate comment on the **closed** operator PR after merge (`gh pr comment` works on closed PRs). That comment carries the packdb PR link for the human merge step.

### Mintlify preview timing

The `sync` job polls the packdb PR head commit for the **`Mintlify Deployment`** GitHub App check for up to ~15 minutes. The Mintlify app starts asynchronously after the aggregate branch push; the workflow waits for the check to appear, then for `conclusion=success`. If polling times out, it posts a deterministic **fallback** preview URL (`https://firebolt-{branch-with-dashes}.mintlify.app/`). The preview may need a few more minutes before it loads.

### Cherry-picks for urgent production

Cherry-pick the **packdb squash commit** on `master` (from merging the aggregation PR), not commits from the operator repository. Operator `main` alone does not update docs.firebolt.io until aggregated content is on packdb `master` and present on the Mintlify deploy branch.

## Author checklist

1. Edit or add `.mdx` under [`docs/`](../docs/); register pages in [`docs/docs.json`](../docs/docs.json).
2. Use lowercase hyphenated paths (Mintlify constraint).
3. Add YAML frontmatter (`title`, `description`, optional `sidebarTitle`) — see [`docs/README.md`](../docs/README.md).
4. Open a **same-repo** operator PR.
5. Watch for Mintlify preview comments on the operator PR and the packdb draft PR (in progress, then preview URL when ready).
6. Iterate on the operator PR; packdb draft PR and preview update automatically.
7. Merge the operator PR when code/docs review is done.
8. Find the **ready** packdb PR (linked in a follow-up comment on the merged operator PR).
9. Review and **squash-merge** the packdb PR into `master`.
10. For urgent live-site updates before the next release cut, cherry-pick the packdb squash commit onto the active `release/packdb-*` branch.

Keep design notes, SDLC, and slides in **`docs-internal/`** only.

## Security properties

| Control | Implementation |
| --- | --- |
| No fork aggregation | Operator workflow gate + packdb `headRepository` check |
| No `pull_request_target` | Operator workflow uses `pull_request` only; secrets not exposed to fork workflows |
| No trusted client refs | Only `operator_pr_number` in dispatch; packdb reads refs from GitHub API |
| Allowlisted branch names | Regex `^aggregate/operator-docs-pr-[0-9]+$` before push, preview, or delete |
| No direct push to `master` | Bot pushes aggregate branch only; `master` via human PR merge |
| Preview exposure | Mintlify preview URLs are public; preview renders full Firebolt site from `master` + operator changes |

## Troubleshooting

### `gh api .../dispatches` → HTTP 403 `Resource not accessible by integration`

This is **not** caused by the packdb aggregation workflow missing on a feature branch (that yields HTTP 204 and a silent no-op).

Per [GitHub’s permissions table](https://docs.github.com/en/rest/overview/permissions-required-for-github-apps#repository-permissions-for-contents), `POST /repos/{owner}/{repo}/dispatches` requires **Contents: write** on the target repo. **Contents: Read-only is not enough** (an earlier draft of this doc incorrectly said read-level access was sufficient).

**Checklist:**

1. FBA docs integration app → repository permission **Contents: Read and write**.
2. App **installed** on `firebolt-analytics` with **`packdb`** in the installation (not operator-only).
3. After changing app permissions, **accept** the updated installation on the org/repo.
4. Operator repo secrets match this app (`GH_APP_FBA_DOCS_INTEGRATION_CLIENT_ID`, `GH_APP_FBA_DOCS_INTEGRATION_APP_KEY_PEM`).
5. Token mint step scopes to packdb (`owner: firebolt-analytics`, `repositories: packdb` in `docs-sync.yml` — already correct).
6. Re-run the failed `docs-sync` job.

### Dispatch succeeds (204) but packdb workflow never runs

`repository_dispatch` only triggers workflows present on packdb **`master`**. Merge the packdb aggregation workflow PR first, then re-dispatch from the operator PR.

## Manual recovery

On packdb, run **Aggregate Mintlify docs** (`workflow_dispatch`):

| Input | When to use |
| --- | --- |
| `action=sync`, `operator_pr_number=N` | Rebuild draft aggregate branch / draft PR while operator PR #N is still open |
| `action=ready`, `operator_pr_number=N` | Re-run post-merge aggregation and mark packdb PR ready |
| `action=cleanup`, `operator_pr_number=N` | Tear down after abandoned operator PR |

Concurrency group `docs-aggregate-operator-pr-{N}` cancels in-progress runs for the same operator PR.

## Related links

- Operator contributor guide: [`docs/README.md`](../docs/README.md)
- Packdb multirepo section: [packdb `docs/README.md`](https://github.com/firebolt-analytics/packdb/blob/master/docs/README.md#multirepo-aggregation-kubernetes-operator-docs)
- Mintlify multirepo action: [mintlify/multirepo-action](https://github.com/mintlify/multirepo-action)
- Mintlify GitHub App: [GitHub integration](https://www.mintlify.com/docs/deploy/github)
