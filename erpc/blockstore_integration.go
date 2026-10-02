package erpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/redis/go-redis/v9"
)

type blockStoreBypassKey struct{}

// withBlockStoreBypass marks a context whose Forward must not be served from
// the head cache (hydration reads must reach upstreams).
func withBlockStoreBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, blockStoreBypassKey{}, true)
}

// BlockStore returns the network's head cache, or nil when disabled.
func (n *Network) BlockStore() *blockstore.Cache { return n.blockStore }

// blockStoreFingerprint hashes the complete configured hydration trust set.
// Alias types deliberately bypass the display serializers' redaction: secrets
// affect trust identity but only the digest ever leaves this function.
func blockStoreFingerprint(prj *common.ProjectConfig, nw *common.NetworkConfig) (string, error) {
	type upstreamTrust common.UpstreamConfig
	type providerTrust common.ProviderConfig
	var upstreams, providers []string
	for _, u := range prj.Upstreams {
		if u == nil {
			continue
		}
		b, err := json.Marshal((*upstreamTrust)(u))
		if err != nil {
			return "", fmt.Errorf("head cache upstream fingerprint: %w", err)
		}
		upstreams = append(upstreams, string(b))
	}
	for _, p := range prj.Providers {
		if p == nil {
			continue
		}
		overrides := make(map[string]*upstreamTrust, len(p.Overrides))
		for key, u := range p.Overrides {
			overrides[key] = (*upstreamTrust)(u)
		}
		b, err := json.Marshal(struct {
			*providerTrust
			Overrides map[string]*upstreamTrust `json:"overrides"`
		}{(*providerTrust)(p), overrides})
		if err != nil {
			return "", fmt.Errorf("head cache provider fingerprint: %w", err)
		}
		providers = append(providers, string(b))
	}
	sort.Strings(upstreams)
	sort.Strings(providers)
	b, err := json.Marshal(struct {
		NetworkID        string
		Upstreams        []string
		Providers        []string
		UpstreamDefaults *upstreamTrust
		NetworkDefaults  *common.NetworkDefaults
		Integrity        *common.IntegrityConfig
		Network          *common.NetworkConfig
	}{nw.NetworkId(), upstreams, providers, (*upstreamTrust)(prj.UpstreamDefaults), prj.NetworkDefaults, prj.Integrity, nw})
	if err != nil {
		return "", fmt.Errorf("head cache trust fingerprint: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// initBlockStore builds and starts the live and/or historical blockstore for an EVM network.
func (nr *NetworksRegistry) initBlockStore(network *Network, nwCfg *common.NetworkConfig) error {
	if nwCfg.Evm == nil || nwCfg.Evm.BlockStore == nil {
		return nil
	}
	hc := nwCfg.Evm.BlockStore
	historicalEnabled := hc.Historical.Enabled
	if !hc.Enabled && !historicalEnabled {
		return nil
	}
	hc.SetDefaults()
	if err := hc.Validate(); err != nil {
		return err
	}
	ns := hc.Namespace
	if ns == "" {
		ns = "default"
	}
	nr.project.cfgMu.RLock()
	fingerprint, err := blockStoreFingerprint(nr.project.Config, nwCfg)
	nr.project.cfgMu.RUnlock()
	if err != nil {
		return err
	}
	ns += ":" + fingerprint
	store, err := nr.blockStoreStore(hc)
	if err != nil {
		return err
	}
	scope := blockstore.Scope{Namespace: ns, ProjectId: network.projectId, NetworkId: network.networkId}
	if hc.Enabled {
		opts := blockstore.Options{
			Scope:        scope,
			Depth:        hc.Depth,
			MaxBytes:     hc.MaxBytes,
			MaxBlockSize: hc.MaxBlockBytes,
			MaxPerTick:   hc.MaxPerTick,
			Concurrency:  hc.Concurrency,
			PollInterval: hc.PollInterval.Duration(),
			FetchTimeout: hc.FetchTimeout.Duration(),
			MaxStaleness: hc.MaxStaleness.Duration(),
			MaxLogsRange: hc.MaxLogsRange,
			RecordTTL:    time.Duration(hc.Depth+16) * 30 * time.Second,
		}
		lg := network.logger.With().Str("component", "blockStore").Logger()
		f := &networkHeadFetcher{n: network}
		live := func(ctx context.Context) int64 {
			raw, err := f.call(ctx, "eth_blockNumber", []interface{}{})
			if err != nil {
				return -1
			}
			var quantity string
			if err := json.Unmarshal(raw, &quantity); err != nil {
				return -1
			}
			number, err := parseExplicitBlockNumber(quantity)
			if err != nil {
				return -1
			}
			return number
		}
		c := blockstore.New(opts, store, f, live, &lg)
		network.blockStore = c
		c.Start(nr.appCtx)
		lg.Info().Str("namespace", ns).Int64("depth", hc.Depth).Msg("blockstore started")
	}
	if historicalEnabled {
		connectorStore, ok := store.(*blockStoreConnectorStore)
		if !ok {
			return fmt.Errorf("historical blockstore requires the initialized Redis connector")
		}
		historicalStore := &blockStoreHistoricalStore{connector: connectorStore.connector}
		f := &networkHeadFetcher{n: network}
		network.historicalBlockStore = blockstore.NewHistorical(blockstore.HistoricalOptions{
			Scope: scope, TTL: hc.Historical.TTL.Duration(), MaxBlockSize: hc.MaxBlockBytes, MaxLogsRange: hc.MaxLogsRange,
		}, historicalStore, f, network.EvmHighestFinalizedBlockNumber)
		network.historicalWarmSem = make(chan struct{}, max(1, hc.Concurrency))
	}
	return nil
}

// networkHeadFetcher hydrates through the network's normal forwarding path
// (routing and failsafe) while bypassing both caches.
type networkHeadFetcher struct{ n *Network }

func (f *networkHeadFetcher) call(ctx context.Context, method string, params []interface{}) (json.RawMessage, error) {
	jrq := common.NewJsonRpcRequest(method, params)
	if err := jrq.SetID(1); err != nil {
		return nil, fmt.Errorf("set head cache request id: %w", err)
	}
	rq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	rq.SetDirectives(&common.RequestDirectives{IsInternal: true, SkipCacheRead: "true", RetryEmpty: true})
	resp, err := f.n.Forward(withCacheWriteBypass(withBlockStoreBypass(ctx)), rq)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("empty response")
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

func (f *networkHeadFetcher) BlockByNumber(ctx context.Context, n int64) (json.RawMessage, error) {
	return f.call(ctx, "eth_getBlockByNumber", []interface{}{fmt.Sprintf("0x%x", n), true})
}

func (f *networkHeadFetcher) HeaderByNumber(ctx context.Context, n int64) (json.RawMessage, error) {
	return f.call(ctx, "eth_getBlockByNumber", []interface{}{fmt.Sprintf("0x%x", n), false})
}

func (f *networkHeadFetcher) LogsByBlockHash(ctx context.Context, hash string) (json.RawMessage, error) {
	return f.call(ctx, "eth_getLogs", []interface{}{map[string]interface{}{"blockHash": hash}})
}

func (n *Network) warmHistoricalAsync(ctx context.Context, req *common.NormalizedRequest, method string, resp *common.NormalizedResponse) {
	h := n.historicalBlockStore
	if h == nil || n.historicalWarmSem == nil || n.appCtx == nil || cacheWriteBypassed(ctx) {
		return
	}
	if d := req.Directives(); d != nil && (d.IsInternal || d.UseUpstream != "" || d.IntegritySelector != "" || d.SkipCacheRead != "") {
		return
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return
	}
	jrq.RLock()
	params := append([]interface{}(nil), jrq.Params...)
	jrq.RUnlock()
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr.Error != nil {
		return
	}
	fill := func(ctx context.Context) error { return nil }
	switch method {
	case "eth_getBlockByNumber", "eth_getBlockByHash":
		if len(params) != 2 {
			return
		}
		full, ok := params[1].(bool)
		if !ok {
			return
		}
		block := append(json.RawMessage(nil), jrr.GetResultBytes()...)
		var number string
		if method == "eth_getBlockByNumber" {
			ref, ok := params[0].(string)
			if !ok {
				return
			}
			n, err := parseExplicitBlockNumber(ref)
			if err != nil {
				return
			}
			number = fmt.Sprintf("0x%x", n)
		} else {
			var body struct {
				Number string `json:"number"`
				Hash   string `json:"hash"`
			}
			if err := json.Unmarshal(block, &body); err != nil {
				return
			}
			requestedHash, ok := params[0].(string)
			if !ok || !strings.EqualFold(requestedHash, body.Hash) {
				return
			}
			number = body.Number
		}
		num, err := parseExplicitBlockNumber(number)
		if err != nil {
			return
		}
		if full {
			fill = func(ctx context.Context) error { return h.WarmBlockFromResult(ctx, num, block) }
		} else {
			fill = func(ctx context.Context) error { return h.WarmBlock(ctx, num) }
		}
	case "eth_getLogs":
		if len(params) != 1 {
			return
		}
		obj, ok := params[0].(map[string]interface{})
		if !ok {
			return
		}
		for key := range obj {
			if key != "fromBlock" && key != "toBlock" && key != "address" && key != "topics" {
				return
			}
		}
		if _, err := blockstore.ParseLogFilter(obj); err != nil {
			return
		}
		fs, ok1 := obj["fromBlock"].(string)
		ts, ok2 := obj["toBlock"].(string)
		if !ok1 || !ok2 {
			return
		}
		from, e1 := parseExplicitBlockNumber(fs)
		to, e2 := parseExplicitBlockNumber(ts)
		if e1 != nil || e2 != nil || to < from {
			return
		}
		limit := int64(0)
		if n.cfg != nil && n.cfg.Evm != nil && n.cfg.Evm.BlockStore != nil {
			limit = n.cfg.Evm.BlockStore.MaxLogsRange
		}
		if limit > 0 && to-from >= limit {
			return
		}
		fill = func(ctx context.Context) error {
			for height := from; height <= to; height++ {
				if err := h.WarmLogs(ctx, height); err != nil {
					return err
				}
			}
			return nil
		}
	default:
		return
	}
	select {
	case n.historicalWarmSem <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-n.historicalWarmSem }()
		ctx, cancel := context.WithTimeout(n.appCtx, 30*time.Second)
		defer cancel()
		if err := fill(ctx); err != nil && n.logger != nil {
			n.logger.Debug().Err(err).Str("method", method).Msg("historical blockstore warm failed")
		}
	}()
}

// tryServeBlockStore answers eth_getBlockByNumber/ByHash and eth_getLogs from
// the head cache when the request is fully covered. Any doubt is a miss.
func (n *Network) tryServeBlockStore(ctx context.Context, req *common.NormalizedRequest, method string) (*common.NormalizedResponse, bool) {
	c := n.blockStore
	if (c == nil && n.historicalBlockStore == nil) || ctx.Value(blockStoreBypassKey{}) != nil {
		return nil, false
	}
	// Directed requests (specific upstreams, integrity profiles, cache
	// bypass) are outside the default trust set the cache was built from.
	if d := req.Directives(); d != nil && (d.IsInternal || d.UseUpstream != "" || d.IntegritySelector != "" || d.SkipCacheRead != "") {
		return nil, false
	}
	switch method {
	case "eth_getBlockByNumber", "eth_getBlockByHash", "eth_getLogs":
	default:
		return nil, false
	}
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return nil, false
	}
	jrq.RLock()
	params := append([]interface{}(nil), jrq.Params...)
	jrq.RUnlock()

	var result interface{}
	switch method {
	case "eth_getBlockByNumber", "eth_getBlockByHash":
		if len(params) != 2 {
			return nil, false
		}
		ref, ok1 := params[0].(string)
		full, ok2 := params[1].(bool)
		if !ok1 || !ok2 {
			return nil, false
		}
		var raw json.RawMessage
		var ok bool
		if method == "eth_getBlockByHash" {
			if !validBlockStoreHash(ref) {
				return nil, false
			}
			if c != nil {
				raw, ok = c.BlockByHash(ref, full)
			}
			if !ok && n.historicalBlockStore != nil {
				if rec, hit := n.historicalBlockStore.ReadBlockByHash(ctx, ref); hit {
					raw, ok = rec.Block, true
					if !full {
						raw, err = rec.BlockJSON(false)
						ok = err == nil
					}
				}
			}
		} else {
			num, err := parseExplicitBlockNumber(ref)
			if err != nil {
				return nil, false
			}
			if c != nil {
				raw, ok = c.BlockByNumber(num, full)
			}
			if !ok && n.historicalBlockStore != nil {
				if rec, hit := n.historicalBlockStore.ReadBlockByNumber(ctx, num); hit {
					raw, ok = rec.Block, true
					if !full {
						raw, err = rec.BlockJSON(false)
						ok = err == nil
					}
				}
			}
		}
		if !ok {
			return nil, false
		}
		result = raw
	case "eth_getLogs":
		if len(params) != 1 {
			return nil, false
		}
		obj, ok := params[0].(map[string]interface{})
		if !ok {
			return nil, false
		}
		for k := range obj {
			switch k {
			case "address", "topics", "blockHash", "fromBlock", "toBlock":
			default:
				return nil, false // unknown filter member: let upstream decide
			}
		}
		filter, err := blockstore.ParseLogFilter(obj)
		if err != nil {
			return nil, false
		}
		var logs []json.RawMessage
		if rawHash, hasHash := obj["blockHash"]; hasHash {
			bh, ok := rawHash.(string)
			if !ok || !validBlockStoreHash(bh) {
				return nil, false
			}
			_, hasFrom := obj["fromBlock"]
			_, hasTo := obj["toBlock"]
			if hasFrom || hasTo {
				return nil, false
			}
			if c != nil {
				logs, ok = c.LogsByHash(bh, filter)
			} else {
				ok = false
			}
			if !ok && n.historicalBlockStore != nil {
				if rec, hit := n.historicalBlockStore.ReadLogsByHash(ctx, bh); hit {
					logs, err = rec.FilterLogs(filter, false)
					ok = err == nil
				}
			}
			if !ok {
				return nil, false
			}
		} else {
			fs, ok1 := obj["fromBlock"].(string)
			ts, ok2 := obj["toBlock"].(string)
			if !ok1 || !ok2 {
				return nil, false
			}
			from, e1 := parseExplicitBlockNumber(fs)
			to, e2 := parseExplicitBlockNumber(ts)
			if e1 != nil || e2 != nil {
				return nil, false
			}
			if c != nil {
				logs, ok = c.LogsRange(from, to, filter)
			} else {
				ok = false
			}
			if !ok && n.historicalBlockStore != nil {
				if records, hit := n.historicalBlockStore.ReadLogsRange(ctx, from, to); hit {
					logs = make([]json.RawMessage, 0)
					for _, rec := range records {
						filtered, e := rec.FilterLogs(filter, false)
						if e != nil {
							ok = false
							logs = nil
							break
						}
						logs = append(logs, filtered...)
						ok = true
					}
					if len(records) == 0 {
						ok = true
					}
				}
			}
			if !ok {
				return nil, false
			}
		}
		if logs == nil {
			logs = []json.RawMessage{}
		}
		result = logs
	}
	jrr, err := common.NewJsonRpcResponse(req.ID(), result, nil)
	if err != nil {
		return nil, false
	}
	resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	resp.SetFromCache(true)
	telemetry.CounterHandle(telemetry.MetricBlockStoreHitsTotal, n.projectId, n.networkId, method).Inc()
	return resp, true
}

// parseExplicitBlockNumber accepts only hex block numbers; tags resolve
// through the normal path (served-tip semantics may differ from our head).
func parseExplicitBlockNumber(s string) (int64, error) {
	if !strings.HasPrefix(s, "0x") || len(s) < 3 || (len(s) > 3 && s[2] == '0') {
		return 0, fmt.Errorf("not an explicit block number")
	}
	n, err := strconv.ParseUint(s[2:], 16, 63)
	return int64(n), err
}

func validBlockStoreHash(s string) bool {
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

type blockStoreConnectorStore struct {
	connector data.Connector
	redis     *data.RedisConnector
}

var _ blockstore.FleetStore = (*blockStoreConnectorStore)(nil)

func (s *blockStoreConnectorStore) partition(scope blockstore.Scope) (string, error) {
	identity, err := json.Marshal(struct {
		Namespace string
		ProjectID string
		NetworkID string
	}{scope.Namespace, scope.ProjectId, scope.NetworkId})
	if err != nil {
		return "", fmt.Errorf("marshal head cache scope: %w", err)
	}
	h := sha256.Sum256(identity)
	return "blockstore:v1:" + hex.EncodeToString(h[:]), nil
}

func (s *blockStoreConnectorStore) GetBlock(ctx context.Context, scope blockstore.Scope, hash string) (*blockstore.BlockRecord, error) {
	partition, err := s.partition(scope)
	if err != nil {
		return nil, err
	}
	value, err := s.connector.Get(ctx, data.ConnectorMainIndex, partition, strings.ToLower(hash), nil)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return nil, blockstore.ErrNotFound
	}
	var record blockstore.BlockRecord
	if err := json.Unmarshal(value, &record); err != nil {
		return nil, fmt.Errorf("decode head cache record: %w", err)
	}
	return &record, nil
}

func (s *blockStoreConnectorStore) PutBlock(ctx context.Context, scope blockstore.Scope, record *blockstore.BlockRecord, ttl time.Duration) error {
	if record == nil {
		return fmt.Errorf("cannot store nil head cache record")
	}
	partition, err := s.partition(scope)
	if err != nil {
		return err
	}
	value, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode head cache record: %w", err)
	}
	key := strings.ToLower(record.Hash)
	return s.connector.Set(ctx, partition, key, value, &ttl)
}

func (s *blockStoreConnectorStore) redisClient() (redis.UniversalClient, error) {
	if s.redis == nil {
		return nil, fmt.Errorf("head cache Redis connector is unavailable")
	}
	client := s.redis.Client()
	if client == nil {
		return nil, blockstore.ErrStoreUnavailable
	}
	return client, nil
}

func (s *blockStoreConnectorStore) fleetKeys(scope blockstore.Scope) (string, string, error) {
	partition, err := s.partition(scope)
	if err != nil {
		return "", "", err
	}
	return partition + ":fleet-lock", partition + ":fleet-snapshot", nil
}

func (s *blockStoreConnectorStore) Acquire(ctx context.Context, scope blockstore.Scope, ttl time.Duration) (blockstore.Lease, error) {
	if ttl < time.Millisecond {
		return nil, fmt.Errorf("head cache fleet lease TTL must be positive")
	}
	client, err := s.redisClient()
	if err != nil {
		return nil, err
	}
	lockKey, snapshotKey, err := s.fleetKeys(scope)
	if err != nil {
		return nil, err
	}
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("generate head cache fleet lease token: %w", err)
	}
	value := hex.EncodeToString(token[:])
	acquired, err := client.SetNX(ctx, lockKey, value, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("acquire head cache fleet lease: %w", err)
	}
	if !acquired {
		return nil, nil
	}
	return &blockStoreRedisLease{redis: s.redis, lockKey: lockKey, snapshotKey: snapshotKey, token: value}, nil
}

func (s *blockStoreConnectorStore) ReadSnapshot(ctx context.Context, scope blockstore.Scope) (*blockstore.Snapshot, error) {
	partition, err := s.partition(scope)
	if err != nil {
		return nil, err
	}
	value, err := s.connector.Get(ctx, data.ConnectorMainIndex, partition, "fleet-snapshot", nil)
	if err != nil {
		var notFound *common.ErrRecordNotFound
		if errors.As(err, &notFound) {
			return nil, blockstore.ErrNotFound
		}
		return nil, err
	}
	if len(value) == 0 {
		return nil, blockstore.ErrNotFound
	}
	var snapshot blockstore.Snapshot
	if err := json.Unmarshal(value, &snapshot); err != nil {
		return nil, fmt.Errorf("decode head cache fleet snapshot: %w", err)
	}
	return &snapshot, nil
}

type blockStoreRedisLease struct {
	redis       *data.RedisConnector
	lockKey     string
	snapshotKey string
	token       string
}

func (l *blockStoreRedisLease) client() (redis.UniversalClient, error) {
	if l.redis == nil {
		return nil, blockstore.ErrStoreUnavailable
	}
	client := l.redis.Client()
	if client == nil {
		return nil, blockstore.ErrStoreUnavailable
	}
	return client, nil
}

func (l *blockStoreRedisLease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if ttl < time.Millisecond {
		return false, fmt.Errorf("head cache fleet lease TTL must be positive")
	}
	const script = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) else return 0 end`
	client, err := l.client()
	if err != nil {
		return false, err
	}
	n, err := client.Eval(ctx, script, []string{l.lockKey}, l.token, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("renew head cache fleet lease: %w", err)
	}
	return n == 1, nil
}

func (l *blockStoreRedisLease) Publish(ctx context.Context, snapshot *blockstore.Snapshot, ttl time.Duration) (bool, error) {
	if snapshot == nil {
		return false, fmt.Errorf("cannot publish nil head cache fleet snapshot")
	}
	if ttl < time.Millisecond {
		return false, fmt.Errorf("head cache fleet snapshot TTL must be positive")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return false, fmt.Errorf("encode head cache fleet snapshot: %w", err)
	}
	const script = `if redis.call('GET', KEYS[1]) == ARGV[1] then redis.call('SET', KEYS[2], ARGV[2], 'PX', ARGV[3]); return 1 else return 0 end`
	client, err := l.client()
	if err != nil {
		return false, err
	}
	n, err := client.Eval(ctx, script, []string{l.lockKey, l.snapshotKey}, l.token, payload, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("publish head cache fleet snapshot: %w", err)
	}
	return n == 1, nil
}

func (l *blockStoreRedisLease) Release(ctx context.Context) error {
	const script = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) else return 0 end`
	client, err := l.client()
	if err != nil {
		return err
	}
	if _, err := client.Eval(ctx, script, []string{l.lockKey}, l.token).Result(); err != nil {
		return fmt.Errorf("release head cache fleet lease: %w", err)
	}
	return nil
}

// blockStoreStore builds the Redis store on the referenced evmJsonRpcCache
// connector. Keep the configured wrapper for its failsafe behavior, but verify
// that it ultimately uses Redis before sharing head-cache payloads.
func (nr *NetworksRegistry) blockStoreStore(hc *common.EvmBlockStoreConfig) (blockstore.Store, error) {
	var conn data.Connector
	if nr.evmJsonRpcCache != nil {
		conn = nr.evmJsonRpcCache.Connector(hc.ConnectorId)
	}
	resolved := conn
	for {
		u, ok := resolved.(interface{ Unwrap() data.Connector })
		if !ok {
			break
		}
		resolved = u.Unwrap()
	}
	rc, ok := resolved.(*data.RedisConnector)
	if !ok || rc == nil {
		return nil, fmt.Errorf("evm.blockStore.connectorId %q is not an initialized redis connector in database.evmJsonRpcCache", hc.ConnectorId)
	}
	return &blockStoreConnectorStore{connector: conn, redis: rc}, nil
}
