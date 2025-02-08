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
	"github.com/rs/zerolog/log"
)

// LowerBoundBlocksSafetyMargin is the number of blocks to subtract from the last available block to be on the safe side,
// when looking for data too close to the lower-end of Full nodes, because they might have pruned the data already.
var LowerBoundBlocksSafetyMargin int64 = 10

func BuildGetLogsRequest(fromBlock, toBlock int64, address interface{}, topics interface{}, blockHash string) (*common.JsonRpcRequest, error) {
	filter := make(map[string]interface{})

	if blockHash != "" {
		// Per EIP-234, if blockHash is present, fromBlock/toBlock should not be included.
		filter["blockHash"] = blockHash
	} else {
		fb, err := common.NormalizeHex(fromBlock)
		if err != nil {
			return nil, err
		}
		tb, err := common.NormalizeHex(toBlock)
		if err != nil {
			return nil, err
		}
		filter["fromBlock"] = fb
		filter["toBlock"] = tb
	}
	if address != nil {
		filter["address"] = address
	}
	if topics != nil {
		filter["topics"] = topics
	}
	jrq := common.NewJsonRpcRequest("eth_getLogs", []interface{}{filter})
	err := jrq.SetID(util.RandomID())
	if err != nil {
		return nil, err
	}

	return jrq, nil
}

func upstreamPreForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest) (handled bool, resp *common.NormalizedResponse, err error) {
	up, ok := u.(common.EvmUpstream)
	if !ok {
		log.Warn().Interface("upstream", u).Object("request", r).Msg("passed upstream is not a common.EvmUpstream")
		return false, nil, nil
	}

	ncfg := n.Config()
	if ncfg == nil ||
		ncfg.Evm == nil ||
		ncfg.Evm.Integrity == nil ||
		ncfg.Evm.Integrity.EnforceGetLogsBlockRange == nil ||
		!*ncfg.Evm.Integrity.EnforceGetLogsBlockRange {
		// If integrity check for eth_getLogs block range is disabled, skip this hook.
		return false, nil, nil
	}

	logger := up.Logger().With().Str("method", "eth_getLogs").Interface("id", r.ID()).Logger()

	jrq, err := r.JsonRpcRequest()
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

	blockHash, ok := filter["blockHash"].(string)
	if ok && blockHash != "" {
		// EIP-234 => if blockHash is present, fromBlock or toBlock is NOT allowed
		if _, hasFB := filter["fromBlock"]; hasFB {
			jrq.RUnlock()
			return true, nil, common.NewErrEndpointServerSideException(
				fmt.Errorf("EIP-234 violation: blockHash and fromBlock cannot both be present for eth_getLogs"),
				map[string]interface{}{
					"blockHash": blockHash,
					"fromBlock": filter["fromBlock"],
				},
			)
		}
		if _, hasTB := filter["toBlock"]; hasTB {
			jrq.RUnlock()
			return true, nil, common.NewErrEndpointServerSideException(
				fmt.Errorf("EIP-234 violation: blockHash and toBlock cannot both be present for eth_getLogs"),
				map[string]interface{}{
					"blockHash": blockHash,
					"toBlock":   filter["toBlock"],
				},
			)
		}

		jrq.RUnlock()
		logger.Debug().
			Str("blockHash", blockHash).
			Msg("blockHash is specified, skipping from/to block range checks")

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

			var subRequests []ethGetLogsSubRequest
			sb := fromBlock
			for sb <= toBlock {
				eb := sb + cfg.Evm.GetLogsMaxBlockRange - 1
				if eb > toBlock {
					eb = toBlock
				}
				subRequests = append(subRequests, ethGetLogsSubRequest{
					fromBlock: sb,
					toBlock:   eb,
					address:   filter["address"],
					topics:    filter["topics"],
				})
				sb = eb + 1
			}

			mergedResponse, err := executeGetLogsSubRequests(ctx, n, u, r, subRequests, r.Directives().SkipCacheRead)
			if err != nil {
				return true, nil, err
			}

			return true, common.NewNormalizedResponse().
				WithRequest(r).
				WithJsonRpcResponse(mergedResponse), nil
		}
	}

	// Continue with the original forward flow
	return false, nil, nil
}

func upstreamPostForward_eth_getLogs(ctx context.Context, n common.Network, u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error, skipCacheRead bool) (*common.NormalizedResponse, error) {
	if re != nil {
		if common.HasErrorCode(re, common.ErrCodeEndpointRequestTooLarge) {
			logger := u.Logger().With().Str("method", "eth_getLogs").Interface("id", rq.ID()).Logger()
			// Split the request in half, first on block range, if 1 block then on addresses, if 1 address then on topics
			subRequests, err := splitEthGetLogsRequest(rq)
			// TODO Update the evmGetLogsMaxRange accordingly?
			if err != nil {
				logger.Warn().Err(err).Object("request", rq).Msg("could not split eth_getLogs request, returning original response")
				return rs, re
			}
			mergedResponse, err := executeGetLogsSubRequests(ctx, n, u, rq, subRequests, skipCacheRead)
			if err != nil {
				logger.Warn().Err(err).Object("request", rq).Msg("could not execute eth_getLogs sub-requests, returning original response")
				return rs, re
			}

			return common.NewNormalizedResponse().
				WithRequest(rq).
				WithJsonRpcResponse(mergedResponse), nil
		}
	} else if rs != nil && rs.IsResultEmptyish() {
		// This is to normalize empty logs responses (e.g. instead of returning "null")
		jrr, err := common.NewJsonRpcResponse(rq.ID(), []interface{}{}, nil)
		if err != nil {
			return nil, err
		}
		nnr := common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(jrr)
		rq.SetLastValidResponse(nnr)
		return nnr, nil
	}

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

	first := true
	for _, result := range g.results {
		// Trim leading/trailing whitespace and brackets
		trimmed := bytes.TrimSpace(result)
		if util.IsBytesEmptyish(trimmed) {
			continue // Skip empty results
		}

		if !first {
			// Write comma separator
			nn, err = w.Write([]byte{','})
			if err != nil {
				return n + int64(nn), err
			}
			n += int64(nn)
		}
		first = false

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

func (g *GetLogsMultiResponseWriter) IsResultEmptyish() bool {
	if len(g.results) == 0 {
		return true
	}

	for _, result := range g.results {
		if !util.IsBytesEmptyish(result) {
			return false
		}
	}

	return true
}

type ethGetLogsSubRequest struct {
	fromBlock int64
	toBlock   int64
	address   interface{}
	topics    interface{}
	blockHash string
}

func splitEthGetLogsRequest(r *common.NormalizedRequest) ([]ethGetLogsSubRequest, error) {
	jrq, err := r.JsonRpcRequest()
	if err != nil {
		return nil, err
	}
	if len(jrq.Params) < 1 {
		return nil, fmt.Errorf("invalid params length")
	}

	filter, ok := jrq.Params[0].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid filter format")
	}

	fb, tb, err := extractBlockRange(filter)
	if err != nil {
		return nil, err
	}

	// First try splitting by block range
	blockRange := tb - fb + 1
	if blockRange > 1 {
		mid := fb + (blockRange / 2)
		return []ethGetLogsSubRequest{
			{fromBlock: fb, toBlock: mid - 1, address: filter["address"], topics: filter["topics"]},
			{fromBlock: mid, toBlock: tb, address: filter["address"], topics: filter["topics"]},
		}, nil
	}

	// If single block, try splitting by address
	addresses, ok := filter["address"].([]interface{})
	if ok && len(addresses) > 1 {
		mid := len(addresses) / 2
		return []ethGetLogsSubRequest{
			{fromBlock: fb, toBlock: tb, address: addresses[:mid], topics: filter["topics"]},
			{fromBlock: fb, toBlock: tb, address: addresses[mid:], topics: filter["topics"]},
		}, nil
	}

	// If single address or no address, try splitting by topics
	topics, ok := filter["topics"].([]interface{})
	if ok && len(topics) > 1 {
		mid := len(topics) / 2
		return []ethGetLogsSubRequest{
			{fromBlock: fb, toBlock: tb, address: filter["address"], topics: topics[:mid]},
			{fromBlock: fb, toBlock: tb, address: filter["address"], topics: topics[mid:]},
		}, nil
	}

	return nil, fmt.Errorf("request cannot be split further")
}

func extractBlockRange(filter map[string]interface{}) (fromBlock, toBlock int64, err error) {
	fb, ok := filter["fromBlock"].(string)
	if !ok || !strings.HasPrefix(fb, "0x") {
		return 0, 0, fmt.Errorf("invalid fromBlock")
	}
	fromBlock, err = strconv.ParseInt(fb, 0, 64)
	if err != nil {
		return 0, 0, err
	}

	tb, ok := filter["toBlock"].(string)
	if !ok || !strings.HasPrefix(tb, "0x") {
		return 0, 0, fmt.Errorf("invalid toBlock")
	}
	toBlock, err = strconv.ParseInt(tb, 0, 64)
	if err != nil {
		return 0, 0, err
	}

	return fromBlock, toBlock, nil
}

func executeGetLogsSubRequests(ctx context.Context, n common.Network, u common.Upstream, r *common.NormalizedRequest, subRequests []ethGetLogsSubRequest, skipCacheRead bool) (*common.JsonRpcResponse, error) {
	logger := u.Logger().With().Str("method", "eth_getLogs").Interface("id", r.ID()).Logger()

	wg := sync.WaitGroup{}
	results := make([][]byte, 0)
	errs := make([]error, 0)
	mu := sync.Mutex{}

	for _, sr := range subRequests {
		wg.Add(1)
		go func(req ethGetLogsSubRequest) {
			defer wg.Done()

			srq, err := BuildGetLogsRequest(req.fromBlock, req.toBlock, req.address, req.topics, req.blockHash)
			logger.Debug().
				Object("request", srq).
				Msg("executing eth_getLogs sub-request")

			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}

			sbnrq := common.NewNormalizedRequestFromJsonRpcRequest(srq)
			dr := r.Directives().Clone()
			dr.SkipCacheRead = skipCacheRead
			// TODO dr.UseUpstream = u.Config().Id should we force this (or opposite of it)?
			sbnrq.SetDirectives(dr)
			sbnrq.SetNetwork(n)

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

			if jrr == nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("unexpected empty json-rpc response %v", rs))
				mu.Unlock()
				return
			}

			if jrr.Error != nil {
				mu.Lock()
				errs = append(errs, jrr.Error)
				mu.Unlock()
				return
			}

			mu.Lock()
			results = append(results, jrr.Result)
			mu.Unlock()
		}(sr)
	}
	wg.Wait()

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	mergedResponse := mergeEthGetLogsResults(results)
	jrq, _ := r.JsonRpcRequest()
	err := mergedResponse.SetID(jrq.ID)
	if err != nil {
		return nil, err
	}

	return mergedResponse, nil
}

func mergeEthGetLogsResults(results [][]byte) *common.JsonRpcResponse {
	writer := NewGetLogsMultiResponseWriter(results)
	jrr := &common.JsonRpcResponse{}
	jrr.SetResultWriter(writer)
	return jrr
}
