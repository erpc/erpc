package erpc

import (
	"context"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/stretchr/testify/require"
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
