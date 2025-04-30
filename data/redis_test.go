package data

import (
	"context"
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
		err = connector.Set(ctx, "testPK", "testRK", "hello", nil)
		require.NoError(t, err, "Set should succeed after successful initialization")

		val, err := connector.Get(ctx, "", "testPK", "testRK")
		require.NoError(t, err, "Get should succeed for existing key")
		require.Equal(t, "hello", val)
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

		// The connector’s initializer should NOT be ready if it failed to connect.
		state := connector.initializer.State()
		require.NotEqual(t, util.StateReady, state, "connector should not be in ready state")

		// Attempting to call Set or Get here should result in an error because checkReady will fail.
		err = connector.Set(ctx, "testPK", "testRK", "value", nil)
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

		//require.Eventually loop and check whether the connector’s initializer has transitioned to READY.
		require.Eventually(t, func() bool {
			return connector.initializer.State() == util.StateReady
		}, 5*time.Second, 200*time.Millisecond,
			"Connector did not become READY within the expected timeframe")

		// At this point, the connector should be ready. Let's confirm by doing a Set/Get.
		err = connector.Set(ctx, "eventual", "rk", "recovered-value", nil)
		require.NoError(t, err, "Set should succeed if the connector is truly ready")

		val, err := connector.Get(ctx, "", "eventual", "rk")
		require.NoError(t, err, "Get should succeed for the newly-set key")
		require.Equal(t, "recovered-value", val)
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
			name: "uri only with 0 timeout in config and no timeout in uri - invalid",
			conn: common.RedisConnectorConfig{
				URI:         "redis://user:pass@<addr>/0",
				InitTimeout: common.Duration(0 * time.Second),
				GetTimeout:  common.Duration(0 * time.Second),
				SetTimeout:  common.Duration(0 * time.Second),
			},
			expectSuccess: false,
			checkConnect:  false,
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
			name: "discrete fields username without password - invalid",
			conn: common.RedisConnectorConfig{
				Addr:        "<addr>",
				Username:    redisUser,
				Password:    "",
				InitTimeout: common.Duration(2 * time.Second),
				GetTimeout:  common.Duration(2 * time.Second),
				SetTimeout:  common.Duration(2 * time.Second),
			},
			expectSuccess: false,
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
