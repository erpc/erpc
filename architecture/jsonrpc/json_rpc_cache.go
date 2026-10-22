package jsonrpc

import (
	"context"
	"fmt"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/rs/zerolog"
)

// GenericJsonRpcCache is the opt-in cache for `jsonrpc` networks. It reuses the
// shared data.CachePolicy + data.Connector abstractions so operators can point
// generic, SVM and EVM networks at one storage backend — the network id
// ("jsonrpc:starknet" vs "evm:1") forms the partition-key prefix, so the
// namespaces are disjoint.
//
// Where it diverges from the EVM/SVM caches, and why:
//
//   - Finality is whatever req.Finality reports: unknown unless the network's
//     `methods` config marks a method finalized/realtime. There is no block or
//     slot to derive it from, so policies are expected to key on
//     `finality: unknown` and rely on their TTL to bound staleness.
//   - The partition key is <networkId>:<suffix>:* — no block/slot reference
//     exists on a generic network, so the literal "*" (the same value svm uses
//     for lookups without slot awareness) is used for every entry.
//   - The request key is JsonRpcRequest.CanonicalKey (case-preserving): eRPC
//     cannot know which params are case-sensitive on an arbitrary protocol.
//   - No compression and no never-cache list: nothing is cached unless a policy
//     matches, and a protocol-agnostic cache has no methods to special-case.
type GenericJsonRpcCache struct {
	projectId string
	policies  []*data.CachePolicy
	logger    *zerolog.Logger
}

// NewGenericJsonRpcCache constructs the cache from a shared common.CacheConfig.
// It mirrors svm.NewSvmJsonRpcCache so erpc/init.go can wire all three in the
// same place without knowing which architecture each config section targets.
func NewGenericJsonRpcCache(ctx context.Context, logger *zerolog.Logger, cfg *common.CacheConfig) (*GenericJsonRpcCache, error) {
	lg := logger.With().Str("component", "genericJsonRpcCache").Logger()

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

	return &GenericJsonRpcCache{policies: policies, logger: &lg}, nil
}

// WithProjectId returns a shallow copy tagged with the project id so per-project
// telemetry and logs show the right owner.
func (c *GenericJsonRpcCache) WithProjectId(projectId string) *GenericJsonRpcCache {
	lg := c.logger.With().Str("projectId", projectId).Logger()
	return &GenericJsonRpcCache{
		projectId: projectId,
		policies:  c.policies,
		logger:    &lg,
	}
}

func (c *GenericJsonRpcCache) SetPolicies(policies []*data.CachePolicy) {
	c.policies = policies
}

// IsObjectNull is the nil-safe check callers use before dispatching to the cache.
func (c *GenericJsonRpcCache) IsObjectNull() bool {
	return c == nil || len(c.policies) == 0
}

// Get tries each matching policy in order and returns the first non-empty hit.
// A miss is indicated by (nil, nil) — the network layer then falls through to
// upstream forwarding.
func (c *GenericJsonRpcCache) Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
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

	// Which of these two we saw decides the `reason` label on the miss counter
	// below; a connector that is erroring must stay distinguishable from a
	// cold cache (see the svm cache for the full rationale).
	sawMiss, sawError := false, false
	for _, policy := range policies {
		connector := policy.GetConnector()
		if req.ShouldSkipCacheRead(connector.Id()) {
			continue
		}
		ttlLabel := ttlString(policy.GetTTL())
		jrr, err := c.doGet(ctx, connector, req, rpcReq)
		if err != nil {
			// Semantic-miss errors: the connector is signalling "no key" /
			// "expired" / "data not available here", not a real failure.
			if common.HasErrorCode(err, common.ErrCodeRecordNotFound) ||
				common.HasErrorCode(err, common.ErrCodeRecordExpired) ||
				common.HasErrorCode(err, common.ErrCodeEndpointMissingData) {
				sawMiss = true
				continue
			}
			telemetry.MetricCacheGetErrorTotal.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
				common.ErrorSummary(err),
			).Inc()
			c.logger.Debug().Err(err).Str("connector", connector.Id()).
				Msg("generic cache get failed; trying next policy")
			sawError = true
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
		sawMiss = true
	}
	// Every matched policy either errored or returned a miss — record one miss
	// against the first policy so dashboards show a flat miss count. A genuine
	// miss outranks an error, so a pool where one connector is down but another
	// simply has no entry still reads as a cold cache rather than an outage.
	missReason := "empty_result"
	switch {
	case sawMiss:
		missReason = "connector_miss"
	case sawError:
		missReason = "connector_error"
	}
	firstPolicy := policies[0]
	firstTTL := ttlString(firstPolicy.GetTTL())
	telemetry.MetricCacheGetSuccessMissTotal.WithLabelValues(
		c.projectId, req.NetworkLabel(), rpcReq.Method,
		firstPolicy.GetConnector().Id(), firstPolicy.String(), firstTTL,
		missReason,
	).Inc()
	telemetry.MetricCacheGetSuccessMissDuration.WithLabelValues(
		c.projectId, req.NetworkLabel(), rpcReq.Method,
		firstPolicy.GetConnector().Id(), firstPolicy.String(), firstTTL,
	).Observe(time.Since(start).Seconds())
	return nil, nil
}

// ttlString returns the string form of a policy TTL, or "none" when the
// pointer is nil, keeping the metric label cardinality bounded.
func ttlString(ttl *time.Duration) string {
	if ttl == nil {
		return "none"
	}
	return ttl.String()
}

// Set writes the response to every matching policy. Failures are logged and
// swallowed per-policy — a single flaky connector must not fail the upstream
// request path.
func (c *GenericJsonRpcCache) Set(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error {
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
		// Never cache a JSON-RPC error body: on a generic network an error's
		// permanence is unknowable, and the caller may expect a fresh attempt.
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

	groupKey, requestKey, err := c.generateKeys(req, rpcReq)
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
		setCtx := data.WithReverseIndex(ctx, req.NetworkId(), req.CacheKeySuffix())
		if err := connector.Set(setCtx, groupKey, requestKey, payload, ttl); err != nil {
			telemetry.MetricCacheSetErrorTotal.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
				common.ErrorSummary(err),
			).Inc()
			c.logger.Warn().Err(err).Str("connector", connector.Id()).
				Str("groupKey", groupKey).Str("requestKey", requestKey).
				Msg("generic cache set failed")
			continue
		}
	}
	return nil
}

func (c *GenericJsonRpcCache) findGetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState) ([]*data.CachePolicy, error) {
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

func (c *GenericJsonRpcCache) findSetPolicies(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmpty bool) ([]*data.CachePolicy, error) {
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

func (c *GenericJsonRpcCache) doGet(ctx context.Context, connector data.Connector, req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (*common.JsonRpcResponse, error) {
	rpcReq.RLockWithTrace(ctx)
	defer rpcReq.RUnlock()

	groupKey, requestKey, err := c.generateKeys(req, rpcReq)
	if err != nil {
		return nil, err
	}

	// MainIndex is always the right index: the group key has no block/slot
	// dimension, so a given (method, params) tuple produces the same groupKey
	// on both Set and Get by construction.
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

func (c *GenericJsonRpcCache) generateKeys(req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (string, string, error) {
	requestKey, err := rpcReq.CanonicalKey()
	if err != nil {
		return "", "", err
	}
	return common.CachePartitionKey(req.NetworkId(), req.CacheKeySuffix(), "*"), requestKey, nil
}

// Compile-time assertion that GenericJsonRpcCache implements common.CacheDAL.
var _ common.CacheDAL = (*GenericJsonRpcCache)(nil)
