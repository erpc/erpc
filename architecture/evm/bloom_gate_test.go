package evm

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	gethcommon "github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

func boolPtr(v bool) *bool { return &v }

func TestBloomGateChecking_IndependentOfIntegrity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		cfg           *common.NetworkConfig
		wantCheck     bool
		wantIntegrity bool
	}{
		{
			name: "no integrity block — still rejects, no metric",
			cfg: &common.NetworkConfig{
				Evm: &common.EvmNetworkConfig{ChainId: 1},
			},
			wantCheck:     true,
			wantIntegrity: false,
		},
		{
			name: "integrity level off — still rejects, no metric",
			cfg: &common.NetworkConfig{
				Evm:       &common.EvmNetworkConfig{ChainId: 1},
				Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{Level: "off"}},
			},
			wantCheck:     true,
			wantIntegrity: false,
		},
		{
			name: "integrity intrinsic enables bloomMatch metric",
			cfg: &common.NetworkConfig{
				Evm:       &common.EvmNetworkConfig{ChainId: 1},
				Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{Level: "intrinsic"}},
			},
			wantCheck:     true,
			wantIntegrity: true,
		},
		{
			name: "intrinsic but bloomMatch explicitly disabled",
			cfg: &common.NetworkConfig{
				Evm: &common.EvmNetworkConfig{ChainId: 1},
				Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
					Level: "intrinsic",
					Checks: map[string]*common.IntegrityCheckConfig{
						"bloomMatch": {Enabled: boolPtr(false)},
					},
				}},
			},
			wantCheck:     true,
			wantIntegrity: false,
		},
		{
			name: "level off but bloomMatch explicitly enabled",
			cfg: &common.NetworkConfig{
				Evm: &common.EvmNetworkConfig{ChainId: 1},
				Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{
					Level: "off",
					Checks: map[string]*common.IntegrityCheckConfig{
						"bloomMatch": {Enabled: boolPtr(true)},
					},
				}},
			},
			wantCheck:     true,
			wantIntegrity: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := new(mockNetwork)
			n.On("Config").Return(tc.cfg)
			g := newBloomGate(n)
			require.Equal(t, tc.wantCheck, g.checking())
			require.Equal(t, tc.wantIntegrity, g.prof().integrityBloom)
		})
	}
}

// bloomGateMismatchFixture is a header whose hash recomputes with a non-zero
// logsBloom and a non-empty transactionsRoot, plus an eth_getBlockReceipts
// body whose OR'd bloom differs.
func bloomGateMismatchFixture(t *testing.T) (hdr *chainHeader, receiptsJSON []byte) {
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
	hdr, err = parseChainHeader(raw)
	require.NoError(t, err)
	require.True(t, hdr.hashRecomputes(), "fixture header must recompute so judgedBloom applies")
	require.False(t, hdr.emptyTxs)
	require.NotNil(t, judgedBloom(hdr))

	zeroBloom := "0x" + strings.Repeat("0", 512)
	receiptsJSON = []byte(fmt.Sprintf(
		`[{"blockHash":"%s","blockNumber":"0x1","transactionHash":"0x%s","logsBloom":"%s","logs":[]}]`,
		hdr.hashHex, strings.Repeat("11", 32), zeroBloom,
	))
	return hdr, receiptsJSON
}

func TestReceiptsSpecDecode_BloomMismatch_MetricOptIn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		cfg           *common.NetworkConfig
		wantMetricInc bool
	}{
		{
			name: "no integrity block — rejects, no integrity metric",
			cfg: &common.NetworkConfig{
				Evm: &common.EvmNetworkConfig{ChainId: 1},
			},
			wantMetricInc: false,
		},
		{
			name: "integrity intrinsic — rejects and increments metric",
			cfg: &common.NetworkConfig{
				Evm:       &common.EvmNetworkConfig{ChainId: 1},
				Integrity: &common.IntegrityConfig{IntegritySettings: common.IntegritySettings{Level: "intrinsic"}},
			},
			wantMetricInc: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hdr, body := bloomGateMismatchFixture(t)

			n := new(mockNetwork)
			n.On("Config").Return(tc.cfg)
			n.On("ProjectId").Return("test-project")
			n.On("Id").Return("evm:1").Maybe()
			n.On("Label").Return("evm:1").Maybe()

			ht := &common.FakeHealthTracker{}
			// Unique upstream ID so parallel subtests don't share one
			// MetricIntegrityViolation series (global counter).
			up := common.NewFakeUpstream("up-bloom-"+strings.ReplaceAll(tc.name, " ", "-"), common.WithHealthTracker(ht))

			fs := newFetchScheduler(t.Context(), n)
			tr := newChainTracker(t.Context(), n, 8, 0, "", "", "", nil, fs)

			labels := []string{
				"test-project", up.VendorName(), tr.networkLabel(), up.Id(),
				"eth_getblockreceipts", "bloomMatch", "reject", "unknown",
			}
			before := testutil.ToFloat64(telemetry.MetricIntegrityViolation.WithLabelValues(labels...))

			spec := tr.receiptsSpec(classTipReceipts, hdr.hash, hdr, false)
			_, err := spec.decode(body, up)
			require.Error(t, err)
			require.True(t, errors.Is(err, errBloomMismatch), "mismatch must fail the fetch: %v", err)

			after := testutil.ToFloat64(telemetry.MetricIntegrityViolation.WithLabelValues(labels...))
			if tc.wantMetricInc {
				require.Equal(t, before+1, after)
			} else {
				require.Equal(t, before, after)
			}
			require.True(t, ht.MisbehaviorRecorded, "mismatch must still score the vendor")
			require.Equal(t, 1, ht.MisbehaviorCount)
		})
	}
}

func TestParseLogs_EmptyAnswer_JudgedBloom(t *testing.T) {
	t.Parallel()

	var bloom gethtypes.Bloom
	bloom[31] = 0x01
	gh := &gethtypes.Header{
		ParentHash:  gethcommon.Hash{0xbb},
		UncleHash:   gethtypes.EmptyUncleHash,
		TxHash:      gethcommon.Hash{0x22},
		ReceiptHash: gethtypes.EmptyReceiptsHash,
		Bloom:       bloom,
		Difficulty:  big.NewInt(0),
		Number:      big.NewInt(2),
		GasLimit:    30_000_000,
		GasUsed:     21_000,
		Time:        1_700_000_001,
		BaseFee:     big.NewInt(7),
	}
	raw, err := gh.MarshalJSON()
	require.NoError(t, err)
	hdr, err := parseChainHeader(raw)
	require.NoError(t, err)
	require.True(t, hdr.hashRecomputes())
	require.NotNil(t, judgedBloom(hdr))

	// checking on (integrity on or off — same): empty passes parse so the
	// bloom verdict can run; non-zero header bloom vs empty logs → mismatch.
	got, err := parseLogs([]byte(`[]`), hdr.hash, hdr, false /*empty*/, true /*checking*/, false, false)
	require.NoError(t, err)
	require.Equal(t, bloomMismatch, got.bloom)

	// checking off (chain-profile bloomMatch disable): empty is refused —
	// no bloom proof may excuse a missing logs answer.
	_, err = parseLogs([]byte(`[]`), hdr.hash, hdr, false, false, false, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no logs")
}
