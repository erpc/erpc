package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func emptyResultFrom(t *testing.T, ups common.Upstream, index int) *execResult {
	t.Helper()
	jrpc, err := common.NewJsonRpcResponse(1, nil, nil)
	require.NoError(t, err)
	return &execResult{
		Result:   common.NewNormalizedResponse().WithJsonRpcResponse(jrpc),
		Upstream: ups,
		Index:    index,
	}
}

// Three empty answers tie three real ones. a.groups is a map, so
// getBestByCount picks either group at random. Pin the pick to the real
// group: the prefer-non-empty rule used to stand down on that pick, and a
// later rule served the empty answer.
func TestPreferNonEmpty_WinsATieForTheLead(t *testing.T) {
	cfg := &config{
		maxParticipants:    6,
		agreementThreshold: 2,
		preferNonEmpty:     true,
		disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}
	a := analyze(cfg, []*execResult{
		emptyResultFrom(t, taggedUpstream("u1"), 0),
		emptyResultFrom(t, taggedUpstream("u2"), 1),
		emptyResultFrom(t, taggedUpstream("u3"), 2),
		resultFrom(t, taggedUpstream("u4"), "0xaa", 3),
		resultFrom(t, taggedUpstream("u5"), "0xaa", 4),
		resultFrom(t, taggedUpstream("u6"), "0xaa", 5),
	})
	for _, g := range a.groups {
		if g.ResponseType == ResponseTypeNonEmpty {
			a.cachedBestByCount = g
		}
	}

	winner := winnerOf(cfg, a)
	require.NotNil(t, winner)
	require.NoError(t, winner.Error)
	jrr, err := winner.Result.JsonRpcResponse()
	require.NoError(t, err)
	assert.Contains(t, string(jrr.GetResultBytes()), "0xaa")
}
