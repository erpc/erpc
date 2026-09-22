package data

import (
	"context"
	"testing"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBlockNumberNetwork satisfies common.Network but only implements Id; Get
// touches nothing else on the network.
type fakeBlockNumberNetwork struct{ common.Network }

func (fakeBlockNumberNetwork) Id() string { return "evm:1" }

// fakeBlockNumberClient satisfies clients.GrpcBdsClient but only implements
// SendRequest; that is the sole client method Get/fetchTaggedBlock invoke.
type fakeBlockNumberClient struct {
	clients.GrpcBdsClient
	resp *common.NormalizedResponse
	err  error
}

func (f *fakeBlockNumberClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return f.resp, f.err
}

func newBlockNumberConnector(t *testing.T, cli clients.GrpcBdsClient) *GrpcConnector {
	t.Helper()
	lg := zerolog.Nop()
	return &GrpcConnector{
		id:                 "test-grpc",
		logger:             &lg,
		clientByNetwork:    map[string]clients.GrpcBdsClient{"evm:1": cli},
		earliestByNetwork:  map[string]uint64{},
		latestByNetwork:    map[string]uint64{},
		finalizedByNetwork: map[string]uint64{},
		latestTsByNetwork:  map[string]int64{},
		initializer:        &util.Initializer{},
	}
}

func blockNumberRequest(t *testing.T) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	req.SetNetwork(fakeBlockNumberNetwork{})
	return req
}

func blockResponse(t *testing.T, resultJSON string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`1`), []byte(resultJSON), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

// eth_blockNumber is translated to getBlockByNumber("latest") and returns just
// the block number as a quoted, canonical (lowercase, no leading zeros) hex
// string.
func TestGrpcConnector_EthBlockNumber_TranslatesFromLatest(t *testing.T) {
	cli := &fakeBlockNumberClient{
		resp: blockResponse(t, `{"number":"0x1a2b3c","timestamp":"0x6500"}`),
	}
	g := newBlockNumberConnector(t, cli)

	out, err := g.Get(context.Background(), "idx", "pk", "rk", blockNumberRequest(t))

	require.NoError(t, err)
	assert.Equal(t, `"0x1a2b3c"`, string(out))
}

// A zero latest block must serialize as "0x0", not "0x" or "0x00".
func TestGrpcConnector_EthBlockNumber_Zero(t *testing.T) {
	cli := &fakeBlockNumberClient{resp: blockResponse(t, `{"number":"0x0"}`)}
	g := newBlockNumberConnector(t, cli)

	out, err := g.Get(context.Background(), "idx", "pk", "rk", blockNumberRequest(t))

	require.NoError(t, err)
	assert.Equal(t, `"0x0"`, string(out))
}

// Hex with leading zeros from the reader is normalized through the uint64
// round-trip, so the eth_blockNumber result is always canonical.
func TestGrpcConnector_EthBlockNumber_NormalizesHex(t *testing.T) {
	cli := &fakeBlockNumberClient{resp: blockResponse(t, `{"number":"0x00ff"}`)}
	g := newBlockNumberConnector(t, cli)

	out, err := g.Get(context.Background(), "idx", "pk", "rk", blockNumberRequest(t))

	require.NoError(t, err)
	assert.Equal(t, `"0xff"`, string(out))
}

// When the latest block cannot be read, the connector reports a miss so the
// request falls through to upstreams rather than surfacing a bogus number.
func TestGrpcConnector_EthBlockNumber_LatestUnavailable(t *testing.T) {
	cli := &fakeBlockNumberClient{err: assert.AnError}
	g := newBlockNumberConnector(t, cli)

	out, err := g.Get(context.Background(), "idx", "pk", "rk", blockNumberRequest(t))

	assert.Nil(t, out)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "expected record-not-found, got %v", err)
}

// A latest-block response with no parsable number is also treated as a miss.
func TestGrpcConnector_EthBlockNumber_NoNumberField(t *testing.T) {
	cli := &fakeBlockNumberClient{resp: blockResponse(t, `{"timestamp":"0x6500"}`)}
	g := newBlockNumberConnector(t, cli)

	out, err := g.Get(context.Background(), "idx", "pk", "rk", blockNumberRequest(t))

	assert.Nil(t, out)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "expected record-not-found, got %v", err)
}
