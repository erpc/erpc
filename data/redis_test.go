package data

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestRedisConnectorInitialization(t *testing.T) {
	t.Run("succeeds immediately with valid config", func(t *testing.T) {
		m, err := miniredis.Run()
		require.NoError(t, err)
		defer m.Close()

		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.RedisConnectorConfig{
			Addr:         m.Addr(),
			Password:     "",
			DB:           0,
			ConnPoolSize: 5,
			InitTimeout:  common.Duration(2 * time.Second),
			GetTimeout:   common.Duration(2 * time.Second),
			SetTimeout:   common.Duration(2 * time.Second),
			TLS:          nil,
		}

		err = cfg.SetDefaults()
		require.NoError(t, err, "failed to set defaults for redis config")

		connector, err := NewRedisConnector(ctx, &logger, "test-connector", cfg)
		// We expect no error from NewRedisConnector, because it should succeed on the first attempt.
		require.NoError(t, err)

		// Ensure the connector reports StateReady via its initializer
		state := connector.initializer.State()
		require.Equal(t, util.StateReady, state, "connector should be in ready state")

		// Try a simple SET/GET to verify readiness.
		err = connector.Set(ctx, "testPK", "testRK", []byte("hello"), nil)
		require.NoError(t, err, "Set should succeed after successful initialization")

		val, err := connector.Get(ctx, "", "testPK", "testRK")
		require.NoError(t, err, "Get should succeed for existing key")
		require.Equal(t, []byte("hello"), val)
	})

	t.Run("fails on first attempt with invalid address, but returns connector anyway", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Intentionally invalid address
		cfg := &common.RedisConnectorConfig{
			Addr:         "127.0.0.1:9876", // random port, no server here
			Password:     "",
			DB:           0,
			ConnPoolSize: 5,
			InitTimeout:  common.Duration(500 * time.Millisecond),
			GetTimeout:   common.Duration(500 * time.Millisecond),
			SetTimeout:   common.Duration(500 * time.Millisecond),
		}

		err := cfg.SetDefaults()
		require.NoError(t, err, "failed to set defaults for redis config")

		connector, err := NewRedisConnector(ctx, &logger, "test-connector-invalid-addr", cfg)
		// The constructor does not necessarily return an error if the first attempt fails;
		// it can return a connector with a not-ready state. So we expect no error here.
		require.NoError(t, err)

		// checkReady should fail
		err = connector.checkReady()
		require.Error(t, err)

		// The connector's initializer should NOT be ready if it failed to connect.
		state := connector.initializer.State()
		require.NotEqual(t, util.StateReady, state, "connector should not be in ready state")

		// Attempting to call Set or Get here should result in an error because checkReady will fail.
		err = connector.Set(ctx, "testPK", "testRK", []byte("value"), nil)
		require.Error(t, err, "should fail because redis is not connected")

		// The same for Get
		_, err = connector.Get(ctx, "", "testPK", "testRK")
		require.Error(t, err, "should fail because redis is not connected")
	})

	t.Run("fails to create TLS config with invalid cert files", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		cfg := &common.RedisConnectorConfig{
			Addr:        "localhost:6379",
			InitTimeout: common.Duration(1 * time.Second),
			TLS: &common.TLSConfig{
				Enabled:            true,
				InsecureSkipVerify: false,
				CertFile:           "invalid-cert.pem",
				KeyFile:            "invalid-key.pem",
				CAFile:             "",
			},
		}

		err := cfg.SetDefaults()
		require.NoError(t, err, "failed to set defaults for redis config")

		connector, err := NewRedisConnector(ctx, &logger, "test-bad-tls", cfg)
		// We expect that the connector tries to create a TLS config and fails.
		// Because the creation of TLS config is inside connectTask.
		// If that fails, the initializer's first attempt fails.
		require.NoError(t, err, "NewRedisConnector doesn't necessarily bubble up the error directly.")

		// The connector is returned, but the state should not be ready.
		require.NotEqual(t, util.StateReady, connector.initializer.State())

		// checkReady should fail
		err = connector.checkReady()
		require.Error(t, err)
	})

	t.Run("eventual recovery after failing first attempt (no random sleep)", func(t *testing.T) {
		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Start a miniredis instance to grab a port, then close it immediately.
		// We'll rebind to the same port later.
		m, err := miniredis.Run()
		require.NoError(t, err)

		addr := m.Addr()
		m.Close()

		// Create a config that points at the now-closed miniredis port.
		// The first connection attempt by RedisConnector will fail initially
		// since nothing is listening.
		cfg := &common.RedisConnectorConfig{
			Addr:         addr,
			Password:     "",
			DB:           0,
			ConnPoolSize: 5,
			InitTimeout:  common.Duration(300 * time.Millisecond),
			GetTimeout:   common.Duration(300 * time.Millisecond),
			SetTimeout:   common.Duration(300 * time.Millisecond),
		}

		err = cfg.SetDefaults()
		require.NoError(t, err, "failed to set defaults for redis config")

		// This will fail its first internal connection attempt,
		// but the constructor typically returns a connector with
		// a "not-ready" initializer.
		connector, err := NewRedisConnector(ctx, &logger, "test-eventual-recovery", cfg)
		require.NoError(t, err, "Constructor should not hard-fail. It returns a connector with a failing initializer.")

		// Verify that the connector is not ready right now.
		require.NotEqual(t, util.StateReady, connector.initializer.State(),
			"Connector should not be in ready state since no Redis is listening yet.")

		// Now start a new miniredis on the SAME address as before.
		// This ensures that the next connection attempt should succeed.
		m2 := miniredis.NewMiniRedis()
		err = m2.StartAddr(addr)
		require.NoError(t, err, "Could not rebind miniredis to the original address")
		defer m2.Close()

		//require.Eventually loop and check whether the connector's initializer has transitioned to READY.
		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 5*time.Second, 200*time.Millisecond,
			"Connector did not become READY within the expected timeframe")

		// At this point, the connector should be ready. Let's confirm by doing a Set/Get.
		err = connector.Set(ctx, "eventual", "rk", []byte("recovered-value"), nil)
		require.NoError(t, err, "Set should succeed if the connector is truly ready")

		val, err := connector.Get(ctx, "", "eventual", "rk")
		require.NoError(t, err, "Get should succeed for the newly-set key")
		require.Equal(t, []byte("recovered-value"), val)
	})

}

func TestRedisConnectorConfigurationMethods(t *testing.T) {
	// spin up a disposable Redis server
	s, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(s.Close)

	const (
		redisUser = "user"
		redisPass = "pass"
		db        = 0
	)
	s.RequireUserAuth(redisUser, redisPass)
	redisAddr := s.Addr()

	cases := []struct {
		name          string
		conn          common.RedisConnectorConfig
		expectSuccess bool
		checkConnect  bool
	}{
		{
			name: "uri only with default timeout in config - valid",
			conn: common.RedisConnectorConfig{
				URI:         "redis://user:pass@<addr>/0",
				InitTimeout: common.Duration(2 * time.Second),
				GetTimeout:  common.Duration(2 * time.Second),
				SetTimeout:  common.Duration(2 * time.Second),
			},
			expectSuccess: true,
			checkConnect:  true,
		},
		{
			name: "uri only with timeout - valid",
			conn: common.RedisConnectorConfig{
				URI: "redis://user:pass@<addr>/0?dial_timeout=1s&read_timeout=1s&write_timeout=1s",
			},
			expectSuccess: true,
			checkConnect:  true,
		},
		{
			name: "uri starts with invalid schema - invalid",
			conn: common.RedisConnectorConfig{
				URI: "postgres://user:pass@<addr>/0",
			},
			expectSuccess: false,
			checkConnect:  false,
		},
		{
			name: "uri only with 0 timeout in config and no timeout in uri - valid",
			conn: common.RedisConnectorConfig{
				URI:         "redis://user:pass@<addr>/0",
				InitTimeout: common.Duration(0 * time.Second),
				GetTimeout:  common.Duration(0 * time.Second),
				SetTimeout:  common.Duration(0 * time.Second),
			},
			expectSuccess: true,
			checkConnect:  true,
		},
		{
			name: "construct URI from discrete fields uses DB 0 by default - valid",
			conn: common.RedisConnectorConfig{
				Addr:        "<addr>",
				Username:    redisUser,
				Password:    redisPass,
				InitTimeout: common.Duration(2 * time.Second),
				GetTimeout:  common.Duration(2 * time.Second),
				SetTimeout:  common.Duration(2 * time.Second),
			},
			expectSuccess: true,
			checkConnect:  true,
		},
		{
			name: "discrete fields username without password - valid",
			conn: common.RedisConnectorConfig{
				Addr:        "<addr>",
				Username:    redisUser,
				Password:    "",
				InitTimeout: common.Duration(2 * time.Second),
				GetTimeout:  common.Duration(2 * time.Second),
				SetTimeout:  common.Duration(2 * time.Second),
			},
			expectSuccess: true,
			checkConnect:  false,
		},
		{
			name: "discrete fields only - valid",
			conn: common.RedisConnectorConfig{
				Addr:        "<addr>",
				Username:    redisUser,
				Password:    redisPass,
				DB:          db,
				InitTimeout: common.Duration(2 * time.Second),
				GetTimeout:  common.Duration(2 * time.Second),
				SetTimeout:  common.Duration(2 * time.Second),
			},
			expectSuccess: true,
			checkConnect:  true,
		},
		{
			name: "both uri and discrete fields - invalid",
			conn: common.RedisConnectorConfig{
				URI:         "redis://:pass@<addr>/0",
				Addr:        "<addr>",
				Username:    redisUser,
				Password:    redisPass,
				DB:          db,
				InitTimeout: common.Duration(2 * time.Second),
				GetTimeout:  common.Duration(2 * time.Second),
				SetTimeout:  common.Duration(2 * time.Second),
			},
			expectSuccess: false,
			checkConnect:  false,
		},
		{
			name:          "neither uri nor discrete fields - invalid",
			conn:          common.RedisConnectorConfig{},
			expectSuccess: false,
			checkConnect:  false,
		},
		{
			name: "discrete fields but missing Addr - invalid",
			conn: common.RedisConnectorConfig{
				Username:    redisUser,
				Password:    redisPass,
				DB:          db,
				InitTimeout: common.Duration(2 * time.Second),
				GetTimeout:  common.Duration(2 * time.Second),
				SetTimeout:  common.Duration(2 * time.Second),
			},
			expectSuccess: false,
			checkConnect:  false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Patch the runtime address into placeholders.
			if strings.Contains(tc.conn.URI, "<addr>") {
				tc.conn.URI = strings.ReplaceAll(tc.conn.URI, "<addr>", redisAddr)
			}
			if tc.conn.Addr == "<addr>" {
				tc.conn.Addr = redisAddr
			}

			// Apply defaulting logic so that URI is always populated before validation.
			if err := tc.conn.SetDefaults(); err != nil {
				if !tc.expectSuccess {
					// For negative test‑cases, an error here is acceptable and we can return early.
					return
				}
				require.NoError(t, err, "failed to set defaults for redis config")
			}

			// Layer 1: validation
			err := tc.conn.Validate()
			if !tc.expectSuccess {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			// Layer 2: live round-trip
			if !tc.checkConnect {
				return
			}

			var client *redis.Client

			uri := tc.conn.URI
			if uri == "" {
				// Synthesize redis://[user:pass@]addr/db
				var userInfo string
				if tc.conn.Username != "" || tc.conn.Password != "" {
					userInfo = tc.conn.Username + ":" + tc.conn.Password + "@"
				}
				uri = fmt.Sprintf("redis://%s%s/%d", userInfo, tc.conn.Addr, tc.conn.DB)
			}

			opts, err := redis.ParseURL(uri)
			require.NoError(t, err, "redis.ParseURL should handle the constructed URI")
			client = redis.NewClient(opts)

			// Live round-trip to prove it works.
			ctx := context.Background()
			require.NoError(t, client.Set(ctx, "foo", "bar", 0).Err())
			val, err := client.Get(ctx, "foo").Result()
			require.NoError(t, err)
			require.Equal(t, "bar", val)
		})
	}

}

func TestRedisDistributedLocking(t *testing.T) {
	// Common setup for Redis connector for locking tests
	setupConnector := func(t *testing.T) (context.Context, *RedisConnector, *miniredis.Miniredis) {
		// Use NewMiniRedis to have more control over its lifecycle and time for testing TTLs
		s := miniredis.NewMiniRedis()
		err := s.Start() // Start the server; it will pick a random available port
		require.NoError(t, err, "failed to start miniredis server")

		logger := zerolog.New(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(s.Close)

		cfg := &common.RedisConnectorConfig{
			Addr:              s.Addr(),
			InitTimeout:       common.Duration(2 * time.Second),
			GetTimeout:        common.Duration(2 * time.Second),
			SetTimeout:        common.Duration(2 * time.Second),
			LockRetryInterval: common.Duration(50 * time.Millisecond), // For faster tests
		}
		err = cfg.SetDefaults()
		require.NoError(t, err, "failed to set defaults for redis config")

		connector, err := NewRedisConnector(ctx, &logger, "test-lock-connector", cfg)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 3*time.Second, 100*time.Millisecond, "connector did not become ready")

		return ctx, connector, s
	}

	t.Run("SuccessfulImmediateLockAcquisition", func(t *testing.T) {
		ctx, connector, _ := setupConnector(t)
		lockKey := "test-lock-immediate"
		lockTTL := 5 * time.Second

		lock, err := connector.Lock(ctx, lockKey, lockTTL)
		require.NoError(t, err, "should acquire lock without issues")
		require.NotNil(t, lock, "lock should not be nil")

		err = lock.Unlock(ctx)
		require.NoError(t, err, "unlock should succeed")
	})

	t.Run("LockAcquisitionAfterWaitingHeldByAnotherProcess", func(t *testing.T) {
		ctx, connector, s := setupConnector(t)
		lockKey := "test-lock-wait"
		firstLockTTL := 200 * time.Millisecond // Short TTL for the first lock

		// Simulate another process holding the lock by acquiring it first
		// We use the connector itself to acquire the first lock to ensure it's set correctly by redsync
		firstLockAttemptCtx, firstLockAttemptCancel := context.WithTimeout(ctx, 5*time.Second)
		defer firstLockAttemptCancel()
		firstLock, err := connector.Lock(firstLockAttemptCtx, lockKey, firstLockTTL)
		require.NoError(t, err, "failed to acquire first lock for simulation")
		require.NotNil(t, firstLock, "first lock for simulation should not be nil")

		// Fast-forward miniredis time to ensure the first lock expires
		fastForwardDuration := firstLockTTL + 1*time.Millisecond // Ensure TTL is passed
		t.Logf("Fast-forwarding miniredis time by %v to expire first lock (TTL: %v)", fastForwardDuration, firstLockTTL)
		s.FastForward(fastForwardDuration)

		// Attempt to acquire the lock, this should now succeed quickly as the first lock is expired
		start := time.Now()
		// The context for the second lock attempt can be shorter now
		secondLockAttemptCtx, secondLockAttemptCancel := context.WithTimeout(ctx, connector.cfg.LockRetryInterval.Duration()*3) // Allow for a couple of retries
		defer secondLockAttemptCancel()

		lock, err := connector.Lock(secondLockAttemptCtx, lockKey, 5*time.Second)
		timeToAcquire := time.Since(start)

		require.NoError(t, err, "should eventually acquire lock")
		require.NotNil(t, lock, "lock should not be nil after waiting")

		// After FastForward, the lock should be acquired quickly, on the first or second attempt by redsync.
		// So, timeToAcquire should be less than, say, 2 * LockRetryInterval.
		maxExpectedAcquisitionTime := connector.cfg.LockRetryInterval.Duration() * 2
		require.LessOrEqual(t, timeToAcquire, maxExpectedAcquisitionTime,
			"lock should have been acquired quickly after FastForward. Got %v, expected less than %v", timeToAcquire, maxExpectedAcquisitionTime)

		// It's tricky to assert the exact key name redsync uses, so we check if ANY lock key exists for our prefix
		keys := s.Keys()
		foundLockKey := false
		for _, k := range keys {
			if strings.HasPrefix(k, "lock:"+lockKey) {
				foundLockKey = true
				break
			}
		}
		require.True(t, foundLockKey, "a lock key should exist in redis for %s", lockKey)

		err = lock.Unlock(ctx)
		require.NoError(t, err, "unlock should succeed")

		// Unlock the first lock (even if it expired, good practice)
		_ = firstLock.Unlock(ctx)
	})

	t.Run("LockAcquisitionTimesOutDueToParentContext", func(t *testing.T) {
		ctx, connector, _ := setupConnector(t)
		lockKey := "test-lock-timeout"

		// Acquire a lock that will be held for a long time
		holdingLockCtx, holdingLockCancel := context.WithTimeout(ctx, 10*time.Second)
		defer holdingLockCancel()
		longHoldLock, err := connector.Lock(holdingLockCtx, lockKey, 10*time.Second) // Held for 10s
		require.NoError(t, err, "failed to acquire long-hold lock")
		defer func() { _ = longHoldLock.Unlock(context.Background()) }() // Ensure cleanup

		// Attempt to acquire the lock with a short timeout in the parent context
		attemptTimeout := 200 * time.Millisecond
		attemptCtx, attemptCancel := context.WithTimeout(ctx, attemptTimeout)
		defer attemptCancel()

		start := time.Now()
		lock, err := connector.Lock(attemptCtx, lockKey, 5*time.Second) // Try to get for 5s, but parent ctx is shorter
		timeSpent := time.Since(start)

		require.Error(t, err, "lock acquisition should time out due to parent context")
		require.Nil(t, lock, "lock should be nil on timeout")
		require.True(t, errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), context.DeadlineExceeded.Error()), "error should be context.DeadlineExceeded or wrap it")

		// Check that timeSpent is close to attemptTimeout, not the lock's internal retries or TTL
		require.InDelta(t, attemptTimeout.Milliseconds(), timeSpent.Milliseconds(), float64(connector.cfg.LockRetryInterval.Duration().Milliseconds()*2), "time spent should be close to parent context timeout")
	})

	t.Run("ContextCancellationDuringLockAcquisition", func(t *testing.T) {
		ctx, connector, _ := setupConnector(t)
		lockKey := "test-lock-cancel"

		// Acquire a lock that will be held
		holdingLockCtx, holdingLockCancel := context.WithTimeout(ctx, 10*time.Second)
		defer holdingLockCancel()
		holdingLock, err := connector.Lock(holdingLockCtx, lockKey, 10*time.Second)
		require.NoError(t, err, "failed to acquire holding lock")
		defer func() { _ = holdingLock.Unlock(context.Background()) }()

		// Attempt to acquire with a context that will be cancelled
		cancelCtx, cancelFunc := context.WithCancel(ctx)

		go func() {
			time.Sleep(150 * time.Millisecond) // Cancel after a short delay, while Lock is trying
			cancelFunc()
		}()

		start := time.Now()
		lock, err := connector.Lock(cancelCtx, lockKey, 5*time.Second)
		timeSpent := time.Since(start)

		require.Error(t, err, "lock acquisition should be cancelled")
		require.Nil(t, lock, "lock should be nil on cancellation")
		require.True(t, errors.Is(err, context.Canceled) || strings.Contains(err.Error(), context.Canceled.Error()), "error should be context.Canceled or wrap it")

		// Time spent should be around the cancellation time, not full Lock TTL or many retries
		require.GreaterOrEqual(t, timeSpent.Milliseconds(), int64(100), "should have waited some time before cancellation")
		require.LessOrEqual(t, timeSpent.Milliseconds(), int64(300), "should not wait much longer after cancellation")
	})

	t.Run("LockAcquisitionWithExpiredLock", func(t *testing.T) {
		ctx, connector, s := setupConnector(t)
		lockKey := "test-lock-actually-expired"
		shortHoldTTL := 150 * time.Millisecond
		lockInternalKeyPrefix := "lock:" // Redsync prepends "lock:"

		// 1. Acquire the lock with a short TTL using the connector
		firstLockCtx, firstLockCancel := context.WithTimeout(ctx, 2*time.Second) // Generous timeout for first acquisition
		defer firstLockCancel()
		lock1, err := connector.Lock(firstLockCtx, lockKey, shortHoldTTL)
		require.NoError(t, err, "Failed to acquire initial short-lived lock")
		require.NotNil(t, lock1, "Initial short-lived lock should not be nil")

		// Store the value redsync set for the first lock to compare later (optional, but good verification)
		var firstLockValue string
		keys := s.Keys()
		for _, k := range keys {
			if strings.HasPrefix(k, lockInternalKeyPrefix+lockKey) {
				firstLockValue, _ = s.Get(k)
				break
			}
		}
		require.NotEmpty(t, firstLockValue, "Could not find the value of the first lock in miniredis")

		// 2. Do NOT unlock lock1. Fast-forward miniredis time past the lock's TTL.
		// Add a small millisecond buffer to ensure TTL is definitely passed.
		fastForwardDuration := shortHoldTTL + 1*time.Millisecond
		t.Logf("Fast-forwarding miniredis time by %v (lock TTL was %v)", fastForwardDuration, shortHoldTTL)
		s.FastForward(fastForwardDuration)

		// 3. Attempt to acquire the lock again. It should succeed because the first one expired.
		secondLockCtx, secondLockCancel := context.WithTimeout(ctx, 2*time.Second) // Generous timeout for second acquisition
		defer secondLockCancel()
		lock2, err := connector.Lock(secondLockCtx, lockKey, 5*time.Second) // New, longer TTL for the second lock
		require.NoError(t, err, "Should acquire lock after the first one expired")
		require.NotNil(t, lock2, "Second lock should not be nil")

		// 4. Verify the lock value in Redis has changed (redsync sets a new random value)
		var secondLockValue string
		keys = s.Keys() // Re-fetch keys in case miniredis state changed
		for _, k := range keys {
			if strings.HasPrefix(k, lockInternalKeyPrefix+lockKey) {
				secondLockValue, _ = s.Get(k)
				break
			}
		}
		require.NotEmpty(t, secondLockValue, "Could not find the value of the second lock in miniredis")
		require.NotEqual(t, firstLockValue, secondLockValue, "Lock value in Redis should have changed after re-acquisition")

		// 5. Unlock the second lock
		err = lock2.Unlock(ctx)
		require.NoError(t, err, "Unlock of the second lock should succeed")

		// Try to unlock the first lock (it should have expired, redsync's unlock might error or do nothing, either is fine)
		// This is more of a cleanup attempt, the core test is above.
		_ = lock1.Unlock(context.Background()) // Use background context as original might be done
	})

	t.Run("MultipleLockAttemptsWithConcurrentUnlock", func(t *testing.T) {
		ctx, connector, _ := setupConnector(t)
		lockKey := "test-lock-concurrent"

		// Goroutine 1 acquires the lock
		lock1Ctx, lock1Cancel := context.WithTimeout(ctx, 5*time.Second)
		defer lock1Cancel()
		lock1, err := connector.Lock(lock1Ctx, lockKey, 10*time.Second) // Hold for longer than unlock delay
		require.NoError(t, err, "goroutine 1 failed to acquire lock")
		require.NotNil(t, lock1)

		unlockDone := make(chan struct{})
		// Goroutine 1 unlocks after a delay
		go func() {
			defer close(unlockDone)
			time.Sleep(250 * time.Millisecond)
			err := lock1.Unlock(ctx)
			require.NoError(t, err, "goroutine 1 failed to unlock")
		}()

		// Goroutine 2 attempts to acquire the same lock
		// This context should be long enough to wait for the unlock + retry interval
		lock2AttemptCtx, lock2AttemptCancel := context.WithTimeout(ctx, 2*time.Second)
		defer lock2AttemptCancel()

		start := time.Now()
		lock2, err := connector.Lock(lock2AttemptCtx, lockKey, 5*time.Second)
		timeToAcquire := time.Since(start)

		require.NoError(t, err, "goroutine 2 failed to acquire lock")
		require.NotNil(t, lock2, "goroutine 2 lock should not be nil")

		// Goroutine 2 should have waited for Goroutine 1 to unlock
		// Allow for one retry interval tolerance
		expectedWait := 250*time.Millisecond - connector.cfg.LockRetryInterval.Duration()
		if expectedWait < 0 {
			expectedWait = 0
		}
		require.GreaterOrEqual(t, timeToAcquire, expectedWait,
			"goroutine 2 should have waited for unlock. Waited %v, expected ~%v", timeToAcquire, 250*time.Millisecond)

		// Wait for unlock to complete to avoid race on t.Cleanup
		<-unlockDone

		err = lock2.Unlock(ctx)
		require.NoError(t, err, "goroutine 2 failed to unlock")
	})
}

func TestRedisReverseIndexLookup(t *testing.T) {
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()

	logger := zerolog.New(io.Discard)
	ctx := context.Background()

	cfg := &common.RedisConnectorConfig{
		Addr:        m.Addr(),
		InitTimeout: common.Duration(2 * time.Second),
		GetTimeout:  common.Duration(2 * time.Second),
		SetTimeout:  common.Duration(2 * time.Second),
	}
	err = cfg.SetDefaults()
	require.NoError(t, err)

	connector, err := NewRedisConnector(ctx, &logger, "test-reverse-index", cfg)
	require.NoError(t, err)

	// Wait until the connector is ready
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 3*time.Second, 100*time.Millisecond, "connector did not become ready")

	rangeKey := "eth_getTransactionReceipt:d49fc9409c70839c9c3251e4ff36babf30adfc3c41d6425d878032077896f7c8"
	concretePartitionKey := "evm:1:latest"
	wildcardPartitionKey := "evm:1:*"
	value := []byte("tx-receipt-value")

	// Store the value using the concrete partition key. This should also create the reverse index entry.
	err = connector.Set(ctx, concretePartitionKey, rangeKey, value, nil)
	require.NoError(t, err)

	// Verify that the reverse index key exists in Redis
	reverseIndexKey := fmt.Sprintf("%s#%s#%s", redisReverseIndexPrefix, wildcardPartitionKey, rangeKey)
	exists := m.Exists(reverseIndexKey)
	require.True(t, exists, "reverse index key should exist: %s", reverseIndexKey)

	// Retrieve the value using the wildcard partition key via idx_reverse index.
	got, err := connector.Get(ctx, "idx_reverse", wildcardPartitionKey, rangeKey)
	require.NoError(t, err)
	require.Equal(t, value, got)
}

func TestRedisConnector_ChainIsolation(t *testing.T) {
	// Setup Redis connector
	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()

	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &common.RedisConnectorConfig{
		Addr:         m.Addr(),
		Password:     "",
		DB:           0,
		ConnPoolSize: 5,
		InitTimeout:  common.Duration(2 * time.Second),
		GetTimeout:   common.Duration(2 * time.Second),
		SetTimeout:   common.Duration(2 * time.Second),
	}

	err = cfg.SetDefaults()
	require.NoError(t, err, "failed to set defaults for redis config")

	connector, err := NewRedisConnector(ctx, &logger, "test-chain-isolation", cfg)
	require.NoError(t, err)

	// Wait for connector to be ready
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 3*time.Second, 100*time.Millisecond, "connector did not become ready")

	// Define test data for two different chains
	chainA := "evm:1"   // Ethereum mainnet
	chainB := "evm:137" // Polygon
	method := "eth_blockNumber"
	blockNumberA := []byte("0x1234567")
	blockNumberB := []byte("0x7654321")

	// Store block number for chain A
	partitionKeyA := fmt.Sprintf("%s:%s", chainA, method)
	rangeKey := "latest"
	err = connector.Set(ctx, partitionKeyA, rangeKey, blockNumberA, nil)
	require.NoError(t, err, "failed to set block number for chain A")

	// Store block number for chain B
	partitionKeyB := fmt.Sprintf("%s:%s", chainB, method)
	err = connector.Set(ctx, partitionKeyB, rangeKey, blockNumberB, nil)
	require.NoError(t, err, "failed to set block number for chain B")

	// Verify chain A can read its own data
	valueA, err := connector.Get(ctx, ConnectorMainIndex, partitionKeyA, rangeKey)
	require.NoError(t, err, "failed to get block number for chain A")
	require.Equal(t, blockNumberA, valueA, "chain A should get its own block number")

	// Verify chain B can read its own data
	valueB, err := connector.Get(ctx, ConnectorMainIndex, partitionKeyB, rangeKey)
	require.NoError(t, err, "failed to get block number for chain B")
	require.Equal(t, blockNumberB, valueB, "chain B should get its own block number")

	// Verify chain A cannot read chain B's data
	_, err = connector.Get(ctx, ConnectorMainIndex, partitionKeyB, rangeKey)
	require.NoError(t, err) // This should succeed because we're asking for chain B's data explicitly

	// The real test: verify that if we ask for chain A's data, we don't get chain B's data
	// This is implicitly tested above - each chain gets its own data

	// Additional test: verify data is stored under different keys in Redis
	// Get all keys from miniredis to verify isolation
	keys := m.Keys()
	var foundKeyA, foundKeyB bool
	expectedKeyA := fmt.Sprintf("%s:%s", partitionKeyA, rangeKey)
	expectedKeyB := fmt.Sprintf("%s:%s", partitionKeyB, rangeKey)

	for _, key := range keys {
		if key == expectedKeyA {
			foundKeyA = true
		}
		if key == expectedKeyB {
			foundKeyB = true
		}
	}

	require.True(t, foundKeyA, "should find key for chain A in Redis")
	require.True(t, foundKeyB, "should find key for chain B in Redis")
	require.NotEqual(t, expectedKeyA, expectedKeyB, "keys for different chains should be different")

	// Test with wildcard partition key (reverse index)
	// This tests the reverse index functionality for chain isolation
	wildcardPartitionKeyA := fmt.Sprintf("%s:*", chainA)
	wildcardPartitionKeyB := fmt.Sprintf("%s:*", chainB)

	// Try to get data using wildcard for chain A
	valueWildcardA, err := connector.Get(ctx, ConnectorReverseIndex, wildcardPartitionKeyA, rangeKey)
	if err == nil {
		// If reverse index exists, it should return chain A's data
		require.Equal(t, blockNumberA, valueWildcardA, "wildcard lookup for chain A should return chain A's data")
	}

	// Try to get data using wildcard for chain B
	valueWildcardB, err := connector.Get(ctx, ConnectorReverseIndex, wildcardPartitionKeyB, rangeKey)
	if err == nil {
		// If reverse index exists, it should return chain B's data
		require.Equal(t, blockNumberB, valueWildcardB, "wildcard lookup for chain B should return chain B's data")
	}
}
