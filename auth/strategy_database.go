package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
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
		s.recordAuthFailureMetric(req, "missing_secret")
		return nil, common.NewErrAuthUnauthorized("database", "no secret provided")
	}

	apiKey := ap.Secret.Value
	if apiKey == "" {
		s.recordAuthFailureMetric(req, "empty_secret")
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
			s.recordAuthFailureMetric(req, "cached_unknown_api_key")
			return nil, common.NewErrAuthUnauthorized("database", "invalid API key")
		}
	}

	// Use singleflight to deduplicate concurrent misses per key
	type authFetchResult struct {
		user      *common.User
		err       error
		neg       bool
		skipCache bool
	}
	v, sfErr, _ := s.sf.Do(apiKey, func() (interface{}, error) {
		rangeKey := "*"
		lookupCtx := ctx
		if s.cfg != nil && s.cfg.MaxWait.Duration() > 0 {
			var cancel context.CancelFunc
			lookupCtx, cancel = context.WithTimeout(ctx, s.cfg.MaxWait.Duration())
			defer cancel()
		}
		valueBytes, err := s.getWithRetries(lookupCtx, data.ConnectorMainIndex, apiKey, rangeKey)
		if err != nil {
			if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
				s.recordAuthFailureMetric(req, "invalid_api_key")
				return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "invalid API key"), neg: true}, nil
			}
			s.logger.Error().
				Err(err).
				Str("apiKey", apiKey).
				Str("driver", string(s.cfg.Connector.Driver)).
				Str("connectorId", s.cfg.Connector.Id).
				Msg("database query failed during authentication")
			s.recordAuthFailureMetric(req, s.classifyDbError(err))
			// Fail-open if configured
			if u := s.buildFailOpenUser(); u != nil {
				s.logger.Error().Str("userId", u.Id).Msg("auth DB error; fail-open enabled, granting emergency user")
				return &authFetchResult{user: u, err: nil, neg: false, skipCache: true}, nil
			}
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", fmt.Sprintf("database query failed: %v", err)), neg: false}, nil
		}

		var userData struct {
			UserId          string `json:"userId"`
			Enabled         *bool  `json:"enabled,omitempty"`
			RateLimitBudget string `json:"rateLimitBudget,omitempty"`
		}
		if err := json.Unmarshal(valueBytes, &userData); err != nil {
			s.logger.Error().Err(err).Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("failed to parse user data from database")
			s.recordAuthFailureMetric(req, "db_record_parse_error")
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "invalid user data format"), neg: false}, nil
		}
		if userData.UserId == "" {
			s.logger.Error().Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("missing user ID in database record")
			s.recordAuthFailureMetric(req, "db_record_missing_user_id")
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "missing user ID in data"), neg: false}, nil
		}
		enabled := true
		if userData.Enabled != nil {
			enabled = *userData.Enabled
		}
		if !enabled {
			s.logger.Warn().Str("apiKey", apiKey).Str("userId", userData.UserId).Msg("authentication attempt with disabled API key")
			s.recordAuthFailureMetric(req, "disabled_key")
			return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "API key is disabled"), neg: true}, nil
		}
		user := &common.User{Id: userData.UserId}
		if userData.RateLimitBudget != "" {
			user.RateLimitBudget = userData.RateLimitBudget
		}
		return &authFetchResult{user: user, err: nil, neg: false}, nil
	})
	if sfErr != nil {
		s.recordAuthFailureMetric(req, "internal_error")
		if u := s.buildFailOpenUser(); u != nil {
			s.logger.Warn().Str("userId", u.Id).Err(sfErr).Msg("singleflight error; fail-open enabled, granting emergency user")
			return u, nil
		}
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

	// Cache the successful result if cache is available and not marked to skip
	if afr.skipCache == false && s.cache != nil && s.cfg.Cache != nil && s.cfg.Cache.TTL != nil {
		ttl := *s.cfg.Cache.TTL
		s.cache.SetWithTTL(apiKey, user, 1, ttl)
		s.logger.Debug().Str("apiKey", apiKey).Dur("ttl", ttl).Msg("cached API key data")
	}

	s.logger.Debug().Str("apiKey", apiKey).Str("userId", user.Id).Str("budget", user.RateLimitBudget).Msg("user authenticated successfully")

	return user, nil
}

// getWithRetries wraps connector.Get with a small retry/backoff for transient errors.
// It retries for all drivers and aborts immediately on record-not-found.
func (s *DatabaseStrategy) getWithRetries(ctx context.Context, index, partitionKey, rangeKey string) ([]byte, error) {
	if s.cfg == nil || s.cfg.Retry == nil || s.cfg.Retry.MaxAttempts <= 1 {
		return s.connector.Get(ctx, index, partitionKey, rangeKey, nil)
	}
	maxAttempts := s.cfg.Retry.MaxAttempts
	baseBackoff := s.cfg.Retry.BaseBackoff.Duration()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		val, err := s.connector.Get(ctx, index, partitionKey, rangeKey, nil)
		if err == nil || common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return val, err
		}

		lastErr = err

		// If context is done, abort immediately
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if attempt < maxAttempts {
			backoff := baseBackoff << uint(attempt-1) // #nosec G115
			s.logger.Warn().
				Err(err).
				Str("driver", string(s.cfg.Connector.Driver)).
				Str("connectorId", s.cfg.Connector.Id).
				Int("attempt", attempt).
				Dur("backoff", backoff).
				Msg("database authentication lookup failed; retrying")

			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, lastErr
}

// GetConnector returns the database connector for admin operations
func (s *DatabaseStrategy) GetConnector() data.Connector {
	return s.connector
}

// InvalidateCache removes an API key from both positive and negative caches
func (s *DatabaseStrategy) InvalidateCache(apiKey string) {
	if s.cache != nil {
		s.cache.Del(apiKey)
		s.logger.Debug().Str("apiKey", apiKey).Msg("invalidated API key positive cache entry")
	}
	if s.negCache != nil {
		s.negCache.Del(apiKey)
		s.logger.Debug().Str("apiKey", apiKey).Msg("invalidated API key negative cache entry")
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

// recordAuthFailureMetric increments the auth failure metric with safe labels
func (s *DatabaseStrategy) recordAuthFailureMetric(req *common.NormalizedRequest, reason string) {
	project := "n/a"
	network := "n/a"
	agent := "unknown"
	if req != nil {
		// Prefer resolved network/project when available
		if n := req.Network(); n != nil {
			project = n.ProjectId()
			network = req.NetworkLabel()
		} else {
			network = req.NetworkId()
		}
		agent = req.AgentName()
	}
	telemetry.MetricAuthFailedTotal.WithLabelValues(
		project,
		network,
		"database",
		reason,
		agent,
	).Inc()
}

// classifyDbError converts database errors into a bounded set of reason labels
func (s *DatabaseStrategy) classifyDbError(err error) string {
	if err == nil {
		return "db_query_error"
	}
	// Timeouts
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "timeout") {
		return "db_timeout"
	}
	// Connection-level issues
	e := err.Error()
	if strings.Contains(e, "not connected") || strings.Contains(e, "connection") || strings.Contains(e, "refused") || strings.Contains(e, "reset") || strings.Contains(e, "broken pipe") || strings.Contains(e, "EOF") {
		return "db_connection"
	}
	return "db_query_error"
}

// buildFailOpenUser returns the configured emergency user when fail-open is enabled.
// Returns nil when fail-open is disabled or not configured.
func (s *DatabaseStrategy) buildFailOpenUser() *common.User {
	if s.cfg == nil || s.cfg.FailOpen == nil || !s.cfg.FailOpen.Enabled {
		return nil
	}
	u := &common.User{Id: s.cfg.FailOpen.UserId}
	if s.cfg.FailOpen.RateLimitBudget != "" {
		u.RateLimitBudget = s.cfg.FailOpen.RateLimitBudget
	}
	return u
}
