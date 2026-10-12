package consensus

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// idUpstream is enough for missing-data attribution. NewErrEndpointMissingData
// only reads Id and Config, and only walks EvmUpstream state when the
// concrete type implements it.
type idUpstream struct {
	common.Upstream
	id string
}

func (u *idUpstream) Id() string { return u.id }
func (u *idUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: u.id, Type: common.UpstreamTypeEvm}
}

// leaderNetwork elects a fixed upstream. newConsensusAnalysis only asks an
// EVM network for its architecture and its leader.
type leaderNetwork struct {
	common.Network
	leader common.Upstream
}

func (n *leaderNetwork) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *leaderNetwork) EvmHighestLatestBlockNumber(context.Context) int64 { return 0 }
func (n *leaderNetwork) EvmHighestFinalizedBlockNumber(context.Context) int64 {
	return 0
}
func (n *leaderNetwork) EvmLeaderUpstream(context.Context) common.Upstream { return n.leader }

func missingDataFrom(ups common.Upstream, message string) error {
	cause := common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorMissingData, message, nil, nil)
	return common.NewErrEndpointMissingData(cause, ups)
}

func TestOnlyBlockHeadLeader_LowParticipants_ReturnsLeaderError(t *testing.T) {
	leader := &idUpstream{id: "zzz-leader"}
	other := &idUpstream{id: "aaa-other"}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	req.SetNetwork(&leaderNetwork{leader: leader})
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	cfg := &config{
		maxParticipants:         2,
		agreementThreshold:      3,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
	}
	// aaa-other sorts first, so RepresentativeError is its error. The leader
	// rule must still return zzz-leader's error: same missing-data verdict,
	// different message.
	parts := []*execResult{
		{Err: missingDataFrom(other, "other has not indexed this block"), Upstream: other, Index: 0},
		{Err: missingDataFrom(leader, "leader has not indexed this block"), Upstream: leader, Index: 1},
	}
	lg := zerolog.Nop()
	analysis := newConsensusAnalysis(&lg, ctx, cfg, parts)
	require.Equal(t, leader, analysis.leaderUpstream)
	require.Len(t, analysis.groups, 1, "both missing-data answers must share one verdict group")

	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)
	require.NotNil(t, winner)
	require.Error(t, winner.Error)
	var md *common.ErrEndpointMissingData
	require.ErrorAs(t, winner.Error, &md)
	require.NotNil(t, md.Upstream())
	assert.Equal(t, "zzz-leader", md.Upstream().Id())
	assert.Contains(t, winner.Error.Error(), "leader has not indexed this block")
	assert.NotContains(t, winner.Error.Error(), "other has not indexed this block")
}

func TestOnlyBlockHeadLeader_LowParticipants_LeaderInfraStaysLowParticipants(t *testing.T) {
	leader := &idUpstream{id: "zzz-leader"}
	other := &idUpstream{id: "aaa-other"}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	req.SetNetwork(&leaderNetwork{leader: leader})
	ctx := context.WithValue(context.Background(), common.RequestContextKey, req)

	cfg := &config{
		maxParticipants:         2,
		agreementThreshold:      3,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
	}
	// The other upstream has a real consensus error. The leader only has an
	// infrastructure failure, which this rule must not surface.
	parts := []*execResult{
		{Err: missingDataFrom(other, "other has not indexed this block"), Upstream: other, Index: 0},
		{Err: fmt.Errorf("dial timeout"), Upstream: leader, Index: 1},
	}
	lg := zerolog.Nop()
	analysis := newConsensusAnalysis(&lg, ctx, cfg, parts)
	e := &executor{consensusPolicy: &consensusPolicy{logger: &lg, config: cfg}}
	winner := e.determineWinner(&lg, analysis)
	require.NotNil(t, winner)
	require.Error(t, winner.Error)
	assert.True(t, common.HasErrorCode(winner.Error, common.ErrCodeConsensusLowParticipants))
	var md *common.ErrEndpointMissingData
	assert.False(t, errors.As(winner.Error, &md), "an infrastructure failure from the leader must not be replaced by another upstream's error")
}
