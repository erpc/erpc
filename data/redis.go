package data

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/flair-sdk/erpc/config"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

type RedisStore struct {
	client *redis.Client
}

type RedisValueWriter struct {
	ctx         context.Context
	redisClient *redis.Client
	key         string
}

func (w *RedisValueWriter) Write(p []byte) (n int, err error) {
	// log.Trace().Msgf("writing to RedisValueWriter key: %s", w.key)
	err = w.redisClient.Append(w.ctx, w.key, string(p)).Err()
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *RedisValueWriter) Close() error {
	return nil
}

func NewRedisStore(cfg *config.RedisStoreConfig) *RedisStore {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	return &RedisStore{client: rdb}
}

func (r *RedisStore) Get(ctx context.Context, key string) (string, error) {
	// log.Trace().Msgf("RedisStore getting key: %s", key)
	return r.client.Get(ctx, key).Result()
}

func (r *RedisStore) GetWithReader(ctx context.Context, key string) (io.Reader, error) {
	log.Trace().Msgf("RedisStore getting key with reader: %s", key)
	value, err := r.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	return strings.NewReader(value), nil
}

func (r *RedisStore) Set(ctx context.Context, key string, value string) (int, error) {
	// log.Trace().Msgf("RedisStore setting key: %s, value: %s", key, value)
	sts := r.client.Set(ctx, key, value, 0)
	return 0, sts.Err()
}

func (r *RedisStore) SetWithWriter(ctx context.Context, key string) (io.WriteCloser, error) {
	// log.Trace().Msgf("RedisStore setting key with writer: %s", key)
	// Ensure the key is deleted before starting to write new data
	err := r.client.Del(ctx, key).Err()
	if err != nil {
		return nil, err
	}
	return &RedisValueWriter{redisClient: r.client, key: key}, nil
}

func (r *RedisStore) Scan(ctx context.Context, prefix string) ([]string, error) {
	var values []string

	iter := r.client.Scan(ctx, 0, fmt.Sprintf("%s:*", prefix), 0).Iterator()
	for iter.Next(ctx) {
		values = append(values, iter.Val())
	}

	if err := iter.Err(); err != nil {
		return values, err
	}

	return values, nil
}

func (r *RedisStore) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}
