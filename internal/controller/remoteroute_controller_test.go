/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Smoke tests only — full reconcile coverage is tracked in a follow-up
// task that needs an Envoy admin double for /clusters polling. For now we
// just verify the reconciler can be constructed and gracefully handles a
// non-existent resource.
var _ = Describe("RemoteRoute Controller", func() {
	Context("When reconciling a non-existent resource", func() {
		It("should return an empty result without error", func() {
			r := &RemoteRouteReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
			}
			ctx := context.Background()
			res, err := r.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: "default",
					Name:      "no-such-rr",
				},
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(BeZero())
		})
	})
})
