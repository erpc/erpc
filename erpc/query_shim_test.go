package erpc

import (
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
