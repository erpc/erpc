package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A network with no upstream yet is reported one of two ways, and the only
// thing separating them is how long it has been that way.
//
// "initializing; please retry shortly" is accurate for the first seconds of a
// bootstrap and a lie forever after: a chain that no configured upstream or
// provider serves stays in exactly that state, so callers keep retrying and
// operators read a permanent condition as a passing blip. That was the actual
// production report behind this behaviour — a removed chain answering
// "initializing" for days.

// withNoUpstreamsAvailableAfter overrides the threshold for one test.
func withNoUpstreamsAvailableAfter(t *testing.T, d time.Duration) {
	t.Helper()
	previous := NoUpstreamsAvailableAfter
	NoUpstreamsAvailableAfter = d
	t.Cleanup(func() { NoUpstreamsAvailableAfter = previous })
}

func TestErrStillInitializing_YoungNetworkIsInitializing(t *testing.T) {
	withNoUpstreamsAvailableAfter(t, 5*time.Minute)
	reg, _ := newBootstrapTestRegistry(t)
	reg.networkFirstPreparedAt.Store("evm:123", time.Now())

	err := reg.errStillInitializing("evm:123")

	assert.True(t, common.HasErrorCode(err, common.ErrCodeNetworkInitializing), "got: %v", err)
}

func TestErrStillInitializing_UnknownNetworkIsInitializing(t *testing.T) {
	withNoUpstreamsAvailableAfter(t, 5*time.Minute)
	reg, _ := newBootstrapTestRegistry(t)

	// No recorded start means nothing is known about how long this has been
	// going on, and an unbacked claim of permanence is the one thing this must
	// never make.
	err := reg.errStillInitializing("evm:123")

	assert.True(t, common.HasErrorCode(err, common.ErrCodeNetworkInitializing), "got: %v", err)
}

func TestErrStillInitializing_PastThresholdReportsNoProviders(t *testing.T) {
	withNoUpstreamsAvailableAfter(t, 5*time.Minute)
	reg, _ := newBootstrapTestRegistry(t)
	reg.networkFirstPreparedAt.Store("evm:534351", time.Now().Add(-6*time.Minute))

	err := reg.errStillInitializing("evm:534351")

	require.True(t, common.HasErrorCode(err, common.ErrCodeNetworkNoUpstreamsAvailable), "got: %v", err)
	assert.Contains(t, err.Error(), "no RPC providers are available for network 'evm:534351' in project 'test'")
	assert.NotContains(t, err.Error(), "retry shortly")

	// What the client reads on the wire — the message must survive translation
	// rather than collapsing into a generic internal error.
	assert.Contains(t, common.TranslateToJsonRpcException(err).Error(), "no RPC providers are available")
}

func TestErrStillInitializing_ZeroThresholdKeepsInitializing(t *testing.T) {
	withNoUpstreamsAvailableAfter(t, 0)
	reg, _ := newBootstrapTestRegistry(t)
	reg.networkFirstPreparedAt.Store("evm:534351", time.Now().Add(-24*time.Hour))

	err := reg.errStillInitializing("evm:534351")

	assert.True(t, common.HasErrorCode(err, common.ErrCodeNetworkInitializing), "got: %v", err)
}

// TestPrepareUpstreamsForNetwork_ReportsNoProvidersOnceStale drives the real
// bootstrap path: an upstream whose endpoint never answers, so the network
// never gets one registered. The first attempt is still "initializing"; once
// the network has been stuck past the threshold, the same call reports that no
// provider is available for it.
func TestPrepareUpstreamsForNetwork_ReportsNoProvidersOnceStale(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	// No mocks at all: chain-id detection fails, so nothing ever registers.

	withNoUpstreamsAvailableAfter(t, 5*time.Minute)
	reg, _ := newBootstrapTestRegistry(t)
	reg.Bootstrap(t.Context())

	prepare := func() error {
		// Short deadline: the wait loop otherwise sits for its full 30s budget
		// hoping an upstream shows up.
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		return reg.PrepareUpstreamsForNetwork(ctx, "evm:123")
	}

	err := prepare()
	require.Error(t, err)
	require.Empty(t, reg.GetNetworkUpstreams(t.Context(), "evm:123"), "no upstream should have registered")
	assert.True(t, common.HasErrorCode(err, common.ErrCodeNetworkInitializing),
		"a network that just started bootstrapping is still initializing, got: %v", err)

	// Same network, same failure, five minutes older.
	reg.networkFirstPreparedAt.Store("evm:123", time.Now().Add(-6*time.Minute))

	err = prepare()
	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeNetworkNoUpstreamsAvailable),
		"a network stuck with zero upstreams past the threshold is unavailable, got: %v", err)
	assert.Contains(t, err.Error(), "evm:123")
}
