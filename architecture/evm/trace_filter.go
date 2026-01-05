package evm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// upstreamPreForward_trace_filter performs block range availability checking
// for trace_filter and arbtrace_filter methods.
// These methods have fromBlock/toBlock parameters similar to eth_getLogs and
// need dual-bound checking to ensure both bounds are available on the upstream.
func upstreamPreForward_trace_filter(ctx context.Context, n common.Network, u common.Upstream, nrq *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	up, ok := u.(common.EvmUpstream)
	if !ok {
		log.Warn().Interface("upstream", u).Object("request", nrq).Msg("passed upstream is not a common.EvmUpstream")
		return false, nil, nil
	}

	ncfg := n.Config()
	// Reuse the same config as eth_getLogs since trace_filter has the same block range semantics
	if ncfg == nil ||
		ncfg.Evm == nil ||
		ncfg.Evm.Integrity == nil ||
		ncfg.Evm.Integrity.EnforceGetLogsBlockRange == nil ||
		!*ncfg.Evm.Integrity.EnforceGetLogsBlockRange {
		// If integrity check for block range is disabled, skip this hook.
		return false, nil, nil
	}

	method, _ := nrq.Method()
	ctx, span := common.StartDetailSpan(ctx, "Upstream.PreForwardHook.trace_filter", trace.WithAttributes(
		attribute.String("request.id", fmt.Sprintf("%v", nrq.ID())),
		attribute.String("network.id", n.Id()),
		attribute.String("upstream.id", up.Id()),
		attribute.String("method", method),
	))
	defer span.End()

	jrq, err := nrq.JsonRpcRequest(ctx)
	if err != nil {
		return true, nil, err
	}

	jrq.RLock()
	if len(jrq.Params) < 1 {
		jrq.RUnlock()
		return false, nil, nil
	}
	filter, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		jrq.RUnlock()
		return false, nil, nil
	}

	fb, ok := filter["fromBlock"].(string)
	if !ok || !strings.HasPrefix(fb, "0x") {
		jrq.RUnlock()
		return false, nil, nil
	}
	fromBlock, err := strconv.ParseInt(fb, 0, 64)
	if err != nil {
		jrq.RUnlock()
		return true, nil, err
	}
	tb, ok := filter["toBlock"].(string)
	if !ok || !strings.HasPrefix(tb, "0x") {
		jrq.RUnlock()
		return false, nil, nil
	}
	toBlock, err := strconv.ParseInt(tb, 0, 64)
	if err != nil {
		jrq.RUnlock()
		return true, nil, err
	}
	jrq.RUnlock()

	if fromBlock > toBlock {
		return true, nil, common.NewErrInvalidRequest(
			errors.New("fromBlock (" + strconv.FormatInt(fromBlock, 10) + ") must be less than or equal to toBlock (" + strconv.FormatInt(toBlock, 10) + ")"),
		)
	}

	if err := CheckBlockRangeAvailability(ctx, up, method, fromBlock, toBlock); err != nil {
		return true, nil, err
	}

	// Continue with the original forward flow
	return false, nil, nil
}
