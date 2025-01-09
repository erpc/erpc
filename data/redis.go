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
	id          string
	logger      *zerolog.Logger
	client      *redis.Client
	ttls        map[string]time.Duration
	initTimeout time.Duration
	getTimeout  time.Duration
	setTimeout  time.Duration
}

func NewRedisConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.RedisConnectorConfig,
) (*RedisConnector, error) {
	lg := logger.With().Str("connector", id).Logger()
	lg.Debug().Interface("config", cfg).Msg("creating RedisConnector")

	connector := &RedisConnector{
		id:          id,
		logger:      &lg,
		ttls:        make(map[string]time.Duration),
		initTimeout: cfg.InitTimeout,
		getTimeout:  cfg.GetTimeout,
		setTimeout:  cfg.SetTimeout,
	}

	// Attempt the actual connecting in background to avoid blocking the main thread.
	go func() {
		for i := 0; i < 600; i++ {
			select {
			case <-ctx.Done():
				lg.Error().Msg("context cancelled while attempting to connect to Redis")
				return
			default:
				lg.Debug().Msgf("attempting to connect to Redis (attempt %d)", i+1)
				err := connector.connect(ctx, cfg)
				if err == nil {
					return
				}
				lg.Warn().Err(err).Msgf("failed to connect to Redis (attempt %d)", i+1)
				time.Sleep(30 * time.Second)
			}
		}
		lg.Error().Msg("failed to connect to Redis after maximum attempts")
	}()

	return connector, nil
}

func (r *RedisConnector) connect(ctx context.Context, cfg *common.RedisConnectorConfig) error {
	options := &redis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password,
		DB:           cfg.DB,
		PoolSize:     cfg.ConnPoolSize,
		DialTimeout:  r.initTimeout,
		ReadTimeout:  r.getTimeout,
		WriteTimeout: r.setTimeout,
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

func (r *RedisConnector) Id() string {
	return r.id
}

func (r *RedisConnector) Set(ctx context.Context, partitionKey, rangeKey, value string, ttl *time.Duration) error {
	if r.client == nil {
		return fmt.Errorf("redis client not initialized yet")
	}

	r.logger.Debug().Msgf("writing to Redis with partition key: %s and range key: %s", partitionKey, rangeKey)

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)

	ctx, cancel := context.WithTimeout(ctx, r.setTimeout)
	defer cancel()

	// Use the provided TTL if one exists, otherwise set with no expiration
	duration := time.Duration(0)
	if ttl != nil && *ttl > 0 {
		duration = *ttl
	}

	rs := r.client.Set(ctx, key, value, duration)
	return rs.Err()
}

func (r *RedisConnector) Get(ctx context.Context, index, partitionKey, rangeKey string) (string, error) {
	if r.client == nil {
		return "", fmt.Errorf("redis client not initialized yet")
	}

	var err error
	var value string

	key := fmt.Sprintf("%s:%s", partitionKey, rangeKey)

	ctx, cancel := context.WithTimeout(ctx, r.getTimeout)
	defer cancel()

	if strings.Contains(key, "*") {
		keys, err := r.client.Keys(ctx, key).Result()
		if err != nil {
			return "", err
		}
		if len(keys) == 0 {
			return "", common.NewErrRecordNotFound(partitionKey, rangeKey, RedisDriverName)
		}
		key = keys[0]
	}

	r.logger.Debug().Msgf("getting item from Redis with key: %s", key)
	value, err = r.client.Get(ctx, key).Result()

	if err == redis.Nil {
		return "", common.NewErrRecordNotFound(partitionKey, rangeKey, RedisDriverName)
	} else if err != nil {
		return "", err
	}

	return value, nil
}
