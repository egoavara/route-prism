/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	routeprismv1alpha1 "github.com/egoavara/route-prism/api/v1alpha1"
)

var _ = Describe("EdgeTransformation Controller", func() {
	const ns = "default"

	makeService := func(name string, labels map[string]string) *corev1.Service {
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
			Spec: corev1.ServiceSpec{
				Selector: labels,
				Ports:    []corev1.ServicePort{{Port: 80, Protocol: corev1.ProtocolTCP}},
			},
		}
	}

	makeEndpointSlice := func(svcName, podIP string) *discoveryv1.EndpointSlice {
		port := int32(80)
		ready := true
		return &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      svcName + "-test",
				Namespace: ns,
				Labels:    map[string]string{discoveryv1.LabelServiceName: svcName},
			},
			AddressType: discoveryv1.AddressTypeIPv4,
			Ports:       []discoveryv1.EndpointPort{{Port: &port}},
			Endpoints: []discoveryv1.Endpoint{{
				Addresses:  []string{podIP},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			}},
		}
	}

	cleanup := func() {
		_ = k8sClient.DeleteAllOf(ctx, &routeprismv1alpha1.EdgeTransformation{}, client.InNamespace(ns))
		_ = k8sClient.DeleteAllOf(ctx, &gwv1.HTTPRoute{}, client.InNamespace(ns))
		_ = k8sClient.DeleteAllOf(ctx, &corev1.Service{}, client.InNamespace(ns))
		_ = k8sClient.DeleteAllOf(ctx, &appsv1.Deployment{}, client.InNamespace(ns))
		_ = k8sClient.DeleteAllOf(ctx, &corev1.ConfigMap{}, client.InNamespace(ns))
		_ = k8sClient.DeleteAllOf(ctx, &discoveryv1.EndpointSlice{}, client.InNamespace(ns))
		Eventually(func() int {
			var l routeprismv1alpha1.EdgeTransformationList
			_ = k8sClient.List(ctx, &l, client.InNamespace(ns))
			return len(l.Items)
		}, 5*time.Second, 100*time.Millisecond).Should(Equal(0))
	}
	AfterEach(cleanup)

	It("provisions translator + applies HTTPRoute on the target Service", func() {
		Expect(k8sClient.Create(ctx, makeService("web", map[string]string{"app": "web"}))).To(Succeed())
		Expect(k8sClient.Create(ctx, makeEndpointSlice("web", "10.0.0.10"))).To(Succeed())

		et := &routeprismv1alpha1.EdgeTransformation{
			ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: ns},
			Spec: routeprismv1alpha1.EdgeTransformationSpec{
				Mode:         routeprismv1alpha1.EdgeModeRouter,
				SourceCookie: "x-route-prism",
				Target: routeprismv1alpha1.TargetReference{
					Service: routeprismv1alpha1.ServiceReference{Name: "web"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, et)).To(Succeed())

		r := &EdgeTransformationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "demo"}})
		Expect(err).NotTo(HaveOccurred())

		var dep appsv1.Deployment
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "route-prism-translator-demo"}, &dep)).To(Succeed())

		var cm corev1.ConfigMap
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "route-prism-translator-demo"}, &cm)).To(Succeed())
		// Smart-mode template embeds the CR's effective routing key
		// (default "<ns>.<target>") and the source cookie name into the
		// lua block. There is no longer a "checked" loop-prevention
		// marker — the translator forwards to a Pod IP directly so it
		// can't re-enter the target Service's HTTPRoute.
		Expect(cm.Data["nginx.conf"]).To(ContainSubstring(`"default.web"`))
		Expect(cm.Data["nginx.conf"]).To(ContainSubstring(`"x-route-prism"`))

		var hr gwv1.HTTPRoute
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "route-prism-et-web"}, &hr)).To(Succeed())
		// Single catch-all → translator. The translator forwards directly
		// to a Pod IP so no second-hop loop rule is needed.
		Expect(hr.Spec.Rules).To(HaveLen(1))
		Expect(hr.Spec.Rules[0].Matches[0].Headers).To(BeEmpty())
		Expect(string(hr.Spec.Rules[0].BackendRefs[0].Name)).To(Equal("route-prism-translator-demo"))
	})

	It("rejects unimplemented modes via status condition", func() {
		Expect(k8sClient.Create(ctx, makeService("web", map[string]string{"app": "web"}))).To(Succeed())
		et := &routeprismv1alpha1.EdgeTransformation{
			ObjectMeta: metav1.ObjectMeta{Name: "lua", Namespace: ns},
			Spec: routeprismv1alpha1.EdgeTransformationSpec{
				Mode:         routeprismv1alpha1.EdgeModeEnvoyLua,
				SourceCookie: "x",
				Target:       routeprismv1alpha1.TargetReference{Service: routeprismv1alpha1.ServiceReference{Name: "web"}},
			},
		}
		Expect(k8sClient.Create(ctx, et)).To(Succeed())

		r := &EdgeTransformationReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "lua"}})
		Expect(err).NotTo(HaveOccurred())

		var got routeprismv1alpha1.EdgeTransformation
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "lua"}, &got)).To(Succeed())
		var ready *metav1.Condition
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == "Ready" {
				ready = &got.Status.Conditions[i]
			}
		}
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal("ModeNotImplemented"))
	})
})
