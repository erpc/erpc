package clients

import (
	"context"
	"errors"
	"testing"

	"github.com/blockchain-data-standards/manifesto/svm"
	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeSvmClient satisfies svm.RPCQueryServiceClient but only implements
// GetBlock; that is the sole method handleSvmGetBlock invokes. It records the
// svm.GetBlockRequest the handler built, which is the only place the
// JSON-RPC -> proto translation (slot, encoding guard, rewards inversion,
// transactionDetails/commitment mapping) is observable without a live server.
type fakeSvmClient struct {
	svm.RPCQueryServiceClient
	got   *svm.GetBlockRequest
	calls int
	resp  *svm.GetBlockResponse
	err   error
}

func (f *fakeSvmClient) GetBlock(ctx context.Context, in *svm.GetBlockRequest, opts ...grpc.CallOption) (*svm.GetBlockResponse, error) {
	f.calls++
	f.got = in
	return f.resp, f.err
}

// svmBlockPresent is a minimal servable block: enough for the handler to take
// the success path without depending on the manifesto's rendering (tested
// upstream).
func svmBlockPresent() *svm.GetBlockResponse {
	return &svm.GetBlockResponse{
		SlotStatus: svm.SlotStatus_SLOT_PRESENT,
		Block:      &svm.ConfirmedBlock{Slot: 42, ParentSlot: 41},
	}
}

// callSvmGetBlock drives handleSvmGetBlock over a raw JSON-RPC params array.
// It reaches into bdsConn because the parameter-building logic has no seam
// above it: SendRequest resolves a pooled, dialed connection first.
func callSvmGetBlock(t *testing.T, paramsJSON string, resp *svm.GetBlockResponse, grpcErr error) (*fakeSvmClient, *common.NormalizedResponse, error) {
	t.Helper()
	fake := &fakeSvmClient{resp: resp, err: grpcErr}
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":` + paramsJSON + `}`,
	))
	jrReq, err := req.JsonRpcRequest()
	require.NoError(t, err)

	c := &GenericGrpcBdsClient{}
	out, err := c.handleSvmGetBlock(context.Background(), &bdsConn{svmClient: fake}, req, jrReq)
	return fake, out, err
}

// TestSvmGetBlockEncodingGuard is the highest-value guard in this file. BDS
// returns a structured block, which renders as encoding "json" and nothing
// else. jsonParsed is not a formatting variant — it needs per-program decoders
// to turn opaque instruction data into named fields — and base58/base64 are a
// different payload shape entirely. Serving json for any of them returns a
// wrong-shaped result a client cannot distinguish from a correct one, so
// anything but json (or absent, which means json per Agave's default) must be
// refused as ErrEndpointUnsupported so the cache reports a miss and the
// request falls through to a live upstream.
func TestSvmGetBlockEncodingGuard(t *testing.T) {
	tests := []struct {
		name     string
		params   string
		servable bool
	}{
		{"no config object at all", `[42]`, true},
		{"null config object", `[42,null]`, true},
		{"empty config object", `[42,{}]`, true},
		{"config without an encoding key", `[42,{"transactionDetails":"full"}]`, true},
		{"explicit json", `[42,{"encoding":"json"}]`, true},
		{"jsonParsed needs decoders BDS has none of", `[42,{"encoding":"jsonParsed"}]`, false},
		{"base58", `[42,{"encoding":"base58"}]`, false},
		{"base64", `[42,{"encoding":"base64"}]`, false},
		{"base64+zstd", `[42,{"encoding":"base64+zstd"}]`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, out, err := callSvmGetBlock(t, tc.params, svmBlockPresent(), nil)

			if tc.servable {
				require.NoError(t, err)
				require.NotNil(t, out)
				assert.Equal(t, 1, fake.calls, "a servable encoding must reach the reader")
				return
			}

			require.Error(t, err, "serving json for a non-json encoding is silent data corruption")
			assert.ErrorIs(t, err, errSvmEncodingUnsupported)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
				"must be ErrEndpointUnsupported so the cache treats it as a miss and falls through; got: %v", err)
			assert.Nil(t, out)
			assert.Zero(t, fake.calls, "must not spend a read it cannot render")
		})
	}
}

// TestSvmGetBlockRewardsInversion pins the polarity flip between Agave's
// request field (`rewards`, defaulting to true) and the proto's
// (`excludeRewards`, proto3-defaulting to false). Inverting it either silently
// drops the rewards array from every response or asks for rewards the caller
// explicitly opted out of.
func TestSvmGetBlockRewardsInversion(t *testing.T) {
	tests := []struct {
		name            string
		params          string
		wantExcludeRwds bool
	}{
		{"rewards absent means Agave's true default", `[42,{}]`, false},
		{"rewards true", `[42,{"rewards":true}]`, false},
		{"rewards false", `[42,{"rewards":false}]`, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, _, err := callSvmGetBlock(t, tc.params, svmBlockPresent(), nil)
			require.NoError(t, err)
			require.NotNil(t, fake.got)
			assert.Equal(t, tc.wantExcludeRwds, fake.got.ExcludeRewards)
		})
	}
}

// TestSvmGetBlockRequestCarriesMappedFields checks the mapped values actually
// land in their own proto fields. The helpers below verify the mappings in
// isolation; this catches them being cross-wired or never called, which the
// isolated tables cannot see. The rows vary details and commitment together so
// a swap cannot pass.
func TestSvmGetBlockRequestCarriesMappedFields(t *testing.T) {
	tests := []struct {
		name           string
		params         string
		wantSlot       uint64
		wantDetails    svm.TransactionDetails
		wantCommitment svm.Commitment
	}{
		{
			name:           "signatures at confirmed",
			params:         `[7,{"transactionDetails":"signatures","commitment":"confirmed"}]`,
			wantSlot:       7,
			wantDetails:    svm.TransactionDetails_TRANSACTION_DETAILS_SIGNATURES,
			wantCommitment: svm.Commitment_COMMITMENT_CONFIRMED,
		},
		{
			name:           "none at processed is downgraded to finalized",
			params:         `[318000000,{"transactionDetails":"none","commitment":"processed"}]`,
			wantSlot:       318000000,
			wantDetails:    svm.TransactionDetails_TRANSACTION_DETAILS_NONE,
			wantCommitment: svm.Commitment_COMMITMENT_FINALIZED,
		},
		{
			name:           "bare slot defaults to full at finalized",
			params:         `[0]`,
			wantSlot:       0,
			wantDetails:    svm.TransactionDetails_TRANSACTION_DETAILS_FULL,
			wantCommitment: svm.Commitment_COMMITMENT_FINALIZED,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake, _, err := callSvmGetBlock(t, tc.params, svmBlockPresent(), nil)
			require.NoError(t, err)
			require.NotNil(t, fake.got)
			assert.Equal(t, tc.wantSlot, fake.got.Slot)
			assert.Equal(t, tc.wantDetails, fake.got.TransactionDetails)
			assert.Equal(t, tc.wantCommitment, fake.got.Commitment)
			assert.Nil(t, fake.got.GenesisHash,
				"the reader publishes no genesis hash; asserting one it cannot verify is refused as NOT_FOUND")
		})
	}
}

// TestSvmTransactionDetails covers the string -> enum mapping and, more
// importantly, the split between the two failure kinds: "accounts" is a shape
// BDS has no representation for and must fall through as unsupported, whereas
// a bogus value is a client error that must NOT be laundered into a benign
// cache miss.
func TestSvmTransactionDetails(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		want       svm.TransactionDetails
		wantErr    bool
		wantBenign bool // ErrEndpointUnsupported => fall through to a live upstream
	}{
		{name: "absent defaults to full", in: "", want: svm.TransactionDetails_TRANSACTION_DETAILS_FULL},
		{name: "full", in: "full", want: svm.TransactionDetails_TRANSACTION_DETAILS_FULL},
		{name: "signatures", in: "signatures", want: svm.TransactionDetails_TRANSACTION_DETAILS_SIGNATURES},
		{name: "none", in: "none", want: svm.TransactionDetails_TRANSACTION_DETAILS_NONE},
		{name: "accounts is unrepresentable and falls through", in: "accounts", wantErr: true, wantBenign: true},
		{name: "unknown value is a real error", in: "sigs", wantErr: true, wantBenign: false},
		{name: "casing is not normalized", in: "Full", wantErr: true, wantBenign: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svmTransactionDetails(tc.in)

			if !tc.wantErr {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
				return
			}

			require.Error(t, err)
			benign := common.HasErrorCode(err, common.ErrCodeEndpointUnsupported)
			assert.Equal(t, tc.wantBenign, benign,
				"unsupported (fall through) vs. invalid (real error) must not be conflated; got: %v", err)
			if tc.wantBenign {
				assert.ErrorIs(t, err, errSvmEncodingUnsupported)
			}
		})
	}
}

// TestSvmCommitment pins that only "confirmed" maps to CONFIRMED. Everything
// else, including "processed", resolves to FINALIZED: a finalized-sealed
// archive cannot honour "processed", and the caller gets the stricter level
// rather than a fresher-looking lie.
func TestSvmCommitment(t *testing.T) {
	tests := []struct {
		in   string
		want svm.Commitment
	}{
		{"confirmed", svm.Commitment_COMMITMENT_CONFIRMED},
		{"finalized", svm.Commitment_COMMITMENT_FINALIZED},
		{"processed", svm.Commitment_COMMITMENT_FINALIZED},
		{"", svm.Commitment_COMMITMENT_FINALIZED},
		{"Confirmed", svm.Commitment_COMMITMENT_FINALIZED},
	}

	for _, tc := range tests {
		t.Run("commitment="+tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, svmCommitment(tc.in))
		})
	}
}

// TestSvmParseSlot guards the decimal-only contract. Solana slots are never
// hex; accepting an "0x…" string here would be an EVM habit leaking in, and
// silently truncating a negative or fractional number would address the wrong
// slot.
func TestSvmParseSlot(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		want    uint64
		wantErr bool
	}{
		{name: "decimal number", params: `[318000000]`, want: 318000000},
		{name: "zero", params: `[0]`, want: 0},
		{name: "negative", params: `[-1]`, wantErr: true},
		{name: "fractional", params: `[42.5]`, wantErr: true},
		{name: "hex string is an EVM habit, not a slot", params: `["0x2a"]`, wantErr: true},
		{name: "decimal string is still a string", params: `["42"]`, wantErr: true},
		{name: "non-numeric", params: `["latest"]`, wantErr: true},
		{name: "null", params: `[null]`, wantErr: true},
		{name: "object", params: `[{"slot":42}]`, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Drive through the handler so the test exercises the same
			// decoding the wire path uses rather than a hand-built value.
			fake, _, err := callSvmGetBlock(t, tc.params, svmBlockPresent(), nil)

			if tc.wantErr {
				require.Error(t, err)
				assert.Zero(t, fake.calls, "an unparsable slot must not reach the reader")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, fake.got)
			assert.Equal(t, tc.want, fake.got.Slot)
		})
	}
}

// TestSvmGetBlockMissingSlotParam: getBlock without a slot is malformed, not a
// cache miss to fall through with a default slot of 0.
func TestSvmGetBlockMissingSlotParam(t *testing.T) {
	fake, out, err := callSvmGetBlock(t, `[]`, svmBlockPresent(), nil)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Zero(t, fake.calls)
}

// TestSvmMapGetBlockError pins which gRPC statuses are benign misses. The
// three benign codes mean "this reader cannot answer, another upstream can",
// so they must surface as ErrEndpointUnsupported and let the request fall
// through. Every other code is a transport or server failure that must stay a
// real error, or a broken backend would silently look like a cache miss
// forever and never be scored against the connector.
func TestSvmMapGetBlockError(t *testing.T) {
	tests := []struct {
		name       string
		in         error
		wantBenign bool
	}{
		{"OutOfRange is outside coverage", status.Error(codes.OutOfRange, "slot 1 below floor"), true},
		{"Unimplemented is not served here", status.Error(codes.Unimplemented, "no svm service"), true},
		{"InvalidArgument is deterministic and client-induced", status.Error(codes.InvalidArgument, "version gate"), true},
		{"Internal is a real failure", status.Error(codes.Internal, "boom"), false},
		{"Unavailable is a real failure", status.Error(codes.Unavailable, "connection refused"), false},
		{"DeadlineExceeded is a real failure", status.Error(codes.DeadlineExceeded, "too slow"), false},
		{"NotFound is a real failure", status.Error(codes.NotFound, "genesis mismatch"), false},
		{"non-status error is a real failure", errors.New("dial tcp: no route to host"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svmMapGetBlockError(tc.in)
			require.Error(t, err)
			assert.Equal(t, tc.wantBenign, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
				"benign-miss vs. real-failure classification decides whether a broken reader gets scored; got: %v", err)
		})
	}
}

// TestSvmGetBlockClassifiesGrpcErrors proves the classification above is
// actually applied on the handler's gRPC error path, not just available as a
// helper.
func TestSvmGetBlockClassifiesGrpcErrors(t *testing.T) {
	tests := []struct {
		name       string
		grpcErr    error
		wantBenign bool
	}{
		{"OutOfRange falls through", status.Error(codes.OutOfRange, "above tip"), true},
		{"Internal stays a real error", status.Error(codes.Internal, "boom"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := callSvmGetBlock(t, `[42]`, nil, tc.grpcErr)
			require.Error(t, err)
			assert.Nil(t, out)
			assert.Equal(t, tc.wantBenign, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
				"got: %v", err)
		})
	}
}

// TestSvmGetBlockSkippedSlotFallsThrough: a skipped slot is a permanent
// answer, but Agave reports it as a JSON-RPC error (-32007/-32009) and a cache
// connector can only return a result. Returning success with an empty or nil
// block would hand the caller a block that does not exist, so the connector
// must report a miss and let the live upstream render the error shape.
func TestSvmGetBlockSkippedSlotFallsThrough(t *testing.T) {
	tests := []struct {
		name string
		resp *svm.GetBlockResponse
	}{
		{"slot explicitly skipped", &svm.GetBlockResponse{SlotStatus: svm.SlotStatus_SLOT_SKIPPED}},
		{"present but block unset", &svm.GetBlockResponse{SlotStatus: svm.SlotStatus_SLOT_PRESENT}},
		{
			name: "skipped wins even if a block is attached",
			resp: &svm.GetBlockResponse{
				SlotStatus: svm.SlotStatus_SLOT_SKIPPED,
				Block:      &svm.ConfirmedBlock{Slot: 42},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, out, err := callSvmGetBlock(t, `[42]`, tc.resp, nil)
			require.Error(t, err)
			assert.Nil(t, out)
			assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
				"got: %v", err)
		})
	}
}
