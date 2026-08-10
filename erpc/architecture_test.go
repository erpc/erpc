package erpc

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The seam's unknown-architecture fallthrough is the path worth pinning: before
// consolidation some EVM hooks were gated on `== ArchitectureEvm` and others ran
// unconditionally, so "what happens on a network this build has no behavior for"
// had no single answer. These tests fix that answer once, at the seam, instead of
// at a dozen call sites.

func TestArchSeam_ResolvesOnlyKnownArchitectures(t *testing.T) {
	require.NotNil(t, archBehaviorFor(common.ArchitectureEvm))
	assert.Nil(t, archBehaviorFor(""))
	assert.Nil(t, archBehaviorFor(common.NetworkArchitecture("not-a-real-architecture")))
}

func TestArchSeam_NetworkWithoutConfigHasNoBehavior(t *testing.T) {
	assert.Nil(t, (*Network)(nil).arch())
	assert.Nil(t, (&Network{}).arch())
	assert.NotNil(t, (&Network{cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}}).arch())
}

func TestArchSeam_UnsupportedArchitectureSkipsArchitectureSpecificSteps(t *testing.T) {
	ctx := context.Background()
	n := &Network{
		networkId: "notreal:1",
		cfg:       &common.NetworkConfig{Architecture: common.NetworkArchitecture("not-a-real-architecture")},
	}
	require.Nil(t, n.arch())

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0000000000000000000000000000000000000000","0x1"]}`))

	// prepareRequest is the one step that must fail loudly: a request nobody can
	// normalize must never reach an upstream.
	err := n.prepareRequest(ctx, req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported architecture")

	// Every other step fails open, exactly as the pre-consolidation
	// `!= ArchitectureEvm` guards did.
	skipErr, retryable := n.checkUpstreamBlockAvailability(ctx, nil, req, "eth_getBalance")
	assert.NoError(t, skipErr)
	assert.False(t, retryable)
	assert.Nil(t, n.eligibleUpstreamIDsForBoundary(ctx, "eth_getBalance", req))
	assert.NotPanics(t, func() { n.enrichStatePoller(ctx, "eth_blockNumber", req, nil) })
}
