package data

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestIntegrationPollLock_NilLockWithoutError_FallsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.SharedStateConfig{
		ClusterKey:      "test",
		FallbackTimeout: common.Duration(200 * time.Millisecond),
		LockTtl:         common.Duration(2 * time.Second),
		LockMaxWait:     common.Duration(50 * time.Millisecond),
		UpdateMaxWait:   common.Duration(100 * time.Millisecond),
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
				// Buggy connector: nil lock, nil error.
				return nil, nil
			}
			return inner.Lock(ctx, key, ttl)
		},
	}

	before, err := telemetry.MetricSharedStatePollLockTotal.GetMetricWithLabelValues("unavailable")
	require.NoError(t, err)
	beforeV := promUtil.ToFloat64(before)

	counter := ssr.GetCounterInt64("poll-lock-nil-lock", 1024).(*counterInt64)
	forceStale(counter)

	calls := int32(0)
	val, err := counter.TryUpdateIfStale(ctx, 5*time.Second, func(ctx context.Context) (int64, error) {
		calls++
		return 3, nil
	})

	require.NoError(t, err)
	assert.Equal(t, int64(3), val)
	assert.Equal(t, int32(1), calls)

	after, err := telemetry.MetricSharedStatePollLockTotal.GetMetricWithLabelValues("unavailable")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, promUtil.ToFloat64(after)-beforeV, 1.0)
}

