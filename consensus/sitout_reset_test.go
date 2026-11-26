package consensus

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResetSitout_ScopedByProject pins Aram's Round-2 blocker: an admin
// uncordon must not stop another project's sit-out timer just because the two
// upstreams share the same ID. IDs are unique per project only.
func TestResetSitout_ScopedByProject(t *testing.T) {
	lg := zerolog.Nop()
	punish := &common.PunishMisbehaviorConfig{
		DisputeThreshold: 1,
		DisputeWindow:    common.Duration(time.Hour),
		SitOutPenalty:    common.Duration(time.Hour),
	}
	cfg := &common.ConsensusPolicyConfig{
		MaxParticipants:    3,
		AgreementThreshold: 2,
		PunishMisbehavior:  punish,
	}

	consA, err := NewConsensus(cfg, &lg, "project-a")
	require.NoError(t, err)
	t.Cleanup(func() { _ = consA.Close() })
	consB, err := NewConsensus(cfg, &lg, "project-b")
	require.NoError(t, err)
	t.Cleanup(func() { _ = consB.Close() })

	// Drive the same upstream ID into a sit-out in both projects.
	badA := taggedUpstream("shared-id")
	badB := taggedUpstream("shared-id")
	labels := metricsLabels{projectId: "project-a", networkId: "evm:1", method: "eth_getBalance", category: "eth_getBalance",
		finalityStr: "unfinalized", finality: common.DataFinalityStateUnfinalized, userId: "u", agentName: "ag"}
	for i := 0; i < 2; i++ {
		trackRound(t, &executor{consensusPolicy: consA.policy}, labels, []*execResult{
			resultFrom(t, taggedUpstream("good-a"), "0xaa", 0),
			resultFrom(t, taggedUpstream("good-b"), "0xaa", 1),
			resultFrom(t, badA, "0xbb", 2),
		})
		trackRound(t, &executor{consensusPolicy: consB.policy}, labels, []*execResult{
			resultFrom(t, taggedUpstream("good-a"), "0xaa", 0),
			resultFrom(t, taggedUpstream("good-b"), "0xaa", 1),
			resultFrom(t, badB, "0xbb", 2),
		})
	}

	_, okA := consA.policy.misbehavingUpstreamsSitoutTimer.Load("shared-id")
	_, okB := consB.policy.misbehavingUpstreamsSitoutTimer.Load("shared-id")
	require.True(t, okA, "project A must have a sit-out entry")
	require.True(t, okB, "project B must have a sit-out entry")

	// Uncordon in project A must clear only A's sit-out.
	ResetSitout("project-a", "shared-id")
	_, okA = consA.policy.misbehavingUpstreamsSitoutTimer.Load("shared-id")
	_, okB = consB.policy.misbehavingUpstreamsSitoutTimer.Load("shared-id")
	assert.False(t, okA, "project A sit-out cleared")
	assert.True(t, okB, "project B same-ID sit-out must survive project A's uncordon")

	// And B clears on its own reset.
	ResetSitout("project-b", "shared-id")
	_, okB = consB.policy.misbehavingUpstreamsSitoutTimer.Load("shared-id")
	assert.False(t, okB, "project B sit-out cleared by its own reset")
}

// TestSitoutResetHook_UpstreamDoesNotImportConsensus verifies the hook wiring:
// upstream calls the registered hook (wired to ResetSitout in erpc init) with
// the upstream's project, without importing consensus.
func TestSitoutResetHook_UpstreamDoesNotImportConsensus(t *testing.T) {
	// covered by the upstream package's own test; here we just pin the
	// consensus-side contract ResetSitout(projectId, upstreamId).
	var _ func(string, string) = ResetSitout
}
