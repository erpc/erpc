package data

import (
	"context"
	"io"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

type Store interface {
	Get(ctx context.Context, key string) (string, error)
	GetWithReader(ctx context.Context, key string) (io.Reader, error)
	Set(ctx context.Context, key string, value string) (int, error)
	SetWithWriter(ctx context.Context, key string) (io.WriteCloser, error)
	Scan(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, key string) error
}

func NewStore(cfg *config.StoreConfig) (Store, error) {
	switch cfg.Driver {
	case "memory":
		return NewMemoryStore(cfg.Memory), nil
	case "redis":
		return NewRedisStore(cfg.Redis), nil
	case "dynamodb":
		return NewDynamoDBStore(cfg.DynamoDB)
	}

	return nil, common.NewErrInvalidStoreDriver(cfg.Driver)
}
