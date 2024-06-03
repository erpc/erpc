package data

import (
	"context"
	"io"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

const (
	ConnectorMainIndex    = "idx_main"
	ConnectorReverseIndex = "idx_reverse"
)

type Connector interface {
	GetWithReader(ctx context.Context, index, partitionKey, rangeKey string) (io.Reader, error)
	SetWithWriter(ctx context.Context, partitionKey, rangeKey string) (io.WriteCloser, error)
	Query(ctx context.Context, index, partitionKey, rangeKey string) ([]*DataValue, error)
	Delete(ctx context.Context, index, partitionKey, rangeKey string) error
}

type KeyResolver func(ctx context.Context, dv *DataValue) (string, string, error)
type ValueResolver func(ctx context.Context, dv *DataValue) (*DataValue, error)

func NewConnector(cfg *config.ConnectorConfig, keyResolver KeyResolver, valueResolver ValueResolver) (Connector, error) {
	switch cfg.Driver {
	// case "memory":
	// 	return NewMemoryStore(cfg.Memory), nil
	// case "redis":
	// 	return NewRedisStore(cfg.Redis), nil
	case "dynamodb":
		return NewDynamoDBConnector(cfg.DynamoDB, keyResolver, valueResolver)
		// case "postgresql":
		// 	return NewPostgreSQLStore(cfg.PostgreSQL)
	}

	return nil, common.NewErrInvalidConnectorDriver(cfg.Driver)
}
