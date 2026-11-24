package evm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
)

// callGasCapCode is init code that returns gasleft() as one 32-byte word:
// GAS PUSH1 0 MSTORE PUSH1 32 PUSH1 0 RETURN. An eth_call with no "to" runs
// it as a contract creation, so the probe needs no deployed contract and no
// state override.
const callGasCapCode = "0x5a60005260206000f3"

const (
	// callGasCapProbeLow is far below the 50M cap geth ships with: its gas
	// left shows what the call itself costs on the upstream's chain.
	callGasCapProbeLow int64 = 10_000_000
	// callGasCapProbeHigh is above any cap a node sets.
	callGasCapProbeHigh int64 = 1 << 40
	// callGasCapProbeEvery is how long a measure stands: a provider changes
	// its cap only by configuration.
	callGasCapProbeEvery   = time.Hour
	callGasCapProbeTimeout = 30 * time.Second
)

// CallGasCap is the most gas the upstream lets one eth_call use, as its last
// probe measured it; 0 while unknown. A read that finds no probe started in
// the last callGasCapProbeEvery starts one in the background, so only an
// upstream asked about, for an eth_call that names its gas, is probed.
func (e *EvmStatePoller) CallGasCap() uint64 {
	if e.IsObjectNull() {
		return 0
	}
	now := time.Now().UnixNano()
	if at := e.callGasCapAt.Load(); now-at >= int64(callGasCapProbeEvery) && e.callGasCapAt.CompareAndSwap(at, now) {
		e.bg.Go(e.probeCallGasCap)
	}
	return e.callGasCap.Load()
}

// probeCallGasCap measures the cap with two runs of callGasCapCode. A node
// lowers a larger gas to its cap without an error, so the high run's gas left
// is the cap less the call's own cost, and the low run, under any cap, shows
// that cost. A failed run keeps the last measure: a node that refuses gas
// above its cap, as Monad does, stays unknown.
func (e *EvmStatePoller) probeCallGasCap() {
	if ok, _ := e.upstream.ShouldHandleMethod("eth_call"); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(e.appCtx, callGasCapProbeTimeout)
	defer cancel()
	low, err := e.callGasLeft(ctx, callGasCapProbeLow)
	if err != nil {
		e.logger.Debug().Err(err).Msg("eth_call gas cap probe failed")
		return
	}
	high, err := e.callGasLeft(ctx, callGasCapProbeHigh)
	if err != nil {
		e.logger.Debug().Err(err).Msg("eth_call gas cap probe failed")
		return
	}
	gasCap := high
	if high > low {
		gasCap += callGasCapProbeLow - low
	}
	e.callGasCap.Store(uint64(gasCap))
	e.stateMu.RLock()
	networkLabel := e.networkLabel
	e.stateMu.RUnlock()
	telemetry.MetricUpstreamCallGasCap.WithLabelValues(e.projectId, e.upstream.VendorName(), networkLabel, e.upstream.Id()).Set(float64(gasCap))
	e.logger.Debug().Int64("callGasCap", gasCap).Msg("measured eth_call gas cap")
}

// callGasLeft is the gas left at the start of callGasCapCode run by an
// eth_call at latest that asks for gas. Only the code's own answer, one
// 32-byte word, counts: any other result did not run it.
func (e *EvmStatePoller) callGasLeft(ctx context.Context, gas int64) (int64, error) {
	pr := common.NewProbeRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"data":"%s","gas":"0x%x"},"latest"]}`,
		util.RandomID(), callGasCapCode, gas,
	)))
	resp, err := e.upstream.Forward(ctx, pr, false, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return 0, err
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil || jrr == nil {
		return 0, fmt.Errorf("no json-rpc response: %w", err)
	}
	if jrr.Error != nil {
		return 0, jrr.Error
	}
	word := strings.Trim(string(jrr.GetResultBytes()), `"`)
	if len(word) != len("0x")+64 {
		return 0, fmt.Errorf("gas cap probe answered %s", word)
	}
	left, ok := parseHexQuantity(word)
	if !ok || left <= 0 || left > gas {
		return 0, fmt.Errorf("gas cap probe answered %s", word)
	}
	return left, nil
}

// CallGas is the gas field of req's first param, the call object of an
// eth_call; 0 when it names none.
func CallGas(ctx context.Context, req *common.NormalizedRequest) uint64 {
	jrq, err := req.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return 0
	}
	jrq.RLock()
	defer jrq.RUnlock()
	if len(jrq.Params) == 0 {
		return 0
	}
	call, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		return 0
	}
	s, ok := call["gas"].(string)
	if !ok {
		return 0
	}
	gas, err := common.HexToUint64(s)
	if err != nil {
		return 0
	}
	return gas
}
