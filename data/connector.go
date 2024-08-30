package data

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

const (
	ConnectorMainIndex    = "idx_main"
	ConnectorReverseIndex = "idx_reverse"
)

type Connector interface {
	Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error)
	Set(ctx context.Context, partitionKey, rangeKey, value string) error
	SetTTL(method string, ttlStr string) error
	HasTTL(method string) bool
	Delete(ctx context.Context, index, partitionKey, rangeKey string) error
}

func NewConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.ConnectorConfig,
) (Connector, error) {
	switch cfg.Driver {
	case "memory":
		return NewMemoryConnector(ctx, logger, cfg.Memory)
	case "redis":
		return NewRedisConnector(ctx, logger, cfg.Redis)
	case "dynamodb":
		return NewDynamoDBConnector(ctx, logger, cfg.DynamoDB)
	case "postgresql":
		return NewPostgreSQLConnector(ctx, logger, cfg.PostgreSQL)
	}

	return nil, common.NewErrInvalidConnectorDriver(cfg.Driver)
}
