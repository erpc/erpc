package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
)

// voteHead is u's poll answering block of the one test chain (tipHead
// "a"): a hash vote, as every real poll and push is, and it moves u's
// counter. A number alone never moves the view (design: one live head).
// A major jump is held while u's chain identity is checked (spec §5): as
// u's next poll would, it answers the same block again until taken. It
// returns once every tracker took the vote, as polls a block apart find
// it: a test's next read is then a read of the next block.
func voteHead(t testing.TB, n *Network, u *upstream.Upstream, block int64) {
	t.Helper()
	h := polledHead(t, "a", uint64(block))
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		n.intake.Polled(u.Id(), h, nil, evm.HeaderPolled, time.Now())
		if u.EvmStatePoller().LatestBlock() >= block {
			evm.SyncTrackersForTest(n.appCtx, n)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("intake never took %s's head %d", u.Id(), block)
		}
	}
}

// mockTestChain makes endpoint answer every block of the voteHead chain
// by number and by hash, as a node holding it does, so the tracker links
// and fills the votes it is given.
func mockTestChain(endpoint string) {
	block := func(body string) (uint64, bool) {
		var rpc struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.Unmarshal([]byte(body), &rpc) != nil || len(rpc.Params) == 0 {
			return 0, false
		}
		var p string
		if json.Unmarshal(rpc.Params[0], &p) != nil {
			return 0, false
		}
		var hexNum string
		switch {
		case rpc.Method == "eth_getBlockByNumber" && strings.HasPrefix(p, "0x"):
			hexNum = p[2:]
		case rpc.Method == "eth_getBlockByHash" && strings.HasPrefix(p, "0xa") && len(p) == 66:
			hexNum = strings.TrimLeft(p[3:], "0")
		default:
			return 0, false
		}
		n, err := strconv.ParseUint(hexNum, 16, 64)
		return n, err == nil
	}
	gock.New(endpoint).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			_, ok := block(util.SafeReadBody(r))
			return ok
		}).
		Reply(200).
		Map(func(res *http.Response) *http.Response {
			n, _ := block(util.SafeReadBody(res.Request))
			b := []byte(`{"jsonrpc":"2.0","id":1,"result":` + string(tipHead("a", n)) + `}`)
			res.Body = io.NopCloser(bytes.NewReader(b))
			res.ContentLength = int64(len(b))
			return res
		})
}

// servedHead computes every lane's Head, as a head move or the 1s timer
// does, and returns the Head of the use-upstream selector of the request
// bound to ctx; a selector with no lane gets its stateless pick.
func servedHead(n *Network, ctx context.Context) common.EvmHead {
	n.refreshHeads()
	return *n.headFor(requestSelector(ctx))
}

// laneCeilingOf is the network-wide lane's ceiling and whether it is known.
func laneCeilingOf(n *Network) (int64, bool) {
	n.refreshHeads()
	c := n.servedTipAll.published().Ceiling
	return c, c > 0
}

// freshHeadMemory wipes the network-wide lane's guard, watchdog and
// finalized-floor memory, the shared floor included, as in a cluster that
// has evaluated nothing yet: the head loop already evaluated the partial
// ballots a fixture's setup pushes, head by head. A max-only counter cannot
// be lowered, so the network stops sharing; only laneTip of this lane reads
// it, under l.mu.
func freshHeadMemory(n *Network) {
	l := &n.servedTipAll
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, a := range []*servedTipAnchor{&l.latest, &l.finalized} {
		a.seenValue.Store(0)
		a.changedAtMs.Store(0)
		a.lastGoodValue.Store(0)
		a.lastGoodAt.Store(nil)
		a.guardState.Store(servedTipGuardHealthy)
	}
	l.finalizedFloor = 0
	n.servedFinalizedShared = nil
	l.head.Store(nil)
}

// highestOf is the first upstream of ups with the highest head: the Leader
// a Head of exactly ups would elect.
func highestOf(ups []common.Upstream) common.Upstream {
	var leader common.Upstream
	var top int64
	for _, u := range ups {
		if h := evmTipHead(u); h > top {
			leader, top = u, h
		}
	}
	return leader
}
