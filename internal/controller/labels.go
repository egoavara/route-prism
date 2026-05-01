/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package controller

const (
	LabelManagedBy = "route-prism.egoavara.net/managed-by"
	LabelKind      = "route-prism.egoavara.net/kind"
	LabelOwner     = "route-prism.egoavara.net/owner"

	ManagedByValue = "route-prism"

	KindContextRoute       = "contextroute"
	KindEdgeTransformation = "edgetransformation"
	KindRemoteRoute        = "remoteroute"

	FieldOwnerCR = "route-prism-cr"
	FieldOwnerET = "route-prism-et"
	FieldOwnerRR = "route-prism-rr"

	FinalizerCR = "route-prism.egoavara.net/contextroute-finalizer"
)

func httpRouteNameForCR(svc string) string { return "route-prism-cr-" + svc }
func httpRouteNameForET(svc string) string { return "route-prism-et-" + svc }
func translatorName(et string) string      { return "route-prism-translator-" + et }
func remoteProxyName(rr string) string     { return "route-prism-remote-" + rr }
