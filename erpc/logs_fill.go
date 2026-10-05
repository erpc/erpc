package erpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
)

// initLogsFill builds the standalone small-range eth_getLogs fill. It shares
// the blockstore connector (redis) when connectorId is set, otherwise it uses
// a bounded per-network in-memory store. It does not require the live window
// or the historical cache.
func (nr *NetworksRegistry) initLogsFill(network *Network, hc *common.EvmBlockStoreConfig, scope blockstore.Scope) error {
	lf := hc.LogsFill
	var store blockstore.LogsFillStore
	// Cross-replica fill locking needs the shared store; the in-memory store
	// is per replica and keeps the singleflight-only behavior.
	var peerWait time.Duration
	if hc.ConnectorId != "" {
		s, err := nr.blockStoreStore(hc)
		if err != nil {
			return err
		}
		store = s.(*blockStoreConnectorStore)
		if lf.PeerWait != nil {
			peerWait = lf.PeerWait.Duration()
		}
	} else {
		store = blockstore.NewMemoryLogsFillStore(lf.MemoryMaxBytes)
	}
	unfinalizedTTL := func() time.Duration {
		if lf.UnfinalizedTTL > 0 {
			return lf.UnfinalizedTTL.Duration()
		}
		return logsFillUnfinalizedTTL(network.EvmBlockTime())
	}
	fetch := func(ctx context.Context, from, to int64) (json.RawMessage, error) {
		return network.fetchUnfilteredLogs(ctx, from, to)
	}
	network.logsFiller = blockstore.NewLogsFiller(blockstore.LogsFillOptions{
		Scope:          scope,
		MaxRange:       lf.MaxRange,
		FinalizedTTL:   lf.FinalizedTTL.Duration(),
		UnfinalizedTTL: unfinalizedTTL,
		EmptyTipGuard:  lf.EmptyTipGuard,
		FetchTimeout:   hc.FetchTimeout.Duration(),
		MaxEntryBytes:  hc.MaxBlockBytes,
		PeerWait:       peerWait,
		Concurrency:    hc.Concurrency,
		// The live window (when enabled) adopts each fill's per-height
		// lists as its logs, so the same blocks are never fetched twice.
		OnFill: func(ctx context.Context, entries []*blockstore.BlockLogs) {
			c := network.blockStore
			if c == nil || network.blockStoreAdoptSem == nil || network.appCtx == nil {
				return
			}
			// Adoption validates against held headers only (no fetch);
			// keep it off the client's response path anyway.
			select {
			case network.blockStoreAdoptSem <- struct{}{}:
			default:
				return
			}
			go func() {
				defer func() { <-network.blockStoreAdoptSem }()
				actx, cancel := context.WithTimeout(network.appCtx, 30*time.Second)
				defer cancel()
				c.AdoptLogs(actx, entries)
			}()
		},
	}, store, fetch, network.EvmHighestLatestBlockNumber, network.EvmHighestFinalizedBlockNumber)
	return nil
}

// logsFillUnfinalizedTTL keeps an unfinalized entry for about one block: long
// enough for every poller of the same recent range to share one fill, short
// enough that a reorged height is wrong for at most one block interval. 2s
// floor (sub-second chains still coalesce a polling burst), 12s ceiling (an
// unknown or inflated block-time estimate cannot keep head data for minutes).
func logsFillUnfinalizedTTL(blockTime time.Duration) time.Duration {
	const floor, ceiling = 2 * time.Second, 12 * time.Second
	return min(max(blockTime, floor), ceiling)
}

// fetchUnfilteredLogs performs one eth_getLogs{fromBlock,toBlock} through the
// network's normal path (selection, failsafe, retries, integrity) while
// bypassing the blockstore, the logs fill itself, and ordinary cache reads and
// writes.
func (n *Network) fetchUnfilteredLogs(ctx context.Context, from, to int64) (json.RawMessage, error) {
	jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{map[string]interface{}{
		"fromBlock": fmt.Sprintf("0x%x", from),
		"toBlock":   fmt.Sprintf("0x%x", to),
	}})
	if err := jrq.SetID(1); err != nil {
		return nil, fmt.Errorf("set logs fill request id: %w", err)
	}
	rq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	// Same directives as networkHeadFetcher.call: an internal hydration fetch that
	// `matchRequestKind: internal` failsafe policies match and the integrity
	// pipeline skips (erpc re-serves this data itself), always reaching upstreams.
	rq.SetDirectives(&common.RequestDirectives{IsInternal: true, SkipCacheRead: "true", RetryEmpty: true})
	// Counted with every other blockstore-initiated upstream request. A fill
	// answers the client's own eth_getLogs miss in its place.
	telemetry.CounterHandle(telemetry.MetricBlockStoreFetchTotal, n.projectId, n.networkId, string(blockstore.PayloadLogs), blockstore.FetchReasonFill).Inc()
	resp, err := n.Forward(withCacheWriteBypass(withBlockStoreBypass(ctx)), rq)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("empty logs fill response")
	}
	defer resp.Release()
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		return nil, err
	}
	if jrr.Error != nil {
		return nil, jrr.Error
	}
	return append(json.RawMessage(nil), jrr.GetResultBytes()...), nil
}

func (n *Network) logsFillMetric(outcome, reason string) {
	telemetry.CounterHandle(telemetry.MetricBlockStoreLogsFillTotal, n.projectId, n.networkId, outcome, reason).Inc()
}

// tryServeLogsFill answers eth_getLogs with explicit hex fromBlock/toBlock
// within logsFill.maxRange and at or below the network head. ok=false means
// the caller forwards the original request unchanged.
func (n *Network) tryServeLogsFill(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, bool) {
	f := n.logsFiller
	if f == nil || ctx.Value(blockStoreBypassKey{}) != nil {
		return nil, false
	}
	// Same gate as tryServeBlockStore: connector-ID patterns bypass, "false" does not.
	if blockStoreDirected(req.Directives()) {
		n.logsFillMetric(blockstore.LogsFillSkipped, "directive")
		return nil, false
	}
	if req.ParentRequestId() != nil || req.IsCompositeRequest() {
		return nil, false
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, false
	}
	jrq.RLock()
	params := append([]interface{}(nil), jrq.Params...)
	jrq.RUnlock()
	from, to, filter, reason := parseLogsFillParams(params)
	if reason != "" {
		n.logsFillMetric(blockstore.LogsFillSkipped, reason)
		return nil, false
	}
	// Never answer locally what the network would reject, and never make the
	// unfiltered call wider than the upstream auto-splitting threshold.
	maxRange := int64(0)
	if n.upstreamsRegistry != nil {
		for _, u := range n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId) {
			if u == nil || u.Config() == nil || u.Config().Evm == nil {
				continue
			}
			if th := u.Config().Evm.GetLogsAutoSplittingRangeThreshold; th > 0 && (maxRange == 0 || th < maxRange) {
				maxRange = th
			}
		}
	}
	if evm := n.cfg.Evm; evm != nil {
		if lim := evm.GetLogsMaxAllowedRange; lim > 0 && (maxRange == 0 || lim < maxRange) {
			maxRange = lim
		}
	}
	// Range is already enforced through maxRange (as "range_too_large");
	// addresses and topics share the network's exact counting.
	if n.exceedsGetLogsLimits(params[0].(map[string]interface{}), from, from) {
		n.logsFillMetric(blockstore.LogsFillSkipped, "limit")
		return nil, false
	}

	res := f.Serve(ctx, from, to, maxRange, filter)
	reasonLabel := res.Reason
	if reasonLabel == "" {
		reasonLabel = "ok"
	}
	n.logsFillMetric(res.Outcome, reasonLabel)
	if !res.OK {
		return nil, false
	}
	jrr, err := common.NewJsonRpcResponse(req.ID(), res.Logs, nil)
	if err != nil {
		n.logsFillMetric(blockstore.LogsFillFallback, "encode_error")
		return nil, false
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	resp.SetFromCache(res.Outcome == blockstore.LogsFillHit)
	return resp, true
}

// parseLogsFillParams accepts exactly one filter object with hex
// fromBlock/toBlock and optional address/topics. Any other shape returns a
// skip reason.
func parseLogsFillParams(params []interface{}) (int64, int64, *blockstore.LogFilter, string) {
	if len(params) != 1 {
		return 0, 0, nil, "params"
	}
	obj, ok := params[0].(map[string]interface{})
	if !ok {
		return 0, 0, nil, "params"
	}
	for k := range obj {
		switch k {
		case "address", "topics", "fromBlock", "toBlock":
		default:
			return 0, 0, nil, "params"
		}
	}
	fs, ok1 := obj["fromBlock"].(string)
	ts, ok2 := obj["toBlock"].(string)
	if !ok1 || !ok2 {
		return 0, 0, nil, "not_explicit_range"
	}
	from, e1 := parseExplicitBlockNumber(fs)
	to, e2 := parseExplicitBlockNumber(ts)
	if e1 != nil || e2 != nil {
		return 0, 0, nil, "not_explicit_range"
	}
	if to < from {
		return 0, 0, nil, "invalid_range"
	}
	filter, err := blockstore.ParseLogFilter(obj)
	if err != nil {
		return 0, 0, nil, "filter"
	}
	return from, to, filter, ""
}

var _ blockstore.LogsFillStore = (*blockStoreConnectorStore)(nil)

func logsFillRangeKey(height int64) string { return "logs/" + strconv.FormatInt(height, 10) }

func (s *blockStoreConnectorStore) GetBlockLogs(ctx context.Context, scope blockstore.Scope, height int64) (*blockstore.BlockLogs, error) {
	partition, err := s.partition(scope)
	if err != nil {
		return nil, err
	}
	value, err := s.connector.Get(ctx, data.ConnectorMainIndex, partition, logsFillRangeKey(height), nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return nil, fmt.Errorf("get logs fill entry: %w: %w", blockstore.ErrNotFound, err)
		}
		return nil, fmt.Errorf("get logs fill entry: %w: %w", blockstore.ErrStoreUnavailable, err)
	}
	if len(value) == 0 {
		return nil, blockstore.ErrNotFound
	}
	var entry blockstore.BlockLogs
	if err := json.Unmarshal(value, &entry); err != nil {
		return nil, fmt.Errorf("decode logs fill entry: %w", err)
	}
	if entry.Number != height || entry.Logs == nil {
		return nil, errors.New("logs fill entry does not match its height")
	}
	return &entry, nil
}

func (s *blockStoreConnectorStore) PutBlockLogs(ctx context.Context, scope blockstore.Scope, entry *blockstore.BlockLogs, ttl time.Duration) error {
	if entry == nil {
		return fmt.Errorf("cannot store nil logs fill entry")
	}
	partition, err := s.partition(scope)
	if err != nil {
		return err
	}
	value, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode logs fill entry: %w", err)
	}
	return s.connector.Set(ctx, partition, logsFillRangeKey(entry.Number), value, &ttl)
}

var _ blockstore.LogsFillLocker = (*blockStoreConnectorStore)(nil)

var _ blockstore.FillLocker = (*blockStoreConnectorStore)(nil)

func (s *blockStoreConnectorStore) fillLockKey(scope blockstore.Scope, key string) (string, error) {
	partition, err := s.partition(scope)
	if err != nil {
		return "", err
	}
	return partition + ":fill-lock/" + key, nil
}

// TryLockFill takes a token-fenced SET NX PX lock on one fill range.
func (s *blockStoreConnectorStore) TryLockFill(ctx context.Context, scope blockstore.Scope, from, to int64, ttl time.Duration) (func(context.Context), bool, error) {
	return s.TryLock(ctx, scope, fmt.Sprintf("%d-%d", from, to), ttl)
}

func (s *blockStoreConnectorStore) FillLocked(ctx context.Context, scope blockstore.Scope, from, to int64) (bool, error) {
	return s.Locked(ctx, scope, fmt.Sprintf("%d-%d", from, to))
}

// TryLock takes a token-fenced SET NX PX lock on <partition>:fill-lock/<key>.
// release deletes it only while the token still matches, so an expired lock
// that a peer has since re-acquired is never removed. Shared by the logs fill
// (key "<from>-<to>") and live-window payload fills (key "<kind>/<hash>").
func (s *blockStoreConnectorStore) TryLock(ctx context.Context, scope blockstore.Scope, lockKey string, ttl time.Duration) (func(context.Context), bool, error) {
	if ttl < time.Millisecond {
		return nil, false, fmt.Errorf("fill lock TTL must be positive")
	}
	client, err := s.redisClient()
	if err != nil {
		return nil, false, err
	}
	key, err := s.fillLockKey(scope, lockKey)
	if err != nil {
		return nil, false, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, false, fmt.Errorf("generate fill lock token: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	acquired, err := client.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("acquire fill lock: %w", err)
	}
	if !acquired {
		return nil, false, nil
	}
	release := func(ctx context.Context) {
		const script = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end`
		// Best effort: on failure the lock expires after ttl and waiters
		// fall back at peerWait.
		_ = client.Eval(ctx, script, []string{key}, token).Err()
	}
	return release, true, nil
}

// Locked reports whether some holder currently has the fill lock for key.
func (s *blockStoreConnectorStore) Locked(ctx context.Context, scope blockstore.Scope, lockKey string) (bool, error) {
	client, err := s.redisClient()
	if err != nil {
		return false, err
	}
	key, err := s.fillLockKey(scope, lockKey)
	if err != nil {
		return false, err
	}
	n, err := client.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("check fill lock: %w", err)
	}
	return n > 0, nil
}
