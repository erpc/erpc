package erpc

import (
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/stretchr/testify/require"
)

func TestProjectBlockFieldsPreservesIdentity(t *testing.T) {
	block := &evm.BlockHeader{
		Number:     10,
		Hash:       []byte{0xaa},
		ParentHash: []byte{0xbb},
		Timestamp:  123,
		GasLimit:   100,
		GasUsed:    50,
	}

	ProjectBlockFields(block, &evm.BlockFieldSelection{Timestamp: true})

	require.Equal(t, uint64(10), block.Number)
	require.Equal(t, []byte{0xaa}, block.Hash)
	require.Equal(t, uint64(123), block.Timestamp)
	require.Nil(t, block.ParentHash)
	require.Zero(t, block.GasLimit)
	require.Zero(t, block.GasUsed)
}

func TestProjectBlockForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.BlockHeader{
		Number:     42,
		Hash:       []byte{0xaa},
		ParentHash: []byte{0xbb},
		Timestamp:  123,
	}

	projected := projectBlockForResponse(original, &evm.BlockFieldSelection{Hash: true})
	cursor := cursorFromBlock(original)

	require.NotSame(t, original, projected)
	require.Equal(t, []byte{0xbb}, cursor.ParentHash)
	require.Equal(t, []byte{0xbb}, original.ParentHash)
	require.Nil(t, projected.ParentHash)
	require.Equal(t, []byte{0xaa}, projected.Hash)
}

func TestProjectLogForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.Log{
		BlockNumber:     42,
		TransactionHash: []byte{0xaa},
		LogIndex:        3,
	}

	projected := projectLogForResponse(original, &evm.LogFieldSelection{LogIndex: true})

	require.NotSame(t, original, projected)
	require.Equal(t, uint64(42), original.BlockNumber)
	require.Equal(t, []byte{0xaa}, original.TransactionHash)
	require.Zero(t, projected.BlockNumber)
	require.Nil(t, projected.TransactionHash)
	require.Equal(t, uint32(3), projected.LogIndex)
}

func TestProjectTransactionForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.Transaction{
		Hash:             []byte{0xaa},
		From:             []byte{0x01},
		TransactionIndex: func() *uint32 { v := uint32(7); return &v }(),
	}

	projected := projectTransactionForResponse(original, &evm.TransactionFieldSelection{From: true})

	require.NotSame(t, original, projected)
	require.Equal(t, []byte{0xaa}, original.Hash)
	require.Equal(t, uint32(7), *original.TransactionIndex)
	require.Equal(t, []byte{0x01}, projected.From)
	require.Nil(t, projected.TransactionIndex)
}

func TestProjectTraceForResponseClonesBeforeProjection(t *testing.T) {
	original := &evm.Trace{
		CallType:        evm.TraceCallType_TRACE_CALL_DELEGATECALL,
		TransactionHash: []byte{0xaa},
		TraceAddress:    []uint32{1, 2},
	}

	projected := projectTraceForResponse(original, &evm.TraceFieldSelection{TransactionHash: true})

	require.NotSame(t, original, projected)
	require.Equal(t, evm.TraceCallType_TRACE_CALL_DELEGATECALL, original.CallType)
	require.Equal(t, []byte{0xaa}, original.TransactionHash)
	require.Equal(t, []uint32{1, 2}, original.TraceAddress)
	require.Equal(t, []byte{0xaa}, projected.TransactionHash)
	require.Equal(t, evm.TraceCallType_TRACE_CALL_CALL, projected.CallType)
	require.Nil(t, projected.TraceAddress)
}

func TestProjectTraceFields(t *testing.T) {
	trace := &evm.Trace{
		From:            []byte{0x1},
		To:              []byte{0x2},
		Value:           "0x10",
		TransactionHash: []byte{0x3},
		BlockHash:       []byte{0x4},
		GasUsed:         21,
	}

	ProjectTraceFields(trace, &evm.TraceFieldSelection{From: true, Value: true})

	require.Equal(t, []byte{0x1}, trace.From)
	require.Equal(t, "0x10", trace.Value)
	require.Nil(t, trace.To)
	require.Nil(t, trace.TransactionHash)
	require.Nil(t, trace.BlockHash)
	require.Zero(t, trace.GasUsed)
}
