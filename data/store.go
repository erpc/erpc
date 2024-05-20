package data

import (
	"io"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

type Store interface {
	Get(key string) (string, error)
	GetWithReader(key string) (io.Reader, error)
	Set(key string, value string) (int, error)
	SetWithWriter(key string) (io.WriteCloser, error)
	Scan(prefix string) ([]string, error)
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
