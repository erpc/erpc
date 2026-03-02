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
)

func setupRedisConnectorForBenchmark(b *testing.B, connectorID string) (context.Context, *RedisConnector) {
	b.Helper()

	m, err := miniredis.Run()
	if err != nil {
		b.Fatalf("failed to start miniredis: %v", err)
	}
	b.Cleanup(m.Close)

	logger := zerolog.New(io.Discard)
	ctx := context.Background()

	cfg := &common.RedisConnectorConfig{
		Addr:        m.Addr(),
		InitTimeout: common.Duration(2 * time.Second),
		GetTimeout:  common.Duration(2 * time.Second),
		SetTimeout:  common.Duration(2 * time.Second),
	}
	if err := cfg.SetDefaults(); err != nil {
		b.Fatalf("failed to set redis defaults: %v", err)
	}

	connector, err := NewRedisConnector(ctx, &logger, connectorID, cfg)
	if err != nil {
		b.Fatalf("failed to create redis connector: %v", err)
	}

	readyDeadline := time.Now().Add(3 * time.Second)
	for connector.initializer.State() != util.StateReady {
		if time.Now().After(readyDeadline) {
			b.Fatalf("connector did not become ready, state=%s", connector.initializer.State())
		}
		time.Sleep(10 * time.Millisecond)
	}

	return ctx, connector
}

func BenchmarkRedisConnectorGetReverseIndex_Hit(b *testing.B) {
	ctx, connector := setupRedisConnectorForBenchmark(b, "bench-reverse-index-hit")

	const (
		rangeKey             = "eth_getTransactionReceipt:bench-hash"
		concretePartitionKey = "evm:1:latest"
		wildcardPartitionKey = "evm:1:*"
	)
	value := []byte("benchmark-value")

	if err := connector.Set(ctx, concretePartitionKey, rangeKey, value, nil); err != nil {
		b.Fatalf("failed to seed benchmark value: %v", err)
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(value)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		got, err := connector.Get(ctx, ConnectorReverseIndex, wildcardPartitionKey, rangeKey, nil)
		if err != nil {
			b.Fatalf("reverse lookup failed: %v", err)
		}
		if len(got) != len(value) {
			b.Fatalf("unexpected value length: got=%d want=%d", len(got), len(value))
		}
	}
}
