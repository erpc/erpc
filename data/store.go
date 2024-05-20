package data

import (
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

type Store interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) (int, error)
	Scan(prefix string) ([][]byte, error)
	Delete(key string) error
}

func NewStore(cfg *config.StoreConfig) (Store, error) {
	switch cfg.Driver {
	case "memory":
		return NewMemoryStore(cfg.Memory), nil
	case "redis":
		return NewRedisStore(cfg.Redis), nil
	}

	return nil, common.NewErrInvalidStoreDriver(cfg.Driver)
}
