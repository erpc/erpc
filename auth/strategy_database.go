package auth

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
)

type DatabaseStrategy struct {
	logger    *zerolog.Logger
	cfg       *common.DatabaseStrategyConfig
	connector data.Connector
	cache     *ristretto.Cache[string, *common.User]
}

var _ AuthStrategy = &DatabaseStrategy{}

func NewDatabaseStrategy(appCtx context.Context, logger *zerolog.Logger, cfg *common.DatabaseStrategyConfig) (*DatabaseStrategy, error) {
	if cfg == nil {
		return nil, fmt.Errorf("database strategy config is nil")
	}

	if cfg.Connector == nil {
		return nil, fmt.Errorf("database strategy connector config is nil")
	}

	connector, err := data.NewConnector(appCtx, logger, cfg.Connector)
	if err != nil {
		return nil, fmt.Errorf("failed to create database connector: %w", err)
	}

	// Initialize cache
	var cache *ristretto.Cache[string, *common.User]
	if cfg.Cache != nil {
		cacheConfig := &ristretto.Config[string, *common.User]{
			NumCounters: *cfg.Cache.NumCounters,
			MaxCost:     *cfg.Cache.MaxCost,
			BufferItems: 64, // Default buffer size
		}

		cache, err = ristretto.NewCache(cacheConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create cache: %w", err)
		}

		logger.Info().
			Dur("ttl", *cfg.Cache.TTL).
			Int64("maxSize", *cfg.Cache.MaxSize).
			Int64("maxCost", *cfg.Cache.MaxCost).
			Int64("numCounters", *cfg.Cache.NumCounters).
			Msg("initialized API key cache for database authentication strategy")
	}

	return &DatabaseStrategy{
		logger:    logger,
		cfg:       cfg,
		connector: connector,
		cache:     cache,
	}, nil
}

func (s *DatabaseStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeSecret || ap.Type == common.AuthTypeDatabase
}

func (s *DatabaseStrategy) Authenticate(ctx context.Context, ap *AuthPayload) (*common.User, error) {
	if ap.Secret == nil {
		return nil, common.NewErrAuthUnauthorized("database", "no secret provided")
	}

	apiKey := ap.Secret.Value
	if apiKey == "" {
		return nil, common.NewErrAuthUnauthorized("database", "empty API key")
	}

	// Check cache first if available
	if s.cache != nil {
		if cachedUser, found := s.cache.Get(apiKey); found {
			s.logger.Debug().Str("apiKey", apiKey).Msg("API key found in cache")
			return cachedUser, nil
		}
		s.logger.Debug().Str("apiKey", apiKey).Msg("API key not found in cache, querying database")
	}

	// Use API key as partition key and wildcard "*" as range key to get the record for this API key
	// (optimized: apiKey -> userId -> data, where userId is now the range key)
	rangeKey := "*"
	valueBytes, err := s.connector.Get(ctx, data.ConnectorMainIndex, apiKey, rangeKey)
	if err != nil {
		// Check for record not found error
		if common.HasErrorCode(err, "ErrRecordNotFound") {
			return nil, common.NewErrAuthUnauthorized("database", "invalid API key")
		}
		s.logger.Error().Err(err).Str("apiKey", apiKey).Msg("database query failed during authentication")
		return nil, common.NewErrAuthUnauthorized("database", fmt.Sprintf("database query failed: %v", err))
	}

	// Parse the JSON value
	var userData struct {
		UserId             string `json:"userId"`
		PerSecondRateLimit *int64 `json:"perSecondRateLimit,omitempty"`
		Enabled            *bool  `json:"enabled,omitempty"`
	}

	if err := json.Unmarshal(valueBytes, &userData); err != nil {
		s.logger.Error().Err(err).Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("failed to parse user data from database")
		return nil, common.NewErrAuthUnauthorized("database", "invalid user data format")
	}

	if userData.UserId == "" {
		s.logger.Error().Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("missing user ID in database record")
		return nil, common.NewErrAuthUnauthorized("database", "missing user ID in data")
	}

	// Check if API key is enabled (default to true if not specified for backward compatibility)
	enabled := true
	if userData.Enabled != nil {
		enabled = *userData.Enabled
	}

	if !enabled {
		s.logger.Warn().Str("apiKey", apiKey).Str("userId", userData.UserId).Msg("authentication attempt with disabled API key")
		return nil, common.NewErrAuthUnauthorized("database", "API key is disabled")
	}

	// Create the user
	user := &common.User{
		Id: userData.UserId,
	}

	if userData.PerSecondRateLimit != nil {
		user.PerSecondRateLimit = *userData.PerSecondRateLimit
	}

	// Cache the successful result if cache is available
	if s.cache != nil && s.cfg.Cache != nil && s.cfg.Cache.TTL != nil {
		ttl := *s.cfg.Cache.TTL
		s.cache.SetWithTTL(apiKey, user, 1, ttl)
		s.logger.Debug().Str("apiKey", apiKey).Dur("ttl", ttl).Msg("cached API key data")
	}

	s.logger.Debug().Str("apiKey", apiKey).Str("userId", user.Id).Int64("rateLimit", user.PerSecondRateLimit).Msg("user authenticated successfully")

	return user, nil
}

// GetConnector returns the database connector for admin operations
func (s *DatabaseStrategy) GetConnector() data.Connector {
	return s.connector
}

// InvalidateCache removes an API key from the cache
func (s *DatabaseStrategy) InvalidateCache(apiKey string) {
	if s.cache != nil {
		s.cache.Del(apiKey)
		s.logger.Debug().Str("apiKey", apiKey).Msg("invalidated API key cache entry")
	}
}

// ClearCache clears all cached API keys
func (s *DatabaseStrategy) ClearCache() {
	if s.cache != nil {
		s.cache.Clear()
		s.logger.Info().Msg("cleared all API key cache entries")
	}
}

// Close closes the cache and performs cleanup
func (s *DatabaseStrategy) Close() {
	if s.cache != nil {
		s.cache.Close()
		s.logger.Debug().Msg("closed API key cache")
	}
}
