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

package controller

// Deterministic exhaustive state-cover testing for the signing-key rotation
// state machine, mirroring the engine_tla_state_test.go and
// instance_tla_state_test.go harnesses.
//
// For every reachable state in the TLC state graph of formal/SigningKeyRotation.tla
// (regenerated via `make formal-gen`), this test materializes a FireboltInstance
// and a fleet whose convergence matches the state, runs the production
// stepSigningKeyRotation once, and verifies that the resulting state lies in the
// model's reconciler closure of the starting state — i.e. is a state TLC says is
// reachable by zero or more consecutive reconciler-only transitions.
//
// This is what keeps the spec honest. Without it, formal/SigningKeyRotation.tla
// could drift from stepSigningKeyRotation and keep passing TLC while proving
// nothing about the code that ships.

import (
	"context"
	"fmt"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

const (
	rotationTLAInstance  = "inst"
	rotationTLANamespace = "ns-1"
	// The fleet size the model is checked at (Engines = {e1, e2}).
	rotationTLAEngines = 2
)

// tlaRotationPhaseToGo maps a modeled phase to the Go phase. "absent" has no Go
// counterpart — such a key simply has no Status.Auth.SigningKeys entry — and is
// filtered out before this is called.
func tlaRotationPhaseToGo(p string) computev1alpha1.SigningKeyPhase {
	switch p {
	case "active":
		return computev1alpha1.SigningKeyActive
	case "validationOnly":
		return computev1alpha1.SigningKeyValidationOnly
	case "removing":
		return computev1alpha1.SigningKeyRemoving
	default:
		panic(fmt.Sprintf("unmappable TLA+ key phase %q", p))
	}
}

// goRotationPhaseToTLA is the inverse.
func goRotationPhaseToTLA(p computev1alpha1.SigningKeyPhase) string {
	switch p {
	case computev1alpha1.SigningKeyActive:
		return "active"
	case computev1alpha1.SigningKeyValidationOnly:
		return "validationOnly"
	case computev1alpha1.SigningKeyRemoving:
		return "removing"
	default:
		return fmt.Sprintf("<unknown:%s>", p)
	}
}

// rotationTLASim is one materialized TLA+ state: the Instance plus the client
// holding its Secrets, Certificates, and engines.
type rotationTLASim struct {
	instance *computev1alpha1.FireboltInstance
	cli      client.Client
	scheme   *runtime.Scheme
	// kids is how many key slots the model has, i.e. MaxGen+1.
	kids int
}

// materializeTLARotationState builds a rotationTLASim matching s.
//
// Certificates are pre-created and pre-marked Ready and their Secrets seeded for
// every key slot, so applySigningCertificate reports ready on the first call.
// That lets one stepSigningKeyRotation perform MintStart and MintReady together,
// which is exactly why the model's closure is transitive.
func materializeTLARotationState(t *testing.T, s tlaRotationState) *rotationTLASim {
	t.Helper()
	sch := authTestScheme(t)
	auth := validAuthSpecForController()
	rotationInterval := metav1.Duration{Duration: time.Hour}
	retainDuration := metav1.Duration{Duration: time.Hour}
	auth.Local.SigningKeys.RotationInterval = &rotationInterval
	auth.Local.SigningKeys.RetainDuration = &retainDuration

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: rotationTLAInstance, Namespace: rotationTLANamespace},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: auth},
	}

	past := metav1.NewTime(time.Now().Add(-48 * time.Hour))
	now := metav1.Now()

	// The model bounds rotations at MaxGen. The Go code has no such bound, so the
	// honest analog of "MintStart is disabled because gen = MaxGen" is "no
	// rotation is due yet": leave the Active key freshly created. Below MaxGen the
	// Active key is backdated so a rotation IS due, matching MintStart's guard.
	activeCreatedAt := past
	if s.Gen >= s.maxGen() {
		activeCreatedAt = now
	}

	var keys []computev1alpha1.SigningKeyStatus
	for i, k := range s.Keys {
		if k.Phase == "absent" {
			continue
		}
		kid := signingKeyID(i + 1)
		key := computev1alpha1.SigningKeyStatus{
			ID:         kid,
			SecretName: signingCertificateName(rotationTLAInstance, kid),
			Phase:      tlaRotationPhaseToGo(k.Phase),
			Algorithm:  "ECDSA",
			Size:       384,
			CreatedAt:  past,
		}
		if key.Phase == computev1alpha1.SigningKeyActive {
			key.CreatedAt = activeCreatedAt
		}
		if k.Demoted {
			d := past
			key.DemotedAt = &d
		}
		if k.Anchored {
			// retainDone is "the window has elapsed": backdate the anchor past
			// RetainDuration. Otherwise anchor it now, so the window is still open.
			anchor := now
			if k.RetainDone {
				anchor = past
			}
			key.RetireEligibleAt = &anchor
		}
		keys = append(keys, key)
	}
	instance.Status.Auth = &computev1alpha1.AuthStatus{
		SigningKeys:          keys,
		SigningKeyGeneration: s.Gen + 1,
	}

	objs := []client.Object{adminSecretForConvergence()}
	for i := 1; i <= len(s.Keys); i++ {
		kid := signingKeyID(i)
		objs = append(objs, signingKeySecretFor(instance, kid), buildSigningCertificate(instance, kid))
	}
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&computev1alpha1.FireboltEngine{}, &certmanagerv1.Certificate{}).
		WithObjects(objs...).Build()
	for i := 1; i <= len(s.Keys); i++ {
		markSigningCertReady(t, cli, instance, signingKeyID(i))
	}

	sim := &rotationTLASim{instance: instance, cli: cli, scheme: sch, kids: len(s.Keys)}

	// Seed the fleet. Convergence is the only thing stepSigningKeyRotation reads
	// about engines, so a converged fleet gets the expected hash on every engine
	// and a diverged fleet gets one engine left behind. Engines are real objects
	// rather than zero of them, so the gate exercises the comparison instead of
	// passing vacuously.
	want := sim.expectedAuthHash(t)
	for i := range rotationTLAEngines {
		hash := want
		if !s.Converged && i == 0 {
			hash = "stale-hash-from-a-previous-generation"
		}
		eng := engineWithHash(fmt.Sprintf("e%d", i+1), rotationTLAInstance, hash)
		eng.Namespace = rotationTLANamespace
		if err := cli.Create(context.Background(), eng); err != nil {
			t.Fatalf("seeding engine: %v", err)
		}
		if err := cli.Status().Update(context.Background(), eng); err != nil {
			t.Fatalf("seeding engine status: %v", err)
		}
	}
	return sim
}

// maxGen recovers the model's MaxGen bound from the number of key slots, since
// Kids == 1..(MaxGen+1).
func (s tlaRotationState) maxGen() int { return len(s.Keys) - 1 }

// expectedAuthHash recomputes the hash every engine must report to count as
// converged, folding in the same admin ResourceVersion and signing-key
// fingerprints that enginesConvergedOn and resolveInstanceInfo read.
func (m *rotationTLASim) expectedAuthHash(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	renderKeys := signingKeysForRender(m.instance.Status.Auth.SigningKeys)

	var admin corev1.Secret
	if err := m.cli.Get(ctx, client.ObjectKey{
		Namespace: rotationTLANamespace, Name: adminSecretNameForController,
	}, &admin); err != nil {
		t.Fatalf("reading admin secret: %v", err)
	}
	fps := make(map[string]string, len(renderKeys))
	for _, k := range renderKeys {
		fp, err := signingKeyFingerprint(ctx, m.cli, rotationTLANamespace, k.SecretName)
		if err != nil {
			t.Fatalf("fingerprinting %s: %v", k.SecretName, err)
		}
		fps[k.ID] = fp
	}
	return authHash(&ResolvedAuthInfo{
		Spec:                   m.instance.Spec.Auth,
		SigningKeys:            renderKeys,
		AdminSecretVersion:     admin.ResourceVersion,
		SigningKeyFingerprints: fps,
	})
}

// project extracts the TLA+ observable variables back out of the sim.
//
// Converged is recomputed rather than carried over: a reconciler step that
// changes what is rendered (a mint appending a key, a removal dropping one) also
// changes the hash engines must match, so a fleet that was converged before the
// step may not be after it — exactly as in the model, where reconciler actions
// leave `observed` unchanged while RenderedConfig moves.
func (m *rotationTLASim) project(t *testing.T) tlaRotationState {
	t.Helper()
	out := tlaRotationState{
		Keys: make([]tlaRotationKey, m.kids),
		Gen:  m.instance.Status.Auth.SigningKeyGeneration - 1,
	}
	for i := range out.Keys {
		out.Keys[i] = tlaRotationKey{Phase: "absent"}
	}
	for _, k := range m.instance.Status.Auth.SigningKeys {
		idx := -1
		for i := 1; i <= m.kids; i++ {
			if signingKeyID(i) == k.ID {
				idx = i - 1
				break
			}
		}
		if idx < 0 {
			t.Fatalf("key %q is outside the model's %d key slots", k.ID, m.kids)
		}
		out.Keys[idx] = tlaRotationKey{
			Phase:      goRotationPhaseToTLA(k.Phase),
			Demoted:    k.DemotedAt != nil,
			Anchored:   k.RetireEligibleAt != nil,
			RetainDone: k.RetireEligibleAt != nil && time.Since(k.RetireEligibleAt.Time) >= time.Hour,
		}
	}

	var list computev1alpha1.FireboltEngineList
	if err := m.cli.List(context.Background(), &list, client.InNamespace(rotationTLANamespace)); err != nil {
		t.Fatalf("listing engines: %v", err)
	}
	want := m.expectedAuthHash(t)
	out.Converged = true
	for i := range list.Items {
		if list.Items[i].Status.ObservedAuthHash != want {
			out.Converged = false
		}
	}
	return out
}

func rotationStateEqual(a, b tlaRotationState) bool {
	if a.Gen != b.Gen || a.Converged != b.Converged || len(a.Keys) != len(b.Keys) {
		return false
	}
	for i := range a.Keys {
		if a.Keys[i] != b.Keys[i] {
			return false
		}
	}
	return true
}

func rotationClosureContains(closureIDs []int, actual tlaRotationState) bool {
	for _, id := range closureIDs {
		if rotationStateEqual(tlaRotationStatePool[id], actual) {
			return true
		}
	}
	return false
}

// tlaRotationInvariants re-checks the spec's safety invariants against the Go
// state, so a violation is caught here too and not only in TLC.
func tlaRotationInvariants(t *testing.T, m *rotationTLASim) {
	t.Helper()
	keys := m.instance.Status.Auth.SigningKeys

	active := 0
	for _, k := range keys {
		if k.Phase == computev1alpha1.SigningKeyActive {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("AtMostOneActive violated: %d Active keys in %+v", active, keys)
	}

	// A demoted key must remain renderable until it is dropped, and the Active
	// key must always be rendered — the Go-side reading of NoValidationGap's
	// premise that whatever is signed can be validated.
	rendered := signingKeysForRender(keys)
	foundActive := false
	for _, k := range rendered {
		if k.Phase == computev1alpha1.SigningKeyActive {
			foundActive = true
		}
	}
	if !foundActive {
		t.Fatalf("the Active key is not rendered, so no engine could validate what it signs: %+v", rendered)
	}
}

func TestTLARotationStateCover(t *testing.T) {
	for i := range tlaRotationStateCases {
		tc := tlaRotationStateCases[i]
		start := tlaRotationStatePool[tc.Start]
		name := fmt.Sprintf("case-%02d/gen=%d/converged=%t", i, start.Gen, start.Converged)
		t.Run(name, func(t *testing.T) {
			m := materializeTLARotationState(t, start)

			// Guard the fixture itself: if materialization does not reproduce the
			// starting state, every closure assertion below is meaningless.
			if got := m.project(t); !rotationStateEqual(got, start) {
				t.Fatalf("materialization does not round-trip\n  want: %+v\n  got:  %+v", start, got)
			}

			r := &FireboltInstanceReconciler{Client: m.cli, Scheme: m.scheme}
			if err := r.stepSigningKeyRotation(context.Background(), m.instance); err != nil {
				t.Fatalf("stepSigningKeyRotation: %v", err)
			}

			tlaRotationInvariants(t, m)

			actual := m.project(t)
			if !rotationClosureContains(tc.Closure, actual) {
				t.Fatalf("result not in the TLA+ reconciler closure of the starting state\n  start:  %+v\n  actual: %+v\n  closure (%d states):\n%s",
					start, actual, len(tc.Closure), formatRotationClosure(tc.Closure))
			}
		})
	}
	t.Logf("rotation state cover: ran %d cases", len(tlaRotationStateCases))
}

func formatRotationClosure(closureIDs []int) string {
	out := ""
	for _, id := range closureIDs {
		out += fmt.Sprintf("    [pool %d] %+v\n", id, tlaRotationStatePool[id])
	}
	return out
}
