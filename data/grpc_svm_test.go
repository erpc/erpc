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

// fakeSvmNetwork satisfies common.Network but only implements Id; Get touches
// nothing else on the network.
type fakeSvmNetwork struct {
	common.Network
	id string
}

func (f fakeSvmNetwork) Id() string { return f.id }

// recordingClient satisfies clients.GrpcBdsClient but only implements
// SendRequest; that is the sole client method Get/fetchTaggedBlock invoke. It
// records the methods it was asked for so tests can assert which reads the
// connector actually issued.
type recordingClient struct {
	clients.GrpcBdsClient
	methods []string
	resp    *common.NormalizedResponse
	err     error
}

func (r *recordingClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	m, _ := req.Method()
	r.methods = append(r.methods, m)
	return r.resp, r.err
}

func newSvmConnector(t *testing.T, clientsByNetwork map[string]clients.GrpcBdsClient) *GrpcConnector {
	t.Helper()
	lg := zerolog.Nop()
	return &GrpcConnector{
		id:                 "test-grpc",
		logger:             &lg,
		clientByNetwork:    clientsByNetwork,
		earliestByNetwork:  map[string]uint64{},
		latestByNetwork:    map[string]uint64{},
		finalizedByNetwork: map[string]uint64{},
		latestTsByNetwork:  map[string]int64{},
		initializer:        &util.Initializer{},
	}
}

func svmRequest(t *testing.T, networkId, method, params string) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`,
	))
	req.SetNetwork(fakeSvmNetwork{id: networkId})
	return req
}

// TestGrpcConnectorSupportedMethodsGatesReads exercises the allowlist through
// its only observable effect: whether Get consults the backing reader at all.
// A method that is not listed is fast-skipped as (nil, nil) — deliberately not
// ErrRecordNotFound — and never reaches the client.
//
// Both directions matter. Solana's getBlock must now be served, and the
// pre-existing eth_* entries must still be, because the allowlist is a single
// map: replacing it rather than extending it would silently disable EVM
// caching wholesale while every EVM test that stubs the client still passed.
func TestGrpcConnectorSupportedMethodsGatesReads(t *testing.T) {
	tests := []struct {
		name      string
		networkId string
		method    string
		params    string
		served    bool
	}{
		{name: "svm getBlock is served", networkId: "svm:solana-mainnet", method: "getBlock", params: `[42]`, served: true},
		{name: "eth_getBlockByNumber still served", networkId: "evm:1", method: "eth_getBlockByNumber", params: `["0x1",false]`, served: true},
		{name: "eth_getBlockByHash still served", networkId: "evm:1", method: "eth_getBlockByHash", params: `["0xabc",false]`, served: true},
		{name: "eth_getLogs still served", networkId: "evm:1", method: "eth_getLogs", params: `[{}]`, served: true},
		{name: "eth_getTransactionByHash still served", networkId: "evm:1", method: "eth_getTransactionByHash", params: `["0xabc"]`, served: true},
		{name: "eth_getTransactionReceipt still served", networkId: "evm:1", method: "eth_getTransactionReceipt", params: `["0xabc"]`, served: true},
		{name: "eth_getBlockReceipts still served", networkId: "evm:1", method: "eth_getBlockReceipts", params: `["0x1"]`, served: true},
		{name: "eth_chainId still served", networkId: "evm:1", method: "eth_chainId", params: `[]`, served: true},
		{name: "unlisted svm method is fast-skipped", networkId: "svm:solana-mainnet", method: "getTransaction", params: `["sig"]`, served: false},
		{name: "unlisted evm method is fast-skipped", networkId: "evm:1", method: "eth_call", params: `[{}]`, served: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cli := &recordingClient{resp: blockResponse(t, `{"blockhash":"abc"}`)}
			g := newSvmConnector(t, map[string]clients.GrpcBdsClient{tc.networkId: cli})

			out, err := g.Get(context.Background(), "idx", "pk", "rk",
				svmRequest(t, tc.networkId, tc.method, tc.params))

			if tc.served {
				require.NoError(t, err)
				assert.Equal(t, []string{tc.method}, cli.methods,
					"a supported method must be read through to the backing reader")
				assert.NotEmpty(t, out)
				return
			}

			require.NoError(t, err, "an unsupported method is a fast skip, not an error")
			assert.Nil(t, out)
			assert.Empty(t, cli.methods, "an unsupported method must not cost a round trip")
		})
	}
}

// TestGrpcConnectorSvmGetBlockReturnsResultBytes: the connector hands back the
// reader's result verbatim. getBlock's result is an object, and any rewriting
// or re-encoding here would change the shape the cache stores and replays.
func TestGrpcConnectorSvmGetBlockReturnsResultBytes(t *testing.T) {
	const result = `{"blockhash":"5vJ","parentSlot":41,"blockHeight":null,"blockTime":null}`
	cli := &recordingClient{resp: blockResponse(t, result)}
	g := newSvmConnector(t, map[string]clients.GrpcBdsClient{"svm:solana-mainnet": cli})

	out, err := g.Get(context.Background(), "idx", "pk", "rk",
		svmRequest(t, "svm:solana-mainnet", "getBlock", `[42,{"encoding":"json"}]`))

	require.NoError(t, err)
	assert.JSONEq(t, result, string(out))
}

// TestGrpcConnectorSvmGetBlockUnknownNetworkIsMiss: an svm: network with no
// client configured must report a miss, not fall into the EVM path or reuse
// another network's client.
func TestGrpcConnectorSvmGetBlockUnknownNetworkIsMiss(t *testing.T) {
	cli := &recordingClient{resp: blockResponse(t, `{"blockhash":"abc"}`)}
	g := newSvmConnector(t, map[string]clients.GrpcBdsClient{"svm:solana-mainnet": cli})

	out, err := g.Get(context.Background(), "idx", "pk", "rk",
		svmRequest(t, "svm:solana-devnet", "getBlock", `[42]`))

	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got: %v", err)
	assert.Nil(t, out)
	assert.Empty(t, cli.methods, "another network's client must not answer")
}

// TestGrpcConnectorSvmNetworksAreNotHeadPolled: the head poller speaks EVM —
// fetchTaggedBlock issues eth_getBlockByNumber for earliest/latest/finalized,
// which a Solana BDS server answers Unimplemented. Polling an svm: network
// would burn three round trips per interval forever for values that stay
// unknown either way, and it would score Unimplemented errors against a
// perfectly healthy reader. EVM networks alongside it must keep being polled.
func TestGrpcConnectorSvmNetworksAreNotHeadPolled(t *testing.T) {
	evmCli := &recordingClient{resp: blockResponse(t, `{"number":"0x10","timestamp":"0x6500"}`)}
	svmCli := &recordingClient{resp: blockResponse(t, `{"number":"0x10","timestamp":"0x6500"}`)}
	g := newSvmConnector(t, map[string]clients.GrpcBdsClient{
		"evm:1":              evmCli,
		"svm:solana-mainnet": svmCli,
	})

	g.pollBlockHeadsOnce(context.Background())

	assert.Empty(t, svmCli.methods,
		"an svm: network must not receive EVM tag reads it can only answer Unimplemented")
	assert.Equal(t, []string{"eth_getBlockByNumber", "eth_getBlockByNumber", "eth_getBlockByNumber"},
		evmCli.methods, "evm networks must still be polled for earliest/latest/finalized")

	// The skipped network keeps no head state, which is what makes
	// CacheLatestBlockTimestamp report "unknown" so the realtime age guard
	// fails open instead of judging SVM by an EVM block's timestamp.
	_, known := g.CacheLatestBlockTimestamp("svm:solana-mainnet")
	assert.False(t, known)
	assert.Equal(t, uint64(0), g.earliestByNetwork["svm:solana-mainnet"])

	latestTs, known := g.CacheLatestBlockTimestamp("evm:1")
	assert.True(t, known)
	assert.Equal(t, int64(0x6500), latestTs)
	assert.Equal(t, uint64(0x10), g.latestByNetwork["evm:1"])
}
