package evm

import (
	"context"
	"iter"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// localAnswerNetwork yields a fixed upstream pool, narrowed by the request's
// use-upstream selector when RequestContextKey is bound — the same contract
// erpc.Network.EvmRoutableUpstreams uses for net_peerCount / eth_syncing.
type localAnswerNetwork struct {
	common.Network
	cfg  *common.NetworkConfig
	pool []common.Upstream
}

func (n *localAnswerNetwork) Config() *common.NetworkConfig { return n.cfg }
func (n *localAnswerNetwork) ProjectId() string             { return "test" }
func (n *localAnswerNetwork) EvmRoutableUpstreams(ctx context.Context) iter.Seq[common.Upstream] {
	return func(yield func(common.Upstream) bool) {
		sel := ""
		if req, ok := ctx.Value(common.RequestContextKey).(*common.NormalizedRequest); ok && req != nil {
			if d := req.Directives(); d != nil {
				sel = d.UseUpstream
			}
		}
		for _, u := range n.pool {
			if sel != "" {
				if m, _ := common.UpstreamMatchesSelector(sel, u); !m {
					continue
				}
			}
			if !yield(u) {
				return
			}
		}
	}
}

func TestProjectPreForward_localAnswer_honorsUseUpstream(t *testing.T) {
	n := &localAnswerNetwork{
		cfg:  &common.NetworkConfig{Evm: &common.EvmNetworkConfig{ChainId: 1}},
		pool: []common.Upstream{common.NewFakeUpstream("rpc1"), common.NewFakeUpstream("rpc2")},
	}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"net_peerCount","params":[]}`))
	req.SetDirectives(&common.RequestDirectives{UseUpstream: "rpc1"})

	handled, resp, err := projectPreForward_localAnswer(context.Background(), n, req, "net_peerCount")
	require.True(t, handled)
	require.NoError(t, err)
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.NotNil(t, jrr)
	assert.Equal(t, `"0x1"`, string(jrr.GetResultBytes()),
		"selector must narrow peers; without binding RequestContextKey this stays 0x2")
}
