//go:build e2e
// +build e2e

/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Floci (https://github.com/floci-io/floci) is a local S3-compatible object
// storage emulator. The engine binary refuses to start in dedicated-pensieve
// mode without managed_storage pointing at object storage, so the E2E suite
// stands up a single-replica floci Deployment in the test namespace and
// pre-creates a bucket the engine can use. The engine's spec.customEngineConfig
// (see CreateEngineWithRollout) wires firebolt.managed_storage at the floci
// Service below. Floci is zero-auth, so dummy AWS credentials suffice.
const (
	flociBucket   = "firebolt-e2e-bucket"
	flociEndpoint = "http://floci." + testNamespace + ".svc.cluster.local:4566"
)

// flociManifest is the floci Deployment + Service applied into testNamespace
// at suite start. Kept in lockstep with scripts/ci/lib/floci.yaml — the helm
// quickstart scripts use the same image and port so a regression on one path
// surfaces on the other.
const flociManifest = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: floci
  labels:
    app: floci
spec:
  replicas: 1
  selector:
    matchLabels:
      app: floci
  template:
    metadata:
      labels:
        app: floci
    spec:
      # The kubelet auto-injects FLOCI_PORT=tcp://<svc-ip>:4566 (and
      # friends) into pods that live in the same namespace as the floci
      # Service. floci's Quarkus runtime treats FLOCI_PORT as a config key
      # for floci.port and tries to coerce "tcp://..." to an integer,
      # which CrashLoopBackOffs the container before it ever listens.
      # enableServiceLinks: false suppresses that whole env block; the
      # pod still reaches the Service by DNS (floci.<ns>.svc:4566), so
      # nothing else is affected.
      enableServiceLinks: false
      containers:
        - name: floci
          image: floci/floci:latest
          imagePullPolicy: IfNotPresent
          ports:
            - name: api
              containerPort: 4566
              protocol: TCP
          env:
            - name: FLOCI_DEFAULT_REGION
              value: us-east-1
          readinessProbe:
            tcpSocket:
              port: 4566
            initialDelaySeconds: 1
            periodSeconds: 2
            failureThreshold: 30
          livenessProbe:
            tcpSocket:
              port: 4566
            initialDelaySeconds: 5
            periodSeconds: 10
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              cpu: "1"
              memory: 1Gi
---
apiVersion: v1
kind: Service
metadata:
  name: floci
  labels:
    app: floci
spec:
  type: ClusterIP
  ports:
    - name: api
      port: 4566
      targetPort: 4566
      protocol: TCP
  selector:
    app: floci
`

// flociMkbucketJob renders the one-shot Job that creates a bucket against the
// floci Service. The aws CLI's create-bucket is idempotent at the API level
// (BucketAlreadyOwnedByYou is treated as success below), and head-bucket at
// the end asserts the bucket exists post-condition so a partial failure
// surfaces as a Job error instead of a silent miss.
func flociMkbucketJob(namespace, bucket string) string {
	return fmt.Sprintf(`
apiVersion: batch/v1
kind: Job
metadata:
  name: floci-mkbucket
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
              value: http://floci.%s.svc.cluster.local:4566
          command:
            - sh
            - -c
            - |
              set -eu
              aws s3api create-bucket --bucket "%s" || true
              aws s3api head-bucket --bucket "%s"
`, namespace, bucket, bucket)
}

// setupFloci applies the floci Deployment + Service into the namespace, waits
// for the rollout, then runs a Job that creates the engine's bucket. Safe to
// call multiple times: kubectl apply is idempotent and the create-bucket step
// tolerates an existing bucket.
func setupFloci(ctx context.Context, namespace string) error {
	if err := kubectlApplyManifest(ctx, namespace, flociManifest); err != nil {
		return fmt.Errorf("apply floci manifest: %w", err)
	}

	rolloutArgs := kubectlArgs("rollout", "status", "deployment/floci",
		"-n", namespace, "--timeout=120s")
	if out, err := exec.CommandContext(ctx, "kubectl", rolloutArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("wait for floci rollout: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	// Delete any previous mkbucket Job so the apply below isn't blocked by
	// an immutable-field rejection from a stale completed Job.
	delArgs := kubectlArgs("delete", "job", "floci-mkbucket",
		"-n", namespace, "--ignore-not-found", "--wait=true")
	_ = exec.CommandContext(ctx, "kubectl", delArgs...).Run()

	if err := kubectlApplyManifest(ctx, namespace, flociMkbucketJob(namespace, flociBucket)); err != nil {
		return fmt.Errorf("apply floci mkbucket job: %w", err)
	}

	waitArgs := kubectlArgs("wait", "--for=condition=complete",
		"--timeout=120s", "job/floci-mkbucket", "-n", namespace)
	if out, err := exec.CommandContext(ctx, "kubectl", waitArgs...).CombinedOutput(); err != nil {
		// Fall through to printing logs so a flake here is debuggable in CI.
		logArgs := kubectlArgs("logs", "job/floci-mkbucket", "-n", namespace, "--tail=200")
		logs, _ := exec.CommandContext(ctx, "kubectl", logArgs...).CombinedOutput()
		return fmt.Errorf("wait for floci mkbucket: %w (%s); logs:\n%s",
			err, strings.TrimSpace(string(out)), string(logs))
	}

	return nil
}

// kubectlApplyManifest pipes the given YAML to `kubectl apply -f -` against
// the namespace. Returns the underlying error with kubectl stderr attached
// so failures are self-describing in CI logs.
func kubectlApplyManifest(ctx context.Context, namespace, manifest string) error {
	args := kubectlArgs("apply", "-n", namespace, "-f", "-")
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("kubectl apply: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// flociSetupTimeout caps the suite-startup setup; exposed as a var so
// individual specs can tighten/relax if needed without rebuilding the
// package — currently unused outside this file but kept for symmetry with
// other suite-level timeouts.
var flociSetupTimeout = 3 * time.Minute //nolint:unused // referenced from e2e_suite_test.go future hook
