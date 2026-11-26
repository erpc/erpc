package svm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestSvmJsonRpcCache_Set_CachingStrategyMark(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name     string
		strategy common.CachingStrategy
		mark     *bool
		wantHit  bool
	}{
		{"defaultAllow_nil_writes", common.CachingStrategyDefaultAllow, nil, true},
		{"defaultAllow_false_skips", common.CachingStrategyDefaultAllow, &no, false},
		{"defaultIgnore_nil_skips", common.CachingStrategyDefaultIgnore, nil, false},
		{"defaultIgnore_true_writes", common.CachingStrategyDefaultIgnore, &yes, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCache(t)
			c.cachingStrategy = tc.strategy
			ctx := context.Background()

			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[100,{"commitment":"finalized"}]}`)
			req := common.NewNormalizedRequest(body)
			req.SetNetwork(finalizedNetwork{})
			jrr, err := common.NewJsonRpcResponse(1, map[string]interface{}{"blockhash": "abc"}, nil)
			require.NoError(t, err)
			resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
			resp.SetCacheable(tc.mark)

			require.NoError(t, c.Set(ctx, req, resp))
			time.Sleep(ristrettoSettleDelay)

			req2 := common.NewNormalizedRequest(body)
			req2.SetNetwork(finalizedNetwork{})
			got, err := c.Get(ctx, req2)
			require.NoError(t, err)
			if tc.wantHit {
				require.NotNil(t, got, "expected cache write under %s mark=%v", tc.strategy, tc.mark)
			} else {
				assert.Nil(t, got, "expected skip under %s mark=%v", tc.strategy, tc.mark)
			}
		})
	}
}
