package erpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

type ethCallBatchInfo struct {
	networkId  string
	blockRef   string
	blockParam interface{}
}

type ethCallBatchCandidate struct {
	index  int
	ctx    context.Context
	req    *common.NormalizedRequest
	logger zerolog.Logger
}

type ethCallBatchProbe struct {
	Method    string        `json:"method"`
	Params    []interface{} `json:"params"`
	NetworkId string        `json:"networkId"`
}

var (
	forwardBatchNetwork = func(ctx context.Context, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return network.Forward(ctx, req)
	}
	forwardBatchProject = func(ctx context.Context, project *PreparedProject, network *Network, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return project.doForward(ctx, network, req)
	}
	newBatchJsonRpcResponse = common.NewJsonRpcResponse
)

func detectEthCallBatchInfo(requests []json.RawMessage, architecture, chainId string) *ethCallBatchInfo {
	if len(requests) < 2 {
		return nil
	}
	if architecture != "" && architecture != string(common.ArchitectureEvm) {
		return nil
	}

	defaultNetworkId := ""
	if architecture != "" && chainId != "" {
		defaultNetworkId = fmt.Sprintf("%s:%s", architecture, chainId)
	}

	var networkId string
	var blockRef string
	var blockParam interface{}

	for _, raw := range requests {
		var probe ethCallBatchProbe
		if err := common.SonicCfg.Unmarshal(raw, &probe); err != nil {
			return nil
		}
		if strings.ToLower(probe.Method) != "eth_call" {
			return nil
		}

		reqNetworkId := defaultNetworkId
		if reqNetworkId == "" {
			reqNetworkId = probe.NetworkId
		}
		if reqNetworkId == "" || !strings.HasPrefix(reqNetworkId, "evm:") {
			return nil
		}
		if networkId == "" {
			networkId = reqNetworkId
		} else if networkId != reqNetworkId {
			return nil
		}

		param := interface{}("latest")
		if len(probe.Params) >= 2 {
			param = probe.Params[1]
		}
		bref, err := evm.NormalizeBlockParam(param)
		if err != nil {
			return nil
		}
		if blockRef == "" {
			blockRef = bref
			blockParam = param
		} else if blockRef != bref {
			return nil
		}
	}

	if networkId == "" || blockRef == "" {
		return nil
	}

	return &ethCallBatchInfo{
		networkId:  networkId,
		blockRef:   blockRef,
		blockParam: blockParam,
	}
}

func (s *HttpServer) forwardEthCallBatchCandidates(
	startedAt *time.Time,
	project *PreparedProject,
	network *Network,
	candidates []ethCallBatchCandidate,
	responses []interface{},
) {
	if project == nil || network == nil {
		err := common.NewErrInvalidRequest(fmt.Errorf("network not available for batch eth_call fallback"))
		for _, cand := range candidates {
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, err)
		}
		return
	}

	for _, cand := range candidates {
		resp, err := forwardBatchProject(withSkipNetworkRateLimit(cand.ctx), project, network, cand.req)
		if err != nil {
			if resp != nil {
				go resp.Release()
			}
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, err)
			continue
		}

		responses[cand.index] = resp
		common.EndRequestSpan(cand.ctx, resp, nil)
	}
}

func (s *HttpServer) handleEthCallBatchAggregation(
	httpCtx context.Context,
	startedAt *time.Time,
	r *http.Request,
	project *PreparedProject,
	baseLogger zerolog.Logger,
	batchInfo *ethCallBatchInfo,
	requests []json.RawMessage,
	headers http.Header,
	queryArgs map[string][]string,
	responses []interface{},
) bool {
	if batchInfo == nil || project == nil {
		return false
	}

	network, networkErr := project.GetNetwork(httpCtx, batchInfo.networkId)
	uaMode := common.UserAgentTrackingModeSimplified
	if project.Config != nil && project.Config.UserAgentMode != "" {
		uaMode = project.Config.UserAgentMode
	}

	candidates := make([]ethCallBatchCandidate, 0, len(requests))
	for i, rawReq := range requests {
		nq := common.NewNormalizedRequest(rawReq)
		rawReq = nil
		requestCtx := common.StartRequestSpan(httpCtx, nq)

		clientIP := s.resolveRealClientIP(r)
		nq.SetClientIP(clientIP)

		if err := nq.Validate(); err != nil {
			responses[i] = processErrorBody(&baseLogger, startedAt, nq, err, &common.TRUE)
			common.EndRequestSpan(requestCtx, nil, responses[i])
			continue
		}

		method, _ := nq.Method()
		rlg := baseLogger.With().Str("method", method).Logger()

		ap, err := auth.NewPayloadFromHttp(method, r.RemoteAddr, headers, queryArgs)
		if err != nil {
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, &common.TRUE)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}

		user, err := project.AuthenticateConsumer(requestCtx, nq, method, ap)
		if err != nil {
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}
		if user != nil {
			rlg = rlg.With().Str("userId", user.Id).Logger()
		}
		nq.SetUser(user)

		if networkErr != nil || network == nil {
			err := networkErr
			if err == nil {
				err = common.NewErrNetworkNotFound(batchInfo.networkId)
			}
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}

		nq.SetNetwork(network)
		nq.ApplyDirectiveDefaults(network.Config().DirectiveDefaults)
		nq.EnrichFromHttp(headers, queryArgs, uaMode)
		rlg.Trace().Interface("directives", nq.Directives()).Msgf("applied request directives")

		if err := project.acquireRateLimitPermit(requestCtx, nq); err != nil {
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}
		if err := network.acquireRateLimitPermit(requestCtx, nq); err != nil {
			responses[i] = processErrorBody(&rlg, startedAt, nq, err, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(requestCtx, nil, err)
			continue
		}

		candidates = append(candidates, ethCallBatchCandidate{
			index:  i,
			ctx:    requestCtx,
			req:    nq,
			logger: rlg,
		})
	}

	if len(candidates) == 0 {
		return true
	}
	if len(candidates) < 2 {
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	reqs := make([]*common.NormalizedRequest, len(candidates))
	for i, cand := range candidates {
		reqs[i] = cand.req
	}

	mcReq, calls, err := evm.BuildMulticall3Request(reqs, batchInfo.blockParam)
	if err != nil {
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	mcCtx := withSkipNetworkRateLimit(httpCtx)
	mcResp, mcErr := forwardBatchNetwork(mcCtx, network, mcReq)
	if mcErr != nil {
		if mcResp != nil {
			mcResp.Release()
		}
		if evm.ShouldFallbackMulticall3(mcErr) {
			s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
			return true
		}
		for _, cand := range candidates {
			responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, mcErr, s.serverCfg.IncludeErrorDetails)
			common.EndRequestSpan(cand.ctx, nil, mcErr)
		}
		return true
	}
	if mcResp == nil {
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	jrr, err := mcResp.JsonRpcResponse(mcCtx)
	if err != nil || jrr == nil || jrr.Error != nil {
		mcResp.Release()
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	var resultHex string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &resultHex); err != nil {
		mcResp.Release()
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}
	resultBytes, err := common.HexToBytes(resultHex)
	if err != nil {
		mcResp.Release()
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	decoded, err := evm.DecodeMulticall3Aggregate3Result(resultBytes)
	if err != nil || len(decoded) != len(calls) {
		mcResp.Release()
		s.forwardEthCallBatchCandidates(startedAt, project, network, candidates, responses)
		return true
	}

	for i, result := range decoded {
		cand := candidates[i]
		if result.Success {
			returnHex := "0x" + hex.EncodeToString(result.ReturnData)
			jrr, err := newBatchJsonRpcResponse(cand.req.ID(), returnHex, nil)
			if err != nil {
				responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, err, s.serverCfg.IncludeErrorDetails)
				common.EndRequestSpan(cand.ctx, nil, err)
				continue
			}

			nr := common.NewNormalizedResponse().WithRequest(cand.req).WithJsonRpcResponse(jrr)
			nr.SetUpstream(mcResp.Upstream())
			nr.SetFromCache(mcResp.FromCache())
			nr.SetAttempts(mcResp.Attempts())
			nr.SetRetries(mcResp.Retries())
			nr.SetHedges(mcResp.Hedges())
			nr.SetEvmBlockRef(mcResp.EvmBlockRef())
			nr.SetEvmBlockNumber(mcResp.EvmBlockNumber())
			responses[cand.index] = nr
			common.EndRequestSpan(cand.ctx, nr, nil)
			continue
		}

		dataHex := "0x" + hex.EncodeToString(result.ReturnData)
		callErr := common.NewErrEndpointExecutionException(
			common.NewErrJsonRpcExceptionInternal(
				0,
				common.JsonRpcErrorEvmReverted,
				"execution reverted",
				nil,
				map[string]interface{}{
					"data": dataHex,
				},
			),
		)
		responses[cand.index] = processErrorBody(&cand.logger, startedAt, cand.req, callErr, s.serverCfg.IncludeErrorDetails)
		common.EndRequestSpan(cand.ctx, nil, callErr)
	}

	mcResp.Release()
	return true
}
