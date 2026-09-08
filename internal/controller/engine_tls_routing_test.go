// Copyright 2026 Firebolt Analytics
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"reflect"
	"testing"

	certmanagerv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
	"github.com/firebolt-db/firebolt-kubernetes-operator/internal/routing"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func routingTLSCertificate(engine *computev1alpha1.FireboltEngine, generation int) *certmanagerv1.Certificate {
	return buildGenEngineTLSCertificate(engine.Name, engine.Namespace, generation, &ResolvedEngineTLSInfo{
		CertManager: &computev1alpha1.CertManagerSpec{IssuerRef: computev1alpha1.CertManagerIssuerRef{Name: "issuer"}, Algorithm: "ECDSA", Size: 384},
	})
}

func TestEngineTLSRoutingAuthorityIdentity(t *testing.T) {
	var previousAuthority string
	for _, identity := range []struct {
		uid        types.UID
		generation int
	}{{"original", 0}, {"original", 1}, {"recreated", 1}} {
		engine := &computev1alpha1.FireboltEngine{ObjectMeta: metav1.ObjectMeta{Name: "query", Namespace: "work", UID: identity.uid}}
		certificate := routingTLSCertificate(engine, identity.generation)
		if err := bindEngineTLSRoutingAuthority(engine, certificate); err != nil {
			t.Fatal(err)
		}
		authority := routing.GenerationServiceName(string(identity.uid), identity.generation) + ".work.svc.cluster.local"
		leaf := &x509.Certificate{DNSNames: certificate.Spec.DNSNames}
		if err := leaf.VerifyHostname(authority); err != nil {
			t.Fatalf("generation authority missing from engine certificate: %v", err)
		}
		if previousAuthority != "" && leaf.VerifyHostname(previousAuthority) == nil {
			t.Fatalf("new identity certificate still authenticates previous authority %s", previousAuthority)
		}
		previousAuthority = authority
		before := certificate.DeepCopy()
		if err := bindEngineTLSRoutingAuthority(engine, certificate); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, certificate) {
			t.Fatal("binding the same identity changed the certificate again")
		}
	}
}

func TestEnsureEngineTLSCertAppliesBoundAuthority(t *testing.T) {
	for _, tt := range []struct {
		name       string
		uid        types.UID
		generation string
		valid      bool
	}{
		{name: "initial generation", uid: "engine-uid", generation: "0", valid: true},
		{name: "missing UID", generation: "0"},
		{name: "missing generation", uid: "engine-uid"},
		{name: "negative generation", uid: "engine-uid", generation: "-1"},
		{name: "malformed generation", uid: "engine-uid", generation: "garbled"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			engine := &computev1alpha1.FireboltEngine{ObjectMeta: metav1.ObjectMeta{Name: "query", Namespace: "work", UID: tt.uid}}
			certificate := routingTLSCertificate(engine, 0)
			certificate.Labels[LabelGeneration] = tt.generation
			var applied *certmanagerv1.Certificate
			scheme := authTestScheme(t)
			cli := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Apply: func(_ context.Context, _ client.WithWatch, configuration runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
					data, err := json.Marshal(configuration)
					if err != nil {
						return err
					}
					applied = &certmanagerv1.Certificate{}
					return json.Unmarshal(data, applied)
				},
			}).Build()
			reconciler := &FireboltEngineReconciler{Client: cli, Scheme: scheme}
			err := reconciler.ensureEngineTLSCert(t.Context(), engine, certificate)
			if !tt.valid {
				if err == nil || applied != nil {
					t.Fatalf("malformed identity reached apply: error=%v applied=%v", err, applied != nil)
				}
				return
			}
			if err != nil || applied == nil {
				t.Fatalf("certificate apply failed: error=%v applied=%v", err, applied != nil)
			}
			authority := routing.GenerationServiceName(string(engine.UID), 0) + ".work.svc.cluster.local"
			if err := (&x509.Certificate{DNSNames: applied.Spec.DNSNames}).VerifyHostname(authority); err != nil {
				t.Fatalf("applied certificate lacks immutable routing authority: %v", err)
			}
			owner := metav1.GetControllerOf(applied)
			if owner == nil || owner.UID != engine.UID {
				t.Fatalf("applied routing certificate has wrong owner: %v", owner)
			}
		})
	}
}
