package data

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

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
	ttls   map[string]time.Duration
}

func NewRedisConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	cfg *common.RedisConnectorConfig,
) (*RedisConnector, error) {
	logger.Debug().Msgf("creating RedisConnector with config: %+v", cfg)

	connector := &RedisConnector{
		logger: logger,
		ttls:   make(map[string]time.Duration),
	}

	// Attempt the actual connecting in background to avoid blocking the main thread.
	go func() {
		for i := 0; i < 30; i++ {
			select {
			case <-ctx.Done():
				logger.Error().Msg("Context cancelled while attempting to connect to Redis")
				return
			default:
				logger.Debug().Msgf("attempting to connect to Redis (attempt %d of 30)", i+1)
				err := connector.connect(ctx, cfg)
				if err == nil {
					return
				}
				logger.Warn().Msgf("failed to connect to Redis (attempt %d of 30): %s", i+1, err)
				time.Sleep(10 * time.Second)
			}
		}
		logger.Error().Msg("Failed to connect to Redis after maximum attempts")
	}()

	return connector, nil
}

func (r *RedisConnector) connect(ctx context.Context, cfg *common.RedisConnectorConfig) error {
	options := &redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}

	if cfg.TLS != nil && cfg.TLS.Enabled {
		tlsConfig, err := createTLSConfig(cfg.TLS)
		if err != nil {
			return fmt.Errorf("failed to create TLS config: %w", err)
		}
		options.TLSConfig = tlsConfig
	}

	r.client = redis.NewClient(options)

	// Test the connection
	_, err := r.client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("failed to connect to Redis: %w", err)
	}

	return nil
}

func createTLSConfig(tlsCfg *common.TLSConfig) (*tls.Config, error) {
	config := &tls.Config{
		InsecureSkipVerify: tlsCfg.InsecureSkipVerify, // #nosec G402
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

func (r *RedisConnector) SetTTL(method string, ttlStr string) error {
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return err
	}
	r.ttls[strings.ToLower(method)] = ttl
	return nil
}

func (r *RedisConnector) HasTTL(method string) bool {
	_, found := r.ttls[strings.ToLower(method)]
	return found
}

func (r *RedisConnector) Set(ctx context.Context, partitionKey, rangeKey, value string) error {
	if r.client == nil {
		return fmt.Errorf("redis client not initialized yet")
	}

	r.logger.Debug().Msgf("writing to Redis with partition key: %s and range key: %s", partitionKey, rangeKey)

	ttl := time.Duration(0)
	if strings.HasSuffix(partitionKey, ":nil") {
		method := strings.ToLower(strings.Split(rangeKey, ":")[0])
		ttl = r.ttls[method]
	}
	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)
	rs := r.client.Set(ctx, key, value, ttl)
	return rs.Err()
}

func (r *RedisConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if r.client == nil {
		return "", fmt.Errorf("redis client not initialized yet")
	}

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
	if r.client == nil {
		return fmt.Errorf("redis client not initialized yet")
	}

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
