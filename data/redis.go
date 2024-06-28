package data

import (
	"context"
	"fmt"
	"strings"

	"github.com/flair-sdk/erpc/common"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"
)

const (
	RedisDriverName    = "redis"
	reverseIndexPrefix = "rvi"
)

var _ Connector = (*RedisConnector)(nil)

type RedisConnector struct {
	client *redis.Client
}

func NewRedisConnector(
	ctx context.Context,
	cfg *common.RedisConnectorConfig,
) (*RedisConnector, error) {
	log.Debug().Msgf("creating RedisConnector with config: %+v", cfg)

	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})

	// Test the connection
	_, err := client.Ping(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return &RedisConnector{
		client: client,
	}, nil
}

func (r *RedisConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	log.Debug().Msgf("writing to Redis with partition key: %s and range key: %s", partitionKey, rangeKey)
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	rs := r.client.Set(ctx, key, value, 0)
	return rs.Err()
}

func (r *RedisConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	var err error
	var value string

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)

	if strings.Contains(key, "*") {
		keys, err := r.client.Keys(ctx, key).Result()
		if err != nil {
			return "", err
		}
		if len(keys) == 0 {
			return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), RedisDriverName)
		}
		key = keys[0]
	}

	log.Debug().Msgf("getting item from Redis with key: %s", key)
	value, err = r.client.Get(ctx, key).Result()

	if err == redis.Nil {
		return "", common.NewErrRecordNotFound(fmt.Sprintf("PK: %s RK: %s", partitionKey, rangeKey), RedisDriverName)
	} else if err != nil {
		return "", err
	}

	return value, nil
}

func (r *RedisConnector) Delete(ctx context.Context, index, partitionKey, rangeKey string) error {
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)

	if strings.Contains(key, "*") {
		keys, err := r.client.Keys(ctx, key).Result()
		if err != nil {
			return err
		}
		rs := r.client.Del(ctx, keys...)
		return rs.Err()
	} else {
		rs := r.client.Del(ctx, key)
		return rs.Err()
	}
}

func (r *RedisConnector) Close(ctx context.Context) error {
	return r.client.Close()
}