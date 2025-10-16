package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

type DatabaseStrategy struct {
	logger    *zerolog.Logger
	cfg       *common.DatabaseStrategyConfig
	connector data.Connector
	cache     *ristretto.Cache[string, *common.User]
	negCache  *ristretto.Cache[string, struct{}]
	negTTL    time.Duration
	sf        singleflight.Group
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

	// Initialize cache(s)
	var cache *ristretto.Cache[string, *common.User]
	var negCache *ristretto.Cache[string, struct{}]
	negTTL := 5 * time.Second
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

		// Negative cache to avoid hammering DB on invalid/disabled keys
		negCacheConfig := &ristretto.Config[string, struct{}]{
			NumCounters: *cfg.Cache.NumCounters,
			MaxCost:     1 << 20, // small cost budget; values are zero-sized
			BufferItems: 64,
		}
		negCache, err = ristretto.NewCache(negCacheConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to create negative cache: %w", err)
		}

		logger.Info().
			Dur("ttl", *cfg.Cache.TTL).
			Dur("negTtl", negTTL).
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
		negCache:  negCache,
		negTTL:    negTTL,
	}, nil
}

func (s *DatabaseStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeSecret || ap.Type == common.AuthTypeDatabase
}

func (s *DatabaseStrategy) Authenticate(ctx context.Context, req *common.NormalizedRequest, ap *AuthPayload) (*common.User, error) {
	if ap.Secret == nil {
		return nil, common.NewErrAuthUnauthorized("database", "no secret provided")
	}

	apiKey := ap.Secret.Value
	if apiKey == "" {
		return nil, common.NewErrAuthUnauthorized("database", "empty API key")
	}

	// Check positive cache first if available
	if s.cache != nil {
		if cachedUser, found := s.cache.Get(apiKey); found {
			s.logger.Debug().Str("apiKey", apiKey).Msg("API key found in cache")
			return cachedUser, nil
		}
		s.logger.Debug().Str("apiKey", apiKey).Msg("API key not found in cache")
	}

	// Negative cache: short-circuit known invalid/disabled keys
	if s.negCache != nil {
		if _, found := s.negCache.Get(apiKey); found {
			s.logger.Debug().Str("apiKey", apiKey).Msg("API key found in negative cache")
			return nil, common.NewErrAuthUnauthorized("database", "invalid API key")
		}
	}

	// Use singleflight to deduplicate concurrent misses per key
	type authFetchResult struct {
		user *common.User
		err  error
		neg  bool
	}
	v, sfErr, _ := s.sf.Do(apiKey, func() (interface{}, error) {
		rangeKey := "*"
		valueBytes, err := s.connector.Get(ctx, data.ConnectorMainIndex, apiKey, rangeKey, nil)
		if err != nil {
			if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
				return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "invalid API key"), neg: true}, nil
			}
			s.logger.Error().Err(err).Str("apiKey", apiKey).Msg("database query failed during authentication")
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", fmt.Sprintf("database query failed: %v", err)), neg: false}, nil
		}

		var userData struct {
			UserId          string `json:"userId"`
			Enabled         *bool  `json:"enabled,omitempty"`
			RateLimitBudget string `json:"rateLimitBudget,omitempty"`
		}
		if err := json.Unmarshal(valueBytes, &userData); err != nil {
			s.logger.Error().Err(err).Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("failed to parse user data from database")
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "invalid user data format"), neg: false}, nil
		}
		if userData.UserId == "" {
			s.logger.Error().Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("missing user ID in database record")
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "missing user ID in data"), neg: false}, nil
		}
		enabled := true
		if userData.Enabled != nil {
			enabled = *userData.Enabled
		}
		if !enabled {
			s.logger.Warn().Str("apiKey", apiKey).Str("userId", userData.UserId).Msg("authentication attempt with disabled API key")
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "API key is disabled"), neg: true}, nil
		}
		user := &common.User{Id: userData.UserId}
		if userData.RateLimitBudget != "" {
			user.RateLimitBudget = userData.RateLimitBudget
		}
		return &authFetchResult{user: user, err: nil, neg: false}, nil
	})
	if sfErr != nil {
		return nil, sfErr
	}
	afr := v.(*authFetchResult)
	if afr.err != nil {
		if afr.neg && s.negCache != nil {
			s.negCache.SetWithTTL(apiKey, struct{}{}, 1, s.negTTL)
		}
		return nil, afr.err
	}
	user := afr.user

	// Cache the successful result if cache is available
	if s.cache != nil && s.cfg.Cache != nil && s.cfg.Cache.TTL != nil {
		ttl := *s.cfg.Cache.TTL
		s.cache.SetWithTTL(apiKey, user, 1, ttl)
		s.logger.Debug().Str("apiKey", apiKey).Dur("ttl", ttl).Msg("cached API key data")
	}

	s.logger.Debug().Str("apiKey", apiKey).Str("userId", user.Id).Str("budget", user.RateLimitBudget).Msg("user authenticated successfully")

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
	if s.negCache != nil {
		s.negCache.Close()
		s.logger.Debug().Msg("closed API key negative cache")
	}
}
