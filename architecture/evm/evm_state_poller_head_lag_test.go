package evm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// These tests drive real state pollers and one real tracker against a
// simulated chain that produces one block per step, one second apart. Each
// step stands for one second of wall-clock time; a 30s statePollerInterval is
// therefore one poll every 30 steps. "Serving traffic" is modelled the way
// the network does it: every response hands the upstream's head to
// SuggestLatestBlock.

const (
	simPollEvery = 30 // steps between polls: 30s interval over 1s blocks
	simLagGate   = 16 // the default policy's blockNumberLagAbove threshold
)

type simChain struct {
	head atomic.Int64
}

// simNodeUpstream answers eth_getBlockByNumber with its node's head: the
// chain head, or the block it froze at.
type simNodeUpstream struct {
	cfg      *common.UpstreamConfig
	logger   zerolog.Logger
	chain    *simChain
	frozenAt atomic.Int64
	down     atomic.Bool
	delay    atomic.Int64 // ns each response waits, like a slow endpoint
}

func (u *simNodeUpstream) nodeHead() int64 {
	if f := u.frozenAt.Load(); f > 0 {
		return f
	}
	return u.chain.head.Load()
}

func (u *simNodeUpstream) freeze() { u.frozenAt.Store(u.chain.head.Load()) }

func (u *simNodeUpstream) goDown() { u.down.Store(true) }

func (u *simNodeUpstream) Id() string                     { return u.cfg.Id }
func (u *simNodeUpstream) VendorName() string             { return "test" }
func (u *simNodeUpstream) NetworkId() string              { return "evm:123" }
func (u *simNodeUpstream) NetworkLabel() string           { return "evm:123" }
func (u *simNodeUpstream) Config() *common.UpstreamConfig { return u.cfg }
func (u *simNodeUpstream) Logger() *zerolog.Logger        { return &u.logger }
func (u *simNodeUpstream) Vendor() common.Vendor          { return nil }
func (u *simNodeUpstream) Tracker() common.HealthTracker  { return nil }
func (u *simNodeUpstream) Cordon(string, string)          {}
func (u *simNodeUpstream) Uncordon(string, string)        {}
func (u *simNodeUpstream) IgnoreMethod(string)            {}
func (u *simNodeUpstream) ShouldHandleMethod(string) (bool, error) {
	return true, nil
}

func (u *simNodeUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	if u.down.Load() {
		return nil, errors.New("sim upstream: connection refused")
	}
	time.Sleep(time.Duration(u.delay.Load()))
	jrq, err := nq.JsonRpcRequest(ctx)
	if err != nil {
		return nil, err
	}
	var result interface{}
	switch jrq.Method {
	case "eth_syncing":
		result = false
	case "eth_getBlockByNumber":
		n := u.nodeHead()
		if bytes.Contains(nq.Body(), []byte(`"finalized"`)) {
			n -= 10
		}
		result = map[string]interface{}{
			"number":    fmt.Sprintf("0x%x", n),
			"timestamp": fmt.Sprintf("0x%x", 1_700_000_000+n),
		}
	default:
		return nil, errors.New("sim upstream: unsupported method " + jrq.Method)
	}
	jrr, err := common.NewJsonRpcResponse(nq.ID(), result, nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(nq).WithJsonRpcResponse(jrr), nil
}

var _ common.Upstream = (*simNodeUpstream)(nil)

type simNetwork struct {
	chain   *simChain
	tracker *health.Tracker
	ups     []*simNodeUpstream
	pollers []*EvmStatePoller
}

// newSimNetwork bootstraps one real state poller per upstream (30s interval)
// against a shared tracker, all starting at the same head.
func newSimNetwork(t *testing.T, ids ...string) *simNetwork {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := zerolog.Nop()
	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	n := &simNetwork{chain: &simChain{}, tracker: health.NewTracker(&logger, "test", time.Minute)}
	n.chain.head.Store(1_000_000)
	for _, id := range ids {
		up := &simNodeUpstream{
			logger: logger,
			chain:  n.chain,
			cfg: &common.UpstreamConfig{
				Id:   id,
				Type: common.UpstreamTypeEvm,
				Evm: &common.EvmUpstreamConfig{
					ChainId:             123,
					StatePollerInterval: common.Duration(simPollEvery * time.Second),
					// Steps run faster than real time; a tiny debounce lets
					// every simulated poll reach the upstream.
					StatePollerDebounce: common.Duration(time.Millisecond),
				},
			},
		}
		p := NewEvmStatePoller("test", ctx, &logger, up, n.tracker, ssr)
		require.NoError(t, p.Bootstrap(ctx))
		n.ups = append(n.ups, up)
		n.pollers = append(n.pollers, p)
	}
	return n
}

// serve hands upstream i's current head to its poller, as a response would.
func (n *simNetwork) serve(i int) { n.pollers[i].SuggestLatestBlock(n.ups[i].nodeHead()) }

func (n *simNetwork) poll(t *testing.T, i int) {
	t.Helper()
	// Past the 1ms debounce. Steps run faster than real time, and every
	// accepted update stamps the counter strictly later than the previous
	// one, so a served upstream's stamp can run ahead of the clock.
	for !n.pollers[i].latestBlockShared.IsStale(time.Millisecond) {
		time.Sleep(time.Millisecond)
	}
	err := n.pollers[i].Poll(context.Background())
	if !n.ups[i].down.Load() {
		require.NoError(t, err)
	}
}

// lag is the block-head lag a network-scope selection policy reads.
func (n *simNetwork) lag(i int) int64 {
	return n.tracker.GetUpstreamMethodMetrics(n.ups[i], "*", common.DataFinalityStateAll).BlockHeadLag.Load()
}

// A healthy upstream that serves no traffic is only re-observed by its poller.
// Between polls its stored head is old, not behind: with 1s blocks and a 30s
// interval the network moves ~29 blocks past it before each poll, which is
// well over the default 16-block exclusion gate.
func TestStatePoller_HealthyPolledUpstreamIsNotLaggingBetweenPolls(t *testing.T) {
	n := newSimNetwork(t, "served-a", "served-b", "polled")

	var maxLag int64
	for step := 1; step <= 4*simPollEvery; step++ {
		n.chain.head.Add(1)
		n.serve(0)
		n.serve(1)
		if step%simPollEvery == 0 {
			n.poll(t, 2)
		}
		maxLag = max(maxLag, n.lag(2))
	}

	require.Zero(t, maxLag,
		"the polled upstream's node never trails the chain; the age of its last poll must not be reported as lag")
}

// A polled upstream whose node stops advancing must still cross the lag gate,
// at the latest on the first poll after its real lag crosses it, and stay
// flagged from then on.
func TestStatePoller_FrozenPolledUpstreamCrossesLagGateWithinOnePollInterval(t *testing.T) {
	n := newSimNetwork(t, "served-a", "served-b", "polled")

	const freezeStep = 35
	crossedAt, flaggedAt := 0, 0
	for step := 1; step <= 4*simPollEvery; step++ {
		n.chain.head.Add(1)
		if step == freezeStep {
			n.ups[2].freeze()
		}
		n.serve(0)
		n.serve(1)
		if step%simPollEvery == 0 {
			n.poll(t, 2)
		}
		if step < freezeStep {
			continue
		}
		if crossedAt == 0 && n.chain.head.Load()-n.ups[2].nodeHead() > simLagGate {
			crossedAt = step
		}
		if flaggedAt == 0 && n.lag(2) > simLagGate {
			flaggedAt = step
		}
		if flaggedAt > 0 {
			require.Greater(t, n.lag(2), int64(simLagGate), "step %d: once flagged, a frozen upstream stays flagged", step)
		}
	}

	require.NotZero(t, crossedAt)
	require.NotZero(t, flaggedAt, "a frozen upstream must be reported as lagging")
	require.LessOrEqual(t, flaggedAt-crossedAt, simPollEvery,
		"a frozen upstream must be flagged within one poll interval of its real lag crossing the gate")
}

// An upstream that keeps serving traffic while its head stays put reports no
// new head, so nothing re-records its lag between polls. Its next poll
// observes the stale head, at the latest one poll interval after its real lag
// crosses the gate.
func TestStatePoller_FrozenServedUpstreamCrossesLagGateWithinOnePollInterval(t *testing.T) {
	n := newSimNetwork(t, "served-frozen", "served-b", "served-c")

	const freezeStep = 35
	crossedAt, flaggedAt := 0, 0
	for step := 1; step <= 4*simPollEvery; step++ {
		n.chain.head.Add(1)
		if step == freezeStep {
			n.ups[0].freeze()
		}
		n.serve(0)
		n.serve(1)
		n.serve(2)
		if step%simPollEvery == 0 {
			n.poll(t, 0)
		}
		if step < freezeStep {
			continue
		}
		if crossedAt == 0 && n.chain.head.Load()-n.ups[0].nodeHead() > simLagGate {
			crossedAt = step
		}
		if flaggedAt == 0 && n.lag(0) > simLagGate {
			flaggedAt = step
		}
	}

	require.NotZero(t, crossedAt)
	require.NotZero(t, flaggedAt, "a frozen upstream that keeps answering must be reported as lagging")
	require.LessOrEqual(t, flaggedAt-crossedAt, simPollEvery,
		"a frozen upstream must be flagged within one poll interval of its real lag crossing the gate")
}

// An upstream whose polls fail (node down, circuit breaker open) observes no
// new head. Each failed poll records its last known head against the current
// network head, so it is flagged within one poll interval of that head
// falling past the gate, instead of keeping the lag of its last good poll.
func TestStatePoller_UpstreamWithFailingPollsCrossesLagGateWithinOnePollInterval(t *testing.T) {
	n := newSimNetwork(t, "served-a", "served-b", "polled")

	const downStep = 35
	var lastKnown int64
	crossedAt, flaggedAt := 0, 0
	for step := 1; step <= 4*simPollEvery; step++ {
		n.chain.head.Add(1)
		if step == downStep {
			lastKnown = n.ups[2].nodeHead()
			n.ups[2].goDown()
		}
		n.serve(0)
		n.serve(1)
		if step%simPollEvery == 0 {
			n.poll(t, 2)
		}
		if step < downStep {
			continue
		}
		if crossedAt == 0 && n.chain.head.Load()-lastKnown > simLagGate {
			crossedAt = step
		}
		if flaggedAt == 0 && n.lag(2) > simLagGate {
			flaggedAt = step
		}
	}

	require.NotZero(t, crossedAt)
	require.NotZero(t, flaggedAt, "an upstream whose polls fail must be reported as lagging")
	require.LessOrEqual(t, flaggedAt-crossedAt, simPollEvery,
		"failed polls must re-record its last known head within one poll interval")
}

// A poll tick whose fetch is skipped by the debounce (a block time above the
// interval, or another pod fetched first) still observes the upstream's last
// known head, so a frozen upstream is flagged on that tick.
func TestStatePoller_TickSkippedByDebounceStillRecordsLag(t *testing.T) {
	n := newSimNetwork(t, "served-a", "served-b", "polled")
	n.ups[2].freeze()
	n.pollers[2].stateMu.Lock()
	n.pollers[2].debounceInterval = time.Hour
	n.pollers[2].stateMu.Unlock()

	for range 2 * simLagGate {
		n.chain.head.Add(1)
		n.serve(0)
		n.serve(1)
	}
	require.NoError(t, n.pollers[2].Poll(context.Background()))

	require.EqualValues(t, 2*simLagGate, n.lag(2),
		"a tick that fetches nothing must still measure the last known head against the current network head")
}

// A fetch slower than the shared counter's foreground wait finishes in the
// background, and the tick returns before it with the counter's old head. That
// old head is not an observation: measuring it against the current network
// head would flag a healthy upstream until the fetch lands.
func TestStatePoller_SlowFetchDoesNotRecordTheOldHead(t *testing.T) {
	n := newSimNetwork(t, "served-a", "served-b", "slow")
	n.ups[2].delay.Store(int64(200 * time.Millisecond))

	for range 2 * simLagGate {
		n.chain.head.Add(1)
		n.serve(0)
		n.serve(1)
	}
	n.poll(t, 2)
	require.Zero(t, n.lag(2), "the tick returned before the fetch; its old head must not be recorded")

	require.Eventually(t, func() bool {
		return n.pollers[2].latestBlockShared.GetValue() == n.chain.head.Load()
	}, 2*time.Second, 10*time.Millisecond)
	require.Zero(t, n.lag(2), "the fetch that landed found the node at the tip")
}
