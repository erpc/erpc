package upstream

import (
	"math/rand"
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
)

type RateLimitersRegistry struct {
	logger          *zerolog.Logger
	cfg             *common.RateLimiterConfig
	budgetsLimiters sync.Map
	envoyCache      limiter.RateLimitCache
	statsManager    stats.Manager
}

func NewRateLimitersRegistry(cfg *common.RateLimiterConfig, logger *zerolog.Logger) (*RateLimitersRegistry, error) {
	r := &RateLimitersRegistry{
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

	// Initialize shared cache if configured
	if r.cfg.Store != nil && r.cfg.Store.Driver == "redis" && r.cfg.Store.Redis != nil {
		store := gostats.NewStore(gostats.NewNullSink(), false)
		mgr := stats.NewStatManager(store, settings.NewSettings())
		useTLS := r.cfg.Store.Redis.TLS != nil && r.cfg.Store.Redis.TLS.Enabled
		url := r.cfg.Store.Redis.URI
		if url == "" {
			url = r.cfg.Store.Redis.Addr
		}
		poolSize := r.cfg.Store.Redis.ConnPoolSize
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
		r.envoyCache = redis.NewFixedRateLimitCacheImpl(
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
		r.statsManager = mgr
	} else if r.cfg.Store != nil && r.cfg.Store.Driver == "memory" {
		store := gostats.NewStore(gostats.NewNullSink(), false)
		mgr := stats.NewStatManager(store, settings.NewSettings())
		r.envoyCache = NewMemoryRateLimitCache(
			utils.NewTimeSourceImpl(),
			rand.New(rand.NewSource(time.Now().Unix())), // #nosec G404
			0,
			defaultNearLimitRatio(r.cfg.Store.NearLimitRatio),
			defaultCacheKeyPrefix(r.cfg.Store.CacheKeyPrefix),
			mgr,
		)
		r.statsManager = mgr
	}

	for _, budgetCfg := range r.cfg.Budgets {
		lg := r.logger.With().Str("budget", budgetCfg.Id).Logger()
		lg.Debug().Msgf("initializing rate limiter budget")
		budget := &RateLimiterBudget{
			Id:       budgetCfg.Id,
			Rules:    make([]*RateLimitRule, 0),
			registry: r,
			logger:   &lg,
			cache:    r.envoyCache,
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

	return nil
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
