#!/usr/bin/env bash
# Deploy floci (S3 emulator) into a namespace and create a bucket for the
# engine to use. Idempotent: re-running re-applies the floci manifest and
# is a no-op if the bucket already exists.
#
# Usage:
#   setup_floci <namespace> <bucket-name>
#
# Side effects, all in <namespace>:
#   - Deployment/floci and Service/floci applied from scripts/ci/lib/floci.yaml
#   - A short-lived Job that creates <bucket-name> against the floci Service
#
# After this returns, the engine can talk to floci via
# http://floci.<namespace>.svc.cluster.local:4566. Inject that endpoint into
# the FireboltEngine via spec.customEngineConfig.firebolt.managed_storage
# (see verify-quickstart-{basic,full}.sh for the yq patch).

set -euo pipefail

# shellcheck disable=SC2034 # SCRIPT_DIR is used to locate floci.yaml below.
FLOCI_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

setup_floci() {
  local namespace="$1"
  local bucket="$2"

  echo "Deploying floci into namespace ${namespace}..."
  kubectl apply -n "$namespace" -f "${FLOCI_LIB_DIR}/floci.yaml"
  kubectl rollout status deployment/floci -n "$namespace" --timeout=120s

  echo "Creating bucket ${bucket} on floci..."
  # Bucket creation is idempotent: if it already exists (e.g. retry after a
  # crash), `aws s3api create-bucket` returns BucketAlreadyOwnedByYou with
  # exit code 254 which we tolerate via the `|| true` guard plus a final
  # head-bucket check that asserts the bucket exists post-condition.
  local job_name
  job_name="floci-mkbucket-$(date +%s)-$$"
  cat <<EOF | kubectl apply -n "$namespace" -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  labels:
    app: floci-mkbucket
spec:
  backoffLimit: 3
  ttlSecondsAfterFinished: 60
  template:
    metadata:
      labels:
        app: floci-mkbucket
    spec:
      restartPolicy: Never
      containers:
        - name: aws
          image: amazon/aws-cli:2.17.0
          env:
            - name: AWS_ACCESS_KEY_ID
              value: mock
            - name: AWS_SECRET_ACCESS_KEY
              value: mock
            - name: AWS_DEFAULT_REGION
              value: us-east-1
            - name: AWS_ENDPOINT_URL
              value: http://floci.${namespace}.svc.cluster.local:4566
          command:
            - sh
            - -c
            - |
              set -eu
              aws s3api create-bucket --bucket "${bucket}" || true
              aws s3api head-bucket --bucket "${bucket}"
EOF
  kubectl wait --for=condition=complete --timeout=120s "job/${job_name}" -n "$namespace"
  echo "Bucket ${bucket} ready on floci."
}
