package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	SupportsNetwork(ctx context.Context, networkId string) (bool, error)
	SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

type GenericHttpJsonRpcClient struct {
	Url *url.URL

	appCtx     context.Context
	logger     *zerolog.Logger
	upstream   *Upstream
	httpClient *http.Client

	enableGzip    bool
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

func NewGenericHttpJsonRpcClient(appCtx context.Context, logger *zerolog.Logger, pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	client := &GenericHttpJsonRpcClient{
		Url: parsedUrl,

		appCtx:   appCtx,
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
		if jc.EnableGzip != nil {
			client.enableGzip = *jc.EnableGzip
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

	go func() {
		<-appCtx.Done()
		client.shutdown()
	}()

	return client, nil
}

func (c *GenericHttpJsonRpcClient) GetType() ClientType {
	return ClientTypeHttpJsonRpc
}

func (c *GenericHttpJsonRpcClient) SupportsNetwork(ctx context.Context, networkId string) (bool, error) {
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
			err = common.NewErrEndpointRequestTimeout(time.Since(startedAt), err)
		}
		return nil, err
	}
}

func (c *GenericHttpJsonRpcClient) shutdown() {
	c.batchMu.Lock()
	if c.batchTimer != nil {
		c.batchTimer.Stop()
	}
	c.processBatch(true)
}

func (c *GenericHttpJsonRpcClient) queueRequest(id interface{}, req *batchRequest) {
	c.logger.Trace().Interface("id", id).Object("request", req.request).Msgf("attempt to queue request for batch")
	c.batchMu.Lock()

	if _, ok := c.batchRequests[id]; ok {
		// We must not include multiple requests with same ID in batch requests
		// to avoid issues when mapping responses.
		c.batchTimer.Stop()
		c.processBatch(true)
		c.queueRequest(id, req)
		return
	}

	c.batchRequests[id] = req
	ctxd, ok := req.ctx.Deadline()
	if ctxd.After(time.Now()) && ok {
		if c.batchDeadline == nil || ctxd.After(*c.batchDeadline) {
			duration := time.Until(ctxd)
			c.logger.Trace().Dur("duration", duration).Msgf("extending current batch deadline")
			c.batchDeadline = &ctxd
		}
	}

	if c.logger.GetLevel() == zerolog.TraceLevel {
		ids := make([]interface{}, 0, len(c.batchRequests))
		for _, req := range c.batchRequests {
			ids = append(ids, req.request.ID())
			jrr, _ := req.request.JsonRpcRequest()
			jrr.Lock()
			rqs, _ := common.SonicCfg.Marshal(jrr)
			jrr.Unlock()
			c.logger.Trace().Interface("id", req.request.ID()).Str("method", jrr.Method).Msgf("request in batch: %s", string(rqs))
		}
		c.logger.Trace().Interface("ids", ids).Msgf("current batch requests")
	} else {
		c.logger.Debug().Msgf("queuing request %+v for batch (current batch size: %d)", id, len(c.batchRequests))
	}

	if len(c.batchRequests) == 1 {
		c.logger.Trace().Interface("id", id).Msgf("starting batch timer")
		c.batchTimer = time.AfterFunc(c.batchMaxWait, func() { c.processBatch(false) })
	}

	if len(c.batchRequests) >= c.batchMaxSize {
		c.logger.Trace().Interface("id", id).Msgf("committing batch to process total of %d requests", len(c.batchRequests))
		c.batchTimer.Stop()
		c.processBatch(true)
	} else {
		c.logger.Trace().Interface("id", id).Msgf("continue waiting for batch")
		c.batchMu.Unlock()
	}
}

func (c *GenericHttpJsonRpcClient) processBatch(alreadyLocked bool) {
	if c.appCtx != nil {
		err := c.appCtx.Err()
		if err != nil {
			var msg string
			if err == context.Canceled {
				msg = "shutting down http client batch processing (ignoring batch requests if any)"
				err = nil
			} else {
				msg = "context error on batch processing (ignoring batch requests if any)"
			}
			if c.logger.GetLevel() == zerolog.TraceLevel {
				if !alreadyLocked {
					c.batchMu.Lock()
					alreadyLocked = false
				}
				ids := make([]interface{}, 0, len(c.batchRequests))
				for _, req := range c.batchRequests {
					ids = append(ids, req.request.ID())
				}
				c.batchMu.Unlock()
				c.logger.Trace().Err(err).Interface("remainingIds", ids).Msg(msg)
			} else {
				c.logger.Debug().Err(err).Msg(msg)
			}
			return
		}
	}
	var batchCtx context.Context
	var cancelCtx context.CancelFunc

	if !alreadyLocked {
		c.batchMu.Lock()
	}
	if c.logger.GetLevel() == zerolog.TraceLevel {
		ids := make([]interface{}, 0, len(c.batchRequests))
		for id := range c.batchRequests {
			ids = append(ids, id)
		}
		c.logger.Debug().Interface("ids", ids).Msgf("processing batch with %d requests", len(c.batchRequests))
	} else {
		c.logger.Debug().Msgf("processing batch with %d requests", len(c.batchRequests))
	}
	requests := c.batchRequests
	if c.batchDeadline != nil {
		duration := time.Until(*c.batchDeadline)
		c.logger.Trace().
			Dur("deadline", duration).
			Msg("creating batch context with highest deadline")
		batchCtx, cancelCtx = context.WithDeadline(c.appCtx, *c.batchDeadline)
		defer cancelCtx()
	} else {
		batchCtx = c.appCtx
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
		c.logger.Trace().Interface("id", req.request.ID()).Str("method", jrReq.Method).Msgf("preparing batch request")
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

	reqStartTime := time.Now()
	httpReq, err := c.prepareRequest(batchCtx, requestBody)
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

	c.logger.Debug().Str("host", c.Url.Host).RawJSON("request", requestBody).Interface("headers", httpReq.Header).Msgf("sending json rpc POST request (batch)")

	// Make the HTTP request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		cause := context.Cause(batchCtx)
		if cause == nil {
			cause = batchCtx.Err()
		}
		if cause != nil {
			err = cause
		}
		if errors.Is(err, context.DeadlineExceeded) {
			for _, req := range requests {
				req.err <- common.NewErrEndpointRequestTimeout(time.Since(reqStartTime), err)
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
	bodyBytes, err := readResponseBody(resp, int(resp.ContentLength))
	if err != nil {
		for _, req := range requests {
			req.err <- err
		}
		return
	}

	bodyStr := util.Mem2Str(bodyBytes)
	searcher := ast.NewSearcher(bodyStr)
	searcher.CopyReturn = false
	searcher.ConcurrentRead = false
	searcher.ValidateJSON = false

	if c.logger.GetLevel() == zerolog.TraceLevel {
		if len(bodyBytes) > 20*1024 {
			c.logger.Trace().Str("head", util.Mem2Str(bodyBytes[:20*1024])).Str("tail", util.Mem2Str(bodyBytes[len(bodyBytes)-20*1024:])).Msgf("processing batch response from upstream (trimmed to first and last 20k)")
		} else {
			c.logger.Trace().RawJSON("response", bodyBytes).Msgf("processing batch response from upstream")
		}
	}

	rootNode, err := searcher.GetByPath()
	if err != nil {
		jrResp := &common.JsonRpcResponse{}
		err = jrResp.ParseError(bodyStr)
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
				c.logger.Warn().Msgf("unexpected response received without ID: %s", bodyStr)
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
			req.err <- fmt.Errorf("no response received for request ID: %d", req.request.ID())
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
			req.err <- common.NewErrUpstreamMalformedResponse(fmt.Errorf("unexpected response type (not array nor object): %s", bodyStr), c.upstream.Config().Id)
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
	// TODO check if context is cancellable and then get the "cause" that is already set
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

	reqStartTime := time.Now()
	httpReq, err := c.prepareRequest(ctx, requestBody)
	if err != nil {
		return nil, &common.BaseError{
			Code:    "ErrHttp",
			Message: fmt.Sprintf("%v", err),
			Details: map[string]interface{}{
				"url":        c.Url.String(),
				"upstreamId": c.upstream.Config().Id,
				"request":    requestBody,
			},
		}
	}
	c.logger.Debug().Str("host", c.Url.Host).RawJSON("request", requestBody).Interface("headers", httpReq.Header).Msg("sending json rpc POST request (single)")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		if cause != nil {
			err = cause
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrEndpointRequestTimeout(time.Since(reqStartTime), err)
		}
		return nil, common.NewErrEndpointTransportFailure(err)
	}
	defer resp.Body.Close()

	var bodyReader io.ReadCloser = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, common.NewErrEndpointTransportFailure(fmt.Errorf("cannot create gzip reader: %w", err))
		}
		defer gzReader.Close()
		bodyReader = gzReader
	}

	nr := common.NewNormalizedResponse().
		WithRequest(req).
		WithBody(bodyReader).
		WithExpectedSize(int(resp.ContentLength))

	return nr, c.normalizeJsonRpcError(resp, nr)
}

func (c *GenericHttpJsonRpcClient) prepareRequest(ctx context.Context, body []byte) (*http.Request, error) {
	var bodyReader io.Reader = bytes.NewReader(body)

	// Check if gzip compression is enabled
	if c.enableGzip {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(body); err != nil {
			return nil, err
		}
		if err := gw.Close(); err != nil {
			return nil, err
		}
		if c.logger.GetLevel() <= zerolog.TraceLevel {
			compressedSize := buf.Len()
			originalSize := len(body)
			c.logger.Debug().
				Int("originalSize", originalSize).
				Int("compressedSize", compressedSize).
				Float64("compressionRatio", float64(compressedSize)/float64(originalSize)).
				Msg("compressed request body")
		}
		bodyReader = &buf
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.Url.String(), bodyReader)
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Accept-Encoding", "gzip")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", fmt.Sprintf("erpc (%s/%s; Project/%s; Budget/%s)",
		common.ErpcVersion,
		common.ErpcCommitSha,
		c.upstream.ProjectId,
		c.upstream.config.RateLimitBudget))

	// Add gzip header if compression is enabled
	if c.enableGzip {
		httpReq.Header.Set("Content-Encoding", "gzip")
	}

	return httpReq, nil
}

func readResponseBody(resp *http.Response, expectedSize int) ([]byte, error) {
	var reader io.ReadCloser = resp.Body
	defer resp.Body.Close()

	// Check if response is gzipped
	if resp.Header.Get("Content-Encoding") == "gzip" {
		var err error
		reader, err = gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("error creating gzip reader: %w", err)
		}
		defer reader.Close()
	}

	return util.ReadAll(reader, 128*1024, expectedSize) // 128KB
}

func (c *GenericHttpJsonRpcClient) normalizeJsonRpcError(r *http.Response, nr *common.NormalizedResponse) error {
	jr, err := nr.JsonRpcResponse()

	if c.logger.GetLevel() == zerolog.TraceLevel {
		if jr != nil {
			maxTraceSize := 20 * 1024
			if len(jr.Result) > maxTraceSize {
				tailStart := len(jr.Result) - maxTraceSize
				if tailStart < maxTraceSize {
					tailStart = maxTraceSize
				}
				c.logger.Trace().Str("head", util.Mem2Str(jr.Result[:maxTraceSize])).Str("tail", util.Mem2Str(jr.Result[tailStart:])).Msgf("processing json rpc response from upstream (trimmed to first and last 20k)")
			} else {
				c.logger.Trace().RawJSON("result", jr.Result).Interface("error", jr.Error).Msgf("processing json rpc response from upstream")
			}
		}
	}

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

	if e := extractJsonRpcError(r, nr, jr); e != nil {
		return e
	}

	if jr.Error == nil {
		return nil
	}

	e := common.NewErrJsonRpcExceptionInternal(
		0,
		common.JsonRpcErrorServerSideException,
		"unknown json-rpc error",
		jr.Error,
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
		} else if strings.HasPrefix(err.Message, "pending block is not available") ||
			strings.HasPrefix(err.Message, "pending block not found") ||
			strings.HasPrefix(err.Message, "Pending block not found") ||
			strings.HasPrefix(err.Message, "safe block not found") ||
			strings.HasPrefix(err.Message, "Safe block not found") ||
			strings.HasPrefix(err.Message, "finalized block not found") ||
			strings.HasPrefix(err.Message, "Finalized block not found") {
			// This error means node does not support "finalized/safe/pending" blocks.
			// ref https://github.com/ethereum/go-ethereum/blob/368e16f39d6c7e5cce72a92ec289adbfbaed4854/eth/api_backend.go#L67-L95
			details["blockTag"] = strings.ToLower(strings.SplitN(err.Message, " ", 2)[0])
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorClientSideException,
					err.Message,
					nil,
					details,
				),
			)
		} else if common.EvmIsMissingDataError(err) {
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
		} else if strings.Contains(err.Message, "execution timeout") {
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorNodeTimeout,
					err.Message,
					nil,
					details,
				),
				nil,
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
				strings.Contains(err.Message, "Block") ||
				strings.Contains(err.Message, "transaction") ||
				strings.Contains(err.Message, "Transaction") {
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
	if jr != nil && jr.Result != nil && len(jr.Result) > 0 {
		dt := util.Mem2Str(jr.Result)
		// keccak256("Error(string)")
		if len(dt) > 11 && dt[1:11] == "0x08c379a0" {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					0,
					common.JsonRpcErrorEvmReverted,
					"transaction reverted",
					nil,
					map[string]interface{}{
						"data": json.RawMessage(jr.Result),
					},
				),
			)
		} else {
			// Trace and debug requests might fail due to operation timeout.
			// The response structure is not a standard json-rpc error response,
			// so we need to check the response body for a timeout message.
			// We avoid using JSON parsing to keep it fast on large (50MB) trace data.
			if rq := nr.Request(); rq != nil {
				m, _ := rq.Method()
				if strings.HasPrefix(m, "trace_") ||
					strings.HasPrefix(m, "debug_") ||
					strings.HasPrefix(m, "eth_trace") {
					if strings.Contains(dt, "execution timeout") {
						// Returning a server-side exception so that retry/failover mechanisms retry same and/or other upstreams.
						return common.NewErrEndpointServerSideException(
							common.NewErrJsonRpcExceptionInternal(
								0,
								common.JsonRpcErrorNodeTimeout,
								"execution timeout",
								nil,
								map[string]interface{}{
									"data": json.RawMessage(jr.Result),
								},
							),
							nil,
						)
					}
				}
			}
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
