package integrity

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func continuityBody(number int64, hash, parent string) []byte {
	return []byte(fmt.Sprintf(`{"number":"0x%x","hash":"%s","parentHash":"%s"}`, number, hash, parent))
}

func runContinuity(t *testing.T, checkID string, body []byte, hist History) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), body, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return Validate(context.Background(), Input{
		Method:   "eth_getBlockByNumber",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   only(checkID, nil),
		Params:   []any{"0x65", false},
		History:  hist,
	})
}

// Inside a followed segment, parentHashLinkage is subsumed: hashStability asks
// the direct question (is this the chain's block at n?) and blockHashRecompute
// proves the header hashes to the hash it claims, so parentHash cannot differ
// from chain[n-1] without one of those firing. Running it anyway buys nothing
// and drags in the parent-pin dispute that produced every false positive.
func TestParentHashLinkageDefersToTheFollowedChain(t *testing.T) {
	// A mismatching parent: linkage would fire on this if it ran.
	body := continuityBody(101, "0xchild", "0xnot-the-parent")

	t.Run("skips inside the followed segment", func(t *testing.T) {
		seg := &segmentHistory{from: 100, to: 101, headers: map[int64]*Header{
			100: {Number: "0x64", Hash: "0xrealparent"},
			101: {Number: "0x65", Hash: "0xchild"},
		}}
		res := runContinuity(t, "parentHashLinkage", body, seg)
		require.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "parentHashLinkage"),
			"the followed chain answers this more directly via hashStability")
	})

	t.Run("still runs OUTSIDE the followed segment", func(t *testing.T) {
		// Followed range is elsewhere, so the parent pin is all we have.
		seg := &segmentHistory{from: 500, to: 600, headers: map[int64]*Header{
			100: {Number: "0x64", Hash: "0xrealparent"},
		}}
		res := runContinuity(t, "parentHashLinkage", body, seg)
		require.Error(t, res.Err, "outside a followed segment the parent pin is real coverage")
		assert.Equal(t, "reject", outcomeOf(res, "parentHashLinkage"))
	})

	t.Run("still runs when nothing is followed at all", func(t *testing.T) {
		res := runContinuity(t, "parentHashLinkage", body,
			plainHistory{headers: map[int64]*Header{100: {Number: "0x64", Hash: "0xrealparent"}}})
		require.Error(t, res.Err, "with the follower off this check is the only continuity coverage")
	})

	// The check that replaces it inside the segment must actually be doing the
	// work — otherwise the skip above would be a coverage hole.
	t.Run("hashStability catches a wrong block at that height", func(t *testing.T) {
		seg := &segmentHistory{from: 100, to: 101, headers: map[int64]*Header{
			100: {Number: "0x64", Hash: "0xrealparent"},
			101: {Number: "0x65", Hash: "0xcanonical-child"},
		}}
		res := runContinuity(t, "hashStability", continuityBody(101, "0ximposter", "0xrealparent"), seg)
		require.Error(t, res.Err)
		assert.Equal(t, "reject", outcomeOf(res, "hashStability"))
	})
}
