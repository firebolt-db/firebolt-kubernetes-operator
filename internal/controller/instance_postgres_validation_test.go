/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	computev1alpha1 "github.com/firebolt-db/firebolt-kubernetes-operator/api/v1alpha1"
)

// Defense-in-depth admission patterns on
// spec.metadata.postgres.{host,database,schema}. The controller XML-escapes
// these fields before rendering the pensieve config; these specs lock in
// that the CRD also rejects XML metacharacters at admission time so a
// malformed CR never reaches the controller.
var _ = Describe("FireboltInstance external postgres admission validation", func() {
	const ns = "default"

	mkInstance := func(name string, pg *computev1alpha1.PostgresSpec) *computev1alpha1.FireboltInstance {
		return &computev1alpha1.FireboltInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: computev1alpha1.FireboltInstanceSpec{
				Metadata: computev1alpha1.MetadataSpec{
					Postgres: pg,
				},
			},
		}
	}

	tryCreate := func(inst *computev1alpha1.FireboltInstance) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := k8sClient.Create(ctx, inst)
		if err == nil {
			// Don't leak resources between specs.
			_ = k8sClient.Delete(context.Background(), inst)
		}
		return err
	}

	validPG := func() *computev1alpha1.PostgresSpec {
		return &computev1alpha1.PostgresSpec{
			Host:                 "pg.example.com",
			Database:             "firebolt",
			Schema:               "public",
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
		}
	}

	It("accepts a typical DNS hostname / database / schema", func() {
		Expect(tryCreate(mkInstance("pg-ok", validPG()))).To(Succeed())
	})

	It("accepts an IPv4 host", func() {
		pg := validPG()
		pg.Host = "10.0.0.1"
		Expect(tryCreate(mkInstance("pg-ipv4", pg))).To(Succeed())
	})

	It("accepts an IPv6 host with brackets", func() {
		pg := validPG()
		pg.Host = "[2001:db8::1]"
		Expect(tryCreate(mkInstance("pg-ipv6", pg))).To(Succeed())
	})

	It("rejects an XML-injection host", func() {
		pg := validPG()
		pg.Host = `evil</host><port>9999</port><host>attacker.example`
		err := tryCreate(mkInstance("pg-bad-host", pg))
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("spec.metadata.postgres.host"))
	})

	It("rejects whitespace in the host", func() {
		pg := validPG()
		pg.Host = "pg example.com"
		err := tryCreate(mkInstance("pg-host-space", pg))
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("spec.metadata.postgres.host"))
	})

	It("rejects a database name containing XML metacharacters", func() {
		pg := validPG()
		pg.Database = `db&name`
		err := tryCreate(mkInstance("pg-bad-db", pg))
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("spec.metadata.postgres.database"))
	})

	It("rejects a schema name containing XML metacharacters", func() {
		pg := validPG()
		pg.Schema = `s"chema'`
		err := tryCreate(mkInstance("pg-bad-schema", pg))
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("spec.metadata.postgres.schema"))
	})

	It("rejects a host that exceeds the DNS length limit", func() {
		pg := validPG()
		pg.Host = strings.Repeat("a", 254)
		err := tryCreate(mkInstance("pg-host-long", pg))
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("spec.metadata.postgres.host"))
	})
})
