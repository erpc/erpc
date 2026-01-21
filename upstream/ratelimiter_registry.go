package upstream

import (
	"context"
	"fmt"
	"math/rand"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	gostats "github.com/lyft/gostats"
	"github.com/rs/zerolog"

	"github.com/envoyproxy/ratelimit/src/limiter"
	"github.com/envoyproxy/ratelimit/src/redis"
	"github.com/envoyproxy/ratelimit/src/settings"
	"github.com/envoyproxy/ratelimit/src/stats"
	"github.com/envoyproxy/ratelimit/src/utils"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
)

type RateLimitersRegistry struct {
	appCtx          context.Context
	logger          *zerolog.Logger
	cfg             *common.RateLimiterConfig
	budgetsLimiters sync.Map
	envoyCache      limiter.RateLimitCache
	statsManager    stats.Manager
	cacheMu         sync.RWMutex
	initializer     *util.Initializer
}

func NewRateLimitersRegistry(appCtx context.Context, cfg *common.RateLimiterConfig, logger *zerolog.Logger) (*RateLimitersRegistry, error) {
	r := &RateLimitersRegistry{
		appCtx: appCtx,
		cfg:    cfg,
		logger: logger,
	}
	err := r.bootstrap()
	return r, err
}

func (r *RateLimitersRegistry) bootstrap() error {
	if r.cfg == nil {
		r.logger.Debug().Msg("no rate limiters defined which means all capacity of both local cpu/memory and remote upstreams will be used")
		return nil
	}

	// Create a default stats manager (needed even if cache is nil)
	store := gostats.NewStore(gostats.NewNullSink(), false)
	r.statsManager = stats.NewStatManager(store, settings.NewSettings())

	// Initialize shared cache if configured
	if r.cfg.Store != nil && r.cfg.Store.Driver == "redis" && r.cfg.Store.Redis != nil {
		// Create initializer for background retry
		r.initializer = util.NewInitializer(r.appCtx, r.logger, nil)

		// Attempt Redis connection with panic recovery - don't block startup
		connectTask := util.NewBootstrapTask("redis-ratelimiter-connect", r.connectRedisTask)
		if err := r.initializer.ExecuteTasks(r.appCtx, connectTask); err != nil {
			// Cache stays nil - rate limiting will fail-open until Redis connects
			r.logger.Warn().Err(err).Msg("failed to initialize Redis rate limiter on first attempt (rate limiting will fail-open until connected, retrying in background)")
		}
	} else if r.cfg.Store != nil && r.cfg.Store.Driver == "memory" {
		// Explicitly configured for memory
		r.envoyCache = NewMemoryRateLimitCache(
			utils.NewTimeSourceImpl(),
			rand.New(rand.NewSource(time.Now().Unix())), // #nosec G404
			0,
			defaultNearLimitRatio(r.cfg.Store.NearLimitRatio),
			defaultCacheKeyPrefix(r.cfg.Store.CacheKeyPrefix),
			r.statsManager,
		)
	}

	// Initialize budgets (cache may be nil for Redis until it connects)
	r.initializeBudgets()

	return nil
}

// connectRedisTask attempts to connect to Redis with panic recovery
func (r *RateLimitersRegistry) connectRedisTask(ctx context.Context) (err error) {
	// Recover from panics in the envoyproxy/ratelimit library
	defer func() {
		if rec := recover(); rec != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"ratelimiter-redis-connect",
				fmt.Sprintf("store:%s", r.cfg.Store.Redis.URI),
				common.ErrorFingerprint(rec),
			).Inc()
			r.logger.Error().
				Interface("panic", rec).
				Str("stack", string(debug.Stack())).
				Msg("panic recovered during Redis rate limiter connection (rate limiting will fail-open)")
			err = fmt.Errorf("panic during Redis connection: %v", rec)
		}
	}()

	store := gostats.NewStore(gostats.NewNullSink(), false)
	mgr := stats.NewStatManager(store, settings.NewSettings())
	useTLS := r.cfg.Store.Redis.TLS != nil && r.cfg.Store.Redis.TLS.Enabled
	url := r.cfg.Store.Redis.URI
	if url == "" {
		url = r.cfg.Store.Redis.Addr
	}
	poolSize := r.cfg.Store.Redis.ConnPoolSize

	r.logger.Debug().Str("url", util.RedactEndpoint(url)).Bool("tls", useTLS).Int("poolSize", poolSize).Msg("attempting to connect to Redis for rate limiting")

	client := redis.NewClientImpl(
		store.Scope("erpc_rl"),
		useTLS,
		r.cfg.Store.Redis.Username,
		"tcp",
		"single",
		url,
		poolSize,
		5*time.Millisecond,
		32,
		nil,
		false,
		nil,
	)

	cache := redis.NewFixedRateLimitCacheImpl(
		client,
		nil,
		utils.NewTimeSourceImpl(),
		rand.New(rand.NewSource(time.Now().UnixNano())), // #nosec G404
		5,
		nil,
		defaultNearLimitRatio(r.cfg.Store.NearLimitRatio),
		defaultCacheKeyPrefix(r.cfg.Store.CacheKeyPrefix),
		mgr,
		false,
	)

	// Successfully connected - update the cache
	// Note: statsManager is NOT updated here to avoid data races.
	// The statsManager created in bootstrap() is sufficient and identical.
	r.cacheMu.Lock()
	r.envoyCache = cache
	r.cacheMu.Unlock()

	r.logger.Info().Str("url", util.RedactEndpoint(url)).Msg("successfully connected to Redis for rate limiting")
	return nil
}

// initializeBudgets creates the rate limiter budgets
func (r *RateLimitersRegistry) initializeBudgets() {
	for _, budgetCfg := range r.cfg.Budgets {
		lg := r.logger.With().Str("budget", budgetCfg.Id).Logger()
		lg.Debug().Msgf("initializing rate limiter budget")
		maxTimeout := time.Duration(0)
		if r.cfg.Store != nil && r.cfg.Store.Redis != nil {
			maxTimeout = r.cfg.Store.Redis.GetTimeout.Duration()
		}
		budget := &RateLimiterBudget{
			Id:         budgetCfg.Id,
			Rules:      make([]*RateLimitRule, 0),
			registry:   r,
			logger:     &lg,
			maxTimeout: maxTimeout,
		}

		for _, rule := range budgetCfg.Rules {
			r.logger.Debug().Msgf("preparing rate limiter rule: %v", rule)

			budget.rulesMu.Lock()
			budget.Rules = append(budget.Rules, &RateLimitRule{Config: rule})
			budget.rulesMu.Unlock()

			scope := []string{}
			if rule.PerUser {
				scope = append(scope, "user")
			}
			if rule.PerNetwork {
				scope = append(scope, "network")
			}
			if rule.PerIP {
				scope = append(scope, "ip")
			}
			telemetry.MetricRateLimiterBudgetMaxCount.WithLabelValues(budgetCfg.Id, rule.Method, strings.Join(scope, ",")).Set(float64(rule.MaxCount))
		}

		r.budgetsLimiters.Store(budgetCfg.Id, budget)
	}
}

// GetCache returns the current rate limit cache (thread-safe)
func (r *RateLimitersRegistry) GetCache() limiter.RateLimitCache {
	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()
	return r.envoyCache
}

func (r *RateLimitersRegistry) GetBudget(budgetId string) (*RateLimiterBudget, error) {
	if budgetId == "" {
		return nil, nil
	}

	if budget, ok := r.budgetsLimiters.Load(budgetId); ok {
		return budget.(*RateLimiterBudget), nil
	}

	return nil, common.NewErrRateLimitBudgetNotFound(budgetId)
}

func (r *RateLimitersRegistry) GetBudgets() []*common.RateLimitBudgetConfig {
	return r.cfg.Budgets
}

// AdjustBudget updates MaxCount for all rules in a budget matching a method (supports wildcards via GetRulesByMethod).
func (r *RateLimitersRegistry) AdjustBudget(budgetId string, method string, newMax uint32) error {
	if budgetId == "" || method == "" {
		return nil
	}
	budget, err := r.GetBudget(budgetId)
	if err != nil || budget == nil {
		return err
	}
	rules, err := budget.GetRulesByMethod(method)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		_ = budget.AdjustBudget(rule, newMax)
	}
	return nil
}

func defaultNearLimitRatio(val float32) float32 {
	if val > 0 && val < 1 {
		return val
	}
	return 0.8
}

func defaultCacheKeyPrefix(val string) string {
	if val != "" {
		return val
	}
	return "erpc_rl_"
}
