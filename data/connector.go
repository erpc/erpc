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
	Query(ctx context.Context, index, partitionKey, rangeKey string) ([]*DataRow, error)
	Delete(ctx context.Context, index, partitionKey, rangeKey string) error
}

func NewConnector(
	ctx context.Context,
	cfg *common.ConnectorConfig,
) (Connector, error) {
	switch cfg.Driver {
	// case "memory":
	// 	return NewMemoryStore(cfg.Memory), nil
	// case "redis":
	// 	return NewRedisStore(cfg.Redis), nil
	case "dynamodb":
		// return NewDynamoDBConnector(ctx, cfg.DynamoDB, keyResolver, valueResolver)
		return NewDynamoDBConnector(ctx, cfg.DynamoDB)
		// case "postgresql":
		// 	return NewPostgreSQLStore(cfg.PostgreSQL)
	}

	return nil, common.NewErrInvalidConnectorDriver(cfg.Driver)
}
