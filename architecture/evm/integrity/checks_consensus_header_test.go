package integrity

import (
	"context"
	"fmt"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func headerBody(uncles, difficulty, nonce string) []byte {
	return []byte(fmt.Sprintf(
		`{"number":"0x65","hash":"0xabc","parentHash":"0xpar","sha3Uncles":"%s","difficulty":"%s","nonce":"%s"}`,
		uncles, difficulty, nonce))
}

func runHeaderCheck(t *testing.T, body []byte, params map[string]string) Result {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x65",false]}`))
	jrr := common.MustNewJsonRpcResponseFromBytes([]byte("1"), body, nil)
	rs := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	return Validate(context.Background(), Input{
		Method:   "eth_getBlockByNumber",
		Upstream: common.NewFakeUpstream("u"),
		Response: rs,
		Checks:   only("headerConsensusInvariants", params),
		Params:   []any{"0x65", false},
	})
}

// Nothing else in the catalog looks at these fields, so a fabricated header can
// carry anything in them today.
func TestHeaderConsensusInvariants(t *testing.T) {
	all := map[string]string{"emptyUncles": "true", "zeroDifficulty": "true", "zeroNonce": "true"}

	t.Run("a compliant post-merge header passes", func(t *testing.T) {
		res := runHeaderCheck(t, headerBody(emptyUnclesHash, "0x0", "0x0000000000000000"), all)
		require.NoError(t, res.Err)
		assert.Equal(t, "pass", outcomeOf(res, "headerConsensusInvariants"))
	})

	t.Run("non-empty ommers are rejected where the regime forbids them", func(t *testing.T) {
		res := runHeaderCheck(t, headerBody("0xdeadbeef", "0x0", "0x0000000000000000"), all)
		require.Error(t, res.Err)
	})

	t.Run("non-zero difficulty is rejected where the regime fixes it at zero", func(t *testing.T) {
		res := runHeaderCheck(t, headerBody(emptyUnclesHash, "0x5", "0x0000000000000000"), all)
		require.Error(t, res.Err)
	})

	t.Run("non-zero nonce is rejected where the regime fixes it at zero", func(t *testing.T) {
		res := runHeaderCheck(t, headerBody(emptyUnclesHash, "0x0", "0x00000000deadbeef"), all)
		require.Error(t, res.Err)
	})

	// The differences between regimes are real and measured: bor reports
	// difficulty 0x1 and Nitro uses a non-zero nonce. Judging those chains by
	// mainnet's invariants would reject every one of their blocks, so only the
	// declared invariants are enforced.
	t.Run("an undeclared invariant is not enforced", func(t *testing.T) {
		polygonLike := map[string]string{"emptyUncles": "true", "zeroNonce": "true"}
		res := runHeaderCheck(t, headerBody(emptyUnclesHash, "0x1", "0x0000000000000000"), polygonLike)
		require.NoError(t, res.Err, "difficulty 0x1 is normal on bor and must not be judged")
		assert.Equal(t, "pass", outcomeOf(res, "headerConsensusInvariants"))
	})

	t.Run("a chain with no declared invariants verifies nothing and says so", func(t *testing.T) {
		res := runHeaderCheck(t, headerBody("0xdeadbeef", "0x9", "0xdeadbeefdeadbeef"), map[string]string{})
		require.NoError(t, res.Err)
		assert.Equal(t, "skip", outcomeOf(res, "headerConsensusInvariants"),
			"reporting a pass would claim a verification that never happened")
	})
}

// The per-architecture wiring must match what was measured on each chain.
func TestHeaderInvariantsWiredPerArchitecture(t *testing.T) {
	params := func(chainId int64) map[string]string {
		cs := CheckSet{"headerConsensusInvariants": CheckConfig{Enabled: true}}
		ApplyChainProfile(cs, chainId)
		return cs["headerConsensusInvariants"].Params
	}

	// Every chain measured carries empty ommers.
	for _, id := range []int64{1, 8453, 137, 42161, 56, 999} {
		assert.Equal(t, "true", params(id)["emptyUncles"], "chain %d", id)
	}
	// ...but difficulty and nonce differ, and were measured that way.
	assert.Equal(t, "true", params(1)["zeroDifficulty"])
	assert.Empty(t, params(137)["zeroDifficulty"], "bor reports 0x1")
	assert.Empty(t, params(42161)["zeroDifficulty"], "nitro reports 0x1")
	assert.Empty(t, params(56)["zeroDifficulty"], "parlia reports 0x2")
	assert.Empty(t, params(42161)["zeroNonce"], "nitro uses a non-zero header nonce")
	assert.Equal(t, "true", params(56)["zeroNonce"])
}
