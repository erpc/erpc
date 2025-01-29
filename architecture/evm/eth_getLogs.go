package evm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/util"
)

// LowerBoundBlocksSafetyMargin is the number of blocks to subtract from the last available block to be on the safe side,
// when looking for data too close to the lower-end of Full nodes, because they might have pruned the data already.
var LowerBoundBlocksSafetyMargin int64 = 10

func BuildGetLogsRequest(fromBlock, toBlock int64, address interface{}, topics interface{}) (*common.JsonRpcRequest, error) {
	fb, err := common.NormalizeHex(fromBlock)
	if err != nil {
		return nil, err
	}
	tb, err := common.NormalizeHex(toBlock)
	if err != nil {
		return nil, err
	}
	filter := map[string]interface{}{
		"fromBlock": fb,
		"toBlock":   tb,
	}
	if address != nil {
		filter["address"] = address
	}
	if topics != nil {
		filter["topics"] = topics
	}
	jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{filter})
	err = jrq.SetID(util.RandomID())
	if err != nil {
		return nil, err
	}

	return jrq, nil
}

func upstreamPreForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	up, ok := u.(common.EvmUpstream)
	if !ok {
		return false, nil, nil
	}

	logger := up.Logger().With().Str("method", "eth_getLogs").Interface("id", r.ID()).Logger()

	ncfg := n.Config()
	if ncfg == nil ||
		ncfg.Evm == nil ||
		ncfg.Evm.Integrity == nil ||
		ncfg.Evm.Integrity.EnforceGetLogsBlockRange == nil ||
		!*ncfg.Evm.Integrity.EnforceGetLogsBlockRange {
		// If integrity check for eth_getLogs block range is disabled, skip this hook.
		return false, nil, nil
	}

	jrq, err := r.JsonRpcRequest()
	if err != nil {
		return true, nil, err
	}
	if len(jrq.Params) < 1 {
		return false, nil, nil
	}
	filter, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		return false, nil, nil
	}
	fb, ok := filter["fromBlock"].(string)
	if !ok || !strings.HasPrefix(fb, "0x") {
		return false, nil, nil
	}
	fromBlock, err := strconv.ParseInt(fb, 0, 64)
	if err != nil {
		return true, nil, err
	}
	tb, ok := filter["toBlock"].(string)
	if !ok || !strings.HasPrefix(tb, "0x") {
		return false, nil, nil
	}
	toBlock, err := strconv.ParseInt(tb, 0, 64)
	if err != nil {
		return true, nil, err
	}

	statePoller := up.EvmStatePoller()
	if statePoller == nil {
		return true, nil, common.NewErrUpstreamRequestSkipped(
			fmt.Errorf("upstream evm state poller is not available"),
			up.Config().Id,
		)
	}

	// Check if upstream state poller has the last block >= logs range end,
	// if not force a poller update, and if still not enough, skip the request.
	latestBlock := statePoller.LatestBlock()

	logger.Debug().Int64("fromBlock", fromBlock).Int64("toBlock", toBlock).Int64("latestBlock", latestBlock).Msg("checking eth_getLogs block range integrity")

	if latestBlock < toBlock {
		latestBlock, err = statePoller.PollLatestBlockNumber(ctx)
		if err != nil {
			return true, nil, err
		}
	}
	if latestBlock < toBlock {
		health.MetricUpstreamEvmGetLogsStaleUpperBound.WithLabelValues(
			n.ProjectId(),
			up.NetworkId(),
			up.Config().Id,
		).Inc()
		return true, nil, common.NewErrUpstreamRequestSkipped(
			fmt.Errorf("upstream latest block %d is less than toBlock %d", latestBlock, toBlock),
			up.Config().Id,
		)
	}

	cfg := up.Config()

	// Check if the log range start is higher than node's max available block range,
	// if not, skip the request.
	if cfg != nil && cfg.Evm != nil && cfg.Evm.MaxAvailableRecentBlocks > 0 {
		lastAvailableBlock := latestBlock - cfg.Evm.MaxAvailableRecentBlocks
		// If range is beyond the last available block, or too close to the last available block, skip the request for safety.
		if fromBlock < (lastAvailableBlock + LowerBoundBlocksSafetyMargin) {
			health.MetricUpstreamEvmGetLogsStaleLowerBound.WithLabelValues(
				n.ProjectId(),
				up.NetworkId(),
				up.Config().Id,
			).Inc()
			return true, nil, common.NewErrUpstreamRequestSkipped(
				fmt.Errorf("requested fromBlock %d is < than upstream latest block %d minus max available recent blocks %d plus safety margin %d", fromBlock, latestBlock, cfg.Evm.MaxAvailableRecentBlocks, LowerBoundBlocksSafetyMargin),
				up.Config().Id,
			)
		}
	}

	// Check evmGetLogsMaxRange and if the range is already bigger try to split in multiple smaller requests, and merge the final result
	// For better performance try to use byte merging (not JSON parsing/encoding)
	if cfg != nil && cfg.Evm != nil && cfg.Evm.GetLogsMaxBlockRange > 0 {
		requestRange := toBlock - fromBlock + 1
		if requestRange > cfg.Evm.GetLogsMaxBlockRange {
			health.MetricUpstreamEvmGetLogsRangeExceeded.WithLabelValues(
				n.ProjectId(),
				up.NetworkId(),
				up.Config().Id,
			).Inc()
			logger.Debug().
				Int64("fromBlock", fromBlock).
				Int64("toBlock", toBlock).
				Int64("requestRange", requestRange).
				Int64("maxRange", cfg.Evm.GetLogsMaxBlockRange).Msg("splitting eth_getLogs request into smaller sub-requests")

			wg := sync.WaitGroup{}
			results := make([][]byte, 0)
			errs := make([]error, 0)
			mu := sync.Mutex{}
			sb := fromBlock
			for sb <= toBlock {
				wg.Add(1)
				eb := sb + cfg.Evm.GetLogsMaxBlockRange - 1
				if eb > toBlock {
					eb = toBlock
				}
				go func(startBlock, endBlock int64) {
					defer wg.Done()
					srq, err := BuildGetLogsRequest(startBlock, endBlock, filter["address"], filter["topics"])
					logger.Debug().Int64("fromBlock", startBlock).Int64("toBlock", endBlock).Object("request", srq).Msg("building eth_getLogs sub-request")
					if err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
						return
					}
					sbnrq := common.NewNormalizedRequestFromJsonRpcRequest(srq)
					dr := r.Directives().Clone()
					dr.UseUpstream = u.Config().Id
					sbnrq.SetDirectives(dr)
					sbnrq.SetNetwork(n)
					// The reason we use Network.Forward instead of Upstream.Forward is to take advantage of cache
					// and multiplexing. We use directives to instruct to use the current upstream for this request.
					rs, re := n.Forward(ctx, sbnrq)
					if re != nil {
						mu.Lock()
						errs = append(errs, re)
						mu.Unlock()
						return
					}
					jrr, err := rs.JsonRpcResponse()
					if err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
						return
					}
					mu.Lock()
					results = append(results, jrr.Result)
					mu.Unlock()
				}(sb, eb)
				sb = eb + 1
			}
			wg.Wait()

			health.MetricUpstreamEvmGetLogsSplitSuccess.WithLabelValues(
				n.ProjectId(),
				up.NetworkId(),
				up.Config().Id,
			).Add(float64(len(results)))
			health.MetricUpstreamEvmGetLogsSplitFailure.WithLabelValues(
				n.ProjectId(),
				up.NetworkId(),
				up.Config().Id,
			).Add(float64(len(errs)))

			if len(errs) > 0 {
				return true, nil, errors.Join(errs...)
			}

			logger.Trace().Interface("results", results).Msg("merging eth_getLogs results")
			mergedResponse := mergeEthGetLogsResults(results, errs)
			err = mergedResponse.SetID(jrq.ID)
			if err != nil {
				return true, nil, err
			}
			return true, common.NewNormalizedResponse().
				WithRequest(r).
				WithJsonRpcResponse(mergedResponse), nil
		} else {
			// Continue with the original forward flow
			return false, nil, nil
		}
	}

	// Continue with the original forward flow
	return false, nil, nil
}

func upstreamPostForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	// TODO If error is about range, update the evmGetLogsMaxRange accordingly, and then retry the request?
	return rs, re
}

type GetLogsMultiResponseWriter struct {
	results [][]byte
}

func NewGetLogsMultiResponseWriter(results [][]byte) *GetLogsMultiResponseWriter {
	return &GetLogsMultiResponseWriter{
		results: results,
	}
}

func (g *GetLogsMultiResponseWriter) WriteTo(w io.Writer) (n int64, err error) {
	// Write opening bracket
	nn, err := w.Write([]byte{'['})
	if err != nil {
		return int64(nn), err
	}
	n += int64(nn)

	for i, result := range g.results {
		if i > 0 {
			// Write comma separator
			nn, err = w.Write([]byte{','})
			if err != nil {
				return n + int64(nn), err
			}
			n += int64(nn)
		}

		// Trim leading/trailing whitespace and brackets
		trimmed := bytes.TrimSpace(result)
		if len(trimmed) < 2 {
			continue // Skip empty results
		}

		// Write the inner content, skipping the outer brackets
		nn, err = w.Write(trimmed[1 : len(trimmed)-1])
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	}

	// Write closing bracket
	nn, err = w.Write([]byte{']'})
	return n + int64(nn), err
}

func mergeEthGetLogsResults(results [][]byte, errs []error) *common.JsonRpcResponse {
	writer := NewGetLogsMultiResponseWriter(results)
	jrr := &common.JsonRpcResponse{}
	jrr.SetResultWriter(writer)
	return jrr
}
