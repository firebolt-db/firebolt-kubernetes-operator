# Local image registry

Kind nodes obtain E2E workload images through a persistent local Docker
registry. This avoids importing the multi-gigabyte Engine image separately into
every node's containerd store. The small, locally built operator image still
uses `kind load docker-image` for `make local-deploy`.

## Topology

```text
host 127.0.0.1:5001
        |
        v
kind-registry:5000 on the Docker "kind" network
        |
        v
Kind containerd mirrors for ghcr.io, docker.io, and oci.firebolt.io
```

The defaults are registry container `kind-registry`, image `registry:2`, and
Kind cluster `operator-test-e2e`. The Make targets accept `REGISTRY_NAME`,
`REGISTRY_PORT`, and `KIND_CLUSTER` overrides.

[`scripts/kind-config.yaml`](../../scripts/kind-config.yaml) enables
containerd's `/etc/containerd/certs.d` configuration. After creating or reusing
a cluster, [`scripts/setup-kind-cluster.sh`](../../scripts/setup-kind-cluster.sh)
writes the mirror configuration on every node. A reused cluster without the
containerd patch must be recreated; host files alone cannot enable the feature.

[`scripts/load-e2e-images.sh`](../../scripts/load-e2e-images.sh) pulls each
workload image, publishes it under the mirrored registry path, and removes its
temporary host tags. Test manifests retain their normal image references because
the node-side mirror redirects the pull.

Image-switch tests use a synthetic `<tag>-uptest` reference. Keep
`UPGRADE_TAG_SUFFIX` in the load script aligned with `upgradeTagSuffix` in
`test/e2e/e2e_suite_test.go`.

## Commands and lifecycle

```bash
make setup-local-registry  # start or repair registry/network attachment
make setup-kind            # create or reuse Kind and configure mirrors
make load-test-images      # publish the selected workload image variant
make prepare-test-e2e      # complete E2E preparation
curl http://localhost:5001/v2/_catalog
```

`make cleanup-test-e2e` deletes the Kind cluster but retains the registry cache.
Do not delete the cluster, registry, or Docker images as routine test cleanup.

Use `make flush-local-registry` only when stale or malformed registry content is
masking the desired image. It recreates the registry and discards cached layers.

## Common failures

- **Registry not attached to the Kind network:** run
  `make setup-local-registry`; the setup is idempotent and repairs attachment.
- **Reused cluster ignores mirrors:** inspect the node containerd configuration
  for `config_path = "/etc/containerd/certs.d"`. Recreate that cluster through
  the repository target if it is absent.
- **Expected tag is missing:** run `make load-test-images` with the same image
  variant and registry overrides used by the E2E suite.
- **Host disk grows after an interrupted publish:** inspect and remove only the
  temporary tags left by the failed load command; do not delete unrelated images.
