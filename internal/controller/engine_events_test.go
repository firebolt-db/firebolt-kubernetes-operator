/*
Copyright 2026 Firebolt Analytics.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
)

// These specs cover the live I/O path that surfaces a StatefulSet's most
// recent Warning event onto the FireboltEngine Ready condition. The pure
// decoration logic is covered by table tests in
// engine_controller_test.go; here we just prove that:
//  1. the field selector (involvedObject.uid + type=Warning) actually
//     reaches the apiserver and returns the right Event;
//  2. multiple events on the same STS resolve to the most recent one;
//  3. an STS with no Warning events returns nil without erroring.
var _ = Describe("FireboltEngineReconciler.latestStatefulSetWarning", func() {
	const ns = "default"

	var (
		ctx       context.Context
		cancel    context.CancelFunc
		clientset *kubernetes.Clientset
		r         *FireboltEngineReconciler
	)

	BeforeEach(func() {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)

		var err error
		clientset, err = kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		r = &FireboltEngineReconciler{
			Client:    k8sClient,
			Clientset: clientset,
		}
	})

	AfterEach(func() {
		cancel()
	})

	// makeSTS materializes a minimal but valid StatefulSet so the apiserver
	// assigns it a UID, which the event lookup keys on.
	makeStatefulSet := func(name string) *appsv1.StatefulSet {
		var one int32 = 1
		labels := map[string]string{"app": name}
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1.StatefulSetSpec{
				Replicas: &one,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Name:  "c",
							Image: "busybox",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("16Mi"),
								},
							},
						}},
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, sts)).To(Succeed())
		return sts
	}

	// emitEvent posts an Event with the supplied type/reason/message
	// targeting sts. We use unique generateName values per call so the
	// apiserver does not collapse them via the legacy aggregator (which
	// would defeat ordering-by-timestamp).
	emitEvent := func(sts *appsv1.StatefulSet, evType, reason, message string, at time.Time) {
		ev := &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: sts.Name + ".",
				Namespace:    ns,
			},
			InvolvedObject: corev1.ObjectReference{
				APIVersion: "apps/v1",
				Kind:       "StatefulSet",
				Name:       sts.Name,
				Namespace:  ns,
				UID:        sts.UID,
			},
			Reason:         reason,
			Message:        message,
			Type:           evType,
			FirstTimestamp: metav1.NewTime(at),
			LastTimestamp:  metav1.NewTime(at),
			Source:         corev1.EventSource{Component: "statefulset-controller"},
			Count:          1,
		}
		Expect(k8sClient.Create(ctx, ev)).To(Succeed())
	}

	It("returns the most recent Warning event for the StatefulSet", func() {
		sts := makeStatefulSet("sts-" + utilrand.String(8))
		now := time.Now()
		emitEvent(sts, "Warning", "FailedCreate", "old failure", now.Add(-2*time.Minute))
		emitEvent(sts, "Warning", "FailedCreate", "new failure", now)
		emitEvent(sts, "Normal", "SuccessfulCreate", "ignored", now.Add(time.Minute))

		// Eventually because envtest's apiserver indexes events
		// asynchronously; the field selector may take a moment to see
		// the writes we just made.
		Eventually(func(g Gomega) {
			got := r.latestStatefulSetWarning(ctx, sts)
			g.Expect(got).NotTo(BeNil())
			g.Expect(got.Message).To(Equal("new failure"))
			g.Expect(got.Type).To(Equal(corev1.EventTypeWarning))
		}, "10s", "200ms").Should(Succeed())
	})

	It("returns nil when the StatefulSet has no Warning events", func() {
		sts := makeStatefulSet("sts-" + utilrand.String(8))
		Consistently(func() *corev1.Event {
			return r.latestStatefulSetWarning(ctx, sts)
		}, "1s", "200ms").Should(BeNil())
	})

	It("returns nil when Clientset is unset (drain-style graceful degradation)", func() {
		sts := makeStatefulSet("sts-" + utilrand.String(8))
		bare := &FireboltEngineReconciler{Client: k8sClient}
		Expect(bare.latestStatefulSetWarning(ctx, sts)).To(BeNil())
	})

	It("scopes events to the right StatefulSet (UID filter, not name)", func() {
		stsA := makeStatefulSet("sts-a-" + utilrand.String(8))
		stsB := makeStatefulSet("sts-b-" + utilrand.String(8))
		now := time.Now()
		emitEvent(stsA, "Warning", "FailedCreate", "for A", now)
		emitEvent(stsB, "Warning", "FailedCreate", "for B", now)

		Eventually(func(g Gomega) {
			got := r.latestStatefulSetWarning(ctx, stsA)
			g.Expect(got).NotTo(BeNil())
			g.Expect(got.Message).To(Equal("for A"))
		}, "10s", "200ms").Should(Succeed())
	})

	It("returns the latest warning for an STS named with the operator's gen suffix", func() {
		// Exercise the real STS naming convention (<engine>-g<gen>) so
		// a future rename of genResourceName forces us back here.
		name := genResourceName("e-"+utilrand.String(6), 0, "")
		sts := makeStatefulSet(name)
		emitEvent(sts, "Warning", "FailedCreate", "schema-checked", time.Now())
		Eventually(func(g Gomega) {
			got := r.latestStatefulSetWarning(ctx, sts)
			g.Expect(got).NotTo(BeNil())
			g.Expect(got.Message).To(Equal("schema-checked"))
		}, "10s", "200ms").Should(Succeed())
	})
})
