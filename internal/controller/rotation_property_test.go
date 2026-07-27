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

// Stateful property tests for the signing-key rotation state machine,
// mirroring instance_property_test.go for the SigningKeyRotation spec.
//
// The TLA+ spec (formal/SigningKeyRotation.tla) fixes the engine fleet and
// abstracts time into per-cycle booleans; this sim drives the real
// ensureSigningKeys against a fake client under a strictly more adversarial
// environment — engines join and leave mid-rotation, and the rotation clock
// can become due at any point — while checking the same safety invariants:
// every engine's signer is validatable by every other engine, and no engine
// still observes a key whose material has been deleted.

import (
	"context"
	"fmt"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"pgregory.net/rapid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// rotationSimEngine is the sim's bookkeeping mirror of one engine: the
// render it last rolled onto, in render order (signer first). The engine CR
// itself carries only the opaque ObservedAuthHash of this list.
type rotationSimEngine struct {
	name string
	obs  []computev1alpha1.SigningKeyStatus
}

// rotationPropSim drives the rotation state machine end to end: the real
// ensureSigningKeys against a fake client, with the environment (cert
// issuance, engine rollouts, time passing, fleet churn) as rapid actions.
type rotationPropSim struct {
	t        *testing.T
	cli      client.Client
	r        *FireboltInstanceReconciler
	instance *computev1alpha1.FireboltInstance
	engines  []rotationSimEngine
	nextEng  int
	adminRV  string
}

func newRotationPropSim(t *testing.T) *rotationPropSim {
	t.Helper()
	ctx := context.Background()
	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&certmanagerv1.Certificate{}).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: rotationTLAAuthSpec()},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{
				SigningKeyGeneration: 1,
				SigningKeys: []computev1alpha1.SigningKeyStatus{
					signingKeyStatusFor(1, computev1alpha1.SigningKeyActive, time.Now()),
				},
			},
		},
	}

	if err := cli.Create(ctx, adminSecretForConvergence()); err != nil {
		t.Fatalf("seeding admin secret: %v", err)
	}
	if err := cli.Create(ctx, signingKeySecretFor(instance, AuthSigningKeyID)); err != nil {
		t.Fatalf("seeding bootstrap secret: %v", err)
	}
	// Prime: apply the bootstrap key's Certificate, then mark it Ready so
	// the rotation state machine is live from the first sim action.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("priming reconcile: %v", err)
	}
	markSigningCertReady(t, cli, instance, AuthSigningKeyID)

	var adminSecret corev1.Secret
	if err := cli.Get(ctx, types.NamespacedName{Namespace: "ns-1", Name: adminSecretNameForController}, &adminSecret); err != nil {
		t.Fatalf("reading admin secret RV: %v", err)
	}

	m := &rotationPropSim{t: t, cli: cli, r: r, instance: instance, adminRV: adminSecret.ResourceVersion}
	m.AddEngine(nil)
	m.AddEngine(nil)
	return m
}

// currentRender is the key list an engine rolling right now would observe.
func (m *rotationPropSim) currentRender() []computev1alpha1.SigningKeyStatus {
	return signingKeysForRender(m.instance.Status.Auth.SigningKeys)
}

// renderHash computes the ObservedAuthHash for an observed render, folding
// in the admin Secret RV and per-key fingerprints exactly as
// resolveInstanceInfo and enginesConvergedOn both do.
func (m *rotationPropSim) renderHash(obs []computev1alpha1.SigningKeyStatus) string {
	ctx := context.Background()
	fps := make(map[string]string, len(obs))
	renderKeys := make([]computev1alpha1.SigningKeyStatus, 0, len(obs))
	for _, k := range obs {
		var secret corev1.Secret
		if err := m.cli.Get(ctx, types.NamespacedName{Namespace: "ns-1", Name: k.SecretName}, &secret); err != nil {
			m.t.Fatalf("observed key %s has no Secret: %v", k.ID, err)
		}
		fp, err := signingKeyPublicKeyFingerprint(secret.Data[corev1.TLSCertKey])
		if err != nil {
			m.t.Fatalf("fingerprinting %s: %v", k.ID, err)
		}
		fps[k.ID] = fp
		renderKeys = append(renderKeys, computev1alpha1.SigningKeyStatus{ID: k.ID, SecretName: k.SecretName})
	}
	return authHash(&ResolvedAuthInfo{
		Spec:                   m.instance.Spec.Auth,
		SigningKeys:            renderKeys,
		AdminSecretVersion:     m.adminRV,
		SigningKeyFingerprints: fps,
	})
}

// ---------- State-machine actions ----------

// Reconcile runs one real ensureSigningKeys pass: apply Certificates,
// then advance at most one rotation step if its gate is satisfied.
func (m *rotationPropSim) Reconcile(_ *rapid.T) {
	if _, err := m.r.ensureSigningKeys(context.Background(), m.instance); err != nil {
		m.t.Fatalf("ensureSigningKeys: %v", err)
	}
}

// CertIssued mirrors EnvCertIssued: cert-manager issues the Secret for a
// mint the reconciler has started (the next generation's Certificate exists
// but its key is not yet tracked in Status).
func (m *rotationPropSim) CertIssued(_ *rapid.T) {
	ctx := context.Background()
	nextID := signingKeyID(m.instance.Status.Auth.SigningKeyGeneration + 1)
	certName := signingCertificateName("inst", nextID)
	var cert certmanagerv1.Certificate
	if err := m.cli.Get(ctx, types.NamespacedName{Namespace: "ns-1", Name: certName}, &cert); err != nil {
		return // no mint in flight
	}
	var secret corev1.Secret
	if err := m.cli.Get(ctx, types.NamespacedName{Namespace: "ns-1", Name: certName}, &secret); err == nil {
		return // already issued
	}
	if err := m.cli.Create(ctx, signingKeySecretFor(m.instance, nextID)); err != nil {
		m.t.Fatalf("issuing minted secret: %v", err)
	}
	markCertReadyForGeneration(m.t, m.cli, "ns-1", certName)
}

// EngineSync mirrors EnvEngineSync: one engine completes a blue-green
// rollout onto the current render.
func (m *rotationPropSim) EngineSync(t *rapid.T) {
	if len(m.engines) == 0 {
		return
	}
	i := rapid.IntRange(0, len(m.engines)-1).Draw(t, "engine")
	m.syncEngine(&m.engines[i])
}

func (m *rotationPropSim) syncEngine(e *rotationSimEngine) {
	ctx := context.Background()
	render := m.currentRender()
	e.obs = append([]computev1alpha1.SigningKeyStatus(nil), render...)

	var engine computev1alpha1.FireboltEngine
	if err := m.cli.Get(ctx, types.NamespacedName{Namespace: "ns-1", Name: e.name}, &engine); err != nil {
		m.t.Fatalf("getting engine %s: %v", e.name, err)
	}
	engine.Status.ObservedAuthHash = m.renderHash(e.obs)
	if err := m.cli.Update(ctx, &engine); err != nil {
		m.t.Fatalf("updating engine %s: %v", e.name, err)
	}
}

// RotationDue backdates the Active key's CreatedAt past the
// RotationInterval — time passing is legal at any point, which is strictly
// more adversarial than the model's between-rotations EnvRotationDue.
func (m *rotationPropSim) RotationDue(_ *rapid.T) {
	keys := m.instance.Status.Auth.SigningKeys
	if active := activeSigningKey(keys); active != nil {
		active.CreatedAt = metav1.NewTime(time.Now().Add(-2 * rotationTLAWindow))
	}
}

// RetainElapsed backdates a stamped RetireEligibleAt past the
// RetainDuration, mirroring EnvRetainElapsed.
func (m *rotationPropSim) RetainElapsed(_ *rapid.T) {
	keys := m.instance.Status.Auth.SigningKeys
	if other := otherSigningKey(keys); other != nil && other.RetireEligibleAt != nil {
		*other.RetireEligibleAt = metav1.NewTime(time.Now().Add(-2 * rotationTLAWindow))
	}
}

// AddEngine creates a new engine already rolled onto the current render — a
// freshly created engine resolves the current InstanceInfo before its first
// pod ever starts. Fleet churn is outside the fixed-fleet model; the gates
// must stay safe under it regardless.
func (m *rotationPropSim) AddEngine(_ *rapid.T) {
	ctx := context.Background()
	m.nextEng++
	e := rotationSimEngine{name: fmt.Sprintf("eng-%d", m.nextEng)}
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: e.name, Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst"},
	}
	if err := m.cli.Create(ctx, engine); err != nil {
		m.t.Fatalf("creating engine %s: %v", e.name, err)
	}
	m.engines = append(m.engines, e)
	m.syncEngine(&m.engines[len(m.engines)-1])
}

// RemoveEngine deletes one engine mid-flight; the convergence gates
// quantify over the remaining fleet.
func (m *rotationPropSim) RemoveEngine(t *rapid.T) {
	if len(m.engines) == 0 {
		return
	}
	i := rapid.IntRange(0, len(m.engines)-1).Draw(t, "engine")
	e := m.engines[i]
	engine := &computev1alpha1.FireboltEngine{
		ObjectMeta: metav1.ObjectMeta{Name: e.name, Namespace: "ns-1"},
	}
	if err := m.cli.Delete(context.Background(), engine); err != nil {
		m.t.Fatalf("deleting engine %s: %v", e.name, err)
	}
	m.engines = append(m.engines[:i], m.engines[i+1:]...)
}

// ---------- Invariants (mirror formal/SigningKeyRotation.tla Safety) ----------

// Check is called by rapid after every action.
func (m *rotationPropSim) Check(t *rapid.T) {
	ctx := context.Background()
	keys := m.instance.Status.Auth.SigningKeys

	// At most one non-Active key, exactly one Active key.
	if len(keys) > 2 {
		t.Fatalf("%d signing keys tracked; the rotation design allows at most 2", len(keys))
	}
	activeCount := 0
	for _, k := range keys {
		if k.Phase == computev1alpha1.SigningKeyActive {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Fatalf("%d Active signing keys, want exactly 1", activeCount)
	}

	// Inv_KeyDirection: a pre-promotion key is exactly the next generation;
	// a post-promotion key is strictly older than the Active key.
	active := activeSigningKey(keys)
	if other := otherSigningKey(keys); other != nil {
		aGen := signingKeyGenOf(m.t, active.ID)
		oGen := signingKeyGenOf(m.t, other.ID)
		if other.DemotedAt == nil && oGen != aGen+1 {
			t.Fatalf("pending key %d is not the Active key's (%d) successor", oGen, aGen)
		}
		if other.DemotedAt != nil && oGen >= aGen {
			t.Fatalf("demoted key %d is not older than the Active key %d", oGen, aGen)
		}
	}

	// Inv_ObservedKeysExist: no engine still observes a key whose Secret has
	// been deleted — a violation means deleteSigningKey ran before the fleet
	// converged off the key.
	for _, e := range m.engines {
		for _, k := range e.obs {
			var secret corev1.Secret
			if err := m.cli.Get(ctx, types.NamespacedName{Namespace: "ns-1", Name: k.SecretName}, &secret); err != nil {
				t.Fatalf("engine %s still observes key %s but its Secret is gone: %v", e.name, k.ID, err)
			}
		}
	}

	// Inv_SignerUniversallyValidatable: every engine signs with the first
	// key of its observed render; every other engine must hold that key.
	for _, signer := range m.engines {
		if len(signer.obs) == 0 {
			continue
		}
		signingID := signer.obs[0].ID
		for _, validator := range m.engines {
			ok := false
			for _, k := range validator.obs {
				if k.ID == signingID {
					ok = true
					break
				}
			}
			if !ok {
				t.Fatalf("engine %s signs with %s, which engine %s cannot validate (holds %d keys)",
					signer.name, signingID, validator.name, len(validator.obs))
			}
		}
	}
}

func TestRotationStateMachine(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		m := newRotationPropSim(t)
		rt.Repeat(rapid.StateMachineActions(m))
	})
}
