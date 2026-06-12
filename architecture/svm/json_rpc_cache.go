package svm

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// SvmJsonRpcCache is the SVM counterpart to EvmJsonRpcCache. It reuses the
// shared data.CachePolicy + data.Connector abstractions so operators can point
// SVM and EVM networks at the same storage backend (Redis, DynamoDB, memory)
// without worrying about key collisions — the network id ("svm:mainnet-beta"
// vs "evm:1") forms the partition-key prefix, so the namespaces are disjoint.
//
// Compared to EvmJsonRpcCache this implementation is intentionally stripped:
//
//   - No zstd compression. SVM payloads are typically smaller than EVM trace
//     blobs and compression can be layered in later if the hot connector shows
//     it in production.
//   - No block-timestamp age guard. SVM data is immutable by commitment level;
//     a "finalized" response written yesterday is still finalized today. For
//     Realtime entries (getLatestBlockhash etc.) the caller should set a short
//     TTL — the TTL itself bounds staleness.
//   - The partition key is <networkId>:<slotRef> rather than <networkId>:<blockRef>.
//     slotRef is derived from the request's minContextSlot (when provided) or
//     the literal "*" for lookups without slot awareness. Reverse-index lookups
//     then scan across slots for the same params hash.
type SvmJsonRpcCache struct {
	projectId string
	policies  []*data.CachePolicy
	logger    *zerolog.Logger
}

// NewSvmJsonRpcCache constructs the cache from a shared common.CacheConfig.
// It mirrors evm.NewEvmJsonRpcCache so erpc/init.go can wire both in the same
// place without knowing which architecture each config section targets.
func NewSvmJsonRpcCache(ctx context.Context, logger *zerolog.Logger, cfg *common.CacheConfig) (*SvmJsonRpcCache, error) {
	lg := logger.With().Str("component", "svmJsonRpcCache").Logger()

	connectors := make(map[string]data.Connector)
	for _, connCfg := range cfg.Connectors {
		c, err := data.NewConnector(ctx, &lg, connCfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create connector %s: %w", connCfg.Id, err)
		}
		connectors[connCfg.Id] = c
	}

	var policies []*data.CachePolicy
	for _, policyCfg := range cfg.Policies {
		connector, ok := connectors[policyCfg.Connector]
		if !ok {
			return nil, fmt.Errorf("connector %s not found for policy", policyCfg.Connector)
		}
		policy, err := data.NewCachePolicy(policyCfg, connector)
		if err != nil {
			return nil, fmt.Errorf("failed to create policy: %w", err)
		}
		policies = append(policies, policy)
	}

	return &SvmJsonRpcCache{policies: policies, logger: &lg}, nil
}

// WithProjectId returns a shallow copy tagged with the project id so per-project
// telemetry and logs show the right owner. Matches evm.EvmJsonRpcCache.WithProjectId.
func (c *SvmJsonRpcCache) WithProjectId(projectId string) *SvmJsonRpcCache {
	lg := c.logger.With().Str("projectId", projectId).Logger()
	return &SvmJsonRpcCache{
		projectId: projectId,
		policies:  c.policies,
		logger:    &lg,
	}
}

func (c *SvmJsonRpcCache) SetPolicies(policies []*data.CachePolicy) {
	c.policies = policies
}

// IsObjectNull is the nil-safe check callers use before dispatching to the cache.
func (c *SvmJsonRpcCache) IsObjectNull() bool {
	return c == nil || len(c.policies) == 0
}

// Get tries each matching policy in order and returns the first non-empty hit.
// A miss is indicated by (nil, nil) — the network layer then falls through to
// upstream forwarding.
func (c *SvmJsonRpcCache) Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	start := time.Now()
	rpcReq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}

	finality := req.Finality(ctx)
	policies, err := c.findGetPolicies(req.NetworkId(), rpcReq.Method, rpcReq.Params, finality)
	if err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		telemetry.MetricCacheGetSkippedTotal.
			WithLabelValues(c.projectId, req.NetworkLabel(), rpcReq.Method).
			Inc()
		return nil, nil
	}

	for _, policy := range policies {
		connector := policy.GetConnector()
		if req.ShouldSkipCacheRead(connector.Id()) {
			continue
		}
		ttlLabel := ttlString(policy.GetTTL())
		jrr, err := c.doGet(ctx, connector, req, rpcReq)
		if err != nil {
			telemetry.MetricCacheGetErrorTotal.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
				common.ErrorSummary(err),
			).Inc()
			c.logger.Debug().Err(err).Str("connector", connector.Id()).
				Msg("svm cache get failed; trying next policy")
			continue
		}
		if jrr != nil {
			telemetry.MetricCacheGetSuccessHitTotal.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
			).Inc()
			telemetry.MetricCacheGetSuccessHitDuration.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
			).Observe(time.Since(start).Seconds())
			return common.NewNormalizedResponse().
				WithRequest(req).
				WithFromCache(true).
				WithJsonRpcResponse(jrr), nil
		}
	}
	// Every matched policy either errored or returned a miss — record one miss
	// against the first policy so dashboards show a flat miss count.
	firstPolicy := policies[0]
	firstTTL := ttlString(firstPolicy.GetTTL())
	telemetry.MetricCacheGetSuccessMissTotal.WithLabelValues(
		c.projectId, req.NetworkLabel(), rpcReq.Method,
		firstPolicy.GetConnector().Id(), firstPolicy.String(), firstTTL,
	).Inc()
	telemetry.MetricCacheGetSuccessMissDuration.WithLabelValues(
		c.projectId, req.NetworkLabel(), rpcReq.Method,
		firstPolicy.GetConnector().Id(), firstPolicy.String(), firstTTL,
	).Observe(time.Since(start).Seconds())
	return nil, nil
}

// ttlString returns the string form of a policy TTL, or "none" when the
// pointer is nil. Keeps the metric label cardinality bounded and avoids a
// nil-deref on policy.GetTTL().String() for policies without an explicit TTL.
func ttlString(ttl *time.Duration) string {
	if ttl == nil {
		return "none"
	}
	return ttl.String()
}

// Set writes the response to every matching policy concurrently. Failures are
// logged and swallowed per-policy — a single flaky connector must not fail the
// upstream request path.
func (c *SvmJsonRpcCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
	if resp == nil || resp.IsObjectNull() {
		return nil
	}
	rpcReq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return err
	}
	rpcResp, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		return err
	}
	if rpcResp == nil || rpcResp.Error != nil {
		// Don't cache responses that carry a JSON-RPC error body; the caller may
		// retry against another upstream and expect a fresh attempt.
		return nil
	}

	finality := req.Finality(ctx)
	isEmpty := resp.IsResultEmptyish()
	policies, err := c.findSetPolicies(req.NetworkId(), rpcReq.Method, rpcReq.Params, finality, isEmpty)
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		return nil
	}

	groupKey, requestKey, err := c.generateKeys(req, rpcReq, ctx)
	if err != nil {
		return err
	}
	payload := rpcResp.GetResultBytes()
	if payload == nil {
		return nil
	}

	for _, policy := range policies {
		connector := policy.GetConnector()
		ttl := policy.GetTTL()
		ttlLabel := ttlString(ttl)
		if !policy.MatchesSizeLimits(len(payload)) {
			telemetry.MetricCacheSetSkippedTotal.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
			).Inc()
			continue
		}
		telemetry.MetricCacheSetOriginalBytes.WithLabelValues(
			c.projectId, req.NetworkLabel(), rpcReq.Method,
			connector.Id(), policy.String(), ttlLabel,
		).Add(float64(len(payload)))
		if err := connector.Set(ctx, groupKey, requestKey, payload, ttl); err != nil {
			telemetry.MetricCacheSetErrorTotal.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
				common.ErrorSummary(err),
			).Inc()
			c.logger.Warn().Err(err).Str("connector", connector.Id()).
				Str("groupKey", groupKey).Str("requestKey", requestKey).
				Msg("svm cache set failed")
			continue
		}
	}
	return nil
}

func (c *SvmJsonRpcCache) findGetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState) ([]*data.CachePolicy, error) {
	var matched []*data.CachePolicy
	seen := make(map[data.Connector]bool)
	for _, p := range c.policies {
		ok, err := p.MatchesForGet(networkId, method, params, finality)
		if err != nil {
			return nil, err
		}
		if ok {
			conn := p.GetConnector()
			if !seen[conn] {
				matched = append(matched, p)
				seen[conn] = true
			}
		}
	}
	return matched, nil
}

func (c *SvmJsonRpcCache) findSetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmpty bool) ([]*data.CachePolicy, error) {
	var matched []*data.CachePolicy
	for _, p := range c.policies {
		ok, err := p.MatchesForSet(networkId, method, params, finality, isEmpty)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, p)
		}
	}
	return matched, nil
}

func (c *SvmJsonRpcCache) doGet(ctx context.Context, connector data.Connector, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (*common.JsonRpcResponse, error) {
	rpcReq.RLockWithTrace(ctx)
	defer rpcReq.RUnlock()

	groupKey, requestKey, err := c.generateKeys(req, rpcReq, ctx)
	if err != nil {
		return nil, err
	}

	// MainIndex is always the right index for SVM. Unlike EVM (which has a
	// bespoke blockRef dimension in its partition key), our slotRef is derived
	// from the request's params, so a given (method, params) tuple always
	// produces the same groupKey on both Set and Get. ReverseIndex wildcard
	// fallback would only matter if two calls with the same params hash could
	// land on different slotRef values — that's not possible by construction.
	resultBytes, err := connector.Get(ctx, data.ConnectorMainIndex, groupKey, requestKey, req)
	if err != nil {
		return nil, err
	}
	if len(resultBytes) == 0 {
		return nil, nil
	}

	jrr, err := common.NewJsonRpcResponseFromBytes(nil, resultBytes, nil)
	if err != nil {
		return nil, err
	}
	_ = jrr.SetID(rpcReq.ID)
	return jrr, nil
}

func (c *SvmJsonRpcCache) generateKeys(req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest, ctx context.Context) (string, string, error) {
	requestKey, err := req.CacheHash(ctx)
	if err != nil {
		return "", "", err
	}
	slotRef := extractSlotRef(rpcReq)
	// Note: rpcReq is already locked by the caller when coming from doGet; Set does
	// not lock, so CacheHash runs on the whole params slice unlocked. That mirrors
	// evm.generateKeysForJsonRpcRequest which also takes the caller's locking for granted.
	return fmt.Sprintf("%s:%s", req.NetworkId(), slotRef), requestKey, nil
}

// extractSlotRef returns a stable slot reference for cache partitioning.
// Looks for minContextSlot in the request's options object; falls back to "*"
// so the key is still deterministic when no slot was supplied.
func extractSlotRef(rpcReq *common.JsonRpcRequest) string {
	if rpcReq == nil {
		return "*"
	}
	for _, p := range rpcReq.Params {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if v, ok := m["minContextSlot"]; ok {
			switch s := v.(type) {
			case float64:
				return strconv.FormatInt(int64(s), 10)
			case int64:
				return strconv.FormatInt(s, 10)
			case string:
				if s != "" {
					return s
				}
			}
		}
	}
	return "*"
}

// Compile-time assertion that SvmJsonRpcCache implements common.CacheDAL.
var _ common.CacheDAL = (*SvmJsonRpcCache)(nil)
