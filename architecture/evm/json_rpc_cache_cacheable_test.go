package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestEvmJsonRpcCache_Set_CachingStrategyMark(t *testing.T) {
	ctx := context.Background()
	yes, no := true, false

	cases := []struct {
		name     string
		strategy common.CachingStrategy
		mark     *bool
		wantSets int32
	}{
		{"defaultAllow_nil_writes", common.CachingStrategyDefaultAllow, nil, 1},
		{"defaultAllow_true_writes", common.CachingStrategyDefaultAllow, &yes, 1},
		{"defaultAllow_false_skips", common.CachingStrategyDefaultAllow, &no, 0},
		{"defaultIgnore_nil_skips", common.CachingStrategyDefaultIgnore, nil, 0},
		{"defaultIgnore_true_writes", common.CachingStrategyDefaultIgnore, &yes, 1},
		{"defaultIgnore_false_skips", common.CachingStrategyDefaultIgnore, &no, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := covCacheNewConn("adm")
			c := covCacheNew(covCacheDiscard(zerolog.Disabled), "cacheable",
				covCachePolicy(t, conn, "*", common.DataFinalityStateFinalized))
			c.cachingStrategy = tc.strategy

			req := covCacheReq("eth_getBlockByNumber", `["0x10",false]`, &testNetwork{finalityState: common.DataFinalityStateFinalized})
			resp := covCacheResp(req, covCacheBlock("0x10"))
			resp.SetCacheable(tc.mark)

			require.NoError(t, c.Set(ctx, req, resp))
			assert.Equal(t, tc.wantSets, conn.sets.Load())
		})
	}
}
