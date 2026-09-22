package clients

import (
	"context"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestSendSingleRequest_SpanAttributes(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	common.SetTracerProviderForTest(tp)
	t.Cleanup(func() { common.IsTracingEnabled = false })

	util.ResetGock()
	defer util.ResetGock()
	gock.New("http://rpc1.localhost:8545").
		Post("/").
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

	logger := zerolog.Nop()
	ups := common.NewFakeUpstream("quicknode-mainnet")
	ups.Config().Type = common.UpstreamTypeEvm

	client, err := NewGenericHttpJsonRpcClient(
		context.Background(),
		&logger,
		"prj1",
		ups,
		&url.URL{Scheme: "http", Host: "rpc1.localhost:8545"},
		ups.Config().JsonRpc,
		nil,
		&noopErrorExtractor{},
	)
	require.NoError(t, err)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	_, _ = client.SendRequest(context.Background(), req)

	require.NoError(t, tp.ForceFlush(context.Background()))

	var found *tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == "HttpJsonRpcClient.sendSingleRequest" {
			s := s
			found = &s
			break
		}
	}
	require.NotNil(t, found, "HttpJsonRpcClient.sendSingleRequest span not emitted")

	assert.Equal(t, trace.SpanKindClient, found.SpanKind, "span.kind must be client for Datadog dependency edges")

	attrMap := make(map[string]string, len(found.Attributes))
	for _, a := range found.Attributes {
		attrMap[string(a.Key)] = a.Value.AsString()
	}
	assert.Equal(t, "rpc1.localhost", attrMap["peer.service"], "peer.service must be the upstream hostname")
}
