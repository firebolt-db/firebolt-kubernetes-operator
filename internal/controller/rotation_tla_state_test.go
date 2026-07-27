/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

// Deterministic exhaustive state-cover testing for the signing-key rotation
// state machine, mirroring the engine_tla_state_test.go and
// instance_tla_state_test.go harnesses for the third spec.
//
// For every reachable state in the TLC state graph of
// formal/SigningKeyRotation.tla (regenerated via `make formal-gen`), this
// test materializes the state against a fake client — Status.Auth signing
// keys with the timestamps the state implies, their cert-manager
// Certificates/Secrets, and two FireboltEngines whose ObservedAuthHash is
// computed from their observed render exactly as resolveInstanceInfo
// would — runs one ensureSigningKeys call, and verifies that the resulting
// state lies in the model's reconciler closure of the starting state.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// rotationTLAWindow is both the RotationInterval and RetainDuration the
// harness configures. The model's rotationDue / retainElapsed booleans are
// materialized as timestamps 2x this window in the past (elapsed) or now
// (not elapsed); a reconcile takes milliseconds, so neither side of the
// window can flip mid-test.
const rotationTLAWindow = time.Hour

// signingKeyGenOf parses the generation number out of a signing key ID
// ("signing-<N>"), the inverse of signingKeyID.
func signingKeyGenOf(t *testing.T, id string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimPrefix(id, "signing-"))
	if err != nil {
		t.Fatalf("unparseable signing key ID %q: %v", id, err)
	}
	return n
}

// rotationTLAAuthSpec is validAuthSpecForController with the rotation policy
// enabled, since every modeled behavior runs under RotationInterval +
// RetainDuration.
func rotationTLAAuthSpec() *computev1alpha1.AuthSpec {
	auth := validAuthSpecForController()
	auth.Local.SigningKeys.RotationInterval = &metav1.Duration{Duration: rotationTLAWindow}
	auth.Local.SigningKeys.RetainDuration = &metav1.Duration{Duration: rotationTLAWindow}
	return auth
}

// rotationSim carries everything one state-cover case needs across
// materialization, reconcile, and projection.
type rotationSim struct {
	cli      client.Client
	r        *FireboltInstanceReconciler
	instance *computev1alpha1.FireboltInstance
	start    tlaRotationState
}

// signingKeyStatusFor builds the SigningKeyStatus entry for one key
// generation of the harness instance ("inst") with the given
// phase-position timestamps.
func signingKeyStatusFor(gen int, phase computev1alpha1.SigningKeyPhase, createdAt time.Time) computev1alpha1.SigningKeyStatus {
	id := signingKeyID(gen)
	return computev1alpha1.SigningKeyStatus{
		ID:         id,
		SecretName: signingCertificateName("inst", id),
		CreatedAt:  metav1.NewTime(createdAt),
		Phase:      phase,
	}
}

// materializeTLARotationState constructs the fake-client world for one TLA+
// state: instance status, Secrets and ready Certificates for every key the
// state says exists, the minting Certificate when one is outstanding, and
// two engines carrying the ObservedAuthHash of their observed render.
func materializeTLARotationState(t *testing.T, s tlaRotationState) *rotationSim {
	t.Helper()
	ctx := context.Background()
	now := time.Now()

	sch := authTestScheme(t)
	cli := fake.NewClientBuilder().WithScheme(sch).
		WithStatusSubresource(&certmanagerv1.Certificate{}).Build()
	r := &FireboltInstanceReconciler{Client: cli, Scheme: sch}

	auth := rotationTLAAuthSpec()

	// The Active key: rotationDue is materialized through its CreatedAt,
	// exactly the anchor stepSigningKeyRotation consults.
	activeCreated := now
	if s.RotationDue {
		activeCreated = now.Add(-2 * rotationTLAWindow)
	}
	keys := []computev1alpha1.SigningKeyStatus{
		signingKeyStatusFor(s.ActiveKey, computev1alpha1.SigningKeyActive, activeCreated),
	}
	generation := s.ActiveKey

	minting := false
	switch s.OtherState {
	case "none":
	case "minting":
		// Certificate applied but the key is not in Status yet, so the
		// generation counter has not been bumped either.
		minting = true
	case "pending":
		keys = append(keys,
			signingKeyStatusFor(s.OtherKey, computev1alpha1.SigningKeyValidationOnly, now))
		generation = s.OtherKey
	case "demoted", "retiring", "removing":
		// The post-promotion key: demoted at promotion time, retire-eligible
		// once every engine confirmed the promotion, Removing once the
		// retain window elapsed.
		phase := computev1alpha1.SigningKeyValidationOnly
		if s.OtherState == "removing" {
			phase = computev1alpha1.SigningKeyRemoving
		}
		other := signingKeyStatusFor(s.OtherKey, phase, now.Add(-3*rotationTLAWindow))
		demoted := metav1.NewTime(now)
		other.DemotedAt = &demoted
		if s.OtherState != "demoted" {
			retireAt := now
			if s.RetainElapsed {
				retireAt = now.Add(-2 * rotationTLAWindow)
			}
			eligible := metav1.NewTime(retireAt)
			other.RetireEligibleAt = &eligible
		}
		keys = append(keys, other)
	default:
		t.Fatalf("unknown TLA+ otherState %q", s.OtherState)
	}

	instance := &computev1alpha1.FireboltInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns-1"},
		Spec:       computev1alpha1.FireboltInstanceSpec{Auth: auth},
		Status: computev1alpha1.FireboltInstanceStatus{
			Auth: &computev1alpha1.AuthStatus{
				SigningKeyGeneration: generation,
				SigningKeys:          keys,
			},
		},
	}

	if err := cli.Create(ctx, adminSecretForConvergence()); err != nil {
		t.Fatalf("seeding admin secret: %v", err)
	}
	// Every key tracked in Status has an issued Secret by construction
	// (mintNextSigningKey appends only once ready; deletion drops the entry).
	for _, k := range keys {
		if err := cli.Create(ctx, signingKeySecretFor(instance, k.ID)); err != nil {
			t.Fatalf("seeding secret for %s: %v", k.ID, err)
		}
	}

	// Prime: apply the Certificates for every Status-tracked key, then mark
	// them Ready. The priming reconcile itself cannot step the rotation:
	// the Active key's Certificate is not Ready until marked below.
	if _, err := r.ensureSigningKeys(ctx, instance); err != nil {
		t.Fatalf("priming reconcile: %v", err)
	}
	for _, k := range keys {
		if k.Phase == computev1alpha1.SigningKeyRemoving {
			// ensureSigningKeys does not apply a Removing key's Certificate;
			// deleteSigningKey is not-found-tolerant, so none is needed.
			continue
		}
		markSigningCertReady(t, cli, instance, k.ID)
	}

	if minting {
		mintID := signingKeyID(s.OtherKey)
		cert := buildSigningCertificate(instance, mintID)
		if err := cli.Create(ctx, cert); err != nil {
			t.Fatalf("seeding minting certificate: %v", err)
		}
		if s.CertReady {
			if err := cli.Create(ctx, signingKeySecretFor(instance, mintID)); err != nil {
				t.Fatalf("seeding minting secret: %v", err)
			}
			markSigningCertReady(t, cli, instance, mintID)
		}
	}

	// Engines: ObservedAuthHash computed from each engine's observed render
	// with the same inputs enginesConvergedOn folds in (admin Secret RV,
	// per-key public-key fingerprints read from the seeded Secrets).
	var adminSecret corev1.Secret
	if err := cli.Get(ctx, types.NamespacedName{Namespace: "ns-1", Name: adminSecretNameForController}, &adminSecret); err != nil {
		t.Fatalf("reading admin secret RV: %v", err)
	}
	for i, obs := range s.EngineObs {
		hash := rotationObservedHash(ctx, t, cli, instance, auth, adminSecret.ResourceVersion, obs)
		engine := &computev1alpha1.FireboltEngine{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("eng-%d", i+1), Namespace: "ns-1"},
			Spec:       computev1alpha1.FireboltEngineSpec{InstanceRef: "inst"},
			Status:     computev1alpha1.FireboltEngineStatus{ObservedAuthHash: hash},
		}
		if err := cli.Create(ctx, engine); err != nil {
			t.Fatalf("seeding engine %d: %v", i+1, err)
		}
	}

	return &rotationSim{cli: cli, r: r, instance: instance, start: s}
}

// rotationObservedHash computes the ObservedAuthHash an engine that last
// rolled onto the given observed render would carry, mirroring
// resolveInstanceInfo's inputs: the auth spec, the observed key list in
// render order (signer first), the admin Secret's ResourceVersion, and each
// observed key's public-key fingerprint.
func rotationObservedHash(ctx context.Context, t *testing.T, cli client.Client, instance *computev1alpha1.FireboltInstance, auth *computev1alpha1.AuthSpec, adminRV string, obs tlaRotationObs) string {
	t.Helper()
	gens := []int{obs.Active}
	if obs.Other != 0 {
		gens = append(gens, obs.Other)
	}
	renderKeys := make([]computev1alpha1.SigningKeyStatus, 0, len(gens))
	fps := make(map[string]string, len(gens))
	for _, g := range gens {
		id := signingKeyID(g)
		secretName := signingCertificateName(instance.Name, id)
		var secret corev1.Secret
		if err := cli.Get(ctx, types.NamespacedName{Namespace: instance.Namespace, Name: secretName}, &secret); err != nil {
			t.Fatalf("observed key %s has no Secret (model invariant Inv_ObservedKeysExist): %v", id, err)
		}
		fp, err := signingKeyPublicKeyFingerprint(secret.Data[corev1.TLSCertKey])
		if err != nil {
			t.Fatalf("fingerprinting %s: %v", id, err)
		}
		renderKeys = append(renderKeys, computev1alpha1.SigningKeyStatus{ID: id, SecretName: secretName})
		fps[id] = fp
	}
	return authHash(&ResolvedAuthInfo{
		Spec:                   auth,
		SigningKeys:            renderKeys,
		AdminSecretVersion:     adminRV,
		SigningKeyFingerprints: fps,
	})
}

// projectTLARotationState extracts the TLA+ observable variables back out of
// the sim after a reconcile. EngineObs is passed through from the starting
// state: no reconciler action writes to FireboltEngine objects (asserted in
// tlaRotationInvariants), exactly as the model's reconciler actions leave
// engineObs unchanged.
func projectTLARotationState(t *testing.T, m *rotationSim) tlaRotationState {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	policy := m.instance.Spec.Auth.Local.SigningKeys

	keys := m.instance.Status.Auth.SigningKeys
	active := activeSigningKey(keys)
	if active == nil {
		t.Fatal("no Active signing key after reconcile")
	}

	out := tlaRotationState{
		ActiveKey: signingKeyGenOf(t, active.ID),
		EngineObs: m.start.EngineObs,
	}

	if other := otherSigningKey(keys); other != nil {
		out.OtherKey = signingKeyGenOf(t, other.ID)
		switch {
		case other.Phase == computev1alpha1.SigningKeyRemoving:
			out.OtherState = "removing"
		case other.DemotedAt == nil:
			out.OtherState = "pending"
		case other.RetireEligibleAt == nil:
			out.OtherState = "demoted"
		default:
			out.OtherState = "retiring"
		}
		out.CertReady = true
		out.RetainElapsed = other.RetireEligibleAt != nil &&
			now.After(other.RetireEligibleAt.Add(policy.RetainDuration.Duration))
	} else {
		out.OtherState = "none"
		// A mint in flight is visible only as the next generation's
		// Certificate: the key is deliberately kept out of Status until its
		// Secret is issued.
		nextGen := m.instance.Status.Auth.SigningKeyGeneration + 1
		nextID := signingKeyID(nextGen)
		certName := signingCertificateName(m.instance.Name, nextID)
		var cert certmanagerv1.Certificate
		err := m.cli.Get(ctx, types.NamespacedName{Namespace: m.instance.Namespace, Name: certName}, &cert)
		if err == nil {
			out.OtherState = "minting"
			out.OtherKey = nextGen
			var secret corev1.Secret
			serr := m.cli.Get(ctx, types.NamespacedName{Namespace: m.instance.Namespace, Name: certName}, &secret)
			out.CertReady = serr == nil && len(secret.Data[corev1.TLSPrivateKeyKey]) > 0 &&
				certificateReadyForCurrentGeneration(&cert)
		}
	}

	out.RotationDue = now.After(active.CreatedAt.Add(policy.RotationInterval.Duration)) &&
		(out.OtherState == "none" || out.OtherState == "minting" || out.OtherState == "pending")

	return out
}

// tlaRotationInvariants checks the Go-side counterparts of the model's
// Safety conjuncts after every reconcile.
func tlaRotationInvariants(t *testing.T, m *rotationSim) {
	t.Helper()
	ctx := context.Background()
	keys := m.instance.Status.Auth.SigningKeys

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

	// Inv_ObservedKeysExist: every key any engine's observed render still
	// references must still have its Secret — a violation means
	// deleteSigningKey ran before the fleet converged off the key.
	for _, obs := range m.start.EngineObs {
		gens := []int{obs.Active}
		if obs.Other != 0 {
			gens = append(gens, obs.Other)
		}
		for _, g := range gens {
			name := signingCertificateName(m.instance.Name, signingKeyID(g))
			var secret corev1.Secret
			if err := m.cli.Get(ctx, types.NamespacedName{Namespace: m.instance.Namespace, Name: name}, &secret); err != nil {
				t.Fatalf("engine still observes key %d but its Secret %s is gone: %v", g, name, err)
			}
		}
	}

	// Inv_SignerUniversallyValidatable over the (unchanged) engine
	// observations: every engine's signer is in every engine's key set.
	for _, signer := range m.start.EngineObs {
		for _, validator := range m.start.EngineObs {
			if signer.Active != validator.Active && signer.Active != validator.Other {
				t.Fatalf("engine signing with key %d is not validatable by engine holding {%d, %d}",
					signer.Active, validator.Active, validator.Other)
			}
		}
	}

	// The reconciler must never write to engine CRs.
	var engines computev1alpha1.FireboltEngineList
	if err := m.cli.List(ctx, &engines, client.InNamespace(m.instance.Namespace)); err != nil {
		t.Fatalf("listing engines: %v", err)
	}
	if len(engines.Items) != len(m.start.EngineObs) {
		t.Fatalf("engine count changed across reconcile: %d -> %d", len(m.start.EngineObs), len(engines.Items))
	}
}

// rotationClosureContains reports whether `actual` is one of the TLA+ states
// the model considers reachable from the test's starting state via 0+
// reconciler-only transitions.
func rotationClosureContains(closureIDs []int, actual tlaRotationState) bool {
	for _, id := range closureIDs {
		if tlaRotationStatePool[id] == actual {
			return true
		}
	}
	return false
}

func TestTLARotationStateCover(t *testing.T) {
	for i := range tlaRotationStateCases {
		tc := tlaRotationStateCases[i]
		start := tlaRotationStatePool[tc.Start]
		name := fmt.Sprintf("case-%02d/a=%d/o=%d-%s/due=%t/cert=%t/retain=%t",
			i, start.ActiveKey, start.OtherKey, start.OtherState,
			start.RotationDue, start.CertReady, start.RetainElapsed)
		t.Run(name, func(t *testing.T) {
			m := materializeTLARotationState(t, start)

			if _, err := m.r.ensureSigningKeys(context.Background(), m.instance); err != nil {
				t.Fatalf("ensureSigningKeys: %v", err)
			}

			tlaRotationInvariants(t, m)

			actual := projectTLARotationState(t, m)
			if !rotationClosureContains(tc.Closure, actual) {
				t.Fatalf("result not in TLA+ reconciler closure of starting state\n  start:    %+v\n  actual:   %+v\n  closure (%d states):\n%s",
					start, actual, len(tc.Closure), formatRotationClosure(tc.Closure))
			}
		})
	}
	t.Logf("rotation state cover: ran %d cases", len(tlaRotationStateCases))
}

// formatRotationClosure renders the first few entries of a closure index
// list for inclusion in a Fatalf message.
func formatRotationClosure(closureIDs []int) string {
	const limit = 8
	out := ""
	for i, id := range closureIDs {
		if i >= limit {
			out += fmt.Sprintf("    ... (%d more)\n", len(closureIDs)-limit)
			break
		}
		out += fmt.Sprintf("    [pool %d] %+v\n", id, tlaRotationStatePool[id])
	}
	return out
}
