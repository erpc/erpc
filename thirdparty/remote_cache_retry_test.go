package thirdparty

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestRemoteDataCache_FailureCooldown(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "stale"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				logger := zerolog.Nop()
				c := NewRemoteDataCache[int]("test")
				var fetchedAt time.Time
				if warm {
					c.TriggerAsyncRefresh(&logger, "k", func(context.Context) (int, error) { return 7, nil })
					synctest.Wait()
					fetchedAt = c.snapshot.Load().fetchedAt["k"]
					time.Sleep(time.Nanosecond)
				}
				var calls atomic.Int32
				fail := func(context.Context) (int, error) {
					calls.Add(1)
					return 0, errors.New("catalog unavailable")
				}
				for range 100 {
					value, usable := c.EnsureFresh(&logger, "k", time.Nanosecond, fail)
					assert.Equal(t, warm, usable)
					if warm {
						assert.Equal(t, 7, value, "failed refreshes keep the stale value usable")
					}
					synctest.Wait()
				}
				require.Equal(t, int32(1), calls.Load(), "completed errors must not allow a fetch on every call")
				_, fresh := c.Lookup("k", time.Hour)
				assert.Equal(t, warm, fresh, "failure must not create a snapshot")
				if warm {
					assert.Equal(t, fetchedAt, c.snapshot.Load().fetchedAt["k"], "failures must not update freshness")
				}

				time.Sleep(3*time.Second - time.Nanosecond)
				c.TriggerAsyncRefresh(&logger, "k", fail)
				synctest.Wait()
				require.Equal(t, int32(1), calls.Load(), "the cooldown includes the instant before its deadline")

				time.Sleep(time.Nanosecond)
				c.TriggerAsyncRefresh(&logger, "k", fail)
				synctest.Wait()
				require.Equal(t, int32(2), calls.Load(), "a fetch is allowed exactly at the deadline")

				time.Sleep(2 * time.Second)
				c.TriggerAsyncRefresh(&logger, "k", fail)
				synctest.Wait()
				require.Equal(t, int32(2), calls.Load(), "each failure starts a new cooldown")
				time.Sleep(time.Second)
				c.TriggerAsyncRefresh(&logger, "k", func(context.Context) (int, error) {
					calls.Add(1)
					return 8, nil
				})
				synctest.Wait()
				value, fresh := c.Lookup("k", time.Hour)
				require.Equal(t, 8, value)
				require.True(t, fresh)
				require.Equal(t, int32(3), calls.Load())
				c.TriggerAsyncRefresh(&logger, "k", fail)
				synctest.Wait()
				require.Equal(t, int32(4), calls.Load(), "a successful retry permits a subsequent refresh immediately")
			})
		})
	}
}

func TestRemoteDataCache_FailureCooldownIsPerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger := zerolog.Nop()
		c := NewRemoteDataCache[int]("test")
		c.TriggerAsyncRefresh(&logger, "failed", func(context.Context) (int, error) {
			return 0, errors.New("catalog unavailable")
		})
		synctest.Wait()
		c.TriggerAsyncRefresh(&logger, "other", func(context.Context) (int, error) { return 9, nil })
		synctest.Wait()
		value, fresh := c.Lookup("other", time.Hour)
		require.Equal(t, 9, value)
		require.True(t, fresh, "one key's failure must not delay another key")
	})
}
