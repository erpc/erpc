package evm

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the chain-identity gate on the out-of-band Suggest* paths.
// A cross-wired / poisoned upstream can answer an ordinary request with a 200-OK
// carrying ANOTHER chain's (much higher) block height; that height reaches the
// shared counter via SuggestLatestBlock / SuggestFinalizedBlock. Without a gate
// it pins a bogus head that skews every lag-based routing decision and does not
// self-heal while the endpoint keeps erroring or serving the wrong chain. A
// MAJOR (> DefaultToleratedBlockHeadRollback) forward jump suggested off-band
// must therefore pass the same fresh eth_chainId check the verified poll path
// uses before it is accepted. Small keep-fresh advances stay ungated and inline.

// suggestGateUpstream is a common.EvmUpstream double with a fully settable
// eth_chainId response (value + error) and an observable Cordon.
type suggestGateUpstream struct {
	id       string
	cfg      *common.UpstreamConfig
	logger   zerolog.Logger
	mu       sync.Mutex
	chainId  string
	chainErr error
	cordoned bool
	reason   string
}

func newSuggestGateUpstream(configuredChainId int64, detected string, detectErr error) *suggestGateUpstream {
	return &suggestGateUpstream{
		id:       "test-ups",
		cfg:      &common.UpstreamConfig{Id: "test-ups", Evm: &common.EvmUpstreamConfig{ChainId: configuredChainId}},
		logger:   zerolog.Nop(),
		chainId:  detected,
		chainErr: detectErr,
	}
}

func (u *suggestGateUpstream) setChainId(detected string, detectErr error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.chainId = detected
	u.chainErr = detectErr
}

func (u *suggestGateUpstream) isCordoned() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.cordoned
}

// common.Upstream
func (u *suggestGateUpstream) Id() string                      { return u.id }
func (u *suggestGateUpstream) VendorName() string              { return "" }
func (u *suggestGateUpstream) NetworkId() string               { return "evm:123" }
func (u *suggestGateUpstream) NetworkLabel() string            { return "evm:123" }
func (u *suggestGateUpstream) Config() *common.UpstreamConfig  { return u.cfg }
func (u *suggestGateUpstream) Logger() *zerolog.Logger         { return &u.logger }
func (u *suggestGateUpstream) Vendor() common.Vendor           { return nil }
func (u *suggestGateUpstream) Tracker() common.HealthTracker   { return nil }
func (u *suggestGateUpstream) IgnoreMethod(string)             {}
func (u *suggestGateUpstream) Uncordon(_, _ string)            {}
func (u *suggestGateUpstream) ShouldHandleMethod(string) (bool, error) {
	return true, nil
}
func (u *suggestGateUpstream) Forward(context.Context, *common.NormalizedRequest, bool, bool) (*common.NormalizedResponse, error) {
	return nil, nil
}
func (u *suggestGateUpstream) Cordon(_, reason string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cordoned = true
	u.reason = reason
}

// common.EvmUpstream
func (u *suggestGateUpstream) EvmGetChainId(context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chainId, u.chainErr
}
func (u *suggestGateUpstream) EvmIsBlockFinalized(context.Context, int64, bool) (bool, error) {
	return false, nil
}
func (u *suggestGateUpstream) EvmAssertBlockAvailability(context.Context, string, common.AvailbilityConfidence, bool, int64) (bool, error) {
	return true, nil
}
func (u *suggestGateUpstream) EvmSyncingState() common.EvmSyncingState { return common.EvmSyncingStateUnknown }
func (u *suggestGateUpstream) EvmStatePoller() common.EvmStatePoller   { return nil }
func (u *suggestGateUpstream) EvmEffectiveLatestBlock() int64          { return 0 }
func (u *suggestGateUpstream) EvmEffectiveFinalizedBlock() int64       { return 0 }
func (u *suggestGateUpstream) EvmBlockAvailabilityBounds() (int64, int64) {
	return math.MinInt64, math.MaxInt64
}

func newGateTestPoller(t *testing.T, up common.Upstream) *EvmStatePoller {
	t.Helper()
	appCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	tracker := health.NewTracker(&logger, "test", 2*time.Second)
	ssr, err := data.NewSharedStateRegistry(appCtx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)
	return NewEvmStatePoller("test", appCtx, &logger, up, tracker, ssr)
}

const gateTolerance = int64(common.DefaultToleratedBlockHeadRollback)

// --- SuggestLatestBlock ---

func TestSuggestLatestBlock_SmallAdvanceAppliesInline(t *testing.T) {
	// Configured chainId matches; but a small advance must never even reach the
	// gate — it applies synchronously.
	up := newSuggestGateUpstream(123, "123", nil)
	p := newGateTestPoller(t, up)

	p.SuggestLatestBlock(1000) // from 0 → inline (cold start)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(1000 + gateTolerance) // small (== tolerance, not major)
	require.Equal(t, int64(1000+gateTolerance), p.LatestBlock(), "small advance applies inline & synchronously")
	require.False(t, up.isCordoned())
}

func TestSuggestLatestBlock_MajorJumpMatchingChainIdApplies(t *testing.T) {
	up := newSuggestGateUpstream(123, "123", nil) // detected == configured
	p := newGateTestPoller(t, up)

	p.SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(1000 + gateTolerance + 1000) // MAJOR forward jump
	require.Eventually(t, func() bool {
		return p.LatestBlock() == 1000+gateTolerance+1000
	}, 2*time.Second, 10*time.Millisecond, "verified major jump must be applied")
	require.False(t, up.isCordoned(), "a matching chainId must not cordon")
}

func TestSuggestLatestBlock_MajorJumpChainIdMismatchDroppedAndCordoned(t *testing.T) {
	up := newSuggestGateUpstream(123, "999", nil) // detected != configured
	p := newGateTestPoller(t, up)

	p.SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(5_000_000) // MAJOR (another chain's height)
	require.Eventually(t, up.isCordoned, 2*time.Second, 10*time.Millisecond, "a proven cross-wired endpoint must be cordoned")
	assert.Equal(t, int64(1000), p.LatestBlock(), "the bogus major jump must NOT enter the shared counter")
}

func TestSuggestLatestBlock_MajorJumpChainIdErrorDroppedNotCordoned(t *testing.T) {
	// A transient eth_chainId failure (not a proven mismatch) drops the sample
	// for now and does NOT cordon — the next verified poll re-observes it.
	up := newSuggestGateUpstream(123, "", assert.AnError)
	p := newGateTestPoller(t, up)

	p.SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(5_000_000) // MAJOR
	require.Never(t, func() bool {
		return p.LatestBlock() != 1000
	}, 300*time.Millisecond, 20*time.Millisecond, "unverifiable major jump must be dropped")
	assert.False(t, up.isCordoned(), "a transient probe error must not cordon")
}

func TestSuggestLatestBlock_MajorJumpTypedMismatchErrorCordons(t *testing.T) {
	// The gRPC BDS client surfaces a cross-wired server as a typed mismatch error
	// on the chainId probe itself — treated as proof, same as a differing answer.
	up := newSuggestGateUpstream(123, "", common.NewErrEndpointChainIdMismatch(999, 123))
	p := newGateTestPoller(t, up)

	p.SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(5_000_000)
	require.Eventually(t, up.isCordoned, 2*time.Second, 10*time.Millisecond, "typed chainId-mismatch error must cordon")
	assert.Equal(t, int64(1000), p.LatestBlock())
}

func TestSuggestLatestBlock_UnpinnedChainIdSkipsGate(t *testing.T) {
	// Chain identity not pinned yet (ChainId <= 0): there is nothing trustworthy
	// to verify against, so the gate passes and the (cold-start) jump applies.
	up := newSuggestGateUpstream(0, "", assert.AnError)
	p := newGateTestPoller(t, up)

	p.SuggestLatestBlock(1000)
	require.Equal(t, int64(1000), p.LatestBlock())

	p.SuggestLatestBlock(5_000_000) // major, but chainId not pinned → not gated
	require.Eventually(t, func() bool {
		return p.LatestBlock() == 5_000_000
	}, 2*time.Second, 10*time.Millisecond)
	require.False(t, up.isCordoned())
}

// --- SuggestFinalizedBlock ---

func TestSuggestFinalizedBlock_MajorJumpMatchingApplies(t *testing.T) {
	up := newSuggestGateUpstream(123, "123", nil)
	p := newGateTestPoller(t, up)

	p.SuggestFinalizedBlock(1000)
	require.Eventually(t, func() bool { return p.FinalizedBlock() == 1000 }, 2*time.Second, 10*time.Millisecond)

	p.SuggestFinalizedBlock(1000 + gateTolerance + 1000) // MAJOR
	require.Eventually(t, func() bool {
		return p.FinalizedBlock() == 1000+gateTolerance+1000
	}, 2*time.Second, 10*time.Millisecond, "verified major finalized jump must be applied")
	require.False(t, up.isCordoned())
}

func TestSuggestFinalizedBlock_MajorJumpChainIdMismatchDroppedAndCordoned(t *testing.T) {
	up := newSuggestGateUpstream(123, "999", nil)
	p := newGateTestPoller(t, up)

	p.SuggestFinalizedBlock(1000)
	require.Eventually(t, func() bool { return p.FinalizedBlock() == 1000 }, 2*time.Second, 10*time.Millisecond)

	p.SuggestFinalizedBlock(5_000_000) // MAJOR wrong-chain height
	require.Eventually(t, up.isCordoned, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, int64(1000), p.FinalizedBlock(), "bogus major finalized jump must not enter the shared counter")
}
