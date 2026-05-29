# Webhook hardening plan (FB-1298)

Closes the divergences between webhook-on and webhook-off behavior, plus the
test coverage gaps, surfaced by the audit on branch
`feat/FB-1298-webhook-coverage`. Supersedes the earlier `webhook-invariants.md`.

## TL;DR

| # | Workstream | Phase | Why |
|---|---|---|---|
| W1 | EngineClass deletion guard (finalizer) | 1 | Data integrity — chart-default install has zero protection today |
| W2 | FireboltInstance template defense-in-depth | 2 | Silent override of Envoy preStop drain hook breaks zero-downtime |
| W3 | EngineClass owned-field consumption gate | 2 | `Ready=False` is currently non-blocking; engine still consumes |
| W4 | Engine resource bounds controller-side | 2 | `--engine-max-*` is documented but unenforced in default deploys |
| W5 | Webhook-on integration suite (envtest) | 3 | Cert / Service / WebhookConfiguration rendering untested in CI |
| W6 | Controller `spec.id` ULID fallback envtest | 3 | Uncovered controller branch |
| W7 | `checkExternalPostgresSecret` empty-name envtest | 3 | Uncovered controller branch |
| W8 | e2e for deletion guard and resource bounds | 3 | Lock W1/W4 behavior end-to-end |
| W9 | Chart values.yaml warnings | 4 | Make webhook-off limitations visible at install |
| W10 | Docs + minor cleanup | 4 | AGENTS.md split note, misleading e2e comment, Makefile dedup |

Each workstream maps to one self-contained commit per the project's commit
conventions (`AGENTS.md` → "Planning work").

---

## Phase 1 — Data integrity

### W1. EngineClass deletion guard (controller-side)

**Problem.** With webhooks off, deleting a bound `EngineClass` succeeds and
orphans every engine referencing it. The validating webhook is the only
enforcement, and the chart ships webhooks off by default.

**Approach.** Add a finalizer
`compute.firebolt.io/engineclass-deletion-guard` managed by
`EngineClassReconciler`:

- Add the finalizer at first reconcile (idempotent Update + requeue).
- Branch on `deletionTimestamp`:
  - Not set → existing reconcile path.
  - Set → call `countBoundEngines`. If `> 0`, set
    `Ready=False/DeletionBlocked` with the count in the message and requeue;
    **do not remove the finalizer**. If `0`, remove the finalizer.

Keep the webhook in place. It provides immediate apply-time rejection; the
finalizer is the always-on backstop that survives `webhook.enabled: false`
and cert outages. They don't conflict — when the webhook rejects, the
finalizer never sees the delete.

**Escape hatch.** `kubectl patch … --type=merge -p
'{"metadata":{"finalizers":null}}'` — the legitimate "I really mean it"
override.

**Files.** `internal/controller/engineclass_controller.go`,
`api/v1alpha1/engineclass_types.go` (new condition reason).

**Tests.**
- envtest: finalizer added on first reconcile; blocked transition with bound engine; unblocked transition after engine delete; force-remove path.
- e2e: covered by W8.

---

## Phase 2 — Silent input loss

### W2. FireboltInstance template defense-in-depth

**Problem.** With webhooks off, users can set operator-owned fields on
`spec.gateway.template` / `spec.metadata.template` (preStop, env, command,
ports, …). The reconciler rebuilds the pod spec from a small allowlist, so
user input is silently dropped — no status signal. Most dangerous: a silent
override of Envoy's `preStop` drain hook breaks the zero-downtime contract.

**Approach.** Mirror the EngineClass `Ready=False/OperatorOwnedFieldSet`
pattern at the FireboltInstance level:

- Run `ValidatePodTemplate(spec.gateway.template, GatewayPodTemplateRules)`
  (and same for metadata) at the top of
  `FireboltInstanceReconciler.Reconcile`, before rendering.
- On error, set `GatewayReady=False/TemplateRejected` (resp.
  `MetadataReady`) with the field path, refuse to render that component,
  bubble to controller-runtime backoff. Existing `Ready` rollup catches it.

When the webhook is on, this branch is dead code — admission already
rejected. When off, the user gets the same field-path error in
`.status.conditions` instead of silently-dropped input.

**Files.** `internal/controller/instance_controller.go`,
`internal/controller/instance_gateway.go`,
`internal/controller/instance_metadata.go`.

**Tests.**
- envtest: forbidden gateway field → `GatewayReady=False/TemplateRejected` with field path; same for metadata.
- envtest: valid template → reconcile produces expected Deployments (false-positive guard).

### W3. EngineClass owned-field consumption gate

**Problem.** `classReadiness` already stamps
`Ready=False/OperatorOwnedFieldSet`, but the engine reconciler still
consumes that class and builds a misshapen StatefulSet off it. The status
is non-blocking.

**Approach.** In `resolveEngineClassInfo` (or its caller), read
`class.Status.Conditions[Ready]`. If `False/OperatorOwnedFieldSet`, surface
`EngineConditionReady=False/EngineClassUnready` on the engine, skip applying
the new STS, bubble for backoff. Same shape as how a missing class is
handled today.

**Files.** `internal/controller/engine_controller.go`.

**Tests.**
- envtest: class admitted with operator-owned field (direct write to bypass admission), engine referencing it stays at `EngineClassUnready`, no STS produced.
- envtest: clear the bad field → engine progresses normally.

### W4. Engine resource bounds (controller-side)

**Problem.** `--engine-max-*` bounds are advertised in
`values.yaml:138-155` but enforced only at admission. With webhooks off
the bounds are silently ignored — flags read fine, no engine ever fails.

**Approach.** Plumb `EngineResourceBounds` into `FireboltEngineReconciler`
(already plumbed into the webhook from `cmd/main.go`). Check
`spec.resources` against the bound at the top of `Reconcile`; on
violation, set `Ready=False/ResourceBoundsExceeded` with the field path and
the configured maximum (mirror the webhook's error message), refuse to
render the STS, bubble for backoff.

**Files.** `internal/controller/engine_controller.go`, `cmd/main.go`
(pass bounds into the reconciler the same way they're passed to the
webhook today).

**Tests.**
- envtest: bound configured, engine with limits above bound → blocked with the matching condition.
- envtest: empty bounds (default) → no-op (false-positive guard).

---

## Phase 3 — Coverage gaps

### W5. Webhook-on integration suite (envtest)

**Problem.** Nothing in CI actually runs a webhook server. Cert wiring,
admission paths (`/validate-compute-firebolt-io-v1alpha1-…`), the rendered
`Service` and `ValidatingWebhookConfiguration` can regress silently.

**Approach.** Add a second envtest suite (e.g.
`internal/controller/webhook_suite_test.go` with build tag
`webhook_integration`, or under `test/integration/`) that uses
controller-runtime's `envtest.WebhookInstallOptions` to install the
operator's webhook configurations and exercises at least:

- `spec.id` defaulting via the network path.
- EngineClass owned-field rejection via the network path.
- EngineClass `DELETE` refused while bound, via the network path.

**Tests.** The three above; reuses existing fixtures.

### W6. Controller `spec.id` ULID fallback envtest

**Problem.** `instance_controller.go:115-122` is uncovered.

**Approach.** envtest: create `FireboltInstance` with empty `spec.id`, run
one reconcile, assert ULID is minted and persisted on the CR.

### W7. `checkExternalPostgresSecret` empty-name envtest

**Problem.** `instance_controller.go:382-400` `errPostgresSecretRefEmpty`
branch is uncovered (only reachable when admission is bypassed).

**Approach.** envtest: external Postgres with empty
`credentialsSecretRef.Name`, assert
`MetadataReady=False/PostgresSecretPreflightFailed`.

### W8. e2e — deletion guard and resource bounds

**Problem.** Phase 1 (W1) and W4 need end-to-end coverage to lock
behavior in.

**Status.**

- **Deletion guard — shipped.** `test/e2e/deletion_guard_test.go`
  starts an in-process `EngineClassReconciler`, creates a class plus
  a "binding carrier" `FireboltEngine` CR (no engine controller
  involved — the engine just exists so `countBoundEngines` finds
  it), DELETEs the class, asserts Terminating +
  `Ready=False/DeletionBlocked` with the count, then removes the
  engine and asserts the class reaps. Spec is lightweight: no
  `FireboltInstance`, no pods. A `StartClassOperator` helper and a
  `CreateBareEngineWithClassRef` helper land in
  `test/e2e/helpers_test.go`.
- **Resource bounds — deferred.** The engine reconciler's instance
  gate (`resolveInstanceInfo` in `engine_controller.go`) runs before
  the bounds gate, so an e2e test that reaches `ResourceBoundsExceeded`
  needs a fully-Ready `FireboltInstance` fixture (pods, metadata
  service, etc.). The benefit doesn't justify the cost: W4's unit
  tests in `engine_resource_bounds_test.go` already exercise the
  gate against a fake instance, and the `Validate` method is shared
  with the webhook (whose unit tests cover the same code path from
  admission). Follow-up if it becomes load-bearing for a customer
  use case: pair it with the upcoming W5 webhook integration suite
  so we get over-the-wire coverage of both paths in one fixture.

---

## Phase 4 — Hygiene

### W9. Chart & `values.yaml`

- Update the `engineResourceBounds` block warning at `values.yaml:142-145`
  to reflect controller-side enforcement landing in W4.
- Add a `NOTES.txt` warning when `engineResourceBounds.*` is set with
  `webhook.enabled: false`, pointing at the controller-side enforcement and
  recommending webhooks for synchronous apply-time signal.
- **Open question (separate ticket).** Flip the chart default to
  `webhook.enabled: true` with cert-manager prerequisite, behind a
  deprecation across one minor version. Out of scope for FB-1298.

### W10. Documentation & minor cleanup

- **`AGENTS.md`.** Add a short subsection documenting the webhook on/off
  split, chart default, and the W1–W4 controller-side fallbacks.
- **`test/e2e/e2e_test.go:539-544`.** Fix the misleading comment ("guarded
  by the validating webhook") — the harness never runs webhooks.
- **`Makefile:280`.** Drop the redundant
  `--set additionalArgs='{--enable-webhooks=false}'` from `local-deploy`;
  the chart template already emits the flag.
- **`docs/architecture.md`.** One paragraph on how deletion-guard,
  template defense-in-depth, and resource-bound enforcement compose
  between admission and reconcile.

---

## Sequencing

```
W1                    ── ship first; highest-risk gap.
W2 → W3 → W4          ── each independently shippable.
W5                    ── parallel to Phase 2; unblocks integration tests.
W6, W7                ── small; can land any time.
W8                    ── after W1 / W4 merge.
W9, W10               ── parallel from day 1.
```
