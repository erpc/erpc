package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

type HttpJsonRpcClient interface {
	GetType() ClientType
	SupportsNetwork(networkId string) (bool, error)
	SendRequest(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error)
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
	batchTimer    *time.Timer
}

type batchRequest struct {
	ctx      context.Context
	request  *NormalizedRequest
	response chan *NormalizedResponse
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
				client.batchMaxSize = 100
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
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConnsPerHost: 10,
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

func (c *GenericHttpJsonRpcClient) SendRequest(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error) {
	if !c.supportsBatch {
		return c.sendSingleRequest(ctx, req)
	}

	responseChan := make(chan *NormalizedResponse, 1)
	errChan := make(chan error, 1)

	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(err, c.upstream.Config().Id, 0)
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
		return nil, ctx.Err()
	}
}

func (c *GenericHttpJsonRpcClient) queueRequest(id interface{}, req *batchRequest) {
	c.batchMu.Lock()
	defer c.batchMu.Unlock()

	c.batchRequests[id] = req
	c.logger.Debug().Msgf("queued request %v for batch", id)

	if len(c.batchRequests) == 1 {
		c.batchTimer = time.AfterFunc(c.batchMaxWait, c.processBatch)
	} else if len(c.batchRequests) >= c.batchMaxSize {
		c.batchTimer.Stop()
		go c.processBatch()
	}
}

func (c *GenericHttpJsonRpcClient) processBatch() {
	c.batchMu.Lock()
	requests := c.batchRequests
	c.batchRequests = make(map[interface{}]*batchRequest)
	c.batchMu.Unlock()

	ln := len(requests)
	if ln == 0 {
		return
	}
	c.logger.Debug().Msgf("processing batch with %d requests", ln)

	batchReq := make([]common.JsonRpcRequest, 0, ln)
	for _, req := range requests {
		jrReq, err := req.request.JsonRpcRequest()
		if err != nil {
			req.err <- common.NewErrUpstreamRequest(err, c.upstream.Config().Id, 0)
			continue
		}
		batchReq = append(batchReq, common.JsonRpcRequest{
			JSONRPC: jrReq.JSONRPC,
			Method:  jrReq.Method,
			Params:  jrReq.Params,
			ID:      jrReq.ID,
		})
	}

	requestBody, err := json.Marshal(batchReq)
	if err != nil {
		for _, req := range requests {
			req.err <- err
		}
		return
	}

	c.logger.Debug().Msgf("sending batch json rpc POST request to %s: %s", c.Url.String(), requestBody)

	httpReq, errReq := http.NewRequestWithContext(context.Background(), "POST", c.Url.String(), bytes.NewBuffer(requestBody))
	httpReq.Header.Set("Content-Type", "application/json")
	if errReq != nil {
		for _, req := range requests {
			req.err <- errReq
		}
		return
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		for _, req := range requests {
			req.err <- err
		}
		return
	}

	respBody, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		for _, req := range requests {
			req.err <- err
		}
		return
	}

	var batchResp []json.RawMessage
	err = json.Unmarshal(respBody, &batchResp)
	if err != nil {
		// Try parsing as single json-rpc object,
		// some providers return a single object on some errors even when request is batch.
		// this is a workaround to handle those cases.
		var singleResp common.JsonRpcResponse
		errsg := json.Unmarshal(respBody, &singleResp)
		if errsg != nil {
			for _, req := range requests {
				req.err <- common.NewErrEndpointServerSideException(
					fmt.Errorf("failed to parse upstream response: %w body: %s", err, respBody),
				)
			}
		} else {
			if singleResp.JSONRPC != "" {
				// This case happens when upstreams a single valid json-rpc object as response
				// to a batch request (e.g. BlastAPI).
				for _, req := range requests {
					nr := NewNormalizedResponse().WithRequest(req.request).WithBody(respBody)
					err := c.normalizeJsonRpcError(resp, nr)
					if err != nil {
						req.err <- err
					} else {
						req.response <- nr
					}
				}
			} else {
				for _, req := range requests {
					req.err <- common.NewErrEndpointServerSideException(
						fmt.Errorf("failed to parse upstream response: %w body: %s", err, respBody),
					)
				}
			}
		}
		return
	}

	for _, rawResp := range batchResp {
		var jrResp common.JsonRpcResponse
		err := json.Unmarshal(rawResp, &jrResp)
		if err != nil {
			continue
		}

		if req, ok := requests[jrResp.ID]; ok {
			nr := NewNormalizedResponse().WithRequest(req.request).WithBody(rawResp)
			err := c.normalizeJsonRpcError(resp, nr)
			if err != nil {
				req.err <- err
			} else {
				req.response <- nr
			}
			delete(requests, jrResp.ID)
		}
	}

	// Handle any remaining requests that didn't receive a response
	for _, req := range requests {
		req.err <- fmt.Errorf("no response received for request")
	}
}

func (c *GenericHttpJsonRpcClient) sendSingleRequest(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error) {
	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(err, c.upstream.Config().Id, 0)
	}

	requestBody, err := json.Marshal(common.JsonRpcRequest{
		JSONRPC: jrReq.JSONRPC,
		Method:  jrReq.Method,
		Params:  jrReq.Params,
		ID:      jrReq.ID,
	})

	if err != nil {
		return nil, err
	}

	c.logger.Debug().Msgf("sending json rpc POST request to %s: %s", c.Url.String(), requestBody)

	httpReq, errReq := http.NewRequestWithContext(ctx, "POST", c.Url.String(), bytes.NewBuffer(requestBody))
	httpReq.Header.Set("Content-Type", "application/json")
	if errReq != nil {
		return nil, &common.BaseError{
			Code:    "ErrHttp",
			Message: fmt.Sprintf("%v", errReq),
			Details: map[string]interface{}{
				"url":      c.Url.String(),
				"upstream": c.upstream.Config().Id,
				"request":  requestBody,
			},
		}
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	respBody, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return nil, err
	}

	nr := NewNormalizedResponse().WithRequest(req).WithBody(respBody)

	return nr, c.normalizeJsonRpcError(resp, nr)
}

func (c *GenericHttpJsonRpcClient) normalizeJsonRpcError(r *http.Response, nr *NormalizedResponse) error {
	jr, err := nr.JsonRpcResponse()

	if err != nil {
		e := common.NewErrJsonRpcExceptionInternal(
			0,
			common.JsonRpcErrorParseException,
			"could not parse json rpc response from upstream",
			err,
			map[string]interface{}{
				"upstream":   c.upstream.Config().Id,
				"statusCode": r.StatusCode,
				"headers":    r.Header,
				"body":       string(nr.Body()),
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
			"upstream":   c.upstream.Config().Id,
			"statusCode": r.StatusCode,
			"headers":    r.Header,
			"body":       string(nr.Body()),
		},
	)

	return e
}

func extractJsonRpcError(r *http.Response, nr common.NormalizedResponse, jr *common.JsonRpcResponse) error {
	if jr != nil && jr.Error != nil {
		err := jr.Error

		if ver := getVendorSpecificErrorIfAny(r, nr, jr); ver != nil {
			return ver
		}

		code := common.JsonRpcErrorNumber(err.Code)

		var details map[string]interface{} = make(map[string]interface{})
		if err.Data != "" {
			details["data"] = err.Data
		}

		// Infer from known status codes
		if r.StatusCode == 401 || r.StatusCode == 403 {
			return common.NewErrEndpointUnauthorized(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnauthorized,
					err.Message,
					nil,
					details,
				),
			)
		} else if r.StatusCode == 415 || code == common.JsonRpcErrorUnsupportedException {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
					err.Message,
					nil,
					details,
				),
			)
		} else if r.StatusCode == 429 || r.StatusCode == 408 {
			return common.NewErrEndpointCapacityExceeded(err)
			// Wrap rpc exception with endpoint-specific errors (useful for erpc specialized handling)
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
		} else if code == -32602 {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorInvalidArgument,
					err.Message,
					nil,
					details,
				),
			)
		} else if code == -32005 {
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCapacityExceeded,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "missing trie node") {
			return common.NewErrEndpointNotSyncedYet(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorNotSyncedYet,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "genesis is not traceable") {
			// This usually happens when sending a trace_* request to a newly created block
			return common.NewErrEndpointNotSyncedYet(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorNotSyncedYet,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "execution reverted") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorEvmReverted,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "insufficient funds") || strings.Contains(err.Message, "out of gas") {
			return common.NewErrEndpointClientSideException(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorCallException,
					err.Message,
					nil,
					details,
				),
			)
		} else if strings.Contains(err.Message, "not found") {
			if strings.Contains(err.Message, "Method") || strings.Contains(err.Message, "method") {
				return common.NewErrEndpointUnsupported(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorUnsupportedException,
						err.Message,
						nil,
						details,
					),
				)
			} else {
				return common.NewErrEndpointClientSideException(
					common.NewErrJsonRpcExceptionInternal(
						int(code),
						common.JsonRpcErrorUnsupportedException,
						err.Message,
						nil,
						details,
					),
				)
			}
		} else if strings.Contains(err.Message, "Unsupported method") ||
			strings.Contains(err.Message, "not supported") ||
			strings.Contains(err.Message, "method is not whitelisted") {
			return common.NewErrEndpointUnsupported(
				common.NewErrJsonRpcExceptionInternal(
					int(code),
					common.JsonRpcErrorUnsupportedException,
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
		)
	}

	return nil
}

func getVendorSpecificErrorIfAny(
	rp *http.Response,
	nr common.NormalizedResponse,
	jr *common.JsonRpcResponse,
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

	return vn.GetVendorSpecificErrorIfAny(rp, jr)
}
