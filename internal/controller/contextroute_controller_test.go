/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

var _ = Describe("ContextRoute Controller", func() {
	const ns = "default"

	reconcileN := func(r *ContextRouteReconciler, name string, n int) {
		for i := 0; i < n; i++ {
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	makeService := func(name string, labels map[string]string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports:    []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}},
			},
		}
	}

	cleanup := func() {
		_ = k8sClient.DeleteAllOf(ctx, &routeprismv1alpha1.ContextRoute{}, client.InNamespace(ns))
		r := &ContextRouteReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		Eventually(func() int {
			var l routeprismv1alpha1.ContextRouteList
			_ = k8sClient.List(ctx, &l, client.InNamespace(ns))
			for i := range l.Items {
				_, _ = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{
					Namespace: l.Items[i].Namespace, Name: l.Items[i].Name,
				}})
			}
			_ = k8sClient.List(ctx, &l, client.InNamespace(ns))
			return len(l.Items)
		}, 5*time.Second, 100*time.Millisecond).Should(Equal(0))
		_ = k8sClient.DeleteAllOf(ctx, &gwv1.HTTPRoute{}, client.InNamespace(ns))
		_ = k8sClient.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace(ns))
	}
	AfterEach(cleanup)

	It("emits one HTTPRoute on the target with rule per variant + catch-all", func() {
		Expect(k8sClient.Create(ctx, makeService("web", map[string]string{"app": "web", "variant": "stable"}))).To(Succeed())
		Expect(k8sClient.Create(ctx, makeService("web-canary", map[string]string{"app": "web", "variant": "canary"}))).To(Succeed())

		cr := &routeprismv1alpha1.ContextRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
			Spec: routeprismv1alpha1.ContextRouteSpec{
				Target: routeprismv1alpha1.TargetReference{
					Service: routeprismv1alpha1.ServiceReference{Name: "web"},
				},
				Variants: routeprismv1alpha1.VariantSelector{
					Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())

		r := &ContextRouteReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileN(r, "demo", 2)

		var hr gwv1.HTTPRoute
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "route-prism-cr-web"}, &hr)).To(Succeed())
		Expect(hr.Spec.ParentRefs).To(HaveLen(1))
		Expect(string(hr.Spec.ParentRefs[0].Name)).To(Equal("web"))
		Expect(hr.Spec.Rules).To(HaveLen(2)) // 1 variant + catch-all
		// Variant rule: header match → web-canary
		// Default routing key is "<ns>.<target-svc>". The CR sits in
		// `default` namespace per envtest setup → "default.web".
		Expect(hr.Spec.Rules[0].Matches[0].Headers[0].Value).To(ContainSubstring(`default\.web=web-canary`))
		Expect(string(hr.Spec.Rules[0].BackendRefs[0].Name)).To(Equal("web-canary"))
		// Catch-all → web (CRD defaults inject path:/ but no header match)
		catchAll := hr.Spec.Rules[1]
		Expect(catchAll.Matches).To(HaveLen(1))
		Expect(catchAll.Matches[0].Headers).To(BeEmpty())
		Expect(string(catchAll.BackendRefs[0].Name)).To(Equal("web"))

		var got routeprismv1alpha1.ContextRoute
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "demo"}, &got)).To(Succeed())
		Expect(got.Status.TargetService).To(Equal("web"))
		Expect(got.Status.VariantServices).To(ConsistOf("web-canary"))
		Expect(got.Status.Conflict).To(BeNil())
	})

	It("rejects via Conflict status when two CRs target the same Service", func() {
		Expect(k8sClient.Create(ctx, makeService("web", map[string]string{"app": "web"}))).To(Succeed())

		older := &routeprismv1alpha1.ContextRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "older", Namespace: ns},
			Spec: routeprismv1alpha1.ContextRouteSpec{
				Target:   routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: "web"}},
				Variants: routeprismv1alpha1.VariantSelector{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
			},
		}
		Expect(k8sClient.Create(ctx, older)).To(Succeed())
		time.Sleep(1100 * time.Millisecond)
		newer := &routeprismv1alpha1.ContextRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "newer", Namespace: ns},
			Spec: routeprismv1alpha1.ContextRouteSpec{
				Target:   routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: "web"}},
				Variants: routeprismv1alpha1.VariantSelector{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
			},
		}
		Expect(k8sClient.Create(ctx, newer)).To(Succeed())

		r := &ContextRouteReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileN(r, "older", 2)
		reconcileN(r, "newer", 2)

		var olderS, newerS routeprismv1alpha1.ContextRoute
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "older"}, &olderS)).To(Succeed())
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "newer"}, &newerS)).To(Succeed())
		Expect(olderS.Status.Conflict).To(BeNil())
		Expect(newerS.Status.Conflict).NotTo(BeNil())
		Expect(newerS.Status.Conflict.OwnedBy).To(Equal("older"))
	})

	It("removes the HTTPRoute on CR delete via finalizer", func() {
		Expect(k8sClient.Create(ctx, makeService("web", map[string]string{"app": "web"}))).To(Succeed())

		cr := &routeprismv1alpha1.ContextRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "to-delete", Namespace: ns},
			Spec: routeprismv1alpha1.ContextRouteSpec{
				Target:   routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: "web"}},
				Variants: routeprismv1alpha1.VariantSelector{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}},
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())

		r := &ContextRouteReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileN(r, "to-delete", 2)

		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
		reconcileN(r, "to-delete", 1)

		var hr gwv1.HTTPRoute
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "route-prism-cr-web"}, &hr)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
