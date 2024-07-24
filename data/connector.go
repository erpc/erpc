package data

import (
	"context"

	"github.com/flair-sdk/erpc/common"
)

const (
	ConnectorMainIndex    = "idx_main"
	ConnectorReverseIndex = "idx_reverse"
)

type Connector interface {
	Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error)
	Set(ctx context.Context, partitionKey, rangeKey, value string) error
	Delete(ctx context.Context, index, partitionKey, rangeKey string) error
	// Close(ctx context.Context) error
}

func NewConnector(
	ctx context.Context,
	cfg *common.ConnectorConfig,
) (Connector, error) {
	switch cfg.Driver {
	case "memory":
		return NewMemoryConnector(ctx, cfg.Memory)
	case "redis":
		return NewRedisConnector(ctx, cfg.Redis)
	case "dynamodb":
		return NewDynamoDBConnector(ctx, cfg.DynamoDB)
	case "postgresql":
		return NewPostgreSQLConnector(ctx, cfg.PostgreSQL)
	}

	return nil, common.NewErrInvalidConnectorDriver(cfg.Driver)
}
