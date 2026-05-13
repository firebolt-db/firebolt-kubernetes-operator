# Troubleshooting

## Instance

### Instance stuck in "Provisioning"

Components are not becoming healthy. Check the underlying resources:

```bash
kubectl get pods -l firebolt.io/instance=<instance-name> -n firebolt
kubectl logs -l firebolt.io/component=metadata -n firebolt
```

### Instance in "Failed" phase

A terminal error occurred (e.g., multiple accounts found in the metadata service). Inspect the operator logs for details and resolve the underlying issue manually. The operator will not automatically recover from this state.

## Engine

### Engine stuck with `InstanceReady=False`

The referenced instance is not ready. Check instance status:

```bash
kubectl get fi -n firebolt
kubectl describe fi <instance-name> -n firebolt
```

Common causes: instance still provisioning, metadata service pods not ready, gateway pods not ready.

### Engine stuck in "creating" phase

Pods in the new generation are not becoming ready:

```bash
kubectl get pods -l firebolt.io/engine=<engine-name> -n firebolt
kubectl describe pod <pod-name> -n firebolt
kubectl logs <pod-name> -n firebolt
```

### Engine stuck in "draining" phase

Old pods still have running queries. This is normal for long-running queries. To force the transition, set `rollout: recreate` in the engine spec.
