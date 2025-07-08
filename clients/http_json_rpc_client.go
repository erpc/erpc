package clients

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type HttpJsonRpcClient interface {
	GetType() ClientType
	SendRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}

type GenericHttpJsonRpcClient struct {
	Url     *url.URL
	headers map[string]string

	proxyPool *ProxyPool

	projectId       string
	upstream        common.Upstream
	upstreamId      string
	appCtx          context.Context
	logger          *zerolog.Logger
	httpClient      *http.Client
	isLogLevelTrace bool

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

func NewGenericHttpJsonRpcClient(
	appCtx context.Context,
	logger *zerolog.Logger,
	projectId string,
	upstream common.Upstream,
	parsedUrl *url.URL,
	jsonRpcCfg *common.JsonRpcUpstreamConfig,
	proxyPool *ProxyPool,
) (HttpJsonRpcClient, error) {
	upsId := "n/a"
	if upstream != nil {
		upsId = upstream.Id()
	}
	client := &GenericHttpJsonRpcClient{
		Url:             parsedUrl,
		appCtx:          appCtx,
		logger:          logger,
		projectId:       projectId,
		upstream:        upstream,
		upstreamId:      upsId,
		proxyPool:       proxyPool,
		isLogLevelTrace: logger.GetLevel() == zerolog.TraceLevel,
	}

	// Default fallback transport (no proxy)
	transport := &http.Transport{
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	}

	if util.IsTest() {
		client.httpClient = &http.Client{}
	} else {
		client.httpClient = &http.Client{
			Timeout:   60 * time.Second,
			Transport: transport,
		}
	}

	if jsonRpcCfg != nil {
		if jsonRpcCfg.SupportsBatch != nil && *jsonRpcCfg.SupportsBatch {
			client.supportsBatch = true
			client.batchMaxSize = jsonRpcCfg.BatchMaxSize
			client.batchMaxWait = jsonRpcCfg.BatchMaxWait.Duration()
			client.batchRequests = make(map[interface{}]*batchRequest)
		}

		if jsonRpcCfg.EnableGzip != nil {
			client.enableGzip = *jsonRpcCfg.EnableGzip
		}

		if jsonRpcCfg.Headers != nil {
			client.headers = jsonRpcCfg.Headers
		}

		client.proxyPool = proxyPool
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
			c.upstreamId,
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
		// TODO For both of these conditions failsafe library can introduce carrying
		//      the "cause" so we know this cancellation is due to Hedge policy for example.
		if errors.Is(err, context.DeadlineExceeded) {
			err = common.NewErrEndpointRequestTimeout(time.Since(startedAt), err)
		} else if errors.Is(err, context.Canceled) {
			err = common.NewErrEndpointRequestCanceled(err)
		}
		return nil, err
	case <-c.appCtx.Done():
		return nil, common.NewErrEndpointRequestCanceled(c.appCtx.Err())
	}
}

func (c *GenericHttpJsonRpcClient) shutdown() {
	c.batchMu.Lock()
	if c.batchTimer != nil {
		c.batchTimer.Stop()
	}
	c.processBatch(true)
}

func (c *GenericHttpJsonRpcClient) getHttpClient() *http.Client {
	if c.proxyPool != nil {
		client, err := c.proxyPool.GetClient()
		if c.isLogLevelTrace {
			proxy, _ := client.Transport.(*http.Transport).Proxy(nil)
			c.logger.Trace().Str("proxyPool", c.proxyPool.ID).Str("ptr", fmt.Sprintf("%p", client.Transport)).Str("proxy", proxy.String()).Msgf("using client from proxy pool")
		}
		if err != nil {
			c.logger.Error().Err(err).Msgf("failed to get client from proxy pool")
			return c.httpClient
		}
		return client
	}

	return c.httpClient
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

	if c.isLogLevelTrace {
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
	}
	c.logger.Debug().Msgf("queuing request %+v for batch (current batch size: %d)", id, len(c.batchRequests))

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
			if c.isLogLevelTrace {
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
	if c.isLogLevelTrace {
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
				c.upstreamId,
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
					"upstreamId": c.upstreamId,
					"request":    requestBody,
				},
			}
		}
		return
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", fmt.Sprintf("erpc (%s/%s; Project/%s)", common.ErpcVersion, common.ErpcCommitSha, c.projectId))

	c.logger.Debug().Str("host", c.Url.Host).RawJSON("request", requestBody).Interface("headers", httpReq.Header).Msgf("sending json rpc POST request (batch)")

	// pick the client from the proxy pool registry (if configured) or fallback
	resp, err := c.getHttpClient().Do(httpReq)
	if err != nil {
		cause := context.Cause(batchCtx)
		if cause == nil {
			cause = batchCtx.Err()
		}
		if cause != nil {
			err = cause
		}
		// TODO For both of these conditions failsafe library can introduce carrying
		//      the "cause" so we know this cancellation is due to Hedge policy for example.
		if errors.Is(err, context.DeadlineExceeded) {
			for _, req := range requests {
				req.err <- common.NewErrEndpointRequestTimeout(time.Since(reqStartTime), err)
			}
		} else if errors.Is(err, context.Canceled) {
			for _, req := range requests {
				req.err <- common.NewErrEndpointRequestCanceled(err)
			}
		} else {
			for _, req := range requests {
				req.err <- common.NewErrEndpointTransportFailure(c.Url, err)
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

	bodyStr := util.B2Str(bodyBytes)
	searcher := ast.NewSearcher(bodyStr)
	searcher.CopyReturn = false
	searcher.ConcurrentRead = false
	searcher.ValidateJSON = false

	if c.isLogLevelTrace {
		if len(bodyBytes) > 20*1024 {
			c.logger.Trace().Str("head", util.B2Str(bodyBytes[:20*1024])).Str("tail", util.B2Str(bodyBytes[len(bodyBytes)-20*1024:])).Msgf("processing batch response from upstream (trimmed to first and last 20k)")
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
			c.logger.Error().Str("response", util.B2Str(bodyBytes)).Msgf("some requests did not receive a response (matching ID)")
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
			req.err <- common.NewErrUpstreamMalformedResponse(fmt.Errorf("unexpected response type (not array nor object): %s", bodyStr), c.upstreamId)
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
		jrResp := &common.JsonRpcResponse{}

		if rawID != "" {
			err := jrResp.SetIDBytes(util.S2Bytes(rawID))
			if err != nil {
				return nil, err
			}
		}

		cause := fmt.Sprintf("cannot parse json rpc response from upstream, for result: %s, for error: %s", rawResult, rawError)
		jrResp.Error = common.NewErrJsonRpcExceptionExternal(
			int(common.JsonRpcErrorParseException),
			cause,
			"",
		)

		return jrResp, nil
	}

	return common.NewJsonRpcResponseFromBytes(
		util.S2Bytes(rawID),
		util.S2Bytes(rawResult),
		util.S2Bytes(rawError),
	)
}

func (c *GenericHttpJsonRpcClient) sendSingleRequest(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	ctx, span := common.StartSpan(ctx, "HttpJsonRpcClient.sendSingleRequest",
		trace.WithAttributes(
			attribute.String("network.id", req.NetworkId()),
			attribute.String("upstream.id", c.upstreamId),
		),
	)
	defer span.End()

	if common.IsTracingDetailed {
		span.SetAttributes(
			attribute.String("request.id", fmt.Sprintf("%v", req.ID())),
		)
	}

	// TODO check if context is cancellable and then get the "cause" that is already set
	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, common.NewErrUpstreamRequest(
			err,
			c.upstreamId,
			req.NetworkId(),
			jrReq.Method,
			0,
			0,
			0,
			0,
		)
	}

	jrReq.RLock()
	span.SetAttributes(attribute.String("request.method", jrReq.Method))
	requestBody, err := common.SonicCfg.Marshal(common.JsonRpcRequest{
		JSONRPC: jrReq.JSONRPC,
		Method:  jrReq.Method,
		Params:  jrReq.Params,
		ID:      jrReq.ID,
	})
	jrReq.RUnlock()
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, err
	}

	reqStartTime := time.Now()
	httpReq, err := c.prepareRequest(ctx, requestBody)
	if err != nil {
		common.SetTraceSpanError(span, err)
		return nil, &common.BaseError{
			Code:    "ErrHttp",
			Message: fmt.Sprintf("%v", err),
			Details: map[string]interface{}{
				"url":        c.Url.String(),
				"upstreamId": c.upstreamId,
				"request":    requestBody,
			},
		}
	}
	c.logger.Debug().Str("host", c.Url.Host).RawJSON("request", requestBody).Interface("headers", httpReq.Header).Msg("sending json rpc POST request (single)")

	resp, err := c.getHttpClient().Do(httpReq)
	if err != nil {
		cause := context.Cause(ctx)
		if cause == nil {
			cause = ctx.Err()
		}
		c.logger.Debug().Err(err).AnErr("contextError", cause).Msg("transport failure while sending single request")
		if cause != nil {
			err = cause
		}
		common.SetTraceSpanError(span, err)
		// TODO For both of these conditions failsafe library can introduce carrying
		//      the "cause" so we know this cancellation is due to Hedge policy for example.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, common.NewErrEndpointRequestTimeout(time.Since(reqStartTime), err)
		} else if errors.Is(err, context.Canceled) {
			return nil, common.NewErrEndpointRequestCanceled(err)
		}
		return nil, common.NewErrEndpointTransportFailure(c.Url, err)
	}
	defer resp.Body.Close()

	var bodyReader io.ReadCloser = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, common.NewErrEndpointTransportFailure(c.Url, fmt.Errorf("cannot create gzip reader: %w", err))
		}
		defer gzReader.Close()
		bodyReader = gzReader
	}

	nr := common.NewNormalizedResponse().
		WithRequest(req).
		WithBody(bodyReader).
		WithExpectedSize(int(resp.ContentLength))

	err = c.normalizeJsonRpcError(resp, nr)
	if err != nil {
		common.SetTraceSpanError(span, err)
	}

	return nr, err
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
		if c.isLogLevelTrace {
			compressedSize := buf.Len()
			originalSize := len(body)
			c.logger.Trace().
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
	httpReq.Header.Set("User-Agent", fmt.Sprintf("erpc (%s/%s; Project/%s)", common.ErpcVersion, common.ErpcCommitSha, c.projectId))

	// Add gzip header if compression is enabled
	if c.enableGzip {
		httpReq.Header.Set("Content-Encoding", "gzip")
	}

	// Add custom headers if provided
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}

	// Inject OpenTelemetry trace context into HTTP headers
	propagator := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
	propagator.Inject(ctx, propagation.HeaderCarrier(httpReq.Header))

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

	if c.isLogLevelTrace {
		if jr != nil {
			maxTraceSize := 20 * 1024
			if len(jr.Result) > maxTraceSize {
				tailStart := len(jr.Result) - maxTraceSize
				if tailStart < maxTraceSize {
					tailStart = maxTraceSize
				}
				c.logger.Trace().Int("statusCode", r.StatusCode).Str("head", util.B2Str(jr.Result[:maxTraceSize])).Str("tail", util.B2Str(jr.Result[tailStart:])).Msgf("processing json rpc response from upstream (trimmed to first and last 20k)")
			} else {
				if len(jr.Result) > 0 {
					if common.IsSemiValidJson(jr.Result) {
						c.logger.Trace().Int("statusCode", r.StatusCode).RawJSON("result", jr.Result).Interface("error", jr.Error).Msgf("processing json rpc response from upstream")
					} else {
						c.logger.Trace().Int("statusCode", r.StatusCode).Str("result", util.B2Str(jr.Result)).Interface("error", jr.Error).Msgf("processing malformed json-rpc result response from upstream")
					}
				} else {
					c.logger.Trace().Int("statusCode", r.StatusCode).Interface("error", jr.Error).Msgf("processing empty json-rpc result response from upstream")
				}
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
				"upstreamId": c.upstreamId,
				"statusCode": r.StatusCode,
				"headers":    r.Header,
			},
		)
		return e
	}

	// TODO Distinguish between different architectures as a property on GenericHttpJsonRpcClient during initialization
	// TODO Move the logic to evm package as a post-response hook?
	if e := evm.ExtractJsonRpcError(r, nr, jr, c.upstream); e != nil {
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
			"upstreamId": c.upstreamId,
			"statusCode": r.StatusCode,
			"headers":    r.Header,
		},
	)

	return e
}
