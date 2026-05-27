# EngineClass CRD Reference

`EngineClass` is a namespaced CRD that holds a reusable pod-template
fragment referenced by `FireboltEngine` via `spec.engineClassRef`.
Engines in the same namespace inherit shared pod-level settings —
service account, scheduling, pod annotations, sidecars, engine
container image — from a single declaration, so the same value does
not have to be repeated on every engine.

The pattern echoes `StorageClass` / `IngressClass` / `GatewayClass` in
the "shared template referenced by name" sense, but `EngineClass` is
**namespaced** rather than cluster-scoped because its template carries
namespace-resolved identifiers — ServiceAccount names, Secret /
ConfigMap / PVC volume references, and the per-tenant IAM annotations
the engine pod needs. Kubernetes resolves those names in the engine's
own namespace at pod admission time; a cluster-scoped class with
`serviceAccountName: foo` referenced from two namespaces would bind
silently to two different ServiceAccounts (and possibly two different
IAM roles) with admission catching nothing. Co-locating the class and
its consumer engines avoids that footgun.

Multiple classes can exist per namespace; the engine picks one by name;
usage is optional.

## Spec Reference

| Field | Required | Default | Description |
|---|---|---|---|
| `spec.template` | **Yes** | – | A Kubernetes [`PodTemplateSpec`](https://kubernetes.io/docs/reference/kubernetes-api/workload-resources/pod-template-v1/) merged underneath each engine's own pod template. Engine spec fields win over class fields; operator defaults sit beneath both. |

The validating webhook rejects user input on paths the operator owns
end-to-end. The rejected set is enumerated below; everything else under
`spec.template` is allowed.

### Pod template metadata

| Path | User-allowed | Notes |
|---|---|---|
| `spec.template.metadata.labels` | Yes | Keys under `firebolt.io/` are **rejected**. Engine spec `spec.podLabels` wins on conflict. |
| `spec.template.metadata.annotations` | Yes | Keys under `firebolt.io/` are **rejected**. Engine spec `spec.podAnnotations` wins on conflict. Typical use: `kube2iam` IAM role binding. |
| `spec.template.metadata.{name,namespace,ownerReferences,...}` | No | Pod identity is operator-assigned by the StatefulSet controller. |

### Pod-level fields under `spec.template.spec`

| Path | User-allowed | Notes |
|---|---|---|
| `serviceAccountName` | Yes | IRSA / Pod Identity binding. Engine `spec.serviceAccountName` wins. |
| `nodeSelector` | Yes | Map-merge; engine keys win on conflict. |
| `tolerations` | Yes | Class tolerations + engine tolerations, concatenated. |
| `affinity` | Yes | Engine `spec.affinity` wins if non-nil; otherwise class. No field-merge. |
| `imagePullSecrets` | Yes | Passed through. |
| `volumes` | Yes | Operator-owned volumes (`nodes-config`, data) come first; class volumes appended. Class entries whose names collide with operator volumes are silently dropped (the operator owns those mount paths). |
| `securityContext` | Yes | Engine `spec.podSecurityContext` wins if non-nil; otherwise class. Operator defaults (`fsGroup`, `fsGroupChangePolicy`) are always stamped on whichever side won. |
| `initContainers[*]` | Yes | Class slice + engine `spec.initContainers`, concatenated (mirrors `tolerations`). An init container named `engine` is **rejected** (collides with the engine container name). |
| `containers[name=="engine"]` | Limited | Engine container. See below. |
| `containers[name!="engine"]` | Yes | Sidecars. Fully user-owned — image, command, ports, env, mounts. |
| `terminationGracePeriodSeconds` | **No** | Stamped from FireboltEngine `spec.terminationGracePeriodSeconds`. |
| `subdomain`, `hostname` | **No** | Headless-DNS contract. |
| `restartPolicy`, `activeDeadlineSeconds` | **No** | StatefulSet semantics. |

### Engine container (`containers[name=="engine"]`)

The engine container's identity is operator-owned; only the fields below
are accepted on the class template. The container name matches the public
CRD ("engine"); the binary entrypoint inside the image is still
`firebolt-core`, but that's an image-internal path and doesn't surface on
the pod template.

| Field | User-allowed | Notes |
|---|---|---|
| `image`, `imagePullPolicy` | Yes | Canonical knob for runtime version upgrades. |
| `resources` | Yes | Engine `spec.resources` wins wholesale if it carries any requests/limits/claims; otherwise the class entry fills in. No field-level merge. |
| `env` | Yes | Operator-injected vars (`POD_INDEX`, `FB_AWS_EC2_METADATA_CLIENT_ENABLED`, `FIREBOLT_CORE_MODE`) appear first on the pod; class entries appended. Class entries that name a reserved key are **rejected** at admission. |
| `envFrom` | Yes | Passed through from class. |
| `volumeMounts` | Yes | Operator-owned mounts first; class mounts appended. Class entries whose names collide with operator-owned volumes (`nodes-config`, data) are silently dropped. |
| `securityContext` | Yes | Engine `spec.securityContext` wins if non-nil; otherwise class. |
| `lifecycle` | Yes | Passed through from class. |
| `name`, `command`, `args`, `ports` | **No** | Hardcoded by the operator. |
| `readinessProbe`, `livenessProbe`, `startupProbe` | **No** | Owned by the operator (`/health/ready` contract). |

A second container also named `engine` is **rejected** — it would collide
with the operator-rendered engine container during the merge.

## Status Reference

| Field | Description |
|---|---|
| `status.observedGeneration` | `metadata.generation` last reconciled. |
| `status.boundEngines` | Count of FireboltEngines in this class's namespace that reference it via `spec.engineClassRef`. Surfaced for visibility; the deletion gate uses a live list of engines, not this cached value. |
| `status.conditions[type=Ready]` | True when `spec.template` contains no operator-owned fields. `False/Reason=OperatorOwnedFieldSet` is a defense-in-depth signal for classes admitted under an older operator with a narrower rejection set. |

## Deletion

The validating webhook **refuses** `DELETE` while any FireboltEngine in
the class's namespace references the class via `spec.engineClassRef`.
EngineClass is namespaced, so cross-namespace references are not
possible — only engines in the same namespace count toward the gate.
The check lists FireboltEngines live from the API server at admission
time rather than reading `status.boundEngines`, so a class bound
between reconciler runs (the field still at its zero default) is still
protected. Clear `spec.engineClassRef` on every referencing engine
first, then delete the class. `failurePolicy: Fail` on the webhook
configuration prevents a webhook outage from opening a window in which
a bound class could be removed.

## Rollouts

Editing the class spec triggers a blue-green rollout on every
referencing engine in the same namespace. The mechanism: the engine
controller watches `EngineClass`; on event it enqueues every engine in
the class's namespace whose `spec.engineClassRef` matches.
`stsMatchesSpec` compares the resolved class content hash against the
`firebolt.io/engine-class-hash` annotation on the StatefulSet — any
mismatch returns false and the engine bumps `currentGeneration`. Same
mechanism whether the class spec was edited in place, the engine
flipped to a different class in the same namespace, or the reference
was cleared.

## Monitoring

```bash
kubectl get firec -n firebolt
```

```
NAME                BOUND   READY   AGE
compute-optimized   3       True    24h
```

Or across all namespaces:

```bash
kubectl get firec --all-namespaces
```

For a worked example, see [`examples/engine-class.yaml`](../examples/engine-class.yaml).
For multi-engine usage that shares a class across two engines, see
[`examples/instance-with-engines.yaml`](../examples/instance-with-engines.yaml).
