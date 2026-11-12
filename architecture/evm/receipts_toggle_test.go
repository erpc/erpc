package evm

import (
	"math/big"
	"testing"

	"github.com/erpc/erpc/common"
	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

func boolPtr(v bool) *bool { return &v }

func receiptsToggleHeader(t *testing.T) *chainHeader {
	t.Helper()
	var bloom gethtypes.Bloom
	bloom[0] = 0xff
	gh := &gethtypes.Header{
		ParentHash:  gethcommon.Hash{0xaa},
		UncleHash:   gethtypes.EmptyUncleHash,
		TxHash:      gethcommon.Hash{0x11}, // non-empty trie: block has txs
		ReceiptHash: gethtypes.EmptyReceiptsHash,
		Bloom:       bloom,
		Difficulty:  big.NewInt(0),
		Number:      big.NewInt(1),
		GasLimit:    30_000_000,
		GasUsed:     21_000,
		Time:        1_700_000_000,
		BaseFee:     big.NewInt(7),
	}
	raw, err := gh.MarshalJSON()
	require.NoError(t, err)
	hdr, err := parseChainHeader(raw)
	require.NoError(t, err)
	return hdr
}

func TestChainTracker_ReceiptsToggleStopsBackgroundFetches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		receipts      *bool
		wantFeedNeeds bool
	}{
		{name: "default on", receipts: nil, wantFeedNeeds: true},
		{name: "explicit on", receipts: boolPtr(true), wantFeedNeeds: true},
		{name: "off", receipts: boolPtr(false), wantFeedNeeds: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := new(mockNetwork)
			n.On("Config").Return(&common.NetworkConfig{
				Evm: &common.EvmNetworkConfig{
					ChainId:      1,
					ChainTracker: &common.EvmChainTrackerConfig{Receipts: tc.receipts},
				},
			})
			tr := newChainTracker(t.Context(), n, 8, 0, "", "", "", nil, newFetchScheduler(t.Context(), n))

			hdr := receiptsToggleHeader(t)
			// A registered MayMatch that wants the block: feedNeeds only
			// reports it when the receipts toggle is on.
			tr.needsWants = []func([]byte) bool{func([]byte) bool { return true }}
			require.Equal(t, tc.wantFeedNeeds, tr.feedNeeds(hdr))

			// The toggle also gates the bloom trust fetch path.
			tr.wantTrust()
			if tc.receipts != nil && !*tc.receipts {
				require.False(t, tr.receiptsOn)
			}
		})
	}
}
