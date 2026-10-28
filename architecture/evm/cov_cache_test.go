package evm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/klauspost/compress/zstd"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// covCacheConn is a race-safe in-memory connector stub. Unlike the testify
// data.MockConnector it never reflects over the request metadata, so it can be
// used by several fan-out goroutines at once under -race. Values are keyed by
// range key only (the partition key carries the block ref, which differs
// between the reverse and main index lookups).
type covCacheConn struct {
	id       string
	mu       sync.Mutex
	store    map[string][]byte
	getErr   error
	getBytes []byte // when non-nil, returned for every Get instead of store
	setErr   error
	gets     atomic.Int32
	sets     atomic.Int32
	lastTTL  *time.Duration
	lastPK   string
	lastIdx  string
	setValue []byte
}

var _ data.Connector = (*covCacheConn)(nil)

func covCacheNewConn(id string) *covCacheConn {
	return &covCacheConn{id: id, store: map[string][]byte{}}
}

func (c *covCacheConn) Id() string { return c.id }

func (c *covCacheConn) Get(_ context.Context, index, partitionKey, rangeKey string, _ interface{}) ([]byte, error) {
	c.gets.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastIdx = index
	if c.getErr != nil {
		return nil, c.getErr
	}
	if c.getBytes != nil {
		return c.getBytes, nil
	}
	v, ok := c.store[rangeKey]
	if !ok {
		return nil, common.NewErrRecordNotFound(partitionKey, rangeKey, c.id)
	}
	return v, nil
}

func (c *covCacheConn) Set(_ context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	c.sets.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.setErr != nil {
		return c.setErr
	}
	cp := append([]byte(nil), value...)
	c.store[rangeKey] = cp
	c.setValue = cp
	c.lastPK = partitionKey
	c.lastTTL = ttl
	return nil
}

func (c *covCacheConn) Delete(context.Context, string, string) error { return nil }
func (c *covCacheConn) List(context.Context, string, int, string) ([]data.KeyValuePair, string, error) {
	return nil, "", nil
}
func (c *covCacheConn) Lock(context.Context, string, time.Duration) (data.DistributedLock, error) {
	return nil, errors.New("not supported")
}
func (c *covCacheConn) WatchCounterInt64(context.Context, string) (<-chan data.CounterInt64State, func(), error) {
	return nil, func() {}, nil
}
func (c *covCacheConn) PublishCounterInt64(context.Context, string, data.CounterInt64State) error {
	return nil
}

func (c *covCacheConn) snapshot() (pk string, idx string, val []byte, ttl *time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastPK, c.lastIdx, c.setValue, c.lastTTL
}

type covCachePolicyOpt func(*common.CachePolicyConfig)

func covCacheWithEmpty(e common.CacheEmptyBehavior) covCachePolicyOpt {
	return func(c *common.CachePolicyConfig) { c.Empty = e }
}

func covCacheWithTTL(d time.Duration) covCachePolicyOpt {
	return func(c *common.CachePolicyConfig) { c.TTL = common.FixedDuration(d) }
}

func covCachePolicy(t *testing.T, conn data.Connector, method string, fin common.DataFinalityState, opts ...covCachePolicyOpt) *data.CachePolicy {
	t.Helper()
	cfg := &common.CachePolicyConfig{
		Connector: conn.Id(),
		Network:   "*",
		Method:    method,
		Finality:  fin,
	}
	for _, o := range opts {
		o(cfg)
	}
	p, err := data.NewCachePolicy(cfg, conn)
	require.NoError(t, err)
	return p
}

func covCacheNew(lg zerolog.Logger, project string, policies ...*data.CachePolicy) *EvmJsonRpcCache {
	return &EvmJsonRpcCache{projectId: project, logger: &lg, policies: policies}
}

func covCacheDiscard(level zerolog.Level) zerolog.Logger {
	return zerolog.New(io.Discard).Level(level)
}

func covCacheReq(method, params string, ntw common.Network) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(
		fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s,"id":7}`, method, params),
	))
	if ntw != nil {
		req.SetNetwork(ntw)
	}
	return req
}

func covCacheResp(req *common.NormalizedRequest, result interface{}) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(common.MustNewJsonRpcResponse(7, result, nil))
}

func covCacheBlock(num string) map[string]interface{} {
	return map[string]interface{}{"number": num, "hash": "0x" + strings.Repeat("ab", 32)}
}

func TestCovCache_NewEvmJsonRpcCache(t *testing.T) {
	ctx := context.Background()
	lg := covCacheDiscard(zerolog.Disabled)
	memConn := func(id string, tags ...string) *common.ConnectorConfig {
		return &common.ConnectorConfig{
			Id:     id,
			Driver: common.DriverMemory,
			Tags:   tags,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"},
		}
	}

	t.Run("InvalidConnectorDriver", func(t *testing.T) {
		_, err := NewEvmJsonRpcCache(ctx, &lg, &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{{Id: "bad", Driver: "nope"}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create connector bad")
		var drvErr *common.ErrInvalidConnectorDriver
		assert.ErrorAs(t, err, &drvErr)
	})

	t.Run("PolicyReferencesUnknownConnector", func(t *testing.T) {
		_, err := NewEvmJsonRpcCache(ctx, &lg, &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{memConn("m1")},
			Policies:   []*common.CachePolicyConfig{{Connector: "missing", Network: "*", Method: "*"}},
		})
		require.Error(t, err)
		assert.Equal(t, "connector missing not found for policy", err.Error())
	})

	t.Run("InvalidPolicySize", func(t *testing.T) {
		bad := "lots"
		_, err := NewEvmJsonRpcCache(ctx, &lg, &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{memConn("m1")},
			Policies:   []*common.CachePolicyConfig{{Connector: "m1", Network: "*", Method: "*", MaxItemSize: &bad}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to create policy")
		assert.Contains(t, err.Error(), "invalid maxItemSize")
	})

	t.Run("ConnectorTagsGatePolicies", func(t *testing.T) {
		c, err := NewEvmJsonRpcCache(ctx, &lg, &common.CacheConfig{
			Connectors: []*common.ConnectorConfig{memConn("tagged", "systx"), memConn("plain")},
			Policies: []*common.CachePolicyConfig{
				{Connector: "tagged", Network: "*", Method: "*"},
				{Connector: "plain", Network: "*", Method: "*"},
			},
		})
		require.NoError(t, err)
		require.Len(t, c.policies, 2)
		ok, err := c.policies[0].MatchesUpstreamSelector("alchemy")
		require.NoError(t, err)
		assert.False(t, ok, "tagged connector must be excluded by a non-matching selector")
		ok, _ = c.policies[0].MatchesUpstreamSelector("systx")
		assert.True(t, ok)
		ok, _ = c.policies[1].MatchesUpstreamSelector("alchemy")
		assert.True(t, ok, "untagged connector is always eligible")
		assert.False(t, c.compressionEnabled)
	})

	t.Run("CompressionLevels", func(t *testing.T) {
		cases := []struct {
			level     string
			threshold int
			wantLevel zstd.EncoderLevel
			wantThr   int
		}{
			{"", 0, zstd.SpeedFastest, 512},
			{"fastest", 10, zstd.SpeedFastest, 10},
			{"DEFAULT", 0, zstd.SpeedDefault, 512},
			{"better", 0, zstd.SpeedBetterCompression, 512},
			{"best", 2048, zstd.SpeedBestCompression, 2048},
			{"turbo", 0, zstd.SpeedFastest, 512},
		}
		for _, tc := range cases {
			t.Run("level="+tc.level, func(t *testing.T) {
				c, err := NewEvmJsonRpcCache(ctx, &lg, &common.CacheConfig{
					Compression: &common.CompressionConfig{Enabled: util.BoolPtr(true), ZstdLevel: tc.level, Threshold: tc.threshold},
				})
				require.NoError(t, err)
				assert.True(t, c.compressionEnabled)
				assert.Equal(t, tc.wantLevel, c.compressionLevel)
				assert.Equal(t, tc.wantThr, c.compressionThreshold)

				clone := c.WithProjectId("p1")
				assert.Equal(t, "p1", clone.projectId)
				assert.Equal(t, tc.wantLevel, clone.compressionLevel)
				assert.Equal(t, tc.wantThr, clone.compressionThreshold)
			})
		}
	})

	t.Run("CompressionDisabledExplicitly", func(t *testing.T) {
		c, err := NewEvmJsonRpcCache(ctx, &lg, &common.CacheConfig{
			Compression: &common.CompressionConfig{Enabled: util.BoolPtr(false)},
		})
		require.NoError(t, err)
		assert.False(t, c.compressionEnabled)
		assert.Nil(t, c.encoderPool)
	})
}

func TestCovCache_IsObjectNull(t *testing.T) {
	var nilCache *EvmJsonRpcCache
	assert.True(t, nilCache.IsObjectNull())
	assert.True(t, (&EvmJsonRpcCache{}).IsObjectNull())
	lg := covCacheDiscard(zerolog.Disabled)
	assert.False(t, covCacheNew(lg, "p").IsObjectNull())
}

func TestCovCache_SetThenGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}
	conn := covCacheNewConn("rt")
	c := covCacheNew(covCacheDiscard(zerolog.TraceLevel), "covcache-rt",
		covCachePolicy(t, conn, "eth_getBlockByNumber", common.DataFinalityStateFinalized, covCacheWithTTL(time.Hour)))

	req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
	require.NoError(t, c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10"))))
	pk, _, stored, ttl := conn.snapshot()
	assert.Equal(t, "test-network:16", pk, "numeric block keys the partition by decimal block number")
	require.NotNil(t, ttl)
	assert.Equal(t, time.Hour, *ttl)
	assert.Contains(t, string(stored), `"number":"0x10"`)

	getReq := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
	resp, err := c.Get(ctx, getReq)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.True(t, resp.FromCache())
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, jrr.GetResultString(), `"number":"0x10"`)
	assert.Equal(t, int64(7), jrr.ID())
	_, idx, _, _ := conn.snapshot()
	assert.Equal(t, data.ConnectorMainIndex, idx)

	// A different block number is a different key: miss.
	miss, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x11",false]`, ntw))
	require.NoError(t, err)
	assert.Nil(t, miss)
}

func TestCovCache_GetFinalityPolicySelection(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		finality common.DataFinalityState
		want     map[string]int32 // connector id -> expected Get calls
	}{
		{"Finalized", common.DataFinalityStateFinalized, map[string]int32{"fin": 1, "unfin": 1, "rt": 0}},
		{"Unfinalized", common.DataFinalityStateUnfinalized, map[string]int32{"fin": 0, "unfin": 1, "rt": 0}},
		{"Realtime", common.DataFinalityStateRealtime, map[string]int32{"fin": 0, "unfin": 0, "rt": 1}},
		{"Unknown", common.DataFinalityStateUnknown, map[string]int32{"fin": 1, "unfin": 1, "rt": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conns := map[string]*covCacheConn{
				"fin":   covCacheNewConn("fin"),
				"unfin": covCacheNewConn("unfin"),
				"rt":    covCacheNewConn("rt"),
			}
			c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-fin",
				covCachePolicy(t, conns["fin"], "*", common.DataFinalityStateFinalized),
				covCachePolicy(t, conns["unfin"], "*", common.DataFinalityStateUnfinalized),
				covCachePolicy(t, conns["rt"], "*", common.DataFinalityStateRealtime),
			)
			req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, &testNetwork{finalityState: tc.finality})
			resp, err := c.Get(ctx, req)
			require.NoError(t, err)
			assert.Nil(t, resp, "all connectors are empty")
			for id, want := range tc.want {
				assert.Equal(t, want, conns[id].gets.Load(), "connector %s", id)
			}
		})
	}
}

func TestCovCache_GetPolicyMatching(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}

	t.Run("SameConnectorDedupedAcrossPolicies", func(t *testing.T) {
		conn := covCacheNewConn("shared")
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-dedup",
			covCachePolicy(t, conn, "eth_*", common.DataFinalityStateFinalized),
			covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized),
		)
		resp, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(1), conn.gets.Load())
	})

	t.Run("MethodAndParamsMismatchSkipsCache", func(t *testing.T) {
		conn := covCacheNewConn("pm")
		p, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "pm", Network: "*", Method: "eth_getBlockByNumber",
			Params: []interface{}{"0x20"}, Finality: common.DataFinalityStateFinalized,
		}, conn)
		require.NoError(t, err)
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-pm", p)
		resp, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
		require.NoError(t, err)
		assert.Nil(t, resp)
		resp, err = c.Get(ctx, covCacheReq("eth_getBalance", `["0xabc","0x10"]`, ntw))
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(0), conn.gets.Load(), "no policy matched, connector never consulted")

		_, err = c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x20",false]`, ntw))
		require.NoError(t, err)
		assert.Equal(t, int32(1), conn.gets.Load(), "matching params consult the connector")
	})

	t.Run("InvalidMethodPatternErrorsOnGetAndSet", func(t *testing.T) {
		conn := covCacheNewConn("bad")
		c := covCacheNew(covCacheDiscard(zerolog.TraceLevel), "covcache-badpat",
			covCachePolicy(t, conn, "(", common.DataFinalityStateFinalized))
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
		_, err := c.Get(ctx, req)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected closing parenthesis")

		err = c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected closing parenthesis")
		assert.Equal(t, int32(0), conn.sets.Load())
	})

	t.Run("InvalidRequestBody", func(t *testing.T) {
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-badbody",
			covCachePolicy(t, covCacheNewConn("x"), "*", common.DataFinalityStateUnknown))
		req := common.NewNormalizedRequest([]byte(`{not json`))
		_, err := c.Get(ctx, req)
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal), "got %v", err)

		req2 := common.NewNormalizedRequest([]byte(`{not json`))
		err = c.Set(ctx, req2, common.NewNormalizedResponse().WithJsonRpcResponse(common.MustNewJsonRpcResponse(1, "0x1", nil)))
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeJsonRpcRequestUnmarshal), "got %v", err)
	})
}

func TestCovCache_GetConnectorOutcomes(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}
	newCache := func(t *testing.T, conn *covCacheConn, opts ...covCachePolicyOpt) *EvmJsonRpcCache {
		return covCacheNew(covCacheDiscard(zerolog.DebugLevel), "covcache-outcomes",
			covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized, opts...))
	}

	semanticMisses := []error{
		common.NewErrRecordNotFound("pk", "rk", "x"),
		common.NewErrRecordExpired("pk", "rk", "x", 2, 1),
		common.NewErrEndpointMissingData(errors.New("range outside available"), nil),
	}
	for _, e := range semanticMisses {
		t.Run("SemanticMiss/"+e.(interface{ CodeChain() string }).CodeChain(), func(t *testing.T) {
			conn := covCacheNewConn("sem")
			conn.getErr = e
			resp, err := newCache(t, conn).Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
			require.NoError(t, err)
			assert.Nil(t, resp)
			assert.Equal(t, int32(1), conn.gets.Load())
		})
	}

	t.Run("ConnectorErrorFallsThroughAsMiss", func(t *testing.T) {
		conn := covCacheNewConn("err")
		conn.getErr = errors.New("redis: connection refused")
		resp, err := newCache(t, conn).Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
		require.NoError(t, err, "connector errors are not surfaced to the caller")
		assert.Nil(t, resp)
	})

	t.Run("EmptyBytesIsMiss", func(t *testing.T) {
		conn := covCacheNewConn("eb")
		conn.getBytes = []byte{}
		resp, err := newCache(t, conn).Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(1), conn.gets.Load())
	})

	t.Run("UnknownMethodHasNoBlockRef", func(t *testing.T) {
		conn := covCacheNewConn("um")
		conn.getBytes = []byte(`"0x1"`)
		resp, err := newCache(t, conn).Get(ctx, covCacheReq("custom_unknownMethod", `[]`, ntw))
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(0), conn.gets.Load(), "no block ref: the connector must not be read")
	})

	t.Run("MalformedBlockNumber", func(t *testing.T) {
		conn := covCacheNewConn("mb")
		conn.getBytes = []byte(`{"number":"0x1"}`)
		c := newCache(t, conn)
		req := covCacheReq("eth_getBlockByNumber", `["0xzz",false]`, ntw)
		resp, err := c.Get(ctx, req)
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(0), conn.gets.Load())

		setReq := covCacheReq("eth_getBlockByNumber", `["0xzz",false]`, ntw)
		err = c.Set(ctx, setReq, covCacheResp(setReq, covCacheBlock("0x1")))
		require.Error(t, err)
		assert.Equal(t, int32(0), conn.sets.Load())
	})

	t.Run("UnhashableParamFailsKeyGeneration", func(t *testing.T) {
		conn := covCacheNewConn("uh")
		conn.getBytes = []byte(`{"blockNumber":"0x1"}`)
		c := newCache(t, conn)
		mk := func() *common.NormalizedRequest {
			r := common.NewNormalizedRequestFromJsonRpcRequest(
				common.NewJsonRpcRequest("eth_getTransactionReceipt", []interface{}{int64(5)}),
			)
			r.SetNetwork(ntw)
			return r
		}
		resp, err := c.Get(ctx, mk())
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(0), conn.gets.Load())

		req := mk()
		err = c.Set(ctx, req, covCacheResp(req, map[string]interface{}{"blockNumber": "0x1"}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported type for value during hash")
	})
}

func TestCovCache_GetEmptyishBehavior(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}
	cases := []struct {
		name    string
		empty   common.CacheEmptyBehavior
		wantHit bool
	}{
		{"Ignore", common.CacheEmptyBehaviorIgnore, false},
		{"Allow", common.CacheEmptyBehaviorAllow, true},
		{"Only", common.CacheEmptyBehaviorOnly, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := covCacheNewConn("em")
			conn.getBytes = []byte(`null`)
			c := covCacheNew(covCacheDiscard(zerolog.DebugLevel), "covcache-empty",
				covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized, covCacheWithEmpty(tc.empty)))
			resp, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
			require.NoError(t, err)
			if !tc.wantHit {
				assert.Nil(t, resp)
				return
			}
			require.NotNil(t, resp)
			assert.True(t, resp.FromCache())
			assert.True(t, resp.IsResultEmptyish())
		})
	}

	t.Run("IgnoredEmptyLetsPeerServe", func(t *testing.T) {
		empty := covCacheNewConn("empty")
		empty.getBytes = []byte(`null`)
		full := covCacheNewConn("full")
		full.getBytes = []byte(`{"number":"0x10"}`)
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-empty-peer",
			covCachePolicy(t, empty, "*", common.DataFinalityStateFinalized),
			covCachePolicy(t, full, "*", common.DataFinalityStateFinalized),
		)
		resp, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
		require.NoError(t, err)
		require.NotNil(t, resp)
		jrr, _ := resp.JsonRpcResponse()
		assert.Equal(t, `{"number":"0x10"}`, jrr.GetResultString())
	})
}

func TestCovCache_GetHitLogging(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}
	cases := []struct {
		name   string
		level  zerolog.Level
		stored string
	}{
		{"TraceJsonResult", zerolog.TraceLevel, `{"number":"0x10"}`},
		{"TraceNonJsonResult", zerolog.TraceLevel, `0x10`},
		{"InfoLevel", zerolog.InfoLevel, `{"number":"0x10"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := covCacheNewConn("lg")
			conn.getBytes = []byte(tc.stored)
			c := covCacheNew(covCacheDiscard(tc.level), "covcache-log",
				covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized))
			resp, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
			require.NoError(t, err)
			require.NotNil(t, resp)
			jrr, _ := resp.JsonRpcResponse()
			assert.Equal(t, tc.stored, jrr.GetResultString())
		})
	}
}

func TestCovCache_GetDirectives(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}
	const project = "covcache-directives"

	setup := func(t *testing.T) (*EvmJsonRpcCache, *covCacheConn, *data.CachePolicy) {
		conn := covCacheNewConn("systx-cache")
		conn.getBytes = []byte(`{"number":"0x10"}`)
		p := covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized)
		p.SetConnectorTags([]string{"systx"})
		return covCacheNew(covCacheDiscard(zerolog.Disabled), project, p), conn, p
	}

	t.Run("UseUpstreamExcludesTaggedConnector", func(t *testing.T) {
		c, conn, p := setup(t)
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
		req.SetDirectives(&common.RequestDirectives{UseUpstream: "alchemy"})
		miss := telemetry.MetricCacheGetSuccessMissTotal.WithLabelValues(project, "test", "eth_getBlockByNumber", conn.Id(), p.String(), p.GetTTL().String(), "empty_result")
		before := testutil.ToFloat64(miss)
		resp, err := c.Get(ctx, req)
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(0), conn.gets.Load())
		assert.Equal(t, before+1, testutil.ToFloat64(miss), "a fully-skipped fan-out is attributed to the first policy")
	})

	t.Run("UseUpstreamAdmitsTaggedConnector", func(t *testing.T) {
		c, conn, _ := setup(t)
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
		req.SetDirectives(&common.RequestDirectives{UseUpstream: "systx"})
		resp, err := c.Get(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		assert.Equal(t, int32(1), conn.gets.Load())
	})

	t.Run("SkipCacheReadPattern", func(t *testing.T) {
		c, conn, _ := setup(t)
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
		req.SetDirectives(&common.RequestDirectives{SkipCacheRead: "systx*"})
		resp, err := c.Get(ctx, req)
		require.NoError(t, err)
		assert.Nil(t, resp)
		assert.Equal(t, int32(0), conn.gets.Load())

		req2 := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
		req2.SetDirectives(&common.RequestDirectives{SkipCacheRead: "redis*"})
		resp, err = c.Get(ctx, req2)
		require.NoError(t, err)
		require.NotNil(t, resp, "non-matching skip pattern still reads")
	})
}

func TestCovCache_GetLogsRangeObservation(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}
	cases := []struct {
		name   string
		params string
	}{
		{"TagRange", `[{"fromBlock":"0x1","toBlock":"latest"}]`},
		{"ConcreteRange", `[{"fromBlock":"0x1","toBlock":"0x5"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := covCacheNewConn("logs")
			conn.getBytes = []byte(`[{"address":"0xabc","blockNumber":"0x2"}]`)
			c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-logs",
				covCachePolicy(t, conn, "eth_getLogs", common.DataFinalityStateFinalized))
			resp, err := c.Get(ctx, covCacheReq("eth_getLogs", tc.params, ntw))
			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.True(t, resp.FromCache())
		})
	}
}

func TestCovCache_DetailedTracing(t *testing.T) {
	common.IsTracingDetailed = true
	t.Cleanup(func() { common.IsTracingDetailed = false })
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}
	conn := covCacheNewConn("tr")
	c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-trace",
		covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized))

	req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
	require.NoError(t, c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10"))))
	assert.Equal(t, int32(1), conn.sets.Load())

	resp, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
	require.NoError(t, err)
	require.NotNil(t, resp)
}

func TestCovCache_SetDecisions(t *testing.T) {
	ctx := context.Background()

	t.Run("UnknownMethodIsNeverCached", func(t *testing.T) {
		for _, lvl := range []zerolog.Level{zerolog.TraceLevel, zerolog.DebugLevel} {
			conn := covCacheNewConn("um")
			c := covCacheNew(covCacheDiscard(lvl), "covcache-set",
				covCachePolicy(t, conn, "*", common.DataFinalityStateUnknown))
			req := covCacheReq("custom_unknownMethod", `[]`, &testNetwork{finalityState: common.DataFinalityStateUnknown})
			require.NoError(t, c.Set(ctx, req, covCacheResp(req, "0x1")))
			assert.Equal(t, int32(0), conn.sets.Load(), "level %s", lvl)
		}
	})

	t.Run("NoMatchingPolicyIsNoop", func(t *testing.T) {
		conn := covCacheNewConn("nm")
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-set",
			covCachePolicy(t, conn, "eth_getBalance", common.DataFinalityStateFinalized))
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, &testNetwork{finalityState: common.DataFinalityStateFinalized})
		require.NoError(t, c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10"))))
		assert.Equal(t, int32(0), conn.sets.Load())
	})

	t.Run("ErrorResponseIsNotCached", func(t *testing.T) {
		conn := covCacheNewConn("er")
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-set",
			covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized, covCacheWithEmpty(common.CacheEmptyBehaviorAllow)))
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, &testNetwork{finalityState: common.DataFinalityStateFinalized})
		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(
			common.MustNewJsonRpcResponse(7, nil, common.NewErrJsonRpcExceptionExternal(-32000, "execution reverted", "")),
		)
		require.NoError(t, c.Set(ctx, req, resp))
		assert.Equal(t, int32(0), conn.sets.Load())
	})

	t.Run("EmptyBehaviorOnSet", func(t *testing.T) {
		cases := []struct {
			name     string
			empty    common.CacheEmptyBehavior
			result   interface{}
			wantSets int32
		}{
			{"IgnoreSkipsEmpty", common.CacheEmptyBehaviorIgnore, nil, 0},
			{"IgnoreStoresNonEmpty", common.CacheEmptyBehaviorIgnore, covCacheBlock("0x10"), 1},
			{"AllowStoresEmpty", common.CacheEmptyBehaviorAllow, nil, 1},
			{"OnlyStoresEmpty", common.CacheEmptyBehaviorOnly, nil, 1},
			{"OnlySkipsNonEmpty", common.CacheEmptyBehaviorOnly, covCacheBlock("0x10"), 0},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				conn := covCacheNewConn("eb")
				c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-set",
					covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized, covCacheWithEmpty(tc.empty)))
				req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, &testNetwork{finalityState: common.DataFinalityStateFinalized})
				require.NoError(t, c.Set(ctx, req, covCacheResp(req, tc.result)))
				assert.Equal(t, tc.wantSets, conn.sets.Load())
			})
		}
	})

	t.Run("SizeLimits", func(t *testing.T) {
		small, large := "10B", "1KB"
		cases := []struct {
			name     string
			min, max *string
			wantSets int32
		}{
			{"TooLarge", nil, &small, 0},
			{"TooSmall", &large, nil, 0},
			{"WithinLimits", &small, &large, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				conn := covCacheNewConn("sz")
				p, err := data.NewCachePolicy(&common.CachePolicyConfig{
					Connector: "sz", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized,
					MinItemSize: tc.min, MaxItemSize: tc.max,
				}, conn)
				require.NoError(t, err)
				c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-set", p)
				req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, &testNetwork{finalityState: common.DataFinalityStateFinalized})
				require.NoError(t, c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10"))))
				assert.Equal(t, tc.wantSets, conn.sets.Load())
			})
		}
	})

	t.Run("EmptyResultBeyondConfidenceHead", func(t *testing.T) {
		cases := []struct {
			name     string
			head     int64
			wantSets int32
		}{
			{"FutureBlockNotCached", 50, 0},
			{"PastBlockCached", 200, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				conn := covCacheNewConn("fb")
				c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-set",
					covCachePolicy(t, conn, "*", common.DataFinalityStateUnfinalized, covCacheWithEmpty(common.CacheEmptyBehaviorAllow)))
				ntw := &testNetwork{
					finalityState: common.DataFinalityStateUnfinalized,
					highestLatest: tc.head,
					cfg:           &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{}},
				}
				req := covCacheReq("eth_getBlockByNumber", `["0x64",false]`, ntw)
				require.NoError(t, c.Set(ctx, req, covCacheResp(req, nil)))
				assert.Equal(t, tc.wantSets, conn.sets.Load())
			})
		}
	})

	t.Run("EnforceHighestBlockSkipsLaggingRealtime", func(t *testing.T) {
		const project = "covcache-enforce"
		cases := []struct {
			name     string
			tip      int64
			enforce  bool
			wantSets int32
		}{
			{"BehindTipSkipped", 100, true, 0},
			{"AtTipCached", 16, true, 1},
			{"NotEnforcedCached", 100, false, 1},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				conn := covCacheNewConn("bn")
				p := covCachePolicy(t, conn, "eth_blockNumber", common.DataFinalityStateRealtime, covCacheWithTTL(2*time.Second))
				c := covCacheNew(covCacheDiscard(zerolog.Disabled), project, p)
				req := covCacheReq("eth_blockNumber", `[]`, &testNetwork{finalityState: common.DataFinalityStateRealtime, highestLatest: tc.tip})
				req.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: tc.enforce})
				skipped := telemetry.MetricCacheSetSkippedTotal.WithLabelValues(project, "test", "eth_blockNumber", "bn", p.String(), p.GetTTL().String())
				before := testutil.ToFloat64(skipped)
				require.NoError(t, c.Set(ctx, req, covCacheResp(req, "0x10")))
				assert.Equal(t, tc.wantSets, conn.sets.Load())
				assert.Equal(t, before+float64(1-tc.wantSets), testutil.ToFloat64(skipped))
			})
		}
	})

	t.Run("NoNetworkUsesFixedTTL", func(t *testing.T) {
		conn := covCacheNewConn("nn")
		c := covCacheNew(covCacheDiscard(zerolog.DebugLevel), "covcache-set",
			covCachePolicy(t, conn, "*", common.DataFinalityStateUnknown, covCacheWithTTL(5*time.Second)))
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, nil)
		require.NoError(t, c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10"))))
		pk, _, _, ttl := conn.snapshot()
		assert.Equal(t, "n/a:16", pk)
		require.NotNil(t, ttl)
		assert.Equal(t, 5*time.Second, *ttl)
	})

	t.Run("AllMatchingPoliciesWritten", func(t *testing.T) {
		a, b := covCacheNewConn("a"), covCacheNewConn("b")
		other := covCacheNewConn("other")
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-set",
			covCachePolicy(t, a, "eth_getBlockByNumber", common.DataFinalityStateFinalized),
			covCachePolicy(t, b, "*", common.DataFinalityStateFinalized),
			covCachePolicy(t, other, "*", common.DataFinalityStateUnfinalized),
		)
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, &testNetwork{finalityState: common.DataFinalityStateFinalized})
		require.NoError(t, c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10"))))
		assert.Equal(t, int32(1), a.sets.Load())
		assert.Equal(t, int32(1), b.sets.Load())
		assert.Equal(t, int32(0), other.sets.Load(), "finality mismatch on set")
	})
}

func TestCovCache_SetConnectorErrors(t *testing.T) {
	ctx := context.Background()
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}

	t.Run("SingleFailureReturnedAsIs", func(t *testing.T) {
		boom := errors.New("dynamodb: ProvisionedThroughputExceededException")
		bad, good := covCacheNewConn("bad"), covCacheNewConn("good")
		bad.setErr = boom
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-seterr",
			covCachePolicy(t, bad, "*", common.DataFinalityStateFinalized),
			covCachePolicy(t, good, "*", common.DataFinalityStateFinalized),
		)
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
		err := c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10")))
		require.ErrorIs(t, err, boom)
		assert.Equal(t, int32(1), good.sets.Load(), "a failing peer does not block other writes")
	})

	t.Run("MultipleFailuresAggregated", func(t *testing.T) {
		a, b := covCacheNewConn("a"), covCacheNewConn("b")
		a.setErr = errors.New("redis: i/o timeout")
		b.setErr = errors.New("postgres: connection reset")
		c := covCacheNew(covCacheDiscard(zerolog.Disabled), "covcache-seterr",
			covCachePolicy(t, a, "*", common.DataFinalityStateFinalized),
			covCachePolicy(t, b, "*", common.DataFinalityStateFinalized),
		)
		req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
		err := c.Set(ctx, req, covCacheResp(req, covCacheBlock("0x10")))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to set cache for 2 policies")
		assert.Contains(t, err.Error(), "redis: i/o timeout")
		assert.Contains(t, err.Error(), "postgres: connection reset")
	})
}

func TestCovCache_CompressionRoundTrip(t *testing.T) {
	ctx := context.Background()
	lg := covCacheDiscard(zerolog.Disabled)
	c, err := NewEvmJsonRpcCache(ctx, &lg, &common.CacheConfig{
		Compression: &common.CompressionConfig{Enabled: util.BoolPtr(true), Threshold: 64},
	})
	require.NoError(t, err)
	conn := covCacheNewConn("z")
	c.SetPolicies([]*data.CachePolicy{covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized)})
	ntw := &testNetwork{finalityState: common.DataFinalityStateFinalized}

	block := covCacheBlock("0x10")
	block["extraData"] = "0x" + strings.Repeat("00", 1024)
	req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw)
	require.NoError(t, c.Set(ctx, req, covCacheResp(req, block)))
	_, _, stored, _ := conn.snapshot()
	require.True(t, c.isCompressed(stored), "large value must be stored zstd-compressed")
	assert.Less(t, len(stored), 1024)

	resp, err := c.Get(ctx, covCacheReq("eth_getBlockByNumber", `["0x10",false]`, ntw))
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, _ := resp.JsonRpcResponse()
	assert.Contains(t, jrr.GetResultString(), strings.Repeat("00", 1024))

	// Small values stay below threshold and are stored verbatim.
	req2 := covCacheReq("eth_getBlockByNumber", `["0x11",false]`, ntw)
	require.NoError(t, c.Set(ctx, req2, covCacheResp(req2, "0x1")))
	_, _, stored, _ = conn.snapshot()
	assert.Equal(t, `"0x1"`, string(stored))
}

func TestCovCache_CompressionHelpers(t *testing.T) {
	lg := covCacheDiscard(zerolog.Disabled)
	c, err := NewEvmJsonRpcCache(context.Background(), &lg, &common.CacheConfig{
		Compression: &common.CompressionConfig{Enabled: util.BoolPtr(true), Threshold: 16},
	})
	require.NoError(t, err)

	t.Run("BelowThresholdUntouched", func(t *testing.T) {
		in := []byte("short")
		out, ok := c.compressValueBytes(in)
		assert.False(t, ok)
		assert.Equal(t, in, out)
	})

	t.Run("IncompressibleStoredRaw", func(t *testing.T) {
		in := make([]byte, 64)
		_, _ = rand.NewChaCha8([32]byte{42}).Read(in)
		out, ok := c.compressValueBytes(in)
		assert.False(t, ok, "zstd output larger than input must be discarded")
		assert.Equal(t, in, out)
	})

	t.Run("CompressibleRoundTrip", func(t *testing.T) {
		in := bytes.Repeat([]byte("abcd"), 100)
		out, ok := c.compressValueBytes(in)
		require.True(t, ok)
		require.True(t, c.isCompressed(out))
		back, err := c.decompressValueBytes(out)
		require.NoError(t, err)
		assert.Equal(t, in, back)
	})

	t.Run("DecompressPassThroughUncompressed", func(t *testing.T) {
		in := []byte(`{"a":1}`)
		out, err := c.decompressValueBytes(in)
		require.NoError(t, err)
		assert.Equal(t, in, out)
	})

	t.Run("DecompressCorruptFrame", func(t *testing.T) {
		corrupt := append([]byte{0x28, 0xB5, 0x2F, 0xFD}, bytes.Repeat([]byte{0xFF}, 16)...)
		_, err := c.decompressValueBytes(corrupt)
		require.Error(t, err)
	})

	t.Run("EmptyPools", func(t *testing.T) {
		nilPool := &sync.Pool{New: func() interface{} { return nil }}
		broken := &EvmJsonRpcCache{
			logger:               &lg,
			compressionEnabled:   true,
			compressionThreshold: 1,
			encoderPool:          nilPool,
			decoderPool:          nilPool,
		}
		in := bytes.Repeat([]byte("x"), 32)
		out, ok := broken.compressValueBytes(in)
		assert.False(t, ok)
		assert.Equal(t, in, out)

		compressed, ok := c.compressValueBytes(bytes.Repeat([]byte("abcd"), 100))
		require.True(t, ok)
		_, err := broken.decompressValueBytes(compressed)
		require.Error(t, err)
		assert.Equal(t, "failed to get decoder from pool", err.Error())
	})

	t.Run("InvalidEncoderLevelStoresUncompressed", func(t *testing.T) {
		bad, err := NewEvmJsonRpcCache(context.Background(), &lg, &common.CacheConfig{
			Compression: &common.CompressionConfig{Enabled: util.BoolPtr(true), Threshold: 1},
		})
		require.NoError(t, err)
		// The pool's New reads the level lazily; an invalid level makes zstd
		// refuse to build an encoder, so the value must be stored as-is.
		bad.compressionLevel = zstd.EncoderLevel(99)
		in := bytes.Repeat([]byte("abcd"), 100)
		out, ok := bad.compressValueBytes(in)
		assert.False(t, ok)
		assert.Equal(t, in, out)
	})

	t.Run("ClosedDecoderFailsReset", func(t *testing.T) {
		closed := &EvmJsonRpcCache{
			logger:             &lg,
			compressionEnabled: true,
			decoderPool: &sync.Pool{New: func() interface{} {
				d, err := zstd.NewReader(nil)
				if err != nil {
					return nil
				}
				d.Close()
				return d
			}},
		}
		compressed, ok := c.compressValueBytes(bytes.Repeat([]byte("abcd"), 100))
		require.True(t, ok)
		_, err := closed.decompressValueBytes(compressed)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to reset zstd decoder")
	})
}
