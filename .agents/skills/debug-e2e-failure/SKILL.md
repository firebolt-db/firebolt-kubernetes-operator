---
name: debug-e2e-failure
description: Debug a failed E2E Tests GitHub Actions run for firebolt-kubernetes-operator by fetching job logs and the `e2e-report` artifact (e2e-report.xml, prepare-kind.log, run-e2e.log, pod-logs/) via the gh CLI, then correlating failed Ginkgo specs with pod logs and step output. Use when the user provides a GitHub Actions run URL or run ID for the E2E Tests workflow, or asks to investigate, triage, root-cause, or reproduce an E2E / Kind / Ginkgo failure from CI.
---

# Debug E2E Test Failure

Triage a failed `E2E Tests` workflow run by fetching the job logs and the `e2e-report` artifact, then correlating failed Ginkgo specs with pod logs and per-step output.

## Inputs

The user supplies one of:

- A run URL: `https://github.com/<owner>/<repo>/actions/runs/<RUN_ID>` (optionally with `/job/<JOB_ID>` and/or `?pr=<N>`)
- A bare run ID: `12345678901`

Extract `RUN_ID` with `rg -o 'runs/(\d+)' -r '$1'` if needed. Owner/repo can be omitted when running inside the repo checkout — `gh` infers them from the git remote.

## Download layout

All downloads land in `e2e-reports/<RUN_ID>-attempt<N>/` (relative to the repo root). `e2e-reports/` is gitignored.

Why per-attempt? On GitHub Actions:

- "Re-run all jobs" / "Re-run failed jobs" keeps the **same** `RUN_ID` but bumps `run_attempt` (1, 2, 3, …). Logs and artifacts differ per attempt.
- A fresh push gets a new `RUN_ID`.

So a `<RUN_ID>` alone is not unique across re-runs; encode `attempt<N>` to keep them side by side. `gh run download` and `gh api .../logs` always serve the **latest** attempt unless `?attempt=<N>` is appended; record what you fetched.

## Retention

In this repo (see `.github/workflows/test-e2e.yaml`):

- `e2e-report` artifact: **30 days** (`retention-days: 30`). Loses `prepare-kind.log`, `run-e2e.log`, `e2e-report.xml`, `pod-logs/` on expiry.
- Job logs (via `gh api .../jobs/<id>/logs`): repo / org default (90 days unless an admin lowered it).

So once the artifact expires, the job log is the only remaining record. Step 2 below has the fallback path.

## Workflow

Track progress with this checklist:

```
- [ ] 1. Resolve run, job, attempt
- [ ] 2. Create e2e-reports/<RUN_ID>-attempt<N>/ and download artifact (or fall back to job-log only)
- [ ] 3. Fetch job logs into the same folder
- [ ] 4. Summarize failed specs from e2e-report.xml
- [ ] 5. Correlate each failed spec with pod logs
- [ ] 6. Inspect prepare-kind.log if the failure is in cluster setup
- [ ] 7. Report root cause + suggested next step
```

### 1. Resolve run, job, attempt

```bash
RUN_ID=<run id>
gh run view "$RUN_ID" \
  --json status,conclusion,headBranch,headSha,displayTitle,workflowName,event,url,attempt,jobs \
  > /tmp/run.json
ATTEMPT=$(jq -r '.attempt' /tmp/run.json)
JOB_ID=$(jq -r '.jobs[] | select(.name=="E2E Tests") | .databaseId' /tmp/run.json)
DEST="e2e-reports/${RUN_ID}-attempt${ATTEMPT}"
mkdir -p "$DEST"
mv /tmp/run.json "$DEST/run.json"
```

If `JOB_ID` is empty, list jobs (`jq '.jobs[].name' "$DEST/run.json"`) — the workflow may have been renamed or run in a matrix.

### 2. Download the `e2e-report` artifact (with fallback)

Try the artifact first — it's by far the highest-signal data source:

```bash
gh run download "$RUN_ID" -n e2e-report -D "$DEST/" 2>"$DEST/download.err"
```

`gh run download` refuses to overwrite existing files (`error extracting "...": file exists`). The per-attempt folder above keeps each invocation in its own clean directory; if you need to re-fetch the same attempt, `rm -rf "$DEST"` first.

After a successful download you should see:

```
e2e-reports/<RUN_ID>-attempt<N>/
├── run.json
├── e2e-report.xml      # Ginkgo JUnit XML
├── prepare-kind.log    # tee'd output of step id `prepare-kind`
├── run-e2e.log         # tee'd output of step id `run-e2e`
└── pod-logs/           # per-test pod logs captured by the suite
```

**If the artifact is missing**, treat it as still triable — degraded but not blocked. Reasons to expect this:

- Run is older than the artifact retention window (currently 30 days; older runs may instead have a `test.log` from before the prep/e2e log split).
- Run failed before the upload step (e.g. checkout, GHCR login, kind install).
- Fork-PR artifact requires extra auth (`gh auth refresh -s read:org`).

In that case, fall through to step 3 and rely entirely on the job log. Note the degradation in your final writeup ("artifact expired; analysis based on job log only — no per-pod logs, no JUnit XML"). The job log still contains:

- The full Ginkgo console output (test names, failure reasons, gomega traces).
- The kind-setup tee'd output from `prepare-kind`.
- Any pod-log dumps the suite printed inline on failure.

That covers most root-cause questions; what you lose is structured per-pod logs and the easy summarizer.

### 3. Fetch job logs

**Use `gh api` directly.** The `gh run view --log` / `--log-failed` wrappers have been observed to silently return 0 bytes (gh ≤ 2.44 against the current API) while `gh api .../logs` returns the full plaintext. Owner/repo come from the git remote when omitted, so the `{owner}/{repo}` placeholder works:

```bash
gh api "repos/{owner}/{repo}/actions/jobs/${JOB_ID}/logs" > "$DEST/job.log"
```

For just the failing tail, slice the full log (the `--log-failed` wrapper has the same emptiness bug):

```bash
rg -n -B 5 -A 200 'Error: |##\[error\]|FAIL\b|panic:' "$DEST/job.log" | head -300 > "$DEST/job-failed.log"
```

To target a specific re-run attempt instead of "latest", append `?attempt=<N>` to the API path. Without it you always get the latest attempt.

### 4. Summarize failed specs

Prefer the artifact JUnit XML when available:

```bash
python3 scripts/summarize-e2e-junit.py "$DEST/e2e-report.xml"
```

For full failure bodies, query the XML directly:

```bash
rg -U --multiline -A 30 '<testcase[^>]*name="<spec name>"' "$DEST/e2e-report.xml"
```

When the artifact is missing, derive the same information from `run-e2e.log` (or the job log if even that is gone):

```bash
rg -n -B 1 -A 30 'FAIL!|\[FAILED\]|Summarizing \d+ Failure' "$DEST/run-e2e.log"
```

When many specs share an identical failure signature, the root cause is almost always shared (engine image, kind setup, shared infra). Confirm by deduping:

```bash
for f in "$DEST"/pod-logs/*-previous.log; do head -2 "$f" | tail -1; done | sort -u
```

### 5. Correlate failures with pod logs

`pod-logs/` is flat, with names like `<namespace-derived-prefix>-<pod-name>.log` and `<...>-previous.log` for the last crashed-container instance. The `-previous.log` files start with a single `# previous container instance: container=... exitCode=... reason=...` header line — read those first; they almost always carry the actual crash reason.

```bash
ls "$DEST/pod-logs/"
ls "$DEST/pod-logs/" | rg -i 'engine'   # narrow to engine pods
```

For each failed spec, read at minimum:

- The `*-engine-*-previous.log` for the actual crash exit code and reason.
- The `envoy` / gateway logs if the failure mentions 5xx, drained, or cutover.
- The `pensieve` / `metadata` / `metadata-pg` logs if the failure mentions metadata or readiness of shared infra.

Cross-reference with `docs/architecture.mdx` ("Graceful pod shutdown" and "Why no EndpointSlice gate") whenever the failure looks like a brief request error during a blue-green cutover — those sections enumerate the layered data-plane contract and the historical FB-661 footgun.

If the artifact is missing, search the job log / `run-e2e.log` for inline pod-log dumps (the suite prints them on failure):

```bash
rg -n -A 50 'Pod .*logs:|Container .* logs:' "$DEST/run-e2e.log"
```

### 6. Inspect the per-step log files

Each tee'd step log is named after its step `id:` in `.github/workflows/test-e2e.yaml`:

| File | Source step (`id:`) | When to read |
|---|---|---|
| `prepare-kind.log` | `prepare-kind` | Failure is in cluster / image setup; the suite never ran. |
| `run-e2e.log` | `run-e2e` | Failure is in the Ginkgo suite. Mirrors the relevant slice of `job.log`. |

```bash
tail -n 200 "$DEST/prepare-kind.log"
rg -i 'error|failed|cannot|denied|timeout' "$DEST/prepare-kind.log"
```

Common kind-setup causes documented in workflow comments: memlock ulimit (io_uring `ENOMEM`), GHCR auth, image-pull regressions in `scripts/load-e2e-images.sh`.

(Older runs from before this split will have a single `test.log` instead of `prepare-kind.log` / `run-e2e.log`. Same content, just one file.)

### 7. Report

Produce a short writeup with:

- **Run**: link, branch/SHA, attempt, conclusion. Note explicitly if the artifact was missing (degraded analysis).
- **Failed specs**: list with one-line reason each (from the summarizer). If many specs share one signature, say so explicitly — that's a single root cause, not N bugs.
- **Root cause hypothesis**: the most specific layer (test, controller, gateway, kind setup, engine image, infra). When the engine image is the suspect, check whether the PR branch is stale relative to `main`: the mutable `:dev` engine alias documented in `config/images/defaults.dev.env` will surface stale-branch breakage as universal `core` CrashLoopBackOff.
- **Evidence**: pointers to specific lines in `e2e-reports/.../job-failed.log`, `e2e-reports/.../pod-logs/...`, `e2e-reports/.../run-e2e.log`, or `e2e-reports/.../prepare-kind.log`.
- **Next step**: suggest a focused local repro, e.g. `GINKGO_FOCUS="<spec>" make test-e2e`.

## Notes

- Per repo rules, never `sleep` longer than 15s and never delete the local kind cluster — assume it is already up if you reproduce locally.
- Keep all downloaded artifacts inside `e2e-reports/`. Do not stage them with `git add`.
- If the run is from a fork PR, artifacts may require `gh auth refresh -s read:org` or running with a token that has access to the forked workflow.
