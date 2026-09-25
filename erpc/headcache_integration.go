package erpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/headcache"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

type headCacheBypassKey struct{}

// withHeadCacheBypass marks a context whose Forward must not be served from
// the head cache (hydration reads must reach upstreams).
func withHeadCacheBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, headCacheBypassKey{}, true)
}

// HeadCache returns the network's head cache, or nil when disabled.
func (n *Network) HeadCache() *headcache.Cache { return n.headCache }

// Connections belong to one registry/application context, never a process-global
// map. TLS identity and verification settings are part of connection identity.
type headCacheRedisPool struct {
	mu      sync.Mutex
	clients map[[32]byte]redis.UniversalClient
}

func (nr *NetworksRegistry) headCacheRedisClient(cfg *common.RedisConnectorConfig) (redis.UniversalClient, error) {
	p := &nr.headCacheRedis
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := nr.appCtx.Err(); err != nil {
		return nil, fmt.Errorf("head cache Redis context: %w", err)
	}
	if p.clients == nil {
		p.clients = make(map[[32]byte]redis.UniversalClient)
		context.AfterFunc(nr.appCtx, func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			for key, client := range p.clients {
				_ = client.Close()
				delete(p.clients, key)
			}
		})
	}
	// Do not use RedisConnectorConfig.MarshalJSON: it redacts credentials.
	identity, err := json.Marshal(struct {
		URI  string
		TLS  *common.TLSConfig
		Pool int
	}{strings.TrimSpace(cfg.URI), cfg.TLS, cfg.ConnPoolSize})
	if err != nil {
		return nil, fmt.Errorf("head cache Redis identity: %w", err)
	}
	key := sha256.Sum256(identity)
	if c, ok := p.clients[key]; ok {
		return c, nil
	}
	opts, err := redis.ParseURL(strings.TrimSpace(cfg.URI))
	if err != nil {
		return nil, fmt.Errorf("evm.headCache.redis.uri: %w", err)
	}
	if cfg.TLS != nil && cfg.TLS.Enabled {
		t, err := common.CreateTLSConfig(cfg.TLS)
		if err != nil {
			return nil, err
		}
		opts.TLSConfig = t
	}
	if cfg.ConnPoolSize > 0 {
		opts.PoolSize = cfg.ConnPoolSize
	}
	opts.ContextTimeoutEnabled = true
	c := redis.NewClient(opts)
	p.clients[key] = c
	return c, nil
}

// headCacheFingerprint hashes the complete configured hydration trust set.
// Alias types deliberately bypass the display serializers' redaction: secrets
// affect trust identity but only the digest ever leaves this function.
func headCacheFingerprint(prj *common.ProjectConfig, nw *common.NetworkConfig) (string, error) {
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
	// Cache sizing/storage is not part of upstream trust. Keep the effective
	// network's selectors, integrity, failsafe and other forwarding settings.
	network := *nw
	if nw.Evm != nil {
		evm := *nw.Evm
		evm.HeadCache = nil
		network.Evm = &evm
	}
	b, err := json.Marshal(struct {
		NetworkID        string
		Upstreams        []string
		Providers        []string
		UpstreamDefaults *upstreamTrust
		NetworkDefaults  *common.NetworkDefaults
		Integrity        *common.IntegrityConfig
		Network          *common.NetworkConfig
	}{nw.NetworkId(), upstreams, providers, (*upstreamTrust)(prj.UpstreamDefaults), prj.NetworkDefaults, prj.Integrity, &network})
	if err != nil {
		return "", fmt.Errorf("head cache trust fingerprint: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// initHeadCache builds and starts the head cache for an EVM network when
// evm.headCache.enabled is set. Errors are returned so misconfiguration is
// loud (shared mode never silently becomes local).
func (nr *NetworksRegistry) initHeadCache(network *Network, nwCfg *common.NetworkConfig) error {
	if nwCfg.Evm == nil || nwCfg.Evm.HeadCache == nil || !nwCfg.Evm.HeadCache.Enabled {
		return nil
	}
	hc := nwCfg.Evm.HeadCache
	hc.SetDefaults()
	if err := hc.Validate(); err != nil {
		return err
	}
	ns := hc.Namespace
	if ns == "" {
		ns = "default"
	}
	nr.project.cfgMu.RLock()
	fingerprint, err := headCacheFingerprint(nr.project.Config, nwCfg)
	nr.project.cfgMu.RUnlock()
	if err != nil {
		return err
	}
	ns += ":" + fingerprint
	var store headcache.Store
	switch hc.Mode {
	case common.HeadCacheModeShared:
		client, err := nr.headCacheRedisClient(hc.Redis)
		if err != nil {
			return err
		}
		store = headcache.NewRedisStore(client, headcache.RedisStoreOptions{SnapshotTTL: 2 * hc.MaxStaleness.Duration()})
	default:
		store = headcache.NewMemoryStore()
	}
	host, _ := os.Hostname()
	holder := fmt.Sprintf("%s/%d/%d", host, os.Getpid(), time.Now().UnixNano())
	opts := headcache.Options{
		Scope:        headcache.Scope{Namespace: ns, ProjectId: network.projectId, NetworkId: network.networkId},
		Holder:       holder,
		Depth:        hc.Depth,
		MaxBytes:     hc.MaxBytes,
		MaxBlockSize: hc.MaxBlockBytes,
		MaxPerTick:   hc.MaxPerTick,
		Concurrency:  hc.Concurrency,
		PollInterval: hc.PollInterval.Duration(),
		FetchTimeout: hc.FetchTimeout.Duration(),
		MaxStaleness: hc.MaxStaleness.Duration(),
		LeaseTTL:     hc.LeaseTTL.Duration(),
		MaxLogsRange: hc.MaxLogsRange,
		RecordTTL:    time.Duration(hc.Depth+16) * 30 * time.Second,
	}
	lg := network.logger.With().Str("component", "headCache").Str("mode", hc.Mode).Logger()
	f := &networkHeadFetcher{n: network}
	headFn := func(ctx context.Context) int64 {
		// Pollers can be disabled, dormant or behind. Subscription delivery must
		// advance without a user HTTP read waking a poller. Only the lease holder
		// calls headFn, and its whole-tick deadline bounds this discovery too.
		raw, err := f.call(ctx, "eth_blockNumber", []interface{}{})
		if err != nil {
			return 0
		}
		var quantity string
		if err := json.Unmarshal(raw, &quantity); err != nil {
			return 0
		}
		number, err := parseExplicitBlockNumber(quantity)
		if err != nil {
			return 0
		}
		return number
	}
	c := headcache.New(opts, store, f, headFn, &lg)
	network.headCache = c
	c.Start(nr.appCtx)
	go network.wireHeadCacheKick(nr.appCtx, &lg)
	lg.Info().Str("namespace", ns).Int64("depth", hc.Depth).Msg("head cache started")
	return nil
}

// wireHeadCacheKick subscribes the cache to upstream head advances once the
// upstreams are registered. The periodic poll covers the time before that.
func (n *Network) wireHeadCacheKick(ctx context.Context, lg *zerolog.Logger) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		ups := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.networkId)
		wired := 0
		for _, up := range ups {
			sp := up.EvmStatePoller()
			if sp == nil || sp.IsObjectNull() {
				continue
			}
			if reg, ok := sp.(interface{ OnLatestBlock(func(int64)) }); ok {
				reg.OnLatestBlock(func(int64) { n.headCache.Kick() })
				wired++
			}
		}
		if wired > 0 {
			lg.Debug().Int("upstreams", wired).Msg("head cache wired to head advances")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// networkHeadFetcher hydrates through the network's normal forwarding path
// (routing, failsafe, priority) while bypassing both caches.
type networkHeadFetcher struct{ n *Network }

func (f *networkHeadFetcher) call(ctx context.Context, method string, params []interface{}) (json.RawMessage, error) {
	jrq := common.NewJsonRpcRequest(method, params)
	if err := jrq.SetID(1); err != nil {
		return nil, fmt.Errorf("set head cache request id: %w", err)
	}
	rq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	rq.SetDirectives(&common.RequestDirectives{IsInternal: true, SkipCacheRead: "true", RetryEmpty: true})
	resp, err := f.n.Forward(withCacheWriteBypass(withHeadCacheBypass(ctx)), rq)
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

// tryServeHeadCache answers eth_getBlockByNumber/ByHash and eth_getLogs from
// the head cache when the request is fully covered. Any doubt is a miss.
func (n *Network) tryServeHeadCache(ctx context.Context, req *common.NormalizedRequest, method string) (*common.NormalizedResponse, bool) {
	c := n.headCache
	if c == nil || ctx.Value(headCacheBypassKey{}) != nil {
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
			if !validHeadCacheHash(ref) {
				return nil, false
			}
			raw, ok = c.BlockByHash(ref, full)
		} else {
			num, err := parseExplicitBlockNumber(ref)
			if err != nil {
				return nil, false
			}
			raw, ok = c.BlockByNumber(num, full)
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
		filter, err := headcache.ParseLogFilter(obj)
		if err != nil {
			return nil, false
		}
		var logs []json.RawMessage
		if rawHash, hasHash := obj["blockHash"]; hasHash {
			bh, ok := rawHash.(string)
			if !ok || !validHeadCacheHash(bh) {
				return nil, false
			}
			_, hasFrom := obj["fromBlock"]
			_, hasTo := obj["toBlock"]
			if hasFrom || hasTo {
				return nil, false
			}
			logs, ok = c.LogsByHash(bh, filter)
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
			logs, ok = c.LogsRange(from, to, filter)
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

func validHeadCacheHash(s string) bool {
	if len(s) != 66 || !strings.HasPrefix(s, "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}
