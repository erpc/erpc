package erpc

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upWithLatest returns a FakeUpstream whose poller reports latest.
func upWithLatest(id string, latest int64) common.Upstream {
	poller := common.NewFakeEvmStatePoller(latest, 0)
	return common.NewFakeUpstream(id, common.WithEvmStatePoller(poller))
}

func ids(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

func TestPartitionUpstreamsByLatestBlock_AllHaveTheBlock_NoReorder(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 100),
		upWithLatest("b", 105),
		upWithLatest("c", 110),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100, 0)
	assert.Equal(t, []string{"a", "b", "c"}, ids(got), "no upstream lags the requested block; partition is identity")
}

func TestPartitionUpstreamsByLatestBlock_NoneHaveTheBlock_NoReorder(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 50),
		upWithLatest("b", 60),
		upWithLatest("c", 70),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100, 0)
	assert.Equal(t, []string{"a", "b", "c"}, ids(got), "no upstream has the requested block; partition is identity (caller can still try them)")
}

func TestPartitionUpstreamsByLatestBlock_SplitsAndPreservesIntraGroupOrder(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 95),
		upWithLatest("b", 110),
		upWithLatest("c", 90),
		upWithLatest("d", 105),
		upWithLatest("e", 88),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100, 0)
	assert.Equal(t, []string{"b", "d", "a", "c", "e"}, ids(got))
}

func TestPartitionUpstreamsByLatestBlock_EqualLatestIsTreatedAsHavingBlock(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 99),
		upWithLatest("b", 100),
		upWithLatest("c", 100),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100, 0)
	assert.Equal(t, []string{"b", "c", "a"}, ids(got))
}

func TestPartitionUpstreamsByLatestBlock_UnknownHeadKeepsItsPlace(t *testing.T) {
	in := []common.Upstream{
		common.NewFakeUpstream("a"),
		upWithLatest("b", 90),
		upWithLatest("c", 110),
		upWithLatest("d", 0),
	}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100, 0)
	assert.Equal(t, []string{"a", "c", "d", "b"}, ids(got), "only a known head below the block moves back")
}

func TestPartitionUpstreamsByLatestBlock_ZeroOrNegativeBlockIsNoOp(t *testing.T) {
	in := []common.Upstream{
		upWithLatest("a", 100),
		upWithLatest("b", 50),
	}
	assert.Equal(t, in, (&Network{}).partitionUpstreamsByLatestBlock(in, 0, 0), "no block reference means no per-block routing preference")
	assert.Equal(t, in, (&Network{}).partitionUpstreamsByLatestBlock(in, -1, 0), "negative block numbers are nonsensical; skip the partition rather than mis-sort")
}

func TestPartitionUpstreamsByLatestBlock_SingleUpstreamIsNoOp(t *testing.T) {
	in := []common.Upstream{upWithLatest("a", 50)}
	got := (&Network{}).partitionUpstreamsByLatestBlock(in, 100, 0)
	assert.Equal(t, in, got, "no other upstream to prefer; partition is a no-op")
}

func TestPreferTipLeaderForNearTipGetBlock(t *testing.T) {
	const method = "eth_getBlockByNumber"

	tied := []common.Upstream{upWithLatest("c", 1000), upWithLatest("b", 1000), upWithLatest("a", 1000)}
	nw := &Network{}
	assert.Equal(t, []string{"c", "b", "a"}, ids(nw.preferTipLeaderForNearTipGetBlock(tied, method, 1000, highestOf(tied), 0)),
		"tied heads keep the selection policy's order")

	ahead := []common.Upstream{upWithLatest("c", 999), upWithLatest("b", 999), upWithLatest("a", 1000)}
	assert.Equal(t, []string{"a", "c", "b"}, ids(nw.preferTipLeaderForNearTipGetBlock(ahead, method, 1001, highestOf(ahead), 0)),
		"a strict leader goes first for its next block")
	assert.Equal(t, []string{"c", "b", "a"}, ids(nw.preferTipLeaderForNearTipGetBlock(ahead, method, 998, highestOf(ahead), 0)),
		"blocks every candidate has keep the policy order")
	assert.Equal(t, []string{"c", "b", "a"}, ids(nw.preferTipLeaderForNearTipGetBlock(ahead, "eth_call", 1000, highestOf(ahead), 0)))
}

// pollAgeEnv drives one network of real pollers, the real tracker and the
// real Network on a pinned clock. Each step is 2s and one block; each mover
// polls it, at its offset from the network head (its fixture's latestBlock
// less 1000). Every head carries its block timestamp, so the tracker learns
// the 2s block time, as in production.
type pollAgeEnv struct {
	t       *testing.T
	ctx     context.Context
	network *Network
	ups     map[string]*upstream.Upstream
	offset  map[string]int64
	advance func(time.Duration)
	head    int64
	movers  []string
	// wrongChain makes id answer eth_chainId with another chain, so its
	// poller refuses any major head move it verifies.
	wrongChain map[string]*atomic.Bool
}

var pollAgeBlockParam = regexp.MustCompile(`"params":\["(0x[0-9a-fA-F]+)"`)

// newPollAgeEnv builds the network and warms it up for 4 blocks with every
// fixture that has a head moving, so the block time is known. Every mock is
// registered before any poller runs: gock is not safe to change under
// requests in flight.
func newPollAgeEnv(t *testing.T, fixtures []servedTipFixture, order ...string) *pollAgeEnv {
	t.Helper()
	advance := pinServedTipClock(t)
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := &pollAgeEnv{t: t, ctx: ctx, advance: advance, head: 1000,
		ups: map[string]*upstream.Upstream{}, offset: map[string]int64{}, wrongChain: map[string]*atomic.Bool{}}
	for i, f := range fixtures {
		fixtures[i].chainID = 123
		fixtures[i].statePollerInterval = 10 * time.Second
		if f.latestBlock > 0 {
			e.offset[f.id] = f.latestBlock - 1000
			e.movers = append(e.movers, f.id)
		}
		host := f.id + ".localhost"
		wrong := &atomic.Bool{}
		e.wrongChain[f.id] = wrong
		gock.New("http://" + host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return r.URL.Host == host && strings.Contains(util.SafeReadBody(r), "eth_chainId")
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				id := "0x7b"
				if wrong.Load() {
					id = "0x1"
				}
				res.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":"` + id + `"}`))
				res.ContentLength = -1
				return res
			})
		// Answers eth_getBlockByNumber for any concrete block with that block.
		gock.New("http://" + host).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				body := util.SafeReadBody(r)
				return r.URL.Host == host && strings.Contains(body, "eth_getBlockByNumber") && pollAgeBlockParam.MatchString(body)
			}).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				hex := pollAgeBlockParam.FindStringSubmatch(util.SafeReadBody(res.Request))[1]
				res.Body = io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{"number":"` + hex + `","hash":"0x` + strings.Repeat("a", 64) + `","parentHash":"0x` + strings.Repeat("b", 64) + `","timestamp":"0x6702a8f1"}}`))
				res.ContentLength = -1
				return res
			})
	}
	network, ups := setupServedTipNetworkWith(t, ctx, fixtures, nil)
	e.network = network
	for _, u := range ups {
		e.ups[u.Id()] = u
	}
	network.PinUpstreamOrderForTest(order...)
	e.step(4)
	require.NotZero(t, network.metricsTracker.GetNetworkBlockTime("evm:123"), "warm-up must teach the block time")
	return e
}

// step moves the chain n blocks, 2s each, through the movers' polls.
func (e *pollAgeEnv) step(n int) {
	for range n {
		e.advance(2 * time.Second)
		e.head++
		for _, id := range e.movers {
			e.report(id, e.head+e.offset[id])
		}
	}
}

// report is one successful poll of id answering h, moved or not, the way the
// poller reports it: the tracker first, with the block timestamp, then the
// poller's counter, dated by the pinned clock as a poll's send time.
func (e *pollAgeEnv) report(id string, h int64) {
	u := e.ups[id]
	e.network.metricsTracker.SetLatestBlockNumber(u, h, 1_800_000_000+2*(h-1000))
	u.EvmStatePoller().SuggestLatestBlock(h, servedTipClock())
}

// first is the upstream Forward tries first for eth_getBlockByNumber(bn);
// every upstream answers it.
func (e *pollAgeEnv) first(bn int64) string {
	e.t.Helper()
	e.network.refreshHeads()
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x%x",false]}`, bn)))
	req.SetNetwork(e.network)
	_, err := e.network.Forward(e.ctx, req)
	require.NoError(e.t, err)
	log := req.ExecState().UpstreamAttemptLog()
	require.NotEmpty(e.t, log)
	return log[0].UpstreamId
}

// headOf is id's head as the head-first order reads it, against the
// network-wide Latest.
func (e *pollAgeEnv) headOf(id string) int64 {
	u := e.ups[id]
	return e.network.creditedHead(u, upstreamLatestBlock(u), servedHead(e.network, e.ctx).Latest)
}

func (e *pollAgeEnv) polled(id string) int64 { return e.ups[id].EvmStatePoller().LatestBlock() }

// A polled head ages by up to a poll interval (10s here, on a 2s chain); the
// tracker credits that age (HeadCredit) and tip reads order by it. Each row is
// one way an upstream can be wrong, and asserts that the credit never puts it
// where a fresh head would be. "reader" and "late" serve no
// eth_getBlockByNumber: they only move the network head.
func TestNetwork_TipGetBlockOrdersByPollAgeCreditedHead(t *testing.T) {
	ignoreGetBlock := []string{"eth_getBlockByNumber"}
	trio := func() []servedTipFixture {
		return []servedTipFixture{
			{id: "fast", latestBlock: 1000},
			{id: "slow", latestBlock: 1000},
			{id: "reader", latestBlock: 1000, ignoreMethods: ignoreGetBlock},
		}
	}

	t.Run("poller stops at tip after serving", func(t *testing.T) {
		e := newPollAgeEnv(t, trio(), "fast", "slow", "reader")
		s := e.head
		e.movers = []string{"slow", "reader"}
		e.step(3)
		require.Equal(t, s, e.polled("fast"))
		assert.Equal(t, "fast", e.first(e.head), "6s after its poll, a head 3 behind only aged: it keeps its place")
		e.step(7)
		assert.InDelta(t, s+5, e.headOf("fast"), 1, "the credit froze one poll period after the last report")
		assert.Equal(t, "slow", e.first(e.head), "silent past its poll period while the chain moved: demoted")
	})

	t.Run("stuck head while its polls succeed", func(t *testing.T) {
		e := newPollAgeEnv(t, trio(), "fast", "slow", "reader")
		s := e.head
		e.movers = []string{"slow", "reader"}
		e.step(5)
		e.report("fast", s)
		assert.Equal(t, s, e.headOf("fast"), "a report of the same head ends the credit")
		assert.Equal(t, "slow", e.first(e.head), "demoted within one poll period")
		for i := 1; i <= 15; i++ {
			e.step(1)
			if i%5 == 0 {
				e.report("fast", s)
			}
			require.LessOrEqual(t, e.headOf("fast"), e.head-5, "step %d: lag at its last report stays", i)
		}
		require.True(t, e.network.metricsTracker.IsHeadBenched(e.ups["fast"]), "a head stuck behind moving peers is benched")
		e.report("fast", s)
		e.step(1)
		assert.Equal(t, s, e.headOf("fast"), "a benched head gets no credit")
		assert.Equal(t, "slow", e.first(e.head))
	})

	t.Run("steady lag of 5 blocks, network head jumps on a late peer", func(t *testing.T) {
		e := newPollAgeEnv(t, []servedTipFixture{
			{id: "lagger", latestBlock: 995},
			{id: "slow", latestBlock: 1000},
			{id: "late", latestBlock: 990, ignoreMethods: ignoreGetBlock},
		}, "lagger", "slow", "late")
		e.movers = []string{"slow", "late"}
		for i := 1; i <= 20; i++ {
			if i == 7 {
				e.offset["late"] = 0 // catches up at once: the network head jumps 10
			}
			e.step(1)
			if i%5 == 0 {
				e.report("lagger", e.head-5)
			}
			require.LessOrEqual(t, e.headOf("lagger"), e.head-5, "step %d: never presumed above network - 5", i)
		}
		assert.Equal(t, "slow", e.first(e.head))
	})

	t.Run("never reported a head", func(t *testing.T) {
		e := newPollAgeEnv(t, []servedTipFixture{
			{id: "never"},
			{id: "slow", latestBlock: 1000},
			{id: "reader", latestBlock: 1000, ignoreMethods: ignoreGetBlock},
		}, "never", "slow", "reader")
		e.step(3)
		require.Zero(t, e.polled("never"))
		before := e.first(e.head)
		e.step(1)
		assert.Zero(t, e.headOf("never"), "served traffic, still no head: no credit")
		assert.Equal(t, before, e.first(e.head), "order unchanged by the credit")
	})

	t.Run("slow-to-answer upstream with the fresher poll", func(t *testing.T) {
		e := newPollAgeEnv(t, trio(), "fast", "slow", "reader")
		e.movers = []string{"slow", "reader"}
		e.step(3)
		assert.Equal(t, "fast", e.first(e.head), "a fresher poll alone does not make a tip leader")
		e.movers = []string{"fast", "reader"}
		e.step(3)
		assert.LessOrEqual(t, e.headOf("slow"), e.head, "credit never above the corroborated head")
		assert.Equal(t, "fast", e.first(e.head))
		assert.Equal(t, "fast", e.first(e.head+1), "credit never makes a tip leader")
	})

	t.Run("far-ahead rogue", func(t *testing.T) {
		e := newPollAgeEnv(t, []servedTipFixture{
			{id: "fast", latestBlock: 1000},
			{id: "slow", latestBlock: 1000},
			{id: "rogue", latestBlock: 1000},
		}, "rogue", "fast", "slow")
		s := e.head
		e.movers = []string{"fast", "slow"}
		// The rogue is on another chain: its poller refuses the jump it verifies,
		// while the tracker stores the far-ahead head it reported.
		e.wrongChain["rogue"].Store(true)
		e.report("rogue", 103_000)
		e.step(3)
		require.Equal(t, s, e.polled("rogue"), "the poller never adopts a head on another chain")
		assert.Equal(t, s, e.headOf("rogue"), "a tracked head above the network earns no credit")
		for _, id := range []string{"fast", "slow"} {
			assert.Equal(t, e.head, e.headOf(id), "%s: at the network head, never above it", id)
		}
		assert.Equal(t, "fast", e.first(e.head), "the rogue stays demoted")
	})

	t.Run("poller stops at tip, never served", func(t *testing.T) {
		e := newPollAgeEnv(t, trio(), "fast", "slow", "reader")
		s := e.head
		// Peers are known to the tracker; fast has served nothing.
		for _, id := range []string{"slow", "reader"} {
			e.network.metricsTracker.GetUpstreamMethodMetrics(e.ups[id], "eth_getBlockByNumber", common.DataFinalityStateAll)
		}
		e.movers = []string{"slow", "reader"}
		e.step(10)
		assert.InDelta(t, s+5, e.headOf("fast"), 1, "the credit froze one poll period after the last report")
		assert.Equal(t, "slow", e.first(e.head), "demoted as if it had served")
	})

	t.Run("two upstreams only", func(t *testing.T) {
		e := newPollAgeEnv(t, []servedTipFixture{
			{id: "fast", latestBlock: 1000},
			{id: "slow", latestBlock: 1000},
		}, "fast", "slow")
		s := e.head
		e.movers = []string{"slow"}
		e.step(3)
		assert.Equal(t, s, e.headOf("fast"), "the corroborated head is the lower one: no credit")
		assert.Equal(t, "slow", e.first(e.head), "same order as without the credit")
		e.step(20)
		assert.Equal(t, "slow", e.first(e.head), "still demoted once benched")
	})
}
