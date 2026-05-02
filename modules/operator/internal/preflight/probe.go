/*
Copyright 2026 egoavara.
SPDX-License-Identifier: MIT
*/

package preflight

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ProbeOutcome categorizes the live-probe result. Stable enum suitable for
// display in a TUI report.
type ProbeOutcome string

const (
	// OutcomeAccepted: an HTTPRoute with parentRef to a Service was
	// accepted by some controller. GAMMA works.
	OutcomeAccepted ProbeOutcome = "accepted"
	// OutcomeRejected: a controller picked it up but rejected it.
	OutcomeRejected ProbeOutcome = "rejected"
	// OutcomeNoController: no controller wrote any status before timeout.
	// Usually means no GAMMA-aware mesh is installed.
	OutcomeNoController ProbeOutcome = "no-controller"
	// OutcomeCRDMissing: the HTTPRoute CRD is not installed at all.
	OutcomeCRDMissing ProbeOutcome = "crd-missing"
	// OutcomeProbeError: setup failed (e.g. could not create probe resources).
	OutcomeProbeError ProbeOutcome = "probe-error"
)

// ProbeReport is the public, TUI-friendly result of one verification run.
type ProbeReport struct {
	Mesh           MeshInfo
	Outcome        ProbeOutcome
	ControllerName string // populated when Outcome=Accepted/Rejected
	Reason         string // condition reason (Rejected only)
	Message        string // condition message
	Err            error  // populated when Outcome=ProbeError
}

// Probe runs a one-shot GAMMA capability check against the cluster
// reachable via c. It creates a dummy Service + HTTPRoute in `namespace`
// (which must already exist), waits for any controller to set Accepted,
// and cleans up afterwards.
//
// scheme must include corev1 + sigs.k8s.io/gateway-api/apis/v1.
func Probe(ctx context.Context, c client.Client, scheme *runtime.Scheme, namespace string, timeout time.Duration) ProbeReport {
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}

	report := ProbeReport{Mesh: DetectMesh(ctx, c)}

	// Reuse the runner's liveProbe by constructing a minimal Runner; we
	// don't need recorder/pod since we never call report().
	r := &Runner{
		scheme: scheme,
		opts: Options{
			Namespace: namespace,
			Timeout:   timeout,
		},
	}

	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res := r.liveProbe(probeCtx, c)

	switch {
	case res.crdMissing:
		report.Outcome = OutcomeCRDMissing
	case res.createErr != nil:
		report.Outcome = OutcomeProbeError
		report.Err = res.createErr
	case res.timedOut:
		report.Outcome = OutcomeNoController
	case res.accepted:
		report.Outcome = OutcomeAccepted
		report.ControllerName = res.controllerName
		report.Message = res.message
	default:
		report.Outcome = OutcomeRejected
		report.ControllerName = res.controllerName
		report.Reason = res.reason
		report.Message = res.message
	}
	return report
}

// ErrCRDMissing is a sentinel returned from helpers that need to surface
// a missing-CRD condition as an error rather than as a ProbeOutcome.
var ErrCRDMissing = errors.New("HTTPRoute CRD not installed")
