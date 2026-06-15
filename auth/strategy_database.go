package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/ristretto/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
	"golang.org/x/sync/singleflight"
)

// connectorDownProbeInterval is how often, at most, the strategy will
// re-attempt a real database lookup while the connector is in the
// known-down state. All other requests during the same window short-circuit
// to fail-open. Sized to be long enough that one "probe" per second across
// the fleet won't re-trigger a reconnect cascade, short enough that recovery
// is detected within a customer's typical retry budget.
const connectorDownProbeInterval = 1 * time.Second

type DatabaseStrategy struct {
	logger    *zerolog.Logger
	cfg       *common.DatabaseStrategyConfig
	connector data.Connector
	cache     *ristretto.Cache[string, *common.User]
	negCache  *ristretto.Cache[string, struct{}]
	negTTL    time.Duration
	sf        singleflight.Group

	// connectorDown tracks whether the connector is currently known to be
	// failing. When true, Authenticate skips the singleflight/Get path
	// entirely and serves the configured fail-open user directly — no
	// goroutine spawn, no log line, no metric increment per request.
	//
	// The 2026-05-13 edge-prod incident root-caused to every failed request
	// going through the full singleflight+Get+Error-log path even after we
	// knew the DB was unreachable. With ~thousands of in-flight auth queries
	// per second, that produced an Error-log fan-out that itself blocked on
	// the stdout fd write lock, which in turn parked the singleflight
	// leaders and grew the goroutine count from ~4k to ~96k.
	connectorDown atomic.Bool
	// connectorDownSince is the unix-nanos timestamp of the most recent
	// transition from up→down. Used to gate a single "probe" attempt per
	// connectorDownProbeInterval so we eventually notice recovery without
	// hammering the DB on every request.
	connectorDownSince atomic.Int64
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

	// Fail-open fast path. When the connector is in a known-down state and
	// fail-open is configured, serve the emergency user immediately without
	// going through singleflight + connector.Get + Error log + metric. This
	// is what eliminates per-request pressure during a sustained outage
	// (see DatabaseStrategy struct comment for the incident reference).
	// One caller per connectorDownProbeInterval still goes through the real
	// DB path so we eventually notice recovery; everyone else fast-paths.
	if u := s.tryFastFailOpen(); u != nil {
		s.recordAuthFailureMetric(req, "db_fail_open_fast_path")
		return u, nil
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
				// RecordNotFound is a business signal (key really doesn't
				// exist). The DB is healthy — don't taint connectorDown.
				s.markConnectorUp()
				s.recordAuthFailureMetric(req, "invalid_api_key")
				return &authFetchResult{user: nil, err: common.NewErrAuthUnauthorized("database", "invalid API key"), neg: true}, nil
			}
			// Real DB error: flip the connector-down latch so subsequent
			// requests in this probe window fast-path to fail-open without
			// re-running this branch.
			if s.isDownSignal(err) {
				s.markConnectorDown()
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

		// Successful query: the DB is healthy. Clear any stale connectorDown
		// latch so subsequent requests resume normal flow.
		s.markConnectorUp()

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

// tryFastFailOpen returns the configured fail-open user when ALL of the
// following hold:
//
//  1. Fail-open is enabled in the config (otherwise there's no emergency
//     user to serve, so we must run the real DB path even during outage).
//  2. The connectorDown latch is set (some prior request observed a
//     transport/timeout failure from the connector).
//  3. We are NOT the elected probe caller for this probe interval. Exactly
//     one caller per interval wins the CAS and runs the real DB path; all
//     others get the fast path.
//
// Returns nil to indicate "go through the normal path". This is the only
// signal needed — the caller doesn't need to know whether we fast-pathed
// because fail-open is disabled vs. because the connector is healthy.
func (s *DatabaseStrategy) tryFastFailOpen() *common.User {
	u := s.buildFailOpenUser()
	if u == nil {
		// Fail-open not configured — every request must go through the real
		// DB path even during an outage. Keeps the strict-auth semantics.
		return nil
	}
	if !s.connectorDown.Load() {
		return nil
	}
	now := time.Now().UnixNano()
	since := s.connectorDownSince.Load()
	if now-since > int64(connectorDownProbeInterval) {
		// Probe window expired. The caller that wins the CAS gets to run a
		// real DB query (which will mark up or mark down again based on the
		// result); everyone else continues to fast-path.
		if s.connectorDownSince.CompareAndSwap(since, now) {
			return nil
		}
	}
	return u
}

// isDownSignal reports whether a connector error should set the
// connectorDown latch. We're deliberately narrow here:
//
//   - ErrConnectorNotReady → yes, the connector itself is signalling unfit
//   - any error classified as db_timeout / db_not_ready / db_connection → yes
//   - everything else (parse errors, syntax errors, "too many connections"
//     capacity-class issues) → no — those are application-level and won't
//     improve by serving fail-open
//
// Keep this in sync with the labels emitted by classifyDbError.
func (s *DatabaseStrategy) isDownSignal(err error) bool {
	if err == nil {
		return false
	}
	switch s.classifyDbError(err) {
	case "db_not_ready", "db_timeout", "db_connection":
		return true
	}
	return false
}

// markConnectorDown latches the connectorDown flag and records the
// timestamp. Idempotent: calling it from many concurrent failing requests
// flips the flag at most once. The transition is logged once at Warn so
// dashboards can alert; subsequent failures in the same down period are
// silent on the auth side.
func (s *DatabaseStrategy) markConnectorDown() {
	// CompareAndSwap guarantees only the goroutine that observes the
	// transition writes the timestamp and logs.
	if s.connectorDown.CompareAndSwap(false, true) {
		s.connectorDownSince.Store(time.Now().UnixNano())
		s.logger.Warn().
			Str("connectorId", s.cfg.Connector.Id).
			Msg("database connector marked DOWN; subsequent requests will fast-path to fail-open until next probe succeeds")
	}
}

// markConnectorUp clears the connectorDown latch. Logged once on transition
// from down→up so the recovery is visible in dashboards. Safe to call from
// any successful query path including RecordNotFound — that's a business
// signal that the DB is reachable.
func (s *DatabaseStrategy) markConnectorUp() {
	if s.connectorDown.CompareAndSwap(true, false) {
		s.logger.Warn().
			Str("connectorId", s.cfg.Connector.Id).
			Msg("database connector marked UP; resuming normal auth flow")
	}
}

// getWithRetries wraps connector.Get with a small retry/backoff for transient errors.
// It retries for all drivers and aborts immediately on record-not-found.
//
// It also aborts immediately on data.ErrConnectorNotReady — that signal means
// the underlying connector knows its pool is unfit and is already running
// its own reconnect loop in a separate goroutine. Retrying here just burns
// the auth request's deadline without affecting recovery and produces a
// rapid burst of Warn logs that mirror the 2026-05-13 fd-lock incident
// pattern.
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
		// Connector signalled it's mid-reconnect. Retrying inside this
		// request's budget will not help — the initializer's auto-retry
		// loop is the only thing that fixes it. Fall through to fail-open.
		if errors.Is(err, data.ErrConnectorNotReady) {
			return nil, err
		}

		lastErr = err

		// If context is done, abort immediately
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if attempt < maxAttempts {
			backoff := baseBackoff << uint(attempt-1)
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

// classifyDbError converts database errors into a bounded set of reason labels.
//
// During the 2026-05-13 incident the previous implementation collapsed two
// very different signals into a single "db_connection" label:
//   - real pgbouncer/network transport failures (rare; needs ops attention)
//   - eRPC's own PostgreSQLConnector signalling "I'm mid-reconnect, try again"
//     (common; harmless once isolated, but ~100% of the dashboard signal
//     during a reconnect storm).
//
// Splitting them lets us alert on the first without being drowned by the
// second.
func (s *DatabaseStrategy) classifyDbError(err error) string {
	if err == nil {
		return "db_query_error"
	}
	// Connector-internal "not ready yet" — distinct from a real transport
	// failure because the connector has its own auto-retry loop. Surfacing
	// this on the metrics dashboard as its own label means we can spot
	// reconnect storms without confusing them with pgbouncer issues.
	if errors.Is(err, data.ErrConnectorNotReady) {
		return "db_not_ready"
	}
	// Timeouts
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") || strings.Contains(err.Error(), "timeout") {
		return "db_timeout"
	}
	// Connection-level issues (real transport faults only). Note: we keep
	// the substring fallback for cases where pgx wraps a transport error
	// without preserving the typed cause, but we no longer match the bare
	// word "connection" — see data/postgresql.go isPostgresConnectionError
	// for the typed equivalent used by the connector itself.
	e := err.Error()
	if strings.Contains(e, "connection refused") ||
		strings.Contains(e, "connection reset") ||
		strings.Contains(e, "broken pipe") ||
		strings.Contains(e, "no route to host") ||
		strings.Contains(e, "EOF") ||
		strings.Contains(e, "use of closed network connection") {
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
