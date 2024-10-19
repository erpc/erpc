package upstream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type HttpJsonRpcClient interface {
	GetType() ClientType
	SupportsNetwork(networkId string) (bool, error)
	SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

type GenericHttpJsonRpcClient struct {
	Url *url.URL

	logger     *zerolog.Logger
	upstream   *Upstream
	httpClient *http.Client

	supportsBatch bool
	batchMaxSize  int
	batchMaxWait  time.Duration

	batchMu       sync.Mutex
	batchRequests map[interface{}]*batchRequest
	batchDeadline *time.Time
	batchTimer    *time.Timer
}

type batchRequest struct {
	ctx      context.Context
	request  *common.NormalizedRequest
	response chan *common.NormalizedResponse
	err      chan error
}

func NewGenericHttpJsonRpcClient(logger *zerolog.Logger, pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	client := &GenericHttpJsonRpcClient{
		Url:      parsedUrl,
		logger:   logger,
		upstream: pu,
	}

	if pu.config.JsonRpc != nil {
		jc := pu.config.JsonRpc
		if jc.SupportsBatch != nil && *jc.SupportsBatch {
			client.supportsBatch = true

			if jc.BatchMaxSize > 0 {
				client.batchMaxSize = jc.BatchMaxSize
			} else {
				client.batchMaxSize = 10
			}
			if jc.BatchMaxWait != "" {
				duration, err := time.ParseDuration(jc.BatchMaxWait)
				if err != nil {
					return nil, err
				}
				client.batchMaxWait = duration
			} else {
				client.batchMaxWait = 50 * time.Millisecond
			}

			client.batchRequests = make(map[interface{}]*batchRequest)
		}
	}

	if util.IsTest() {
		client.httpClient = &http.Client{}
	} else {
		client.httpClient = &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        1024,
				MaxIdleConnsPerHost: 256,
				IdleConnTimeout:     90 * time.Second,
			},
		}
	}

	return client, nil
}

func (c *GenericHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeHttpJsonRpc
}

func (c *GenericHttpJsonRpcClient) SupportsNetwork(networkId string) (bool, error) {
	cfg := c.upstream.Config()
	if cfg.Evm != nil && cfg.Evm.ChainId > 0 {
		return util.EvmNetworkId(cfg.Evm.ChainId) == networkId, nil
	}
	return false, nil
}

func (c *GenericHttpJsonRpcClient) SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	if !c.supportsBatch {
		return c.sendSingleRequest(ctx, req)
	}

	responseChan := make(chan *common.NormalizedResponse, 1)
	errChan := make(chan error, 1)

	startedAt := time.Now()
	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(
			err,
			c.upstream.Config().Id,
			req.NetworkId(),
			jrReq.Method,
			0, 0, 0, 0,
		)
	}

	bReq := &batchRequest{
		ctx:      ctx,
		request:  req,
		response: responseChan,
		err:      errChan,
	}

	c.queueRequest(jrReq.ID, bReq)

	select {
	case response := <-responseChan:
		return response, nil
	case err := <-errChan:
		return nil, err
	case <-ctx.Done():
		err := ctx.Err()
		if errors.Is(err, context.DeadlineExceeded) {
			err = common.NewErrEndpointRequestTimeout(time.Since(startedAt))
		}
		return nil, err
	}
}

func (c *GenericHttpJsonRpcClient) queueRequest(id interface{}, req *batchRequest) {
	c.batchMu.Lock()

	if _, ok := c.batchRequests[id]; ok {
		// We must not include multiple requests with same ID in batch requests
		// to avoid issues when mapping responses.
		c.batchTimer.Stop()
		c.batchMu.Unlock()
		c.processBatch()
		c.queueRequest(id, req)
		return
	}

	c.batchRequests[id] = req
	ctxd, ok := req.ctx.Deadline()
	if ctxd.After(time.Now()) && ok {
		if c.batchDeadline == nil || ctxd.After(*c.batchDeadline) {
			c.batchDeadline = &ctxd
		}
	}
	c.logger.Debug().Msgf("queuing request %+v for batch (current batch size: %d)", id, len(c.batchRequests))

	if c.logger.GetLevel() == zerolog.TraceLevel {
		for _, req := range c.batchRequests {
			jrr, _ := req.request.JsonRpcRequest()
			jrr.Lock()
			rqs, _ := common.SonicCfg.Marshal(jrr)
			jrr.Unlock()
			c.logger.Trace().Interface("id", req.request.Id()).Str("method", jrr.Method).Msgf("pending batch request: %s", string(rqs))
		}
	}

	if len(c.batchRequests) == 1 {
		c.logger.Trace().Interface("id", id).Msgf("starting batch timer")
		c.batchTimer = time.AfterFunc(c.batchMaxWait, c.processBatch)
		c.batchMu.Unlock()
	} else if len(c.batchRequests) >= c.batchMaxSize {
		c.logger.Trace().Interface("id", id).Msgf("stopping batch timer to process total of %d requests", len(c.batchRequests))
		c.batchTimer.Stop()
		c.batchMu.Unlock()
		c.processBatch()
	} else {
		c.logger.Trace().Interface("id", id).Msgf("continue waiting for batch")
		c.batchMu.Unlock()
	}
}

func (c *GenericHttpJsonRpcClient) processBatch() {
	var batchCtx context.Context
	var cancelCtx context.CancelFunc

	c.batchMu.Lock()
	c.logger.Debug().Msgf("processing batch with %d requests", len(c.batchRequests))
	requests := c.batchRequests
	if c.batchDeadline != nil {
		batchCtx, cancelCtx = context.WithDeadline(context.Background(), *c.batchDeadline)
		defer cancelCtx()
	} else {
		batchCtx = context.Background()
	}
	c.batchRequests = make(map[interface{}]*batchRequest)
	c.batchDeadline = nil
	c.batchMu.Unlock()

	ln := len(requests)
	if ln == 0 {
		return
	}

	batchReq := make([]common.JsonRpcRequest, 0, ln)
	for _, req := range requests {
		jrReq, err := req.request.JsonRpcRequest()
		c.logger.Trace().Interface("id", req.request.Id()).Str("method", jrReq.Method).Msgf("preparing batch request")
		if err != nil {
			req.err <- common.NewErrUpstreamRequest(
				err,
				c.upstream.Config().Id,
				req.request.NetworkId(),
				jrReq.Method,
				0, 0, 0, 0,
			)
			continue
		}
		req.request.RLock()
		jrReq.RLock()
		batchReq = append(batchReq, common.JsonRpcRequest{
			JSONRPC: jrReq.JSONRPC,
			Method:  jrReq.Method,
			Params:  jrReq.Params,
			ID:      jrReq.ID,
		})
	}

	requestBody, err := common.SonicCfg.Marshal(batchReq)
	for _, req := range requests {
		req.request.RUnlock()
		jrReq, _ := req.request.JsonRpcRequest()
		if jrReq != nil {
			jrReq.RUnlock()
		}
	}
	if err != nil {
		for _, req := range requests {
			req.err <- err
		}
		return
	}

	c.logger.Debug().Msgf("sending batch json rpc POST request to %s: %s", c.Url.Host, requestBody)

	reqStartTime := time.Now()
	httpReq, err := http.NewRequestWithContext(batchCtx, "POST", c.Url.String(), bytes.NewReader(requestBody))
	if err != nil {
		for _, req := range requests {
			req.err <- &common.BaseError{
				Code:    "ErrHttp",
				Message: fmt.Sprintf("%v", err),
				Details: map[string]interface{}{
					"url":        c.Url.String(),
					"upstreamId": c.upstream.Config().Id,
					"request":    requestBody,
				},
			}
		}
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", fmt.Sprintf("erpc (%s/%s; Project/%s; Budget/%s)", common.ErpcVersion, common.ErpcCommitSha, c.upstream.ProjectId, c.upstream.config.RateLimitBudget))

	// Make the HTTP request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			for _, req := range requests {
				req.err <- common.NewErrEndpointRequestTimeout(time.Since(reqStartTime))
			}
		} else {
			for _, req := range requests {
				req.err <- common.NewErrEndpointTransportFailure(err)
			}
		}
		return
	}

	c.processBatchResponse(requests, resp)
}

func (c *GenericHttpJsonRpcClient) processBatchResponse(requests map[interface{}]*batchRequest, resp *http.Response) {
	defer resp.Body.Close()

	bodyBytes, err := util.ReadAll(resp.Body, 128*1024, int(resp.ContentLength)) // 128KB
	if err != nil {
		for _, req := range requests {
			req.err <- err
		}
		return
	}

	searcher := ast.NewSearcher(util.Mem2Str(bodyBytes))
	searcher.CopyReturn = false
	searcher.ConcurrentRead = false
	searcher.ValidateJSON = false

	rootNode, err := searcher.GetByPath()
	if err != nil {
		jrResp := &common.JsonRpcResponse{}
		err = jrResp.ParseError(util.Mem2Str(bodyBytes))
		if err != nil {
			for _, req := range requests {
				req.err <- err
			}
			return
		}
		for _, req := range requests {
			jrr, err := jrResp.Clone()
			if err != nil {
				req.err <- err
			} else {
				nr := common.NewNormalizedResponse().
					WithRequest(req.request).
					WithJsonRpcResponse(jrr)
				err = c.normalizeJsonRpcError(resp, nr)
				req.err <- err
			}
		}
		return
	}

	if rootNode.TypeSafe() == ast.V_ARRAY {
		arrNodes, err := rootNode.ArrayUseNode()
		if err != nil {
			for _, req := range requests {
				req.err <- err
			}
			return
		}
		for _, elemNode := range arrNodes {
			var id interface{}
			jrResp, err := getJsonRpcResponseFromNode(elemNode)
			if jrResp != nil {
				id = jrResp.ID()
			}
			if id == nil {
				c.logger.Warn().Msgf("unexpected response received without ID: %s", util.Mem2Str(bodyBytes))
			} else if req, ok := requests[id]; ok {
				nr := common.NewNormalizedResponse().WithRequest(req.request).WithJsonRpcResponse(jrResp)
				if err != nil {
					req.err <- err
				} else {
					err := c.normalizeJsonRpcError(resp, nr)
					if err != nil {
						req.err <- err
					} else {
						req.response <- nr
					}
				}
				delete(requests, id)
			} else {
				c.logger.Warn().Msgf("unexpected response received with ID: %s", id)
			}
		}
		// Handle any remaining requests that didn't receive a response
		anyMissingId := false
		for _, req := range requests {
			req.err <- fmt.Errorf("no response received for request ID: %d", req.request.Id())
			anyMissingId = true
		}
		if anyMissingId {
			c.logger.Error().Str("response", util.Mem2Str(bodyBytes)).Msgf("some requests did not receive a response (matching ID)")
		}
	} else if rootNode.TypeSafe() == ast.V_OBJECT {
		// Single object response
		jrResp, err := getJsonRpcResponseFromNode(rootNode)
		if err != nil {
			for _, req := range requests {
				req.err <- err
			}
			return
		}
		for _, req := range requests {
			nr := common.NewNormalizedResponse().WithRequest(req.request).WithJsonRpcResponse(jrResp)
			err := c.normalizeJsonRpcError(resp, nr)
			if err != nil {
				req.err <- err
			} else {
				req.response <- nr
			}
		}
	} else {
		// Unexpected response type
		for _, req := range requests {
			req.err <- common.NewErrUpstreamMalformedResponse(fmt.Errorf("unexpected response type (not array nor object): %s", util.Mem2Str(bodyBytes)), c.upstream.Config().Id)
		}
	}
}

func getJsonRpcResponseFromNode(rootNode ast.Node) (*common.JsonRpcResponse, error) {
	idNode := rootNode.GetByPath("id")
	rawID, _ := idNode.Raw()
	resultNode := rootNode.GetByPath("result")
	rawResult, rawResultErr := resultNode.Raw()
	errorNode := rootNode.GetByPath("error")
	rawError, rawErrorErr := errorNode.Raw()

	if rawResultErr != nil && rawErrorErr != nil {
		var jrResp *common.JsonRpcResponse

		if rawID != "" {
			err := jrResp.SetIDBytes(util.Str2Mem(rawID))
			if err != nil {
				return nil, err
			}
		}

		cause := fmt.Sprintf("cannot parse json rpc response from upstream, for result: %s, for error: %s", rawResult, rawError)
		jrResp = &common.JsonRpcResponse{
			Result: util.Str2Mem(rawResult),
			Error: common.NewErrJsonRpcExceptionExternal(
				int(common.JsonRpcErrorParseException),
				cause,
				"",
			),
		}

		return jrResp, nil
	}

	return common.NewJsonRpcResponseFromBytes(
		util.Str2Mem(rawID),
		util.Str2Mem(rawResult),
		util.Str2Mem(rawError),
	)
}

func (c *GenericHttpJsonRpcClient) sendSingleRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(
			err,
			c.upstream.Config().Id,
			req.NetworkId(),
			jrReq.Method,
			0,
			0,
			0,
			0,
		)
	}

	jrReq.RLock()
	requestBody, err := common.SonicCfg.Marshal(common.JsonRpcRequest{
		JSONRPC: jrReq.JSONRPC,
		Method:  jrReq.Method,
		Params:  jrReq.Params,
		ID:      jrReq.ID,
	})
	jrReq.RUnlock()

	if err != nil {
		return nil, err
	}

	c.logger.Debug().Msgf("sending json rpc POST request to %s: %s", c.Url.Host, requestBody)

	reqStartTime := time.Now()
	httpReq, errReq := http.NewRequestWithContext(ctx, "POST", c.Url.String(), bytes.NewBuffer(requestBody))
	httpReq.Header.Set("Content-Type", "application/json")
	if errReq != nil {
		return nil, &common.BaseError{
			Code:    "ErrHttp",
			Message: fmt.Sprintf("%v", errReq),
			Details: map[string]interface{}{
				"url":        c.Url.String(),
				"upstreamId": c.upstream.Config().Id,
				"request":    requestBody,
			},
		}
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrEndpointRequestTimeout(time.Since(reqStartTime))
		}
		return nil, common.NewErrEndpointTransportFailure(err)
	}

	nr := common.NewNormalizedResponse().
		WithRequest(req).
		WithBody(resp.Body).
		WithExpectedSize(int(resp.ContentLength))

	return nr, c.normalizeJsonRpcError(resp, nr)
}

func (c *GenericHttpJsonRpcClient) normalizeJsonRpcError(r *http.Response, nr *common.NormalizedResponse) error {
	jr, err := nr.JsonRpcResponse()

	if err != nil {
		e := common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorParseException,
			"could not parse json rpc response from upstream",
			err,
			map[string]interface{}{
				"upstreamId": c.upstream.Config().Id,
				"statusCode": r.StatusCode,
				"headers":    r.Header,
			},
		)
		return e
	}

	if jr.Error == nil {
		return nil
	}

	if e := extractJsonRpcError(r, nr, jr); e != nil {
		return e
	}

	e := common.NewErrJsonRpcExceptionInternal(
		0,
		common.JsonRpcErrorServerSideException,
		"unknown json-rpc response",
		nil,
		map[string]interface{}{
			"upstreamId": c.upstream.Config().Id,
			"statusCode": r.StatusCode,
			"headers":    r.Header,
		},
	)

	return e
}

func extractJsonRpcError(r *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse) error {
	if jr != nil && jr.Error != nil {
		err := jr.Error

		var details map[string]interface{} = make(map[string]interface{})
		details["statusCode"] = r.StatusCode
		details["headers"] = util.ExtractUsefulHeaders(r)

		if ver := getVendorSpecificErrorIfAny(r, nr, jr, details); ver != nil {
			return ver
		}

		code := common.JsonRpcErrorNumber(err.Code)

		switch err.Data.(type) {
		case string:
			s := err.Data.(string)
			if s != "" {
				// Some providers such as Alchemy prefix the data with this string
				// we omit this prefix for standardization.
				if strings.HasPrefix(s, "Reverted ") {
					details["data"] = s[9:]
				} else {
					details["data"] = s
				}
			}
		default:
			// passthrough error data as is
			details["data"] = err.Data
		}

		// Infer from known status codes
		if r.StatusCode == 415 || code == common.JsonRpcErrorUnsupportedException {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
					err.Message,
					nil,
					details,
				),
			)
		} else if r.StatusCode == 429 ||
			strings.Contains(err.Message, "requests limited to") ||
			strings.Contains(err.Message, "has exceeded") ||
			strings.Contains(err.Message, "Exceeded the quota") ||
			strings.Contains(err.Message, "Too many requests") ||
			strings.Contains(err.Message, "Too Many Requests") ||
			strings.Contains(err.Message, "under too much load") {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "block range") ||
			strings.Contains(err.Message, "exceeds the range") ||
			strings.Contains(err.Message, "Max range") ||
			strings.Contains(err.Message, "limited to") ||
			strings.Contains(err.Message, "response size should not") ||
			strings.Contains(err.Message, "returned more than") ||
			strings.Contains(err.Message, "exceeds max results") ||
			strings.Contains(err.Message, "response too large") ||
			strings.Contains(err.Message, "query exceeds limit") ||
			strings.Contains(err.Message, "exceeds the range") ||
			strings.Contains(err.Message, "range limit exceeded") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
				common.EvmBlockRangeTooLarge,
			)
		} else if strings.Contains(err.Message, "specify less number of address") {
			return common.NewErrEndpointRequestTooLarge(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
				common.EvmAddressesTooLarge,
			)
		} else if strings.Contains(err.Message, "reached the free tier") ||
			strings.Contains(err.Message, "Monthly capacity limit") {
			return common.NewErrEndpointBillingIssue(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "missing trie node") ||
			strings.Contains(err.Message, "header not found") ||
			strings.Contains(err.Message, "unknown block") ||
			strings.Contains(err.Message, "Unknown block") ||
			strings.Contains(err.Message, "height must be less than or equal") ||
			strings.Contains(err.Message, "invalid blockhash finalized") ||
			strings.Contains(err.Message, "Expect block number from id") ||
			strings.Contains(err.Message, "block not found") ||
			strings.Contains(err.Message, "block height passed is invalid") ||
			// Usually happens on Avalanche when querying a pretty recent block:
			strings.Contains(err.Message, "cannot query unfinalized") ||
			strings.Contains(err.Message, "height is not available") ||
			// This usually happens when sending a trace_* request to a newly created block:
			strings.Contains(err.Message, "genesis is not traceable") ||
			strings.Contains(err.Message, "could not find FinalizeBlock") ||
			strings.Contains(err.Message, "no historical rpc") ||
			(strings.Contains(err.Message, "blocks specified") && strings.Contains(err.Message, "cannot be found")) ||
			strings.Contains(err.Message, "transaction not found") ||
			strings.Contains(err.Message, "cannot find transaction") ||
			strings.Contains(err.Message, "after last accepted block") ||
			strings.Contains(err.Message, "is greater than latest") ||
			strings.Contains(err.Message, "No state available") {
			return common.NewErrEndpointMissingData(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorMissingData,
					err.Message,
					nil,
					details,
				),
			)
		} else if code == -32004 || code == -32001 {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
					err.Message,
					nil,
					details,
				),
			)
		} else if code == -32602 ||
			strings.Contains(err.Message, "param is required") ||
			strings.Contains(err.Message, "Invalid Request") ||
			strings.Contains(err.Message, "validation errors") ||
			strings.Contains(err.Message, "invalid argument") {
			if dt, ok := err.Data.(map[string]interface{}); ok {
				if msg, ok := dt["message"]; ok {
					if strings.Contains(msg.(string), "validation errors in batch") {
						// Intentionally return a server-side error for failed requests in a batch
						// so they are retried in a different batch.
						// TODO Should we split a batch instead on json-rpc client level?
						return common.NewErrEndpointServerSideException(
							common.NewErrJsonRpcExceptionInternal(
								int(code),
								common.JsonRpcErrorServerSideException,
								err.Message,
								nil,
								details,
							),
							nil,
						)
					}
				}
			}
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "reverted") ||
			strings.Contains(err.Message, "VM execution error") ||
			strings.Contains(err.Message, "transaction: revert") ||
			strings.Contains(err.Message, "VM Exception") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmReverted,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "insufficient funds") ||
			strings.Contains(err.Message, "insufficient balance") ||
			strings.Contains(err.Message, "out of gas") ||
			strings.Contains(err.Message, "gas too low") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCallException,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "not found") ||
			strings.Contains(err.Message, "does not exist") ||
			strings.Contains(err.Message, "is not available") ||
			strings.Contains(err.Message, "is disabled") {
			if strings.Contains(err.Message, "Method") ||
				strings.Contains(err.Message, "method") ||
				strings.Contains(err.Message, "Module") ||
				strings.Contains(err.Message, "module") {
				return common.NewErrEndpointUnsupported(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorUnsupportedException,
						err.Message,
						nil,
						details,
					),
				)
			} else if strings.Contains(err.Message, "header") ||
				strings.Contains(err.Message, "block") ||
				strings.Contains(err.Message, "Header") ||
				strings.Contains(err.Message, "Block") {
				return common.NewErrEndpointMissingData(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorMissingData,
						err.Message,
						nil,
						details,
					),
				)
			} else {
				return common.NewErrEndpointClientSideException(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorClientSideException,
						err.Message,
						nil,
						details,
					),
				)
			}
		} else if strings.Contains(err.Message, "Unsupported method") ||
			strings.Contains(err.Message, "not supported") ||
			strings.Contains(err.Message, "method is not whitelisted") ||
			strings.Contains(err.Message, "is not included in your current plan") {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
					err.Message,
					nil,
					details,
				),
			)
		} else if code == -32600 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			)
		} else if r.StatusCode == 401 || r.StatusCode == 403 || strings.Contains(err.Message, "not allowed to access") {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnauthorized,
					err.Message,
					nil,
					details,
				),
			)
		}

		// By default we consider a problem on the server so that retry/failover mechanisms try other upstreams
		return common.NewErrEndpointServerSideException(
			common.NewErrJsonRpcExceptionInternal(
				int(code),
				common.JsonRpcErrorServerSideException,
				err.Message,
				nil,
				details,
			),
			nil,
		)
	}

	// There's a special case for certain clients that return a normal response for reverts:
	if jr != nil {
		dt := util.Mem2Str(jr.Result)
		// keccak256("Error(string)")
		if strings.HasPrefix(dt, "0x08c379a0") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					0,
					common.JsonRpcErrorEvmReverted,
					"transaction reverted",
					nil,
					map[string]interface{}{
						"data": dt,
					},
				),
			)
		}
	}

	return nil
}

func getVendorSpecificErrorIfAny(
	rp *http.Response,
	nr *common.NormalizedResponse,
	jr *common.JsonRpcResponse,
	details map[string]interface{},
) error {
	req := nr.Request()
	if req == nil {
		return nil
	}

	ups := req.LastUpstream()
	if ups == nil {
		return nil
	}

	vn := ups.Vendor()
	if vn == nil {
		return nil
	}

	return vn.GetVendorSpecificErrorIfAny(rp, jr, details)
}
