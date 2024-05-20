package data

import (
	"context"

	"github.com/flair-sdk/erpc/config"
	"github.com/redis/go-redis/v9"
)

var ctx = context.Background()

type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(cfg *config.RedisStore) *RedisStore {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	return &RedisStore{client: rdb}
}

func (r *RedisStore) Get(key string) ([]byte, error) {
	return r.client.Get(ctx, key).Bytes()
}

func (r *RedisStore) Set(key string, value []byte) (int, error) {
	sts := r.client.Set(ctx, key, value, 0)
	return 0, sts.Err()
}

func (r *RedisStore) Scan(prefix string) ([][]byte, error) {
	var values [][]byte
	var cur uint64

	for {
		keys, cur, err := r.client.Scan(ctx, cur, prefix+"*", 100).Result()
		if err != nil {
			return nil, err
		}

		for _, key := range keys {
			value, err := r.Get(key)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}

		if cur == 0 {
			break
		}
	}

	return values, nil
}

func (r *RedisStore) Delete(key string) error {
	return r.client.Del(ctx, key).Err()
}
