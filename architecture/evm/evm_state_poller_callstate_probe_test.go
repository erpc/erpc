package evm

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canaryUpstream is a scriptable common.Upstream for the callState probe: the
// handler decides each response, and every request is recorded so tests can
// assert WHICH canary was asked and whether the fallback was consulted.
type canaryUpstream struct {
	cfg     *common.UpstreamConfig
	logger  zerolog.Logger
	handler func(method, params string) (*common.NormalizedResponse, error)
	calls   []string // methods, in order
}

func (f *canaryUpstream) Id() string                     { return f.cfg.Id }
func (f *canaryUpstream) VendorName() string             { return "test" }
func (f *canaryUpstream) NetworkId() string              { return "evm:123" }
func (f *canaryUpstream) NetworkLabel() string           { return "evm:123" }
func (f *canaryUpstream) Config() *common.UpstreamConfig { return f.cfg }
func (f *canaryUpstream) Logger() *zerolog.Logger        { return &f.logger }
func (f *canaryUpstream) Vendor() common.Vendor          { return nil }
func (f *canaryUpstream) Tracker() common.HealthTracker  { return nil }
func (f *canaryUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	jrq, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	params, _ := common.SonicCfg.Marshal(jrq.Params)
	f.calls = append(f.calls, jrq.Method)
	return f.handler(jrq.Method, string(params))
}
func (f *canaryUpstream) Cordon(method string, reason string)   {}
func (f *canaryUpstream) Uncordon(method string, reason string) {}
func (f *canaryUpstream) IgnoreMethod(method string)            {}
func (f *canaryUpstream) ShouldHandleMethod(method string) (bool, error) {
	return true, nil
}

var _ common.Upstream = (*canaryUpstream)(nil)

func newCanaryPoller(chainId int64, handler func(method, params string) (*common.NormalizedResponse, error)) (*EvmStatePoller, *canaryUpstream) {
	lg := zerolog.Nop()
	up := &canaryUpstream{
		logger:  lg,
		handler: handler,
		cfg: &common.UpstreamConfig{
			Id:   "test-upstream",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: chainId},
		},
	}
	return &EvmStatePoller{upstream: up, logger: &lg}, up
}

func hexWordResponse(v int64) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(fmt.Sprintf(`"0x%064x"`, v)), nil))
}

func rawResultResponse(result string) *common.NormalizedResponse {
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`"1"`), []byte(result), nil))
}

// The callState availability probe must measure EXECUTION at the pinned
// height, not the mere presence of a well-formed answer: eth_getBalance(0x0, N)
// is answerable from any state (a stale node returns 0x0), while the
// per-architecture execution canary returns the height of the context the node
// actually executed in. The balance heuristic survives only as the fallback
// where the canary yields no evidence — discovered per height, never assumed.
func TestCheckCallStateProbe_ExecutionCanary(t *testing.T) {
	const block = int64(0x1234)

	t.Run("canary executing at the pinned height is available, without consulting the fallback", func(t *testing.T) {
		e, up := newCanaryPoller(123456, func(method, params string) (*common.NormalizedResponse, error) {
			require.Equal(t, "eth_call", method)
			return hexWordResponse(block), nil
		})
		ok, unsupported, err := e.checkCallStateProbe(context.Background(), block)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, unsupported)
		assert.Equal(t, []string{"eth_call"}, up.calls)
	})

	t.Run("canary executing at a DIFFERENT height is unavailable, even though a balance would answer", func(t *testing.T) {
		e, up := newCanaryPoller(123456, func(method, params string) (*common.NormalizedResponse, error) {
			if method == "eth_getBalance" {
				return rawResultResponse(`"0x0"`), nil // the stale-node alibi the canary exists to pierce
			}
			return hexWordResponse(block - 500), nil // executes 500 back from the pin
		})
		ok, unsupported, err := e.checkCallStateProbe(context.Background(), block)
		require.NoError(t, err)
		assert.False(t, ok, "a wrong execution height is evidence AGAINST availability, not a gap")
		assert.False(t, unsupported)
		assert.Equal(t, []string{"eth_call"}, up.calls,
			"a definite canary answer must not be second-guessed by the weak fallback")
	})

	t.Run("canary absent (0x returndata) falls back to the balance heuristic", func(t *testing.T) {
		e, up := newCanaryPoller(123456, func(method, params string) (*common.NormalizedResponse, error) {
			if method == "eth_getBalance" {
				return rawResultResponse(`"0x0"`), nil
			}
			return rawResultResponse(`"0x"`), nil // no code at either canary address
		})
		ok, _, err := e.checkCallStateProbe(context.Background(), block)
		require.NoError(t, err)
		assert.True(t, ok, "no canary deployed -> exactly the previous (balance) behavior")
		assert.Equal(t, []string{"eth_call", "eth_call", "eth_getBalance"}, up.calls, "ArbSys first, then Multicall3, then the balance")
	})

	t.Run("canary erroring falls back too, and the fallback's verdict stands", func(t *testing.T) {
		for _, balance := range []struct {
			result string
			want   bool
		}{
			{`"0x0"`, true},
			{`null`, false},
		} {
			e, _ := newCanaryPoller(123456, func(method, params string) (*common.NormalizedResponse, error) {
				if method == "eth_getBalance" {
					return rawResultResponse(balance.result), nil
				}
				return nil, fmt.Errorf("execution reverted")
			})
			ok, _, err := e.checkCallStateProbe(context.Background(), block)
			require.NoError(t, err)
			assert.Equal(t, balance.want, ok, "balance=%s", balance.result)
		}
	})

	t.Run("Nitro is wherever ArbSys answers, whatever the chain id; the first answer decides for good", func(t *testing.T) {
		// On Nitro chains (Arbitrum One, Nova, every Orbit chain) block.number
		// is the parent chain's height, so Multicall3 getBlockNumber() would
		// mismatch on every honest node; ArbSys arbBlockNumber() answers the
		// chain's own. Off Nitro, ArbSys has no code and answers "0x".
		const arbSys, multicall3 = "0x0000000000000000000000000000000000000064", "0xcA11bde05977b3631167028862bE2a173976CA11"
		for _, nitro := range []bool{true, false} {
			var asked []string
			e, _ := newCanaryPoller(42170, func(method, params string) (*common.NormalizedResponse, error) {
				switch {
				case strings.Contains(params, arbSys):
					asked = append(asked, "arbSys")
					if !nitro {
						return rawResultResponse(`"0x"`), nil
					}
					return hexWordResponse(block), nil
				case strings.Contains(params, multicall3):
					asked = append(asked, "multicall3")
					if nitro {
						return hexWordResponse(block / 4), nil // the parent chain's height
					}
					return hexWordResponse(block), nil
				}
				return nil, fmt.Errorf("unexpected call %s %s", method, params)
			})
			for range 2 {
				ok, _, err := e.checkCallStateProbe(context.Background(), block)
				require.NoError(t, err)
				assert.True(t, ok, "nitro=%v", nitro)
			}
			if nitro {
				assert.Equal(t, []string{"arbSys", "arbSys"}, asked)
			} else {
				assert.Equal(t, []string{"arbSys", "multicall3", "multicall3"}, asked)
			}
		}
	})
}
