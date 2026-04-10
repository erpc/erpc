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
