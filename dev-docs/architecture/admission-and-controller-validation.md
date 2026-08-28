# Admission and controller-side validation

The Firebolt Operator must enforce the same invariant whether admission webhooks are enabled or disabled. The binary enables webhooks by default, but the Helm chart disables them by default because certificate bootstrap belongs to the installer. Unit tests and Kind E2E tests also run without the webhook server unless a test explicitly uses the webhook integration build tag.

As a result, admission is an early feedback mechanism, not the sole correctness boundary.

## Enforcement layers

| Layer | Runs when | Best use |
| --- | --- | --- |
| CRD schema and CEL | Every API-server write | Structural rules, immutable transitions, and race-closing invariants |
| Admission webhook | Webhooks are installed and reachable | Reject invalid writes immediately with precise field errors; apply defaults before persistence |
| Controller fallback | Every reconciliation | Prevent invalid resources from being rendered and surface actionable status when admission was bypassed |

The implementation should share validation functions between webhook and controller wherever possible. Reimplementing the same rule twice invites different accepted inputs.

## Current invariant map

| Input or operation | Admission behavior | Controller behavior |
| --- | --- | --- |
| Empty `FireboltInstance.spec.id` | Mutating webhook generates a ULID | First reconcile generates the ULID and updates the CR |
| Instance authentication | `ValidateAuth` rejects invalid combinations | `ensureAuth` re-runs `ValidateAuth` and reports `AuthReady=False/AuthSpecInvalid` |
| Instance TLS | `ValidateTLS` rejects invalid listener, CA, CRL, and protected-Secret combinations | Engine and gateway TLS reconcilers re-run `ValidateTLS` and report `TLSSpecInvalid` |
| External PostgreSQL Secret reference | Webhook rejects an empty name; CEL fixes replicas at one | Metadata preflight reports `PostgresSecretPreflightFailed` when the Secret input is unusable |
| Instance gateway or metadata pod template | Shared operator-authority rules reject owned paths | `validateInstanceTemplates` reports `TemplateRejected` and skips the offending component |
| EngineClass pod template | Shared rules reject owned paths | EngineClass readiness reports `OperatorOwnedFieldSet`; consuming Engines refuse to render from that class |
| ClusterFireboltEngineClass SKU-only / owned paths | Webhook rejects | Engine resolver re-validates the live spec and refuses to render (`FireboltEngineClassUnready`) |
| EngineClass deletion while referenced | Delete webhook rejects the request | Finalizer holds deletion and reports `DeletionBlocked` with the bound-Engine count |
| ClusterFireboltEngineClass deletion | Delete is allowed | Engines emit Warning `EngineClassNotFound` and keep the last applied StatefulSet |
| Missing EngineClass reference | Engine webhook rejects the write | Engine resolution emits Warning `EngineClassNotFound` and retries with backoff |
| Engine pod template | Shared rules reject owned paths | `validateEngineTemplates` reports `TemplateRejected` and skips StatefulSet rendering |
| Engine-container resource bounds | Engine webhook applies configured maximums | Reconciler uses the same `EngineResourceBounds` value and reports `ResourceBoundsExceeded` |

The Instance ID transition also has a CEL rule that allows exactly the controller's empty-to-generated update and prevents subsequent mutation. Webhook plus controller alone cannot close the race between two writers changing an immutable identity field.

## Shared validation sources

| Concern | Source |
| --- | --- |
| Pod-template ownership rules | [`api/v1alpha1/operatorauthority.go`](../../api/v1alpha1/operatorauthority.go) |
| FireboltInstance webhook, auth, and TLS validation | [`api/v1alpha1/fireboltinstance_webhook.go`](../../api/v1alpha1/fireboltinstance_webhook.go) |
| FireboltEngine webhook and resource bounds | [`api/v1alpha1/fireboltengine_webhook.go`](../../api/v1alpha1/fireboltengine_webhook.go) |
| FireboltEngineClass webhook | [`api/v1alpha1/fireboltengineclass_webhook.go`](../../api/v1alpha1/fireboltengineclass_webhook.go) |
| ClusterFireboltEngineClass webhook | [`api/v1alpha1/clusterfireboltengineclass_webhook.go`](../../api/v1alpha1/clusterfireboltengineclass_webhook.go) |
| Instance controller fallbacks | [`internal/controller/instance_controller.go`](../../internal/controller/instance_controller.go), [`instance_auth.go`](../../internal/controller/instance_auth.go), and [`instance_tls.go`](../../internal/controller/instance_tls.go) |
| Engine controller fallbacks | [`internal/controller/engine_controller.go`](../../internal/controller/engine_controller.go) |
| EngineClass readiness and deletion guard | [`internal/controller/fireboltengineclass_controller.go`](../../internal/controller/fireboltengineclass_controller.go) |
| Webhook registration and shared bounds wiring | [`cmd/main.go`](../../cmd/main.go) |

`cmd/main.go` parses Engine resource limits once and passes the resulting value to both the FireboltEngine webhook and reconciler. Keep that single wiring path; separate parsing or defaults would make admission posture change behavior.

## Pod-template authority

Engine and EngineClass templates merge into one pod, so both use `FireboltEngineClassPodTemplateRules`. Instance gateway and metadata templates use component-specific rule sets.

The rule sets distinguish:

- operator-owned identity, command, ports, probes, topology, and protected environment variables;
- user-controlled container and pod customization;
- reserved labels, annotations, volumes, mounts, and container names;
- security-sensitive pod fields that are rejected rather than silently ignored.

Controller validation must run before a builder consumes the template. This is especially important for container environment variables because Kubernetes uses the last duplicate name, allowing an appended user value to replace an operator value if validation is skipped.

## Error-surfacing policy

Admission errors use Kubernetes field paths so clients can reject the write before it enters storage. Controller fallbacks cannot retroactively reject a stored object, so they must:

1. avoid rendering or mutating the affected workload;
2. set the most specific condition available;
3. include the invalid field path in the message;
4. requeue so correcting the CR recovers without manual cleanup.

Not every fallback uses a dedicated condition. A missing EngineClass emits a Warning `EngineClassNotFound` Event and follows controller backoff; an invalid or explicitly unready class has a surfaced Ready condition because the user needs the class-side reason.

## Adding or changing an invariant

1. Decide whether the invariant belongs in schema/CEL, admission and controller, or all three.
2. Put reusable validation in `api/v1alpha1` so admission and controllers call the same function.
3. Wire identical runtime configuration into the webhook and reconciler.
4. Add webhook unit tests for accepted and rejected objects.
5. Add controller or envtest coverage with webhooks disabled.
6. Add webhook-on integration coverage when registration, conversion, defaulting, or API-server admission behavior matters.
7. Regenerate CRDs when markers or CEL rules change.
8. Update the relevant CRD reference and public behavior documentation.

Run the ordinary suite and the webhook-on integration suite as appropriate:

```bash
make test
make test-webhook-integration
```

The controller fallback test is mandatory even when the webhook test already covers the rule, because the default Helm posture does not install the webhook.
