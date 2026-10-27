package data

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReadyRedisConnector starts a miniredis server and a connector bound to
// it, waiting until the connector reports ready.
func newReadyRedisConnector(t *testing.T, mutate func(*common.RedisConnectorConfig)) (context.Context, *RedisConnector, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	cfg := &common.RedisConnectorConfig{
		Addr:              s.Addr(),
		InitTimeout:       common.Duration(2 * time.Second),
		GetTimeout:        common.Duration(2 * time.Second),
		SetTimeout:        common.Duration(2 * time.Second),
		LockRetryInterval: common.Duration(50 * time.Millisecond),
	}
	if mutate != nil {
		mutate(cfg)
	}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := NewRedisConnector(ctx, &logger, "redis-test", cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, c.initializer.State(), "errors: %v", c.initializer.Errors())
	return ctx, c, s
}

func TestRedisConnector_SetGetDeleteAndTTL(t *testing.T) {
	ctx, c, s := newReadyRedisConnector(t, nil)
	assert.Equal(t, "redis-test", c.Id())

	_, err := c.Get(ctx, ConnectorMainIndex, "pk", "missing", nil)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)

	ttl := time.Minute
	big := bytes.Repeat([]byte("x"), 2048)
	require.NoError(t, c.Set(ctx, "pk", "big", big, &ttl))
	got, err := c.Get(ctx, ConnectorMainIndex, "pk", "big", nil)
	require.NoError(t, err)
	assert.Equal(t, big, got)
	assert.Equal(t, time.Minute, s.TTL("pk:big"))

	s.FastForward(2 * time.Minute)
	_, err = c.Get(ctx, ConnectorMainIndex, "pk", "big", nil)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "expired key must be a miss, got %v", err)

	require.NoError(t, c.Set(ctx, "pk", "keep", []byte("v"), nil))
	assert.Equal(t, time.Duration(0), s.TTL("pk:keep"), "nil ttl stores a persistent key")
	require.NoError(t, c.Delete(ctx, "pk", "keep"))
	assert.False(t, s.Exists("pk:keep"))
}

func TestRedisConnector_ReverseIndexResolution(t *testing.T) {
	ctx, c, s := newReadyRedisConnector(t, nil)
	rctx := WithReverseIndex(ctx, "evm:1", "")
	wildcard := common.CachePartitionKey("evm:1", "", "*")
	short := 30 * time.Second

	require.NoError(t, c.Set(rctx, "evm:1:100", "rk-ttl", []byte("with-ttl"), &short))
	require.NoError(t, c.Set(rctx, "evm:1:200", "rk-persist", []byte("persistent"), nil))

	got, err := c.Get(ctx, ConnectorReverseIndex, wildcard, "rk-ttl", nil)
	require.NoError(t, err)
	assert.Equal(t, "with-ttl", string(got))

	got, err = c.Get(ctx, ConnectorReverseIndex, wildcard, "rk-persist", nil)
	require.NoError(t, err)
	assert.Equal(t, "persistent", string(got))

	t.Run("dangling reverse entry is a miss", func(t *testing.T) {
		// The reverse entry outlives the primary key it points to.
		s.Del("evm:1:200:rk-persist")
		_, err := c.Get(ctx, ConnectorReverseIndex, wildcard, "rk-persist", nil)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
	})

	t.Run("no reverse entry falls back to the literal key", func(t *testing.T) {
		_, err := c.Get(ctx, ConnectorReverseIndex, wildcard, "never-set", nil)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
	})

	t.Run("delete removes primary and reverse entries", func(t *testing.T) {
		require.NoError(t, c.Delete(rctx, "evm:1:100", "rk-ttl"))
		assert.False(t, s.Exists("evm:1:100:rk-ttl"))
		assert.False(t, s.Exists(redisReverseIndexPrefix+"#"+wildcard+"#rk-ttl"))
	})
}

func TestRedisConnector_List(t *testing.T) {
	ctx, c, _ := newReadyRedisConnector(t, nil)
	rctx := WithReverseIndex(ctx, "evm:1", "")
	require.NoError(t, c.Set(ctx, "a", "1", []byte("va"), nil))
	require.NoError(t, c.Set(ctx, "b", "2", []byte("vb"), nil))
	require.NoError(t, c.Set(rctx, "evm:1:100", "rk", []byte("vblock"), nil))

	t.Run("main index lists primary keys split on the first colon", func(t *testing.T) {
		items, next, err := c.List(ctx, ConnectorMainIndex, 100, "")
		require.NoError(t, err)
		assert.Empty(t, next)
		got := map[string]string{}
		for _, it := range items {
			got[it.PartitionKey+"|"+it.RangeKey] = string(it.Value)
		}
		assert.Equal(t, "va", got["a|1"])
		assert.Equal(t, "vb", got["b|2"])
		assert.Equal(t, "vblock", got["evm|1:100:rk"])
	})

	t.Run("reverse index lists only reverse entries", func(t *testing.T) {
		items, _, err := c.List(ctx, ConnectorReverseIndex, 100, "")
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, common.CachePartitionKey("evm:1", "", "*"), items[0].PartitionKey)
		assert.Equal(t, "rk", items[0].RangeKey)
		assert.Equal(t, "evm:1:100", string(items[0].Value), "reverse entry value is the concrete partition key")
	})

	t.Run("cursor token pages through keys", func(t *testing.T) {
		seen := map[string]bool{}
		token := ""
		for range 10 {
			items, next, err := c.List(ctx, ConnectorMainIndex, 1, token)
			require.NoError(t, err)
			for _, it := range items {
				seen[it.PartitionKey+":"+it.RangeKey] = true
			}
			if next == "" {
				break
			}
			token = next
		}
		keys := make([]string, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		assert.Contains(t, keys, "a:1")
		assert.Contains(t, keys, "b:2")
	})

	t.Run("non-numeric token is rejected", func(t *testing.T) {
		_, _, err := c.List(ctx, ConnectorMainIndex, 1, "abc")
		require.ErrorContains(t, err, "invalid pagination token")
	})
}

func TestRedisConnector_LockSemantics(t *testing.T) {
	ctx, c, s := newReadyRedisConnector(t, nil)

	t.Run("held lock fails fast once tries are exhausted", func(t *testing.T) {
		lock, err := c.Lock(ctx, "held", 300*time.Millisecond)
		require.NoError(t, err)
		assert.False(t, lock.IsNil())
		defer func() { _ = lock.Unlock(ctx) }()

		start := time.Now()
		_, err = c.Lock(ctx, "held", 300*time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to acquire lock for key 'held'")
		assert.Less(t, time.Since(start), 5*time.Second)
	})

	t.Run("deadline shorter than the operation buffer still bounds the wait", func(t *testing.T) {
		lock, err := c.Lock(ctx, "short-deadline", 5*time.Second)
		require.NoError(t, err)
		defer func() { _ = lock.Unlock(ctx) }()

		dctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		_, err = c.Lock(dctx, "short-deadline", 5*time.Second)
		require.Error(t, err)
	})

	t.Run("long deadline is capped by the lock ttl", func(t *testing.T) {
		dctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		lock, err := c.Lock(dctx, "long-deadline", time.Second)
		require.NoError(t, err)
		require.NoError(t, lock.Unlock(ctx))
	})

	t.Run("unlocking an expired lock reports an error", func(t *testing.T) {
		lock, err := c.Lock(ctx, "expires", time.Second)
		require.NoError(t, err)
		s.FastForward(2 * time.Second)
		err = lock.Unlock(ctx)
		require.Error(t, err)
	})

	t.Run("unlocking a lock taken over by another owner is refused", func(t *testing.T) {
		lock, err := c.Lock(ctx, "stolen", 5*time.Second)
		require.NoError(t, err)
		require.NoError(t, s.Set("lock:stolen", "someone-else"))
		require.Error(t, lock.Unlock(ctx))
	})

	var nilLock *redisLock
	assert.True(t, nilLock.IsNil())
}

func TestRedisConnector_WatchAndPublishCounter(t *testing.T) {
	ctx, c, _ := newReadyRedisConnector(t, nil)
	key := "shared/latest"

	stored := CounterInt64State{Value: 5, UpdatedAt: 100, UpdatedBy: "seed"}
	payload, err := common.SonicCfg.Marshal(stored)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, key, "value", payload, nil))

	updates, cleanup, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	defer cleanup()

	select {
	case st := <-updates:
		assert.Equal(t, stored, st, "subscriber receives the stored value first")
	case <-time.After(5 * time.Second):
		t.Fatal("no initial value")
	}

	pub := CounterInt64State{Value: 6, UpdatedAt: 200, UpdatedBy: "pub"}
	require.NoError(t, c.PublishCounterInt64(ctx, key, pub))
	select {
	case st := <-updates:
		assert.Equal(t, pub, st)
	case <-time.After(5 * time.Second):
		t.Fatal("published value not delivered")
	}

	t.Run("unparsable and zero-timestamp messages are ignored", func(t *testing.T) {
		require.NoError(t, c.client.Publish(ctx, "counter:"+key, "garbage").Err())
		require.NoError(t, c.client.Publish(ctx, "counter:"+key, `{"v":9,"t":0}`).Err())
		valid := CounterInt64State{Value: 7, UpdatedAt: 300}
		require.NoError(t, c.PublishCounterInt64(ctx, key, valid))
		select {
		case st := <-updates:
			assert.Equal(t, valid, st, "only the valid message reaches the subscriber")
		case <-time.After(5 * time.Second):
			t.Fatal("valid publish not delivered")
		}
	})

	t.Run("poll pushes the stored value to subscribers", func(t *testing.T) {
		polled := CounterInt64State{Value: 8, UpdatedAt: 400}
		b, err := common.SonicCfg.Marshal(polled)
		require.NoError(t, err)
		require.NoError(t, c.Set(ctx, key, "value", b, nil))
		c.pubsubManager.pollAllKeys()
		select {
		case st := <-updates:
			assert.Equal(t, polled, st)
		case <-time.After(5 * time.Second):
			t.Fatal("poll did not deliver")
		}
	})
}

func TestRedisPubSubManager_GetCurrentValue(t *testing.T) {
	ctx, c, _ := newReadyRedisConnector(t, nil)
	m := c.pubsubManager

	cases := []struct {
		name   string
		stored []byte
		wantOk bool
	}{
		{name: "absent", stored: nil},
		{name: "not json", stored: []byte("{")},
		{name: "zero timestamp", stored: []byte(`{"v":1,"t":0}`)},
		{name: "valid", stored: []byte(`{"v":3,"t":9,"b":"x"}`), wantOk: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "gcv-" + tc.name
			if tc.stored != nil {
				require.NoError(t, c.Set(ctx, key, "value", tc.stored, nil))
			}
			st, ok, err := m.getCurrentValue(ctx, key)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOk, ok)
			if tc.wantOk {
				assert.Equal(t, CounterInt64State{Value: 3, UpdatedAt: 9, UpdatedBy: "x"}, st)
			}
		})
	}
}

func TestRedisPubSubManager_ReconnectsAfterServerRestart(t *testing.T) {
	ctx, c, s := newReadyRedisConnector(t, nil)
	key := "restart"
	updates, cleanup, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	defer cleanup()

	addr := s.Addr()
	s.Close()
	require.NoError(t, s.StartAddr(addr))

	// The message loop must notice the dropped connection and resubscribe;
	// keep publishing until a value comes through.
	n := int64(0)
	require.Eventually(t, func() bool {
		n++
		_ = c.PublishCounterInt64(ctx, key, CounterInt64State{Value: n, UpdatedAt: n})
		select {
		case st := <-updates:
			return st.UpdatedAt > 0
		case <-time.After(100 * time.Millisecond):
			return false
		}
	}, 20*time.Second, 50*time.Millisecond, "subscriber must keep receiving after a redis restart")
}

func TestRedisPubSubManager_StopClosesSubscribers(t *testing.T) {
	s := miniredis.RunT(t)
	cfg := &common.RedisConnectorConfig{Addr: s.Addr()}
	require.NoError(t, cfg.SetDefaults())
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	c, err := NewRedisConnector(ctx, &logger, "redis-stop", cfg)
	require.NoError(t, err)

	_, cleanup, err := c.pubsubManager.Subscribe("k")
	require.NoError(t, err)
	defer cleanup()
	v, ok := c.pubsubManager.subscribers.Load("k")
	require.True(t, ok)
	sc := v.([]*subscriberChannel)[0]

	cancel()
	select {
	case <-sc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber not closed when the app context ends")
	}
	_, _, err = c.pubsubManager.Subscribe("k")
	require.ErrorContains(t, err, "pubsub manager is stopped")
}

func TestRedisPubSubManager_SubscribeRestartsStoppedManager(t *testing.T) {
	_, c, _ := newReadyRedisConnector(t, nil)
	m := c.pubsubManager
	m.stop()
	require.False(t, m.running.Load())

	ch, cleanup, err := m.Subscribe("k")
	require.NoError(t, err)
	defer cleanup()
	assert.NotNil(t, ch)
	assert.True(t, m.running.Load(), "Subscribe must restart a stopped manager")

	t.Run("start fails without a client", func(t *testing.T) {
		lg := zerolog.Nop()
		bare := &RedisPubSubManager{connector: &RedisConnector{}, logger: &lg, appCtx: context.Background()}
		_, _, err := bare.Subscribe("k")
		require.ErrorContains(t, err, "redis client not available")
		assert.False(t, bare.running.Load())
	})
}

func TestRedisConnector_OperationsFailAfterServerLoss(t *testing.T) {
	ctx, c, s := newReadyRedisConnector(t, func(cfg *common.RedisConnectorConfig) {
		cfg.GetTimeout = common.Duration(300 * time.Millisecond)
		cfg.SetTimeout = common.Duration(300 * time.Millisecond)
	})
	s.Close()

	require.Error(t, c.Set(ctx, "pk", "rk", []byte("v"), nil))
	_, err := c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
	assert.Error(t, err)
	assert.False(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "transport failure is not a miss")
	assert.Error(t, c.Delete(ctx, "pk", "rk"))
	_, _, err = c.List(ctx, ConnectorMainIndex, 10, "")
	assert.Error(t, err)
	assert.Error(t, c.PublishCounterInt64(ctx, "k", CounterInt64State{Value: 1, UpdatedAt: 1}))
}

func TestRedisConnector_MarkConnectionLost(t *testing.T) {
	_, c, _ := newReadyRedisConnector(t, nil)

	benign := []error{
		nil,
		redis.Nil,
		redis.TxFailedErr,
		context.Canceled,
		context.DeadlineExceeded,
		common.NewErrRecordNotFound("pk", "rk", RedisDriverName),
		errors.New("WRONGTYPE Operation against a key holding the wrong kind of value"),
	}
	for _, err := range benign {
		c.markConnectionAsLostIfNecessary(err)
		require.Equal(t, util.StateReady, c.initializer.State(), "%v must not mark the connection lost", err)
	}

	c.markConnectionAsLostIfNecessary(errors.New("dial tcp: connection refused"))
	require.NotEqual(t, util.StateReady, c.initializer.State())
	assert.Error(t, c.checkReady())

	// The retry finds the existing client healthy and keeps it.
	client := c.client
	require.Eventually(t, func() bool { return c.initializer.State() == util.StateReady }, 20*time.Second, 50*time.Millisecond)
	assert.Same(t, client, c.client, "a healthy client must be reused, not replaced")

	(&RedisConnector{}).markConnectionAsLostIfNecessary(errors.New("connection refused")) // nil initializer is a no-op
}

func TestRedisConnector_NotReadyEntryPoints(t *testing.T) {
	ctx := context.Background()
	lg := zerolog.Nop()
	cases := map[string]*RedisConnector{
		"nil initializer": {logger: &lg},
		"no client":       {logger: &lg, initializer: readyInitializer(t)},
		"no redsync":      {logger: &lg, initializer: readyInitializer(t), client: redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, c.Set(ctx, "pk", "rk", nil, nil))
			_, err := c.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
			assert.Error(t, err)
			_, err = c.Lock(ctx, "k", time.Second)
			assert.Error(t, err)
			assert.Error(t, c.Delete(ctx, "pk", "rk"))
			_, _, err = c.List(ctx, ConnectorMainIndex, 1, "")
			assert.Error(t, err)
			_, _, err = c.WatchCounterInt64(ctx, "k")
			assert.Error(t, err)
			assert.Error(t, c.PublishCounterInt64(ctx, "k", CounterInt64State{}))
		})
	}

	t.Run("watch without pubsub manager", func(t *testing.T) {
		_, c, _ := newReadyRedisConnector(t, nil)
		c.pubsubManager = nil
		_, _, err := c.WatchCounterInt64(ctx, "k")
		require.ErrorContains(t, err, "pubsub manager not initialized")
	})
}

// readyInitializer returns an initializer whose single task has succeeded.
func readyInitializer(t *testing.T) *util.Initializer {
	t.Helper()
	lg := zerolog.Nop()
	i := util.NewInitializer(context.Background(), &lg, nil)
	require.NoError(t, i.ExecuteTasks(context.Background(), util.NewBootstrapTask("ok", func(context.Context) error { return nil })))
	require.Equal(t, util.StateReady, i.State())
	return i
}

func TestRedisConnector_InitFailures(t *testing.T) {
	logger := zerolog.New(io.Discard)
	newConn := func(t *testing.T, cfg *common.RedisConnectorConfig) *RedisConnector {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		c, err := NewRedisConnector(ctx, &logger, "redis-init", cfg)
		require.NoError(t, err)
		return c
	}

	t.Run("unparsable uri is fatal", func(t *testing.T) {
		c := newConn(t, &common.RedisConnectorConfig{URI: "mysql://nope"})
		assert.Equal(t, util.StateFatal, c.initializer.State())
	})

	t.Run("unsupported iam auth mode is fatal", func(t *testing.T) {
		c := newConn(t, &common.RedisConnectorConfig{
			URI:     "rediss://127.0.0.1:1",
			IAMAuth: &common.RedisIAMAuthConfig{Enabled: true, CacheName: "c", UserID: "u", Region: "us-east-1", Auth: &common.AwsAuthConfig{Mode: "bogus"}},
		})
		assert.Equal(t, util.StateFatal, c.initializer.State())
	})

	t.Run("iam auth against an unreachable host is retryable", func(t *testing.T) {
		c := newConn(t, &common.RedisConnectorConfig{
			URI:         "rediss://127.0.0.1:1",
			InitTimeout: common.Duration(200 * time.Millisecond),
			IAMAuth: &common.RedisIAMAuthConfig{Enabled: true, CacheName: "c", UserID: "u", Region: "us-east-1",
				Auth: &common.AwsAuthConfig{Mode: "secret", AccessKeyID: "AK", SecretAccessKey: "SK"}},
		})
		st := c.initializer.State()
		assert.NotEqual(t, util.StateReady, st)
		assert.NotEqual(t, util.StateFatal, st)
	})

	t.Run("uri timeouts fill unset connector timeouts", func(t *testing.T) {
		s := miniredis.RunT(t)
		c := newConn(t, &common.RedisConnectorConfig{URI: "redis://" + s.Addr() + "?dial_timeout=2s&read_timeout=3s&write_timeout=4s"})
		require.Equal(t, util.StateReady, c.initializer.State())
		assert.Equal(t, 2*time.Second, c.initTimeout)
		assert.Equal(t, 3*time.Second, c.getTimeout)
		assert.Equal(t, 4*time.Second, c.setTimeout)
	})
}

// writeSelfSignedCert writes a self-signed server cert/key for 127.0.0.1 and
// returns their paths plus a TLS config that serves it.
func writeSelfSignedCert(t *testing.T) (certFile, keyFile string, serverCfg *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDer, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}), 0o600))
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)
	return certFile, keyFile, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
}

func TestRedisConnector_TLS(t *testing.T) {
	certFile, keyFile, serverCfg := writeSelfSignedCert(t)
	s, err := miniredis.RunTLS(serverCfg)
	require.NoError(t, err)
	t.Cleanup(s.Close)
	logger := zerolog.New(io.Discard)

	connect := func(t *testing.T, cfg *common.RedisConnectorConfig) *RedisConnector {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		cfg.InitTimeout = common.Duration(2 * time.Second)
		require.NoError(t, cfg.SetDefaults())
		c, err := NewRedisConnector(ctx, &logger, "redis-tls", cfg)
		require.NoError(t, err)
		return c
	}

	t.Run("yaml tls with the server CA connects over redis://", func(t *testing.T) {
		c := connect(t, &common.RedisConnectorConfig{URI: "redis://" + s.Addr(), TLS: &common.TLSConfig{Enabled: true, CAFile: certFile}})
		require.Equal(t, util.StateReady, c.initializer.State(), "errors: %v", c.initializer.Errors())
		require.NoError(t, c.Set(context.Background(), "pk", "rk", []byte("tls"), nil))
	})

	t.Run("rediss:// merges yaml CA and client cert", func(t *testing.T) {
		c := connect(t, &common.RedisConnectorConfig{URI: "rediss://" + s.Addr(), TLS: &common.TLSConfig{Enabled: true, CAFile: certFile, CertFile: certFile, KeyFile: keyFile}})
		require.Equal(t, util.StateReady, c.initializer.State(), "errors: %v", c.initializer.Errors())
	})

	t.Run("untrusted server certificate is fatal with guidance", func(t *testing.T) {
		c := connect(t, &common.RedisConnectorConfig{URI: "redis://" + s.Addr(), TLS: &common.TLSConfig{Enabled: true}})
		require.Equal(t, util.StateFatal, c.initializer.State())
		assert.Contains(t, c.initializer.Errors().Error(), "tls.caFile")
	})

	t.Run("certificate signed by a different CA names the caFile", func(t *testing.T) {
		otherCA, otherKey, _ := writeSelfSignedCert(t)
		c := connect(t, &common.RedisConnectorConfig{URI: "redis://" + s.Addr(), TLS: &common.TLSConfig{Enabled: true, CAFile: otherCA, CertFile: otherCA, KeyFile: otherKey}})
		require.Equal(t, util.StateFatal, c.initializer.State())
		msg := c.initializer.Errors().Error()
		assert.Contains(t, msg, "signed by the provided 'tls.caFile'")
		assert.Contains(t, msg, "client certificate")
	})

	t.Run("rediss:// without yaml tls uses the uri default", func(t *testing.T) {
		c := connect(t, &common.RedisConnectorConfig{URI: "rediss://" + s.Addr()})
		// The URI-implied config verifies against system CAs, which do not
		// trust this self-signed server.
		assert.NotEqual(t, util.StateReady, c.initializer.State())
	})
}

func TestRedisZerologAdapter_LevelsByMessage(t *testing.T) {
	var buf bytes.Buffer
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	lg := zerolog.New(&buf).Level(zerolog.DebugLevel)
	a := &zerologAdapter{logger: &lg}

	a.Printf(context.Background(), "redis: discarding bad PubSub connection: %s\n", "EOF")
	assert.Contains(t, buf.String(), `"level":"warn"`)
	assert.NotContains(t, buf.String(), `\n"`, "trailing newline is trimmed")

	buf.Reset()
	a.Printf(context.Background(), "redis: pool stats %d", 3)
	assert.Empty(t, buf.String(), "routine messages are logged at trace level")
}
