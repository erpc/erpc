package upstream

import (
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StartQueryStream records one native eth_query* stream sent to u, as
// tryForward records a request, so selection and the network's adaptive
// timeout learn eth_query* like any other method. The returned func is called
// once: with nil at the stream's first page, which records the latency to
// it, or with the error that ended the stream before that page.
func (u *Upstream) StartQueryStream(nrq *common.NormalizedRequest, method string) func(err error) {
	// A query range spans finalized and unfinalized blocks.
	finality := common.DataFinalityStateUnknown
	cfg := u.Config()
	u.metricsTracker.RecordUpstreamRequest(u, method, finality)
	telemetry.CounterHandle(telemetry.MetricUpstreamRequestTotal,
		u.ProjectId,
		u.VendorName(),
		u.NetworkLabel(),
		cfg.Id,
		method,
		"1",
		nrq.CompositeType(),
		finality.String(),
		nrq.UserId(),
		nrq.AgentName()).Inc()
	timer := u.metricsTracker.RecordUpstreamDurationStart(u, method, nrq.CompositeType(), finality, nrq.UserId())

	return func(err error) {
		if err == nil {
			timer.ObserveDuration(true)
			return
		}
		code := status.Code(err)
		switch code {
		case codes.Canceled:
			// The client left: no verdict on u.
			timer.ObserveCancelled()
			return
		case codes.InvalidArgument, codes.OutOfRange, codes.NotFound,
			codes.FailedPrecondition, codes.ResourceExhausted, codes.Unimplemented:
			// The request, a range u does not hold, or a MIP-16 limit:
			// shaped by the request, not by u's health.
			timer.ObserveDuration(false)
			return
		}
		timer.ObserveDuration(false)
		u.metricsTracker.RecordUpstreamFailure(u, method, finality, err)
		telemetry.CounterHandle(telemetry.MetricUpstreamErrorTotal,
			u.ProjectId,
			u.VendorName(),
			u.NetworkLabel(),
			cfg.Id,
			method,
			// The gRPC code, not the message: messages carry addresses and
			// block numbers, which would make a series per request.
			"grpc_"+code.String(),
			string(common.ClassifySeverity(err)),
			nrq.CompositeType(),
			finality.String(),
			nrq.UserId(),
			nrq.AgentName()).Inc()
	}
}
