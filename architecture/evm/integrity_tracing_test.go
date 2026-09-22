package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestIntegrity_DetailedTracingRecordsValues proves detailed tracing pins the
// actual mismatch values (verbatim, no redaction) so an operator can see exactly
// which field was wrong; simple mode keeps the outcome but drops the per-check
// detail.
func TestIntegrity_DetailedTracingRecordsValues(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	common.SetTracerProviderForTest(tp)
	t.Cleanup(func() { common.IsTracingEnabled = false; common.IsTracingDetailed = false })

	res := integrity.Result{
		Err:             errors.New(`integrity check "transactionsRootRecompute" failed: transactionsRoot 0xAAA does not match recomputed 0xBBB`),
		RejectedCheckID: "transactionsRootRecompute",
		Outcomes:        []integrity.CheckOutcome{{CheckID: "transactionsRootRecompute", Outcome: "reject"}},
	}

	t.Run("detailed mode records the actual values as an event", func(t *testing.T) {
		exp.Reset()
		common.IsTracingDetailed = true

		_, span := common.StartSpan(context.Background(), "Integrity.Validate")
		annotateIntegritySpan(span, res)
		span.End()

		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		s := spans[0]

		attrs := map[string]string{}
		for _, a := range s.Attributes {
			attrs[string(a.Key)] = a.Value.AsString()
		}
		assert.Equal(t, "reject", attrs["integrity.outcome"])
		assert.Equal(t, "transactionsRootRecompute", attrs["integrity.rejected_check"])

		require.Len(t, s.Events, 1, "detailed mode emits the violation event")
		ev := s.Events[0]
		assert.Equal(t, "integrity.reject", ev.Name)
		evAttrs := map[string]string{}
		for _, a := range ev.Attributes {
			evAttrs[string(a.Key)] = a.Value.AsString()
		}
		assert.Contains(t, evAttrs["reason"], "0xAAA", "the claimed value, unredacted")
		assert.Contains(t, evAttrs["reason"], "0xBBB", "the recomputed value, unredacted")
	})

	t.Run("the rejected response body is captured for a by-hand sanity check", func(t *testing.T) {
		exp.Reset()
		common.IsTracingDetailed = true

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xabf61f02a6c77b28a9465a2256e26d2fe25714b60bb8edabb7d0ce794fba932e"]}`))
		jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), receiptResultBytes(underflowedLogIndexes), nil)
		rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)

		_, err := HandleUpstreamPostForward(context.Background(), indexMagnitudeNetwork(), common.NewFakeUpstream("test-upstream"), req, rs, nil, false)
		require.Error(t, err)

		var s *tracetest.SpanStub
		spans := exp.GetSpans()
		for i := range spans {
			if spans[i].Name == "Integrity.Validate" {
				s = &spans[i]
			}
		}
		require.NotNil(t, s, "the hook emits an Integrity.Validate span")
		attrs := map[string]string{}
		for _, a := range s.Attributes {
			attrs[string(a.Key)] = a.Value.AsString()
		}
		assert.Equal(t, "reject", attrs["integrity.outcome"])
		assert.Contains(t, attrs["integrity.response"], "0xfffffff7",
			"the actual rejected response body is recorded so it can be inspected by hand")
	})

	t.Run("simple mode keeps the outcome but drops per-check detail", func(t *testing.T) {
		exp.Reset()
		common.IsTracingDetailed = false

		_, span := common.StartSpan(context.Background(), "Integrity.Validate")
		annotateIntegritySpan(span, res)
		span.End()

		spans := exp.GetSpans()
		require.Len(t, spans, 1)
		s := spans[0]
		attrs := map[string]string{}
		for _, a := range s.Attributes {
			attrs[string(a.Key)] = a.Value.AsString()
		}
		assert.Equal(t, "reject", attrs["integrity.outcome"], "outcome is still present in simple mode")
		assert.Empty(t, s.Events, "simple mode does not emit per-check value events")
	})
}
