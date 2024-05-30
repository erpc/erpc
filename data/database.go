package data

import "github.com/flair-sdk/erpc/config"

type Database struct {
	EvmJsonRpcCache *EvmJsonRpcCache
	// EvmBlockIngestions *EvmBlockIngestions
	// RateLimitSnapshots *RateLimitSnapshots
}

func NewDatabase(cfg *config.DatabaseConfig) (*Database, error) {
	db := &Database{}

	if cfg.EvmJsonRpcCache != nil {
		c, err := NewEvmJsonRpcCache(cfg.EvmJsonRpcCache)
		if err != nil {
			return nil, err
		}
		db.EvmJsonRpcCache = c
	}

	return db, nil
}
