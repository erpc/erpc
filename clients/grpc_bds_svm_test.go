package clients

import (
	"context"
	"encoding/binary"
	"encoding/json"
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
// above it: SendRequest resolves a pooled, dialed connection first. It takes
// a testing.TB so the benchmarks at the bottom of this file drive the exact
// same path the tests do.
func callSvmGetBlock(tb testing.TB, paramsJSON string, resp *svm.GetBlockResponse, grpcErr error) (*fakeSvmClient, *common.NormalizedResponse, error) {
	tb.Helper()
	fake := &fakeSvmClient{resp: resp, err: grpcErr}
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":` + paramsJSON + `}`,
	))
	jrReq, err := req.JsonRpcRequest()
	require.NoError(tb, err)

	c := &GenericGrpcBdsClient{}
	out, err := c.handleSvmGetBlock(context.Background(), &bdsConn{svmClient: fake}, req, jrReq)
	return fake, out, err
}

// TestSvmGetBlockEncodingGuard is the highest-value guard in this file. BDS
// returns a structured block, which renders as encoding "json" directly, and
// as "jsonParsed" by running Agave's instruction parsers over that same block
// in-process before rendering the parsed envelope. base58 / base64 /
// base64+zstd are a different payload shape entirely: serving json for them
// returns a wrong-shaped result a client cannot distinguish from a correct
// one, so they must be refused as ErrEndpointUnsupported so the cache reports
// a miss and the request falls through to a live upstream. Absent encoding
// means json per Agave's default.
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
		{"jsonParsed is served by parsing in-process", `[42,{"encoding":"jsonParsed"}]`, true},
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
				require.NotNil(t, fake.got)
				// Every servable encoding sends the reader the SAME request:
				// eRPC asks it for nothing extra, jsonParsed included. The
				// reader is a data lake that ships raw instruction data and
				// knows nothing about encodings; the parsers run here, so the
				// rendered output depends only on the eRPC binary and not on
				// which reader version happened to answer.
				assert.False(t, fake.got.IncludeParsed,
					"the reader must never be asked to parse — jsonParsed is derived in-process from the raw block")
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

// svmTestKey returns a distinct, recognizable 32-byte pubkey.
func svmTestKey(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

// svmGetBlockResult drives the handler and decodes the JSON-RPC result bytes,
// which is the only shape a client ever sees. Asserting on the decoded tree
// rather than the bytes keeps the tests independent of key order.
func svmGetBlockResult(t *testing.T, paramsJSON string, resp *svm.GetBlockResponse) map[string]interface{} {
	t.Helper()
	_, out, err := callSvmGetBlock(t, paramsJSON, resp, nil)
	require.NoError(t, err)
	require.NotNil(t, out)
	jr, err := out.JsonRpcResponse()
	require.NoError(t, err)
	var result map[string]interface{}
	require.NoError(t, json.Unmarshal(jr.GetResultBytes(), &result))
	return result
}

// svmFirstTx digs transactions[0]'s message and meta out of a decoded result.
// meta comes back nil when the transaction carries none.
func svmFirstTx(t *testing.T, result map[string]interface{}) (msg, meta map[string]interface{}) {
	t.Helper()
	txs, ok := result["transactions"].([]interface{})
	require.True(t, ok, "result carries no transactions array: %v", result)
	require.NotEmpty(t, txs)
	tx0, ok := txs[0].(map[string]interface{})
	require.True(t, ok, "transactions[0] is not an object: %v", txs[0])
	txObj, ok := tx0["transaction"].(map[string]interface{})
	require.True(t, ok, "transactions[0].transaction is not an object: %v", tx0)
	msg, ok = txObj["message"].(map[string]interface{})
	require.True(t, ok, "transaction.message is not an object: %v", txObj)
	meta, _ = tx0["meta"].(map[string]interface{})
	return msg, meta
}

// TestSvmGetBlockRenderingDispatch pins that the encoding actually selects the
// renderer, observed through the result bytes a client decodes. The two
// envelopes differ in exactly the places asserted here: json carries base58
// STRING accountKeys, a message header, and meta.loadedAddresses; jsonParsed
// carries accountKey OBJECTS over the merged (static ++ loaded) list, no
// header, and no loadedAddresses in meta. Wiring includeParsed correctly but
// rendering with the wrong function passes the guard test above and corrupts
// every response; this test is what catches it.
func TestSvmGetBlockRenderingDispatch(t *testing.T) {
	staticKey := svmTestKey(0xAA)
	loadedKey := svmTestKey(0xBB)
	block := func() *svm.GetBlockResponse {
		return &svm.GetBlockResponse{
			SlotStatus: svm.SlotStatus_SLOT_PRESENT,
			Block: &svm.ConfirmedBlock{
				Slot:       42,
				ParentSlot: 41,
				Transactions: []*svm.ConfirmedTransaction{{
					Transaction: &svm.Transaction{
						Signatures: [][]byte{svmTestKey(0x01)},
						Message: &svm.Message{
							Header:          &svm.MessageHeader{NumRequiredSignatures: 1},
							AccountKeys:     [][]byte{staticKey},
							RecentBlockhash: svmTestKey(0xCC),
						},
					},
					Meta: &svm.TransactionStatusMeta{
						LoadedWritableAddresses: [][]byte{loadedKey},
					},
				}},
			},
		}
	}

	t.Run("json renders string keys, header and loadedAddresses", func(t *testing.T) {
		msg, meta := svmFirstTx(t, svmGetBlockResult(t, `[42,{"encoding":"json"}]`, block()))

		keys, ok := msg["accountKeys"].([]interface{})
		require.True(t, ok)
		require.Len(t, keys, 1, "json keeps the static list; loaded keys stay in meta")
		assert.Equal(t, svm.Base58Encode(staticKey), keys[0],
			"json accountKeys are base58 strings")
		assert.Contains(t, msg, "header")

		require.NotNil(t, meta)
		require.Contains(t, meta, "loadedAddresses")
		la, ok := meta["loadedAddresses"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, []interface{}{svm.Base58Encode(loadedKey)}, la["writable"])
	})

	t.Run("jsonParsed renders key objects over the merged list, no header, no loadedAddresses", func(t *testing.T) {
		msg, meta := svmFirstTx(t, svmGetBlockResult(t, `[42,{"encoding":"jsonParsed"}]`, block()))

		keys, ok := msg["accountKeys"].([]interface{})
		require.True(t, ok)
		require.Len(t, keys, 2, "parsed accountKeys merge static ++ loaded")
		assert.Equal(t, map[string]interface{}{
			"pubkey":   svm.Base58Encode(staticKey),
			"signer":   true,
			"writable": true,
			"source":   "transaction",
		}, keys[0])
		assert.Equal(t, map[string]interface{}{
			"pubkey":   svm.Base58Encode(loadedKey),
			"signer":   false,
			"writable": true,
			"source":   "lookupTable",
		}, keys[1])
		assert.NotContains(t, msg, "header", "UiParsedMessage carries no header")

		require.NotNil(t, meta)
		assert.NotContains(t, meta, "loadedAddresses",
			"parsed merges loaded keys into accountKeys instead of repeating them in meta")
	})
}

// Program ids exactly as they appear on the wire. Spelled out rather than
// imported from svm/parse so this file pins the identities Agave itself keys
// on: a registry that quietly rekeyed a program would fail here rather than
// agree with itself.
const (
	svmSystemProgramID        = "11111111111111111111111111111111"
	svmTokenProgramID         = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	svmATAProgramID           = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	svmComputeBudgetProgramID = "ComputeBudget111111111111111111111111111111"
)

// svmProgramKey decodes a base58 program id into the raw 32 bytes the proto
// carries in accountKeys.
func svmProgramKey(tb testing.TB, id string) []byte {
	tb.Helper()
	k, err := svm.Base58Decode(id)
	require.NoError(tb, err)
	require.Len(tb, k, 32)
	return k
}

// svmSystemTransferData is a real System::Transfer payload: bincode fixint
// little-endian, u32 discriminant 2 followed by u64 lamports.
func svmSystemTransferData(lamports uint64) []byte {
	d := make([]byte, 12)
	binary.LittleEndian.PutUint32(d[0:4], 2)
	binary.LittleEndian.PutUint64(d[4:12], lamports)
	return d
}

// svmTokenTransferCheckedData is a real spl-token TransferChecked payload:
// u8 discriminant 12, u64 little-endian amount, u8 decimals.
func svmTokenTransferCheckedData(amount uint64, decimals uint8) []byte {
	d := make([]byte, 10)
	d[0] = 12
	binary.LittleEndian.PutUint64(d[1:9], amount)
	d[9] = decimals
	return d
}

// TestSvmGetBlockLocalInstructionParsing pins the jsonParsed instruction
// contract now that eRPC derives it: the handler runs parse.AttachToBlock over
// the raw block in-process immediately before rendering. The parsers
// themselves are exhaustively tested upstream in the manifesto, so this does
// not re-test them; what is only observable HERE is the wiring — that every
// instruction site is reached (top-level AND inner), that account indexes
// resolve to the right pubkeys, that a program outside Agave's registry
// degrades to partiallyDecoded instead of being invented, and that a
// reader-supplied attachment is thrown away. Each of those failing produces
// output a client parses fine and trusts.
func TestSvmGetBlockLocalInstructionParsing(t *testing.T) {
	systemProgram := svmProgramKey(t, svmSystemProgramID)
	tokenProgram := svmProgramKey(t, svmTokenProgramID)
	computeBudget := svmProgramKey(t, svmComputeBudgetProgramID)

	// Key list by index: 0 feePayer, 1 destination, 2 systemProgram,
	// 3 tokenSource, 4 mint, 5 tokenDest, 6 tokenOwner, 7 tokenProgram,
	// 8 budgetAccount, 9 computeBudget.
	feePayer := svmTestKey(0x11)
	destination := svmTestKey(0x22)
	tokenSource := svmTestKey(0x33)
	mint := svmTestKey(0x44)
	tokenDest := svmTestKey(0x55)
	tokenOwner := svmTestKey(0x66)
	budgetAccount := svmTestKey(0x77)
	budgetData := []byte{0x02, 0x40, 0x0d, 0x03, 0x00}

	// An attachment a reader might have shipped, deliberately wrong. It is
	// well-formed on a program whose real parse differs, so splicing it would
	// render cleanly and lie silently.
	bogus := []byte(`{"program":"system","programId":"` + svmSystemProgramID +
		`","parsed":{"type":"NOT_REAL","info":{"lamports":999999}},"stackHeight":null}`)

	innerStack := uint32(2)

	resp := &svm.GetBlockResponse{
		SlotStatus: svm.SlotStatus_SLOT_PRESENT,
		Block: &svm.ConfirmedBlock{
			Slot:       42,
			ParentSlot: 41,
			Transactions: []*svm.ConfirmedTransaction{{
				Transaction: &svm.Transaction{
					Signatures: [][]byte{svmTestKey(0x01)},
					Message: &svm.Message{
						Header: &svm.MessageHeader{NumRequiredSignatures: 1},
						AccountKeys: [][]byte{
							feePayer, destination, systemProgram,
							tokenSource, mint, tokenDest, tokenOwner, tokenProgram,
							budgetAccount, computeBudget,
						},
						RecentBlockhash: svmTestKey(0xCC),
						Instructions: []*svm.CompiledInstruction{
							{ProgramIdIndex: 2, Accounts: []byte{0, 1}, Data: svmSystemTransferData(5000000000)},
							{ProgramIdIndex: 7, Accounts: []byte{3, 4, 5, 6}, Data: svmTokenTransferCheckedData(1500000, 6)},
							{ProgramIdIndex: 9, Accounts: []byte{8}, Data: budgetData},
							{ProgramIdIndex: 2, Accounts: []byte{1, 0}, Data: svmSystemTransferData(7), Parsed: bogus},
						},
					},
				},
				Meta: &svm.TransactionStatusMeta{
					InnerInstructions: []*svm.InnerInstructions{{
						Index: 1,
						Instructions: []*svm.CompiledInstruction{{
							ProgramIdIndex: 2,
							Accounts:       []byte{0, 1},
							Data:           svmSystemTransferData(21),
							StackHeight:    &innerStack,
						}},
					}},
				},
			}},
		},
	}

	msg, meta := svmFirstTx(t, svmGetBlockResult(t, `[42,{"encoding":"jsonParsed"}]`, resp))
	instrs, ok := msg["instructions"].([]interface{})
	require.True(t, ok)
	require.Len(t, instrs, 4)

	t.Run("a real System transfer renders Agave's full parsed envelope", func(t *testing.T) {
		assert.Equal(t, map[string]interface{}{
			"program":   "system",
			"programId": svmSystemProgramID,
			"parsed": map[string]interface{}{
				"type": "transfer",
				"info": map[string]interface{}{
					"source":      svm.Base58Encode(feePayer),
					"destination": svm.Base58Encode(destination),
					"lamports":    float64(5000000000),
				},
			},
			"stackHeight": nil,
		}, instrs[0], "the whole envelope is the contract; a right-shaped object with a wrong field is the failure mode")
	})

	t.Run("spl-token transferChecked proves a second program routes", func(t *testing.T) {
		assert.Equal(t, map[string]interface{}{
			"program":   "spl-token",
			"programId": svmTokenProgramID,
			"parsed": map[string]interface{}{
				"type": "transferChecked",
				"info": map[string]interface{}{
					"source":      svm.Base58Encode(tokenSource),
					"mint":        svm.Base58Encode(mint),
					"destination": svm.Base58Encode(tokenDest),
					"authority":   svm.Base58Encode(tokenOwner),
					"tokenAmount": map[string]interface{}{
						"amount":         "1500000",
						"decimals":       float64(6),
						"uiAmount":       1.5,
						"uiAmountString": "1.5",
					},
				},
			},
			"stackHeight": nil,
		}, instrs[1], "one program parsing is a hardcode; two is a registry")
	})

	t.Run("a program outside Agave's registry stays partiallyDecoded", func(t *testing.T) {
		// ComputeBudget is deliberately absent from Agave's registry, so a
		// real node emits partiallyDecoded for it. Inventing a parsed
		// envelope here would diverge from every other provider serving the
		// same slot — the one thing a cache in front of them must never do.
		ix, ok := instrs[2].(map[string]interface{})
		require.True(t, ok)
		assert.NotContains(t, ix, "program")
		assert.NotContains(t, ix, "parsed")
		assert.Equal(t, map[string]interface{}{
			"programId":   svmComputeBudgetProgramID,
			"accounts":    []interface{}{svm.Base58Encode(budgetAccount)},
			"data":        svm.Base58Encode(budgetData),
			"stackHeight": nil,
		}, ix)
	})

	t.Run("a reader-supplied attachment is discarded, not spliced", func(t *testing.T) {
		// parse.AttachToBlock clears Parsed before re-deriving it. That is
		// deliberate: trusting whatever the reader attached would make the
		// response depend on which reader version answered, and the reader is
		// a data lake shipping raw instruction data, not an encoder. One
		// parser implementation means output depends only on this binary.
		assert.Equal(t, map[string]interface{}{
			"program":   "system",
			"programId": svmSystemProgramID,
			"parsed": map[string]interface{}{
				"type": "transfer",
				"info": map[string]interface{}{
					"source":      svm.Base58Encode(destination),
					"destination": svm.Base58Encode(feePayer),
					"lamports":    float64(7),
				},
			},
			"stackHeight": nil,
		}, instrs[3], "the locally derived envelope must win over the attachment")
	})

	t.Run("inner instructions are parsed too and keep their stackHeight", func(t *testing.T) {
		// CPI instructions are where jsonParsed earns its keep; walking only
		// the message leaves every inner instruction raw while the top level
		// looks perfect.
		require.NotNil(t, meta)
		inner, ok := meta["innerInstructions"].([]interface{})
		require.True(t, ok, "meta carries no innerInstructions: %v", meta)
		require.Len(t, inner, 1)
		group, ok := inner[0].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, float64(1), group["index"])
		ixs, ok := group["instructions"].([]interface{})
		require.True(t, ok)
		require.Len(t, ixs, 1)
		assert.Equal(t, map[string]interface{}{
			"program":   "system",
			"programId": svmSystemProgramID,
			"parsed": map[string]interface{}{
				"type": "transfer",
				"info": map[string]interface{}{
					"source":      svm.Base58Encode(feePayer),
					"destination": svm.Base58Encode(destination),
					"lamports":    float64(21),
				},
			},
			"stackHeight": float64(2),
		}, ixs[0])
	})
}

// TestSvmGetBlockParsedResolvesLoadedAddresses pins index resolution for v0
// transactions: a parsed instruction routinely references accounts that exist
// only in the address-lookup-table result, and indexes resolve against
// static ++ loadedWritable ++ loadedReadonly in that order. Resolving against
// the static list alone, or getting the writable/readonly order backwards,
// yields a parsed envelope naming the wrong accounts — valid JSON that is a
// plausible-looking lie about who moved the tokens.
func TestSvmGetBlockParsedResolvesLoadedAddresses(t *testing.T) {
	tokenProgram := svmProgramKey(t, svmTokenProgramID)
	feePayer := svmTestKey(0x11)
	tokenSource := svmTestKey(0x33)
	mint := svmTestKey(0x44)
	tokenDest := svmTestKey(0x55)

	// Merged indexes: 0 feePayer, 1 tokenProgram (static),
	// 2 tokenSource, 3 tokenDest (loaded writable), 4 mint (loaded readonly).
	resp := &svm.GetBlockResponse{
		SlotStatus: svm.SlotStatus_SLOT_PRESENT,
		Block: &svm.ConfirmedBlock{
			Slot:       42,
			ParentSlot: 41,
			Transactions: []*svm.ConfirmedTransaction{{
				Transaction: &svm.Transaction{
					Signatures: [][]byte{svmTestKey(0x01)},
					Message: &svm.Message{
						Header:          &svm.MessageHeader{NumRequiredSignatures: 1},
						AccountKeys:     [][]byte{feePayer, tokenProgram},
						RecentBlockhash: svmTestKey(0xCC),
						Instructions: []*svm.CompiledInstruction{{
							ProgramIdIndex: 1,
							Accounts:       []byte{2, 4, 3, 0},
							Data:           svmTokenTransferCheckedData(1000000, 6),
						}},
					},
				},
				Meta: &svm.TransactionStatusMeta{
					LoadedWritableAddresses: [][]byte{tokenSource, tokenDest},
					LoadedReadonlyAddresses: [][]byte{mint},
				},
			}},
		},
	}

	msg, _ := svmFirstTx(t, svmGetBlockResult(t, `[42,{"encoding":"jsonParsed"}]`, resp))
	instrs, ok := msg["instructions"].([]interface{})
	require.True(t, ok)
	require.Len(t, instrs, 1)
	ix, ok := instrs[0].(map[string]interface{})
	require.True(t, ok)
	parsed, ok := ix["parsed"].(map[string]interface{})
	require.True(t, ok, "a lookup-table index must not defeat the parser and drop it to partiallyDecoded: %v", ix)
	info, ok := parsed["info"].(map[string]interface{})
	require.True(t, ok)

	assert.Equal(t, svm.Base58Encode(tokenSource), info["source"],
		"loaded writable addresses follow the static keys")
	assert.Equal(t, svm.Base58Encode(tokenDest), info["destination"])
	assert.Equal(t, svm.Base58Encode(mint), info["mint"],
		"loaded readonly addresses follow the loaded writable ones")
	assert.Equal(t, svm.Base58Encode(feePayer), info["authority"],
		"static keys keep their own indexes in the merged list")
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

// svmBenchBlock builds a block sized like a busy mainnet slot: txCount
// transactions of four mixed instructions each — a System transfer, an
// spl-token transferChecked, an ATA createIdempotent, and a ComputeBudget
// instruction Agave deliberately leaves unparsed. Three parsers plus the
// partiallyDecoded fallback, which is roughly the mix a real slot carries.
func svmBenchBlock(tb testing.TB, txCount int) *svm.GetBlockResponse {
	tb.Helper()
	systemProgram := svmProgramKey(tb, svmSystemProgramID)
	tokenProgram := svmProgramKey(tb, svmTokenProgramID)
	ataProgram := svmProgramKey(tb, svmATAProgramID)
	computeBudget := svmProgramKey(tb, svmComputeBudgetProgramID)

	// 0 feePayer, 1 destination, 2 systemProgram, 3 tokenSource, 4 mint,
	// 5 tokenDest, 6 tokenOwner, 7 tokenProgram, 8 ataAccount, 9 wallet,
	// 10 ataProgram, 11 computeBudget.
	keys := [][]byte{
		svmTestKey(0x11), svmTestKey(0x22), systemProgram,
		svmTestKey(0x33), svmTestKey(0x44), svmTestKey(0x55), svmTestKey(0x66), tokenProgram,
		svmTestKey(0x88), svmTestKey(0x99), ataProgram, computeBudget,
	}
	transferData := svmSystemTransferData(5000000000)
	tokenData := svmTokenTransferCheckedData(1500000, 6)
	budgetData := []byte{0x02, 0x40, 0x0d, 0x03, 0x00}
	blockhash := svmTestKey(0xCC)
	signature := make([]byte, 64)
	balances := make([]uint64, len(keys))
	for i := range balances {
		balances[i] = uint64(1_000_000 * (i + 1))
	}

	txs := make([]*svm.ConfirmedTransaction, 0, txCount)
	for range txCount {
		// Fresh instruction structs per transaction: AttachToBlock writes
		// into them, so shared pointers would measure one parse, not txCount.
		txs = append(txs, &svm.ConfirmedTransaction{
			Transaction: &svm.Transaction{
				Signatures: [][]byte{signature},
				Message: &svm.Message{
					Header:          &svm.MessageHeader{NumRequiredSignatures: 1},
					AccountKeys:     keys,
					RecentBlockhash: blockhash,
					Instructions: []*svm.CompiledInstruction{
						{ProgramIdIndex: 2, Accounts: []byte{0, 1}, Data: transferData},
						{ProgramIdIndex: 7, Accounts: []byte{3, 4, 5, 6}, Data: tokenData},
						{ProgramIdIndex: 10, Accounts: []byte{0, 8, 9, 4, 2, 7}, Data: []byte{1}},
						{ProgramIdIndex: 11, Accounts: []byte{0}, Data: budgetData},
					},
				},
			},
			Meta: &svm.TransactionStatusMeta{
				Fee: 5000, PreBalances: balances, PostBalances: balances,
			},
		})
	}

	return &svm.GetBlockResponse{
		SlotStatus: svm.SlotStatus_SLOT_PRESENT,
		Block:      &svm.ConfirmedBlock{Slot: 42, ParentSlot: 41, Transactions: txs},
	}
}

// BenchmarkSvmGetBlockJsonParsed and BenchmarkSvmGetBlockJson run the same
// handler over the same block, so the difference between them is exactly what
// moving Agave's parsers in-process costs per getBlock. That number is a
// serving decision, not trivia: prism getBlock already answers in 1.2-3.9s
// against ~697ms p95 for live providers, so any per-block surcharge lands on
// a path that is already the slow one.
func BenchmarkSvmGetBlockJsonParsed(b *testing.B) { benchmarkSvmGetBlock(b, "jsonParsed") }

func BenchmarkSvmGetBlockJson(b *testing.B) { benchmarkSvmGetBlock(b, "json") }

func benchmarkSvmGetBlock(b *testing.B, encoding string) {
	// One block, reused across iterations: AttachToBlock clears every Parsed
	// value before re-deriving it, so iteration N pays what iteration 1 paid.
	resp := svmBenchBlock(b, 1000)
	params := `[42,{"encoding":"` + encoding + `"}]`

	b.ReportAllocs()
	for b.Loop() {
		_, out, err := callSvmGetBlock(b, params, resp, nil)
		if err != nil || out == nil {
			b.Fatalf("getBlock(%s) failed: %v", encoding, err)
		}
	}
}
