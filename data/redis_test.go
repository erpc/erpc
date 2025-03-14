package data

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
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
