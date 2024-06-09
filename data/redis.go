package data

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	RedisConnectorDriver = "redis"
)

type RedisStore struct {
	client *redis.Client
}

type RedisValueWriter struct {
	ctx    context.Context
	client *redis.Client
	key    string
	buffer strings.Builder
}

func (w *RedisValueWriter) Write(p []byte) (n int, err error) {
	w.buffer.Write(p)
	return len(p), nil
}

func (w *RedisValueWriter) Close() error {
	log.Trace().Msgf("RedisStore setting key: %s, value: %s", w.key, w.buffer.String())
	sts := w.client.Set(w.ctx, w.key, w.buffer.String(), 0)
	return sts.Err()
}

func NewRedisStore(cfg *config.RedisConnectorConfig) *RedisStore {
	log.Info().Msgf("initializing redis store on addr: %v", cfg.Addr)
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	return &RedisStore{client: rdb}
}

func (r *RedisStore) Get(ctx context.Context, key string) (string, error) {
	// log.Trace().Msgf("RedisStore getting key: %s", key)
	rs, err := r.client.Get(ctx, key).Result()

	if err != nil {
		if err == redis.Nil {
			return "", common.NewErrRecordNotFound(key, RedisConnectorDriver)
		}

		return "", err
	}

	return rs, nil
}

func (r *RedisStore) GetWithReader(ctx context.Context, key string) (io.Reader, error) {
	// log.Trace().Msgf("RedisStore getting key with reader: %s", key)
	value, err := r.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	return strings.NewReader(value), nil
}

func (r *RedisStore) Set(ctx context.Context, key string, value string) (int, error) {

	log.Debug().Msgf("SetWithWriter rrrrrrrrr ======: %v", r)
	log.Debug().Msgf("SetWithWriter INSII r.client ======: %v", r.client)

	// log.Trace().Msgf("RedisStore setting key: %s value: %s", key, value)
	sts := r.client.Set(ctx, key, value, 0)
	return 0, sts.Err()
}

func (r *RedisStore) SetWithWriter(ctx context.Context, key string) (io.WriteCloser, error) {
	// log.Trace().Msgf("RedisStore setting key with writer: %s", key)
	return &RedisValueWriter{client: r.client, key: key, ctx: ctx}, nil
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
