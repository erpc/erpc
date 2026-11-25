package erpc

import (
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/test/chainsim"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// TestChainFeed_ReceiptsOutageEndsWhileLogsKeepFailing: every receipts and
// logs fetch fails until new logs subscribes are refused (spec §11); then
// receipts work again while eth_getLogs keeps failing. The outage's probes
// still ask for receipts, so the first receipts answer ends it and a logs
// subscribe is accepted again within a few block times.
func TestChainFeed_ReceiptsOutageEndsWhileLogsKeepFailing(t *testing.T) {
	scale := []struct {
		d *util.Duration
		v time.Duration
	}{
		{evm.LogsFallbackAfter, 5 * chaosBlock},
		{evm.ReceiptsOutageAfter, 15 * chaosBlock},
		{evm.ReceiptsProbeCeiling, 5 * chaosBlock},
	}
	for _, sc := range scale {
		orig := sc.d.Load()
		sc.d.Store(sc.v)
		t.Cleanup(func() { sc.d.Store(orig) })
	}

	s := newChaosSeed(t, 7, true, chaosOpts{})
	s.startERPC()
	defer s.stopERPC()
	s.chain.Start()
	s.blocks(3)

	// A logs client wants receipts, so the feed fetches them.
	conn, e := s.subscribeLogs(0)
	require.Nil(t, e, "a logs subscribe is accepted before the outage")
	defer func() { _ = conn.Close() }()

	s.setAll(func(f *chainsim.Faults) { f.ReceiptsDown, f.LogsDown = true, true })
	until := time.Now().Add(evm.ReceiptsOutageAfter.Load() + 30*chaosBlock)
	refused := false
	for time.Now().Before(until) && !refused {
		c, e := s.subscribeLogs(0)
		if c != nil {
			_ = c.Close()
			time.Sleep(2 * chaosBlock)
			continue
		}
		refused = e != nil
	}
	require.True(t, refused, "logs subscribes are refused once every receipts and logs fetch failed for %s", evm.ReceiptsOutageAfter.Load())

	// Past the logs fallback's delay, so the probes have switched to logs.
	time.Sleep(evm.LogsFallbackAfter.Load())
	s.setAll(func(f *chainsim.Faults) { f.ReceiptsDown = false })
	require.Eventually(t, func() bool {
		c, _ := s.subscribeLogs(0)
		if c != nil {
			_ = c.Close()
			return true
		}
		return false
	}, 20*chaosBlock+2*evm.ReceiptsProbeCeiling.Load(), chaosBlock,
		"receipts that work again end the outage while eth_getLogs keeps failing")
	s.setAll(func(f *chainsim.Faults) { f.LogsDown = false })
}
