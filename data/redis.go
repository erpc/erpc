package data

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

const (
	RedisDriverName    = "redis"
	reverseIndexPrefix = "rvi"
)

var _ Connector = (*RedisConnector)(nil)

type RedisConnector struct {
	logger *zerolog.Logger
	client *redis.Client
}

func NewRedisConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.RedisConnectorConfig,
) (*RedisConnector, error) {
	logger.Debug().Msgf("creating RedisConnector with config: %+v", cfg)

	options := &redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}

	if cfg.TLS != nil && cfg.TLS.Enabled {
		tlsConfig, err := createTLSConfig(cfg.TLS)
		if err != nil {
			return nil, fmt.Errorf("failed to create TLS config: %w", err)
		}
		options.TLSConfig = tlsConfig
	}

	client := redis.NewClient(options)

	// Test the connection
	_, err := client.Ping(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %w", err)
	}

	go func() {
		<-ctx.Done()
		client.Close()
	}()

	return &RedisConnector{
		logger: logger,
		client: client,
	}, nil
}

func createTLSConfig(tlsCfg *common.TLSConfig) (*tls.Config, error) {
	config := &tls.Config{
		InsecureSkipVerify: tlsCfg.InsecureSkipVerify,
	}

	if tlsCfg.CertFile != "" && tlsCfg.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(tlsCfg.CertFile, tlsCfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client cert/key pair: %w", err)
		}
		config.Certificates = []tls.Certificate{cert}
	}

	if tlsCfg.CAFile != "" {
		caCert, err := os.ReadFile(tlsCfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA file: %w", err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		config.RootCAs = caCertPool
	}

	return config, nil
}

func (r *RedisConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	r.logger.Debug().Msgf("writing to Redis with partition key: %s and range key: %s", partitionKey, rangeKey)
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

	r.logger.Debug().Msgf("getting item from Redis with key: %s", key)
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
