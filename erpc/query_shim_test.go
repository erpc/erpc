package erpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestPaginateLogsByBlock_DoesNotSplitBoundaryBlock(t *testing.T) {
	rawLogs := []*evm.Log{
		{BlockNumber: 10, LogIndex: 0},
		{BlockNumber: 10, LogIndex: 1},
		{BlockNumber: 11, LogIndex: 0},
		{BlockNumber: 11, LogIndex: 1},
	}

	page, cursor := paginateLogsByBlock(rawLogs, 3)

	require.Len(t, page, 2)
	require.Equal(t, uint64(10), page[0].BlockNumber)
	require.Equal(t, uint64(10), page[1].BlockNumber)
	require.NotNil(t, cursor)
	require.Equal(t, uint64(10), cursor.Number)
}

func TestPaginateLogsByBlock_IncludesWholeFirstBlock(t *testing.T) {
	rawLogs := []*evm.Log{
		{BlockNumber: 20, LogIndex: 0},
		{BlockNumber: 20, LogIndex: 1},
		{BlockNumber: 20, LogIndex: 2},
		{BlockNumber: 20, LogIndex: 3},
		{BlockNumber: 21, LogIndex: 0},
	}

	page, cursor := paginateLogsByBlock(rawLogs, 3)

	require.Len(t, page, 4)
	for _, log := range page {
		require.Equal(t, uint64(20), log.BlockNumber)
	}
	require.NotNil(t, cursor)
	require.Equal(t, uint64(20), cursor.Number)
}

func TestPaginateLogsByBlock_NoCursorWhenAllLogsFit(t *testing.T) {
	rawLogs := []*evm.Log{
		{BlockNumber: 30, LogIndex: 0},
		{BlockNumber: 30, LogIndex: 1},
		{BlockNumber: 31, LogIndex: 0},
	}

	page, cursor := paginateLogsByBlock(rawLogs, 3)

	require.Len(t, page, 3)
	require.Nil(t, cursor)
}

func TestLoadQueryLogParentData_CachesByBlockNumber(t *testing.T) {
	fetchCalls := 0
	cache := map[uint64]*queryLogParentData{}

	first, err := loadQueryLogParentData(
		context.Background(),
		cache,
		42,
		func(ctx context.Context, blockNum uint64) (*evm.BlockHeader, []*evm.Transaction, error) {
			fetchCalls++
			return &evm.BlockHeader{Number: blockNum}, []*evm.Transaction{
				{Hash: []byte("tx-1")},
				{Hash: []byte("tx-2")},
			}, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, fetchCalls)
	require.Len(t, first.txsByHash, 2)
	require.Equal(t, uint64(42), first.header.Number)

	second, err := loadQueryLogParentData(
		context.Background(),
		cache,
		42,
		func(ctx context.Context, blockNum uint64) (*evm.BlockHeader, []*evm.Transaction, error) {
			fetchCalls++
			return nil, nil, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, 1, fetchCalls)
	require.Same(t, first, second)
}

func TestShimQueryLogs_EmptyRangeAfterCursorDoesNotForward(t *testing.T) {
	qe := &EvmQueryExecutor{}

	called := false
	err := qe.shimQueryLogs(context.Background(), &evm.QueryLogsRequest{}, 10, 9, func(msg proto.Message) error {
		called = true

		resp, ok := msg.(*evm.QueryLogsResponse)
		require.True(t, ok)
		require.Empty(t, resp.Logs)
		require.Empty(t, resp.Transactions)
		require.Empty(t, resp.Blocks)
		require.NotNil(t, resp.FromBlock)
		require.NotNil(t, resp.ToBlock)
		require.Equal(t, uint64(10), resp.FromBlock.Number)
		require.Equal(t, uint64(9), resp.ToBlock.Number)
		require.Nil(t, resp.CursorBlock)

		return nil
	})
	require.NoError(t, err)
	require.True(t, called)
}

func TestShimQueryTransactions_ReturnsCursorWhenNextBlockWouldOverflowLimit(t *testing.T) {
	qe := &EvmQueryExecutor{
		forwardSubrequestFn: func(ctx context.Context, method string, params interface{}) ([]byte, error) {
			switch method {
			case "eth_getBlockByNumber":
				args, ok := params.([]interface{})
				require.True(t, ok)
				blockRef, ok := args[0].(string)
				require.True(t, ok)

				switch blockRef {
				case "0x1":
					return mustMarshalJSON(t, makeProtoBlockResult(1, []interface{}{
						makeProtoTransactionResult(0x111, 1, 0),
					})), nil
				case "0x2":
					return mustMarshalJSON(t, makeProtoBlockResult(2, []interface{}{
						makeProtoTransactionResult(0x221, 2, 0),
						makeProtoTransactionResult(0x222, 2, 1),
					})), nil
				default:
					return nil, fmt.Errorf("unexpected block ref %s", blockRef)
				}
			default:
				return nil, fmt.Errorf("unexpected method %s", method)
			}
		},
	}

	var page *evm.QueryTransactionsResponse
	err := qe.shimQueryTransactions(context.Background(), &evm.QueryTransactionsRequest{
		Limit: uint32Ptr(2),
	}, 1, 2, func(msg proto.Message) error {
		page = msg.(*evm.QueryTransactionsResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Transactions, 1)
	require.NotNil(t, page.CursorBlock)
	assert.Equal(t, uint64(1), page.CursorBlock.Number)
}

func TestShimQueryTraces_ReturnsCursorWhenNextBlockWouldOverflowLimit(t *testing.T) {
	qe := &EvmQueryExecutor{
		forwardSubrequestFn: func(ctx context.Context, method string, params interface{}) ([]byte, error) {
			switch method {
			case "eth_getBlockByNumber":
				args, ok := params.([]interface{})
				require.True(t, ok)
				blockRef, ok := args[0].(string)
				require.True(t, ok)

				switch blockRef {
				case "0x1":
					return mustMarshalJSON(t, makeProtoBlockResult(1, []interface{}{})), nil
				case "0x2":
					return mustMarshalJSON(t, makeProtoBlockResult(2, []interface{}{})), nil
				default:
					return nil, fmt.Errorf("unexpected block ref %s", blockRef)
				}
			case "trace_block":
				args, ok := params.([]interface{})
				require.True(t, ok)
				blockRef, ok := args[0].(string)
				require.True(t, ok)

				switch blockRef {
				case "0x1":
					return mustMarshalJSON(t, []map[string]interface{}{
						makeParityTraceResult(1, 0, 0),
					}), nil
				case "0x2":
					return mustMarshalJSON(t, []map[string]interface{}{
						makeParityTraceResult(2, 0, 0),
						makeParityTraceResult(2, 1, 1),
					}), nil
				default:
					return nil, fmt.Errorf("unexpected trace block ref %s", blockRef)
				}
			default:
				return nil, fmt.Errorf("unexpected method %s", method)
			}
		},
	}

	var page *evm.QueryTracesResponse
	err := qe.shimQueryTraces(context.Background(), &evm.QueryTracesRequest{
		Limit: uint32Ptr(2),
	}, 1, 2, func(msg proto.Message) error {
		page = msg.(*evm.QueryTracesResponse)
		return nil
	})

	require.NoError(t, err)
	require.NotNil(t, page)
	require.Len(t, page.Traces, 1)
	require.NotNil(t, page.CursorBlock)
	assert.Equal(t, uint64(1), page.CursorBlock.Number)
}

func mustMarshalJSON(t *testing.T, value interface{}) []byte {
	t.Helper()

	data, err := sonic.Marshal(value)
	require.NoError(t, err)
	return data
}

func uint32Ptr(v uint32) *uint32 {
	return &v
}

func makeProtoBlockResult(number uint64, txs []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"number":           fmt.Sprintf("0x%x", number),
		"hash":             fmt.Sprintf("0x%064x", number),
		"parentHash":       fmt.Sprintf("0x%064x", number-1),
		"timestamp":        "0x64",
		"gasLimit":         "0x5208",
		"gasUsed":          "0x5208",
		"logsBloom":        "0x" + strings.Repeat("0", 512),
		"transactionsRoot": "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"stateRoot":        "0x0000000000000000000000000000000000000000000000000000000000000000",
		"receiptsRoot":     "0x56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421",
		"sha3Uncles":       "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
		"miner":            "0x0000000000000000000000000000000000000000",
		"extraData":        "0x",
		"nonce":            "0x0",
		"mixHash":          "0x0000000000000000000000000000000000000000000000000000000000000000",
		"difficulty":       "0x0",
		"baseFeePerGas":    "0x1",
		"transactions":     txs,
	}
}

func makeProtoTransactionResult(hashSeed uint64, blockNumber uint64, txIndex uint64) map[string]interface{} {
	return map[string]interface{}{
		"hash":             fmt.Sprintf("0x%064x", hashSeed),
		"nonce":            "0x0",
		"from":             "0x0000000000000000000000000000000000000001",
		"to":               "0x0000000000000000000000000000000000000002",
		"value":            "0x0",
		"input":            "0x12345678",
		"type":             "0x2",
		"gas":              "0x5208",
		"gasPrice":         "0x1",
		"blockNumber":      fmt.Sprintf("0x%x", blockNumber),
		"blockHash":        fmt.Sprintf("0x%064x", blockNumber),
		"transactionIndex": fmt.Sprintf("0x%x", txIndex),
		"r":                "0x01",
		"s":                "0x02",
		"v":                "0x1b",
	}
}

func makeParityTraceResult(blockNumber uint64, txIndex uint64, traceSeed uint64) map[string]interface{} {
	return map[string]interface{}{
		"type": "call",
		"action": map[string]interface{}{
			"callType": "call",
			"from":     "0x0000000000000000000000000000000000000001",
			"to":       "0x0000000000000000000000000000000000000002",
			"value":    "0x1",
			"input":    "0x",
			"gas":      "0x5208",
		},
		"result": map[string]interface{}{
			"output":  "0x",
			"gasUsed": "0x5208",
		},
		"subtraces":        "0x0",
		"traceAddress":     []interface{}{},
		"transactionHash":  fmt.Sprintf("0x%064x", blockNumber*100+traceSeed),
		"transactionIndex": fmt.Sprintf("0x%x", txIndex),
	}
}
