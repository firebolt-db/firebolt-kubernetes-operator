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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Engine Failure Isolation
//
// These specs pin down a property the operator promises but does not
// explicitly test elsewhere: a misbehaving engine cannot affect
// queries against a healthy engine on the same FireboltInstance. The
// healthy engine is created once per test suite and has a gateway-
// routed background query runner attached for the duration; each It
// introduces a different broken-peer engine and asserts the runner's
// failure counter stays at zero while the broken peer is observable in
// the cluster.
//
// The three failure modes — CrashLoopBackOff (broken image),
// OOMKilled-at-startup (memory limit below the engine binary's
// minimum), and Pending (unschedulable nodeSelector) — were chosen
// because they reliably manifest on Kind within seconds and they
// exercise different layers of the operator's per-engine isolation
// surface (image pull, scheduler, kubelet OOM-killer).
var _ = Describe("Engine Failure Isolation", Ordered, func() {
	var (
		instanceName  = "inst-isol" + queryConfig.Suffix
		healthyEngine = "test-isol" + queryConfig.Suffix + "-healthy"
		badEngine     = "test-isol" + queryConfig.Suffix + "-bad"
		badClass      = "test-isol" + queryConfig.Suffix + "-bad-class"
		clientPod     = "client-isol" + queryConfig.Suffix
		engineNames   = []string{healthyEngine, badEngine}
		lc            *TestInstanceLifecycle
		bgRunner      *GatewayBackgroundQueryRunner
	)
	RegisterFailedSpecPodLogDumpMulti(&instanceName, &engineNames)

	BeforeAll(func() {
		By("Setting up FireboltInstance for failure-isolation tests")
		var err error
		lc, err = SetupTestInstance(ctx, instanceName)
		Expect(err).NotTo(HaveOccurred())

		By("Creating client pod")
		Expect(CreateClientPod(ctx, clientPod)).To(Succeed())

		By("Creating healthy engine")
		Expect(CreateEngine(ctx, instanceName, healthyEngine, 1)).To(Succeed())
		Expect(WaitForEngineReady(ctx, healthyEngine, 1, clusterReadyTimeout)).To(Succeed())
		Expect(WaitForEngineStable(ctx, healthyEngine, clusterReadyTimeout)).To(Succeed())
	})

	AfterAll(func() {
		if bgRunner != nil {
			bgRunner.Stop()
		}
		DeleteClientPod(ctx, clientPod)
		Expect(DeleteEngine(ctx, healthyEngine)).To(Succeed())
		Expect(DeleteEngine(ctx, badEngine)).To(Succeed())
		Expect(DeleteEngineClass(ctx, badClass)).To(Succeed())
		Expect(WaitForResourcesDeleted(ctx, healthyEngine, resourceCleanupTimeout)).To(Succeed())
		Expect(WaitForResourcesDeleted(ctx, badEngine, resourceCleanupTimeout)).To(Succeed())
		TeardownTestInstance(ctx, lc)
	})

	AfterEach(func() {
		if bgRunner != nil {
			bgRunner.Stop()
			bgRunner = nil
		}
		// Each It owns its own broken peer; tear it down so the next It
		// starts from the same baseline (healthy engine alone, no class).
		Expect(DeleteEngine(ctx, badEngine)).To(Succeed())
		Expect(WaitForResourcesDeleted(ctx, badEngine, resourceCleanupTimeout)).To(Succeed())
		Expect(DeleteEngineClass(ctx, badClass)).To(Succeed())
	})

	// assertHealthyEngineStaysServing runs the gateway background query
	// runner on the healthy engine for `window`, asserts the failure
	// counter stays at zero throughout, and asserts the success counter
	// grew enough to make the zero-failure assertion meaningful (a
	// runner with zero successes proves nothing). It also re-asserts
	// Ready=True on the healthy engine at the end, so a regression that
	// silently degrades the healthy engine (e.g. an instance-level
	// condition flipping false because of the broken peer) is caught.
	assertHealthyEngineStaysServing := func(window time.Duration, minSuccesses int32) {
		By("Polling failure counter to be zero for the entire window")
		Consistently(func() int32 {
			_, failures := bgRunner.GetStats()
			return failures
		}, window, 1*time.Second).Should(BeZero(),
			"gateway queries against the healthy engine must not fail while a peer engine is broken")

		By("Verifying the healthy engine collected enough successful samples to be meaningful")
		successes, failures := bgRunner.GetStats()
		fmt.Fprintf(GinkgoWriter, "Healthy engine stats during peer failure: %d successes, %d failures\n",
			successes, failures)
		Expect(successes).To(BeNumerically(">=", minSuccesses),
			"the background runner must accumulate enough successful queries for the zero-failure assertion to be meaningful")

		By("Verifying the healthy engine still reports Ready=True")
		Expect(WaitForEngineStable(ctx, healthyEngine, 30*time.Second)).To(Succeed())
	}

	startRunnerOnHealthyEngine := func() {
		bgRunner = NewGatewayBackgroundQueryRunnerWithValidator(
			clientPod, instanceName, healthyEngine,
			queryConfig.Query, queryConfig.Validator,
		)
		bgRunner.Start(ctx)
	}

	It("isolates a peer in ImagePullBackOff (broken engine image)", func() {
		By("Starting background query runner on the healthy engine")
		startRunnerOnHealthyEngine()

		By("Creating an EngineClass pointing at a non-existent image")
		// Tag intentionally absurd so kubelet returns ErrImagePull / ImagePullBackOff
		// rather than pulling any cached layer; "ghcr.io" exists but the
		// repo path does not so the registry returns 404 on the manifest.
		Expect(CreateEngineClass(ctx, badClass, "ghcr.io/firebolt-db/does-not-exist:v0.0.0")).To(Succeed())

		By("Creating the broken peer that references the bad class")
		Expect(CreateEngineWithClass(ctx, instanceName, badEngine, 1, badClass)).To(Succeed())

		assertHealthyEngineStaysServing(30*time.Second, 20)
	})

	It("isolates a peer that OOMKills at startup (memory limit too low)", func() {
		By("Starting background query runner on the healthy engine")
		startRunnerOnHealthyEngine()

		By("Creating the broken peer with a memory limit below the engine binary's startup footprint")
		// 16Mi is well below the engine container's RSS at any startup
		// step; the kernel OOM-killer reaps it before /health/ready ever
		// returns 200, so the pod cycles in CrashLoopBackOff. The
		// requests intentionally match the limit so the scheduler still
		// accepts the pod (otherwise this would degenerate into the
		// "Pending" case below).
		Expect(CreateEngineWithResources(ctx, instanceName, badEngine, 1, corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
		})).To(Succeed())

		assertHealthyEngineStaysServing(30*time.Second, 20)
	})

	It("isolates a peer whose pods are Pending (unsatisfiable nodeSelector)", func() {
		By("Starting background query runner on the healthy engine")
		startRunnerOnHealthyEngine()

		By("Creating the broken peer with an unschedulable nodeSelector")
		Expect(CreateEngine(ctx, instanceName, badEngine, 1)).To(Succeed())
		Expect(UpdateEngineScheduling(ctx, badEngine,
			map[string]string{"firebolt.io/no-such-node": "true"},
			nil, nil,
		)).To(Succeed())

		assertHealthyEngineStaysServing(30*time.Second, 20)
	})
})
