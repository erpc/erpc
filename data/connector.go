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
	IsNil() bool
}

type KeyValuePair struct {
	PartitionKey string
	RangeKey     string
	Value        []byte
}

// CounterInt64State is the canonical JSON payload stored for shared int64 counters.
//
// NOTE:
// - UpdatedAt is unix milliseconds; UpdatedAt <= 0 indicates uninitialized state.
// - UpdatedBy is best-effort (e.g., hostname/pod name) and is used for diagnostics only.
// - Value can be 0 for valid cases like earliest block = genesis; use UpdatedAt to check initialization.
type CounterInt64State struct {
	Value     int64  `json:"v"`
	UpdatedAt int64  `json:"t"`
	UpdatedBy string `json:"b,omitempty"`
}

type Connector interface {
	Id() string
	Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error)
	// Note if "value" is going to be stored/kept in memory for longer than response lifecycle it must be
	// copied to a new memory location because B2Str is used to provide "value" as a string reference.
	Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error
	Delete(ctx context.Context, partitionKey, rangeKey string) error
	List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error)
	Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error)
	WatchCounterInt64(ctx context.Context, key string) (<-chan CounterInt64State, func(), error)
	PublishCounterInt64(ctx context.Context, key string, value CounterInt64State) error
}

func NewConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.ConnectorConfig,
) (Connector, error) {
	var connector Connector
	var err error

	switch cfg.Driver {
	case common.DriverMemory:
		connector, err = NewMemoryConnector(ctx, logger, cfg.Id, cfg.Memory)
	case common.DriverRedis:
		connector, err = NewRedisConnector(ctx, logger, cfg.Id, cfg.Redis)
	case common.DriverDynamoDB:
		connector, err = NewDynamoDBConnector(ctx, logger, cfg.Id, cfg.DynamoDB)
	case common.DriverPostgreSQL:
		connector, err = NewPostgreSQLConnector(ctx, logger, cfg.Id, cfg.PostgreSQL)
	case common.DriverGrpc:
		connector, err = NewGrpcConnector(ctx, logger, cfg.Id, cfg.Grpc)
	default:
		if util.IsTest() && cfg.Driver == "mock" {
			connector, err = NewMockMemoryConnector(ctx, logger, "mock", cfg.Mock)
		} else {
			return nil, common.NewErrInvalidConnectorDriver(cfg.Driver)
		}
	}

	if err != nil {
		return nil, err
	}

	// Wrap with failsafe if configured
	if len(cfg.FailsafeForGets) > 0 || len(cfg.FailsafeForSets) > 0 {
		connector, err = NewFailsafeConnector(logger, connector, cfg.FailsafeForGets, cfg.FailsafeForSets)
		if err != nil {
			return nil, err
		}
	}

	return connector, nil
}
