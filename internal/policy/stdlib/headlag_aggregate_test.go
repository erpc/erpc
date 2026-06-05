package stdlib_test

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// These tests pin a single invariant that the prod lag-exclusion bug violated:
//
//	After SetLatestBlockNumber, the {ups, "*", All} aggregate — the bucket a
//	network-scope selection policy reads via GetUpstreamMethodMetrics(ups,
//	"*", All) — MUST carry the upstream's block-head-lag.
//
// In prod the lag write iterated the dedup index (`upstreamsByNetwork`) and
// filled every per-method bucket, but the "*" wildcard aggregate was missing
// from that index, so it stayed 0 forever and `blockNumberLagAbove` never
// fired. Request metrics reached the aggregate only because getUpsKeys writes
// it directly — lag did not. These reproduce that across the sequences that
// can leave the "*" aggregate out of the index (policy reads it before/while
// traffic indexes per-method keys, per-finality tracking, idle eviction).

func recordTraffic(tr *health.Tracker, u common.Upstream, method string, fin common.DataFinalityState, n int) {
	for i := 0; i < n; i++ {
		tr.RecordUpstreamRequest(u, method, fin)
		tr.RecordUpstreamDuration(u, method, 5*time.Millisecond, true, "none", fin, "n/a")
	}
}

func wildcardLag(t *testing.T, tr *health.Tracker, u common.Upstream) int64 {
	t.Helper()
	m := tr.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll)
	require.NotNil(t, m, "{*, All} aggregate must exist")
	return m.BlockHeadLag.Load()
}

// setHeads makes ups[1] the tip and ups[0] frozen `behind` blocks back.
func setHeads(tr *health.Tracker, ups []common.Upstream, behind int64) {
	tip := int64(1_000_000)
	tr.SetLatestBlockNumber(ups[1], tip, 0)
	tr.SetLatestBlockNumber(ups[0], tip-behind, 0)
}

func TestWildcardLag_FinalityOff_SingleMethod(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	ups := mkUps("rpc1", "rpc2")
	for _, u := range ups {
		recordTraffic(tr, u, "eth_call", common.DataFinalityStateUnknown, 5)
	}
	setHeads(tr, ups, 900)
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

func TestWildcardLag_FinalityOff_MultiMethod(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	ups := mkUps("rpc1", "rpc2")
	methods := []string{"eth_getBlockByNumber", "eth_syncing", "eth_blockNumber", "eth_getTransactionReceipt", "eth_getTransactionCount", "eth_gasPrice", "eth_estimateGas"}
	for _, u := range ups {
		for _, m := range methods {
			recordTraffic(tr, u, m, common.DataFinalityStateUnknown, 3)
		}
	}
	setHeads(tr, ups, 900)
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

func TestWildcardLag_FinalityOn_MultiMethod_Unfinalized(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	tr.EnableFinalityTracking()
	ups := mkUps("rpc1", "rpc2")
	methods := []string{"eth_getBlockByNumber", "eth_syncing", "eth_getTransactionReceipt", "eth_getTransactionCount"}
	for _, u := range ups {
		for _, m := range methods {
			recordTraffic(tr, u, m, common.DataFinalityStateUnfinalized, 3)
		}
	}
	setHeads(tr, ups, 900)
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

// PolicyReadsAggregateFirst mirrors prod: the selection-policy tick reads the
// {*, All} aggregate (lazily creating it) BEFORE per-method traffic indexes
// the concrete keys. If the read-created aggregate isn't in the index, the
// later lag write misses it.
func TestWildcardLag_PolicyReadsAggregateFirst(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	tr.EnableFinalityTracking()
	ups := mkUps("rpc1", "rpc2")

	// Policy tick reads the wildcard aggregate first (lazy-creates it).
	for _, u := range ups {
		_ = tr.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll)
	}
	// Then real traffic flows per-method.
	for _, u := range ups {
		recordTraffic(tr, u, "eth_getBlockByNumber", common.DataFinalityStateUnfinalized, 5)
	}
	setHeads(tr, ups, 900)
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

// Interleaved is the closest reproduction of prod: continuous policy reads of
// the aggregate interleaved with per-method traffic and lag writes.
func TestWildcardLag_Interleaved_Continuous(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	tr.EnableFinalityTracking()
	ups := mkUps("rpc1", "rpc2")
	methods := []string{"eth_getBlockByNumber", "eth_getTransactionReceipt", "eth_syncing"}

	tip := int64(1_000_000)
	for round := 0; round < 6; round++ {
		for _, u := range ups {
			_ = tr.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll) // policy read
		}
		for _, u := range ups {
			recordTraffic(tr, u, methods[round%len(methods)], common.DataFinalityStateUnfinalized, 2)
		}
		tr.SetLatestBlockNumber(ups[1], tip, 0)
		tr.SetLatestBlockNumber(ups[0], tip-900, 0)
		tip += 10
	}
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

// WithIdleEviction adds aggressive idle eviction (which deletes per-method
// buckets from upsMetrics) on top of continuous churn.
func TestWildcardLag_WithIdleEviction(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", 100*time.Millisecond)
	tr.EnableFinalityTracking()
	tr.SetIdleEvictionAfter(40 * time.Millisecond)
	ups := mkUps("rpc1", "rpc2")

	for _, u := range ups {
		_ = tr.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll)
		recordTraffic(tr, u, "eth_getBlockByNumber", common.DataFinalityStateUnfinalized, 3)
	}
	setHeads(tr, ups, 900)
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

// --- ordering-window scenarios: lag write relative to aggregate creation ---

// Poller runs before any request traffic (cold start): SetLatestBlockNumber
// fires while the index + upsMetrics are empty, then the policy reads {*,All}.
func TestWildcardLag_PollerOnly_NoTraffic(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	tr.EnableFinalityTracking()
	ups := mkUps("rpc1", "rpc2")
	setHeads(tr, ups, 900)
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

// Poller fires once before traffic; traffic then creates the aggregate; no
// further poll. The lag computed pre-traffic must still surface at {*,All}.
func TestWildcardLag_PollerBeforeTraffic_SinglePoll(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	tr.EnableFinalityTracking()
	ups := mkUps("rpc1", "rpc2")
	setHeads(tr, ups, 900)
	for _, u := range ups {
		recordTraffic(tr, u, "eth_getBlockByNumber", common.DataFinalityStateUnfinalized, 5)
	}
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}

// Poller fires, traffic flows, poller fires again (steady state): repeated
// polls must self-heal the aggregate even if the first poll lost the lag.
func TestWildcardLag_PollerBeforeTraffic_RepeatedPoll(t *testing.T) {
	lg := zerolog.Nop()
	tr := health.NewTracker(&lg, "p", time.Minute)
	tr.EnableFinalityTracking()
	ups := mkUps("rpc1", "rpc2")
	setHeads(tr, ups, 900)
	for _, u := range ups {
		recordTraffic(tr, u, "eth_getBlockByNumber", common.DataFinalityStateUnfinalized, 5)
	}
	setHeads(tr, ups, 900) // steady-state re-poll
	require.EqualValues(t, 900, wildcardLag(t, tr, ups[0]))
}
