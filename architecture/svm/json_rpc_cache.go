package svm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/klauspost/compress/zstd"
	"github.com/rs/zerolog"
)

// SvmJsonRpcCache is the SVM counterpart to EvmJsonRpcCache. It reuses the
// shared data.CachePolicy + data.Connector abstractions so operators can point
// SVM and EVM networks at the same storage backend (Redis, DynamoDB, memory)
// without worrying about key collisions — the network id ("svm:mainnet-beta"
// vs "evm:1") forms the partition-key prefix, so the namespaces are disjoint.
//
// Where it diverges from EvmJsonRpcCache, and why:
//
//   - The request key comes from svmRequestKey, NOT req.CacheHash(). The shared
//     hasher lowercases string params, which is right for EVM hex and wrong for
//     case-sensitive base58 pubkeys/signatures. See svmRequestKey below.
//   - No block-timestamp age guard. Only genuinely immutable responses are
//     classified Finalized (see GetFinality in finality.go); everything that
//     tracks the rooted head is Realtime, so the TTL on the realtime policy is
//     what bounds staleness — the same lever EVM uses for `latest`.
//   - The partition key is <networkId>:<slotRef> rather than <networkId>:<blockRef>.
//     slotRef is derived from the request's minContextSlot (when provided) or
//     the literal "*" for lookups without slot awareness. Reverse-index lookups
//     then scan across slots for the same params hash.
//
// zstd compression works the same as EvmJsonRpcCache — Solana getBlock payloads
// routinely exceed a megabyte, so ignoring cache.compression (which defaults to
// enabled) would silently multiply storage cost.
type SvmJsonRpcCache struct {
	projectId string
	policies  []*data.CachePolicy
	logger    *zerolog.Logger

	// compressionThreshold is the minimum payload size (bytes) worth
	// compressing; 0 means compression is disabled for writes. The decoder is
	// always present so entries written while compression was enabled stay
	// readable after an operator turns it off.
	compressionThreshold int
	encoder              *zstd.Encoder
	decoder              *zstd.Decoder
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

	cache := &SvmJsonRpcCache{policies: policies, logger: &lg}

	// One stateless encoder/decoder pair is enough: zstd's EncodeAll/DecodeAll
	// are documented safe for concurrent use and pull from their own internal
	// worker pools, so the sync.Pool dance EvmJsonRpcCache does for its
	// *streaming* encoders is unnecessary here.
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd decoder: %w", err)
	}
	cache.decoder = decoder

	if cfg.Compression != nil && cfg.Compression.Enabled != nil && *cfg.Compression.Enabled {
		level := zstd.SpeedFastest // optimal for caching workloads; matches EVM's default
		switch strings.ToLower(cfg.Compression.ZstdLevel) {
		case "", "fastest":
		case "default":
			level = zstd.SpeedDefault
		case "better":
			level = zstd.SpeedBetterCompression
		case "best":
			level = zstd.SpeedBestCompression
		default:
			lg.Warn().Str("level", cfg.Compression.ZstdLevel).Msg("unknown compression level, using 'fastest'")
		}
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
		if err != nil {
			return nil, fmt.Errorf("failed to create zstd encoder: %w", err)
		}
		cache.encoder = encoder
		cache.compressionThreshold = 512
		if cfg.Compression.Threshold > 0 {
			cache.compressionThreshold = cfg.Compression.Threshold
		}
		lg.Info().
			Int("threshold", cache.compressionThreshold).
			Str("level", level.String()).
			Msg("svm cache compression configured")
	}

	return cache, nil
}

// WithProjectId returns a shallow copy tagged with the project id so per-project
// telemetry and logs show the right owner. Matches evm.EvmJsonRpcCache.WithProjectId.
func (c *SvmJsonRpcCache) WithProjectId(projectId string) *SvmJsonRpcCache {
	lg := c.logger.With().Str("projectId", projectId).Logger()
	return &SvmJsonRpcCache{
		projectId:            projectId,
		policies:             c.policies,
		logger:               &lg,
		compressionThreshold: c.compressionThreshold,
		encoder:              c.encoder,
		decoder:              c.decoder,
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

	// Hard never-cache: effectful and freshness-critical methods are never read
	// from cache, regardless of any configured `finality: realtime` policy.
	// GetFinality maps them to Realtime (used for failsafe/metrics), but Realtime
	// is still a *cacheable* finality at the policy layer, so the exclusion must
	// be enforced here to honor the "never cached" guarantee.
	if neverCacheMethods[rpcReq.Method] {
		return nil, nil
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
	// below. Tracking both matters: without it a connector that is erroring or
	// timing out is indistinguishable from a cold cache, which reads as a
	// hit-rate problem instead of a latency problem — the exact confusion the
	// label was added to prevent.
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
			// "expired" / "data not available here", not a real failure. Every
			// data driver reports a cold key this way (memory, redis, dynamodb,
			// postgresql all return ErrRecordNotFound), so without this guard a
			// cold cache is indistinguishable from a broken one — it invents a
			// cache error rate, logs a failure per cold read, and pins the miss
			// counter to connector_error, which is the exact inversion the
			// reason label was added to prevent. Mirrors the EVM cache
			// (architecture/evm/json_rpc_cache.go).
			//   ErrRecordNotFound      — generic data connector miss
			//   ErrRecordExpired       — connector miss past TTL
			//   ErrEndpointMissingData — gRPC connector (prism) "range outside
			//                            available" / cold storage range
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
				Msg("svm cache get failed; trying next policy")
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
	// against the first policy so dashboards show a flat miss count.
	//
	// Precedence mirrors the EVM cache: a genuine miss outranks an error, so a
	// pool where one connector is down but another simply has no entry still
	// reads as a cold cache rather than an outage. `empty_result` is the
	// default for the case where every policy was skipped by the
	// skip-cache-read directive and nothing was actually attempted.
	//
	// SVM has no ttl_rejected branch — that guard is EVM's block-timestamp age
	// check, and there is no slot-based equivalent here.
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

	// Hard never-cache (mirrors Get): effectful methods (sendTransaction,
	// requestAirdrop, …) and sub-slot freshness-critical reads (getLatestBlockhash,
	// …) must never be stored, even if an operator configured a matching
	// `finality: realtime` policy. See neverCacheMethods in finality.go.
	if neverCacheMethods[rpcReq.Method] {
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
	// Compressed at most once, and only once a policy actually accepts the
	// payload: every matched connector stores the same bytes, and a multi-MB
	// getBlock that every policy rejects on size must not pay for zstd first.
	// Size limits gate on the ORIGINAL payload — they express a response-size
	// ceiling, not a storage-footprint one.
	var valueToStore []byte

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
		if valueToStore == nil {
			valueToStore = c.compress(payload)
		}
		telemetry.MetricCacheSetOriginalBytes.WithLabelValues(
			c.projectId, req.NetworkLabel(), rpcReq.Method,
			connector.Id(), policy.String(), ttlLabel,
		).Add(float64(len(payload)))
		if len(valueToStore) != len(payload) {
			telemetry.MetricCacheSetCompressedBytes.WithLabelValues(
				c.projectId, req.NetworkLabel(), rpcReq.Method,
				connector.Id(), policy.String(), ttlLabel,
			).Add(float64(len(valueToStore)))
		}
		if err := connector.Set(ctx, groupKey, requestKey, valueToStore, ttl); err != nil {
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

	groupKey, requestKey, err := c.generateKeys(req, rpcReq)
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
	resultBytes, err = c.decompress(resultBytes)
	if err != nil {
		return nil, err
	}

	jrr, err := common.NewJsonRpcResponseFromBytes(nil, resultBytes, nil)
	if err != nil {
		return nil, err
	}
	_ = jrr.SetID(rpcReq.ID)
	return jrr, nil
}

func (c *SvmJsonRpcCache) generateKeys(req *common.NormalizedRequest, rpcReq *common.JsonRpcRequest) (string, string, error) {
	requestKey, err := svmRequestKey(rpcReq)
	if err != nil {
		return "", "", err
	}
	slotRef := extractSlotRef(rpcReq)
	// Note: rpcReq is already locked by the caller when coming from doGet; Set does
	// not lock, so the key derivation runs on the whole params slice unlocked. That
	// mirrors evm.generateKeysForJsonRpcRequest which also takes the caller's
	// locking for granted.
	return fmt.Sprintf("%s:%s", req.NetworkId(), slotRef), requestKey, nil
}

// svmRequestKey derives the per-request cache key for SVM.
//
// It deliberately does NOT use req.CacheHash(): the shared hasher
// (common/json_rpc.go hashValue) lowercases every string param, which is the
// right normalization for EVM hex but catastrophic here — Solana base58
// pubkeys and transaction signatures are case-sensitive, so two DISTINCT valid
// accounts differing only by letter case would collapse onto one key and be
// served each other's data. This key preserves case exactly.
//
// encoding/json is the encoder on purpose (not sonic): the standard library
// documents that it sorts map keys, which is what makes the key deterministic
// across runs and across Go map iteration order. Its grammar is also both
// type- and structure-delimiting, so structurally different params cannot
// collide: "abc", ["a","bc"] and {"a":"bc"} all encode to distinct bytes, as
// do the number 1 and the string "1".
//
// Not memoized (unlike CacheHash, which caches on the request object): the
// JsonRpcRequest field that would hold it lives in common/ and is owned by the
// EVM derivation. Two marshal+hash passes per request (one Get, one Set) over
// a handful of small params is not worth a second memo field; revisit if a
// profile says otherwise.
func svmRequestKey(rpcReq *common.JsonRpcRequest) (string, error) {
	if rpcReq == nil {
		return "", fmt.Errorf("cannot derive svm cache key from a nil json-rpc request")
	}
	params := rpcReq.Params
	if params == nil {
		// Normalize "no params" and "params: []" to the same key — they are the
		// same call, and json.Marshal would otherwise emit `null` vs `[]`.
		params = []interface{}{}
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return "", fmt.Errorf("failed to encode svm cache key params: %w", err)
	}
	h := sha256.New()
	// Method is hashed as well as prefixed so a method name containing the ':'
	// separator cannot forge another method's key.
	_, _ = h.Write([]byte(rpcReq.Method))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(encoded)
	return fmt.Sprintf("%s:%x", rpcReq.Method, h.Sum(nil)), nil
}

// RequestKey exposes the SVM request-identity key to the rest of the pipeline.
//
// Any component that decides "are these two requests the same request?" on an
// SVM network must use THIS, never req.CacheHash(): the shared hasher
// lowercases string params, which collapses case-sensitive base58 pubkeys and
// signatures onto one identity. The cache learned that the hard way; in-flight
// multiplexing (erpc.Network.multiplexKey) has the same requirement, because a
// follower is handed the leader's response verbatim.
func RequestKey(ctx context.Context, r *common.NormalizedRequest) (string, error) {
	if r == nil {
		return "", fmt.Errorf("cannot derive svm request key from a nil request")
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil {
		return "", err
	}
	return svmRequestKey(rpcReq)
}

// compress returns the zstd-compressed payload when compression is enabled and
// the payload is both over the threshold and actually smaller compressed;
// otherwise it returns payload unchanged. Callers detect which happened by
// comparing lengths — the zstd magic number makes reads self-describing.
func (c *SvmJsonRpcCache) compress(payload []byte) []byte {
	if c.encoder == nil || len(payload) < c.compressionThreshold {
		return payload
	}
	compressed := c.encoder.EncodeAll(payload, nil)
	if len(compressed) >= len(payload) {
		return payload
	}
	return compressed
}

// decompress inflates a stored value when it carries the zstd magic number.
// The check is on the bytes, not on the current config, so entries written
// while compression was enabled stay readable after it is turned off.
func (c *SvmJsonRpcCache) decompress(stored []byte) ([]byte, error) {
	if len(stored) < 4 || stored[0] != 0x28 || stored[1] != 0xB5 || stored[2] != 0x2F || stored[3] != 0xFD {
		return stored, nil
	}
	if c.decoder == nil {
		return nil, fmt.Errorf("cached value is zstd-compressed but no decoder is configured")
	}
	decompressed, err := c.decoder.DecodeAll(stored, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress cached value: %w", err)
	}
	return decompressed, nil
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
