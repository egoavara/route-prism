/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

const (
	LabelManagedBy = "route-prism.egoavara.net/managed-by"
	LabelKind      = "route-prism.egoavara.net/kind"
	LabelOwner     = "route-prism.egoavara.net/owner"

	ManagedByValue = "route-prism"

	KindContextRoute       = "contextroute"
	KindEdgeTransformation = "edgetransformation"

	FieldOwnerCR = "route-prism-cr"
	FieldOwnerET = "route-prism-et"

	FinalizerCR = "route-prism.egoavara.net/contextroute-finalizer"
)

func httpRouteNameForCR(svc string) string { return "route-prism-cr-" + svc }
func httpRouteNameForET(svc string) string { return "route-prism-et-" + svc }
func translatorName(et string) string      { return "route-prism-translator-" + et }
