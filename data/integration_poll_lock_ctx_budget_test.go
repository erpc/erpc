package data

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestIntegrationPollLock_RespectsCallerDeadlineForLockAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(250 * time.Millisecond),
		LockTtl:         common.Duration(2 * time.Second),
		LockMaxWait:     common.Duration(1 * time.Second),  // should be clamped to caller deadline
		UpdateMaxWait:   common.Duration(20 * time.Millisecond),
		Connector: &common.ConnectorConfig{
			Id:     "test-memory",
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100,
				MaxTotalSize: "10MB",
			},
		},
	}
	require.NoError(t, cfg.SetDefaults("test"))
	ssr, err := NewSharedStateRegistry(ctx, &log.Logger, cfg)
	require.NoError(t, err)

	reg := ssr.(*sharedStateRegistry)
	inner := reg.connector

	reg.connector = &lockOverrideConnector{
		inner: inner,
		lock: func(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
			if strings.HasSuffix(key, "/poll") {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return inner.Lock(ctx, key, ttl)
		},
	}

	counter := ssr.GetCounterInt64("poll-lock-budget", 1024).(*counterInt64)
	forceStale(counter)

	shortCtx, shortCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer shortCancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = counter.TryUpdateIfStale(shortCtx, 5*time.Second, func(ctx context.Context) (int64, error) {
			return 1, errors.New("should not matter")
		})
	}()

	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("TryUpdateIfStale did not return quickly; poll-lock attempt likely ignored caller deadline")
	}
}

