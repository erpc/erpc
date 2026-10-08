package data

import (
	"context"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"testing"
	"time"
)

func TestSharedHeadBlockTimeSmoke(t *testing.T) {
	logger := zerolog.Nop()
	tr := health.NewTracker(&logger, "shared-head-repro", time.Minute)
	up := common.NewFakeUpstream("a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connector, err := NewMemoryConnector(ctx, &logger, "smoke", &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"})
	if err != nil {
		t.Fatal(err)
	}
	c := &counterInt64{registry: &sharedStateRegistry{appCtx: ctx, logger: &logger, connector: connector, fallbackTimeout: time.Second, updateMaxWait: time.Second, lockMaxWait: time.Second, lockTtl: time.Second}}
	c.OnValue(func(n int64) { tr.SetLatestBlockNumber(up, n, 0) })
	for n := int64(100); n < 110; n++ {
		c.processNewState("peer", CounterInt64State{Value: n, UpdatedAt: time.Now().UnixMilli() + n, UpdatedBy: "peer"})
		calls := 0
		_, err := c.TryUpdateIfLocallyStale(ctx, -time.Second, func(context.Context) (int64, error) {
			calls++
			tr.SetLatestBlockNumber(up, n, 1800000000+2*(n-100))
			return n, nil
		})
		if err != nil || calls != 1 {
			t.Fatalf("local sample suppressed: calls=%d err=%v", calls, err)
		}
	}
	t.Logf("shared-before-local blockTime=%s", tr.GetNetworkBlockTime(up.NetworkId()))
	if got := tr.GetNetworkBlockTime(up.NetworkId()); got != 2*time.Second {
		t.Fatalf("want 2s, got %s", got)
	}
	calls := 0
	_, err = c.TryUpdateIfLocallyStale(ctx, time.Hour, func(context.Context) (int64, error) { calls++; return 110, nil })
	if err != nil || calls != 0 {
		t.Fatalf("local debounce failed: calls=%d err=%v", calls, err)
	}
}
