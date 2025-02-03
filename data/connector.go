package data

import (
	"context"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const (
	ConnectorMainIndex    = "idx_main"
	ConnectorReverseIndex = "idx_reverse"
)

type DistributedLock interface {
	Unlock(ctx context.Context) error
}

type Connector interface {
	Id() string
	Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error)
	Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error
	Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error)
	WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error)
	PublishCounterInt64(ctx context.Context, key string, value int64) error
}

func NewConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.ConnectorConfig,
) (Connector, error) {
	switch cfg.Driver {
	case common.DriverMemory:
		return NewMemoryConnector(ctx, logger, cfg.Id, cfg.Memory)
	case common.DriverRedis:
		return NewRedisConnector(ctx, logger, cfg.Id, cfg.Redis)
	case common.DriverDynamoDB:
		return NewDynamoDBConnector(ctx, logger, cfg.Id, cfg.DynamoDB)
	case common.DriverPostgreSQL:
		return NewPostgreSQLConnector(ctx, logger, cfg.Id, cfg.PostgreSQL)
	}

	if util.IsTest() && cfg.Driver == "mock" {
		return NewMockMemoryConnector(ctx, logger, "mock", &common.MemoryConnectorConfig{
			MaxItems: 1000,
		}, 100*time.Millisecond)
	}

	return nil, common.NewErrInvalidConnectorDriver(cfg.Driver)
}
