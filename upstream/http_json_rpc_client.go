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
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/util"
	"github.com/rs/zerolog/log"
)

type HttpJsonRpcClient interface {
	GetType() ClientType
	SupportsNetwork(networkId string) (bool, error)
	SendRequest(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error)
}

type GenericHttpJsonRpcClient struct {
	Url *url.URL

	upstream   *Upstream
	httpClient *http.Client
}

func NewGenericHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (HttpJsonRpcClient, error) {
	var client HttpJsonRpcClient

	if util.IsTest() {
		client = &GenericHttpJsonRpcClient{
			Url:        parsedUrl,
			upstream:   pu,
			httpClient: &http.Client{},
		}
	} else {
		client = &GenericHttpJsonRpcClient{
			Url:      parsedUrl,
			upstream: pu,
			httpClient: &http.Client{
				Timeout: 30 * time.Second, // Set a timeout
				Transport: &http.Transport{
					MaxIdleConns:        100,
					IdleConnTimeout:     90 * time.Second,
					MaxIdleConnsPerHost: 10,
				},
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
	jrReq, err := req.JsonRpcRequest()
	if err != nil {
		return nil, common.NewErrUpstreamRequest(err, c.upstream.Config().Id, req)
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

	log.Debug().Msgf("sending json rpc POST request to %s: %s", c.Url.String(), requestBody)

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

	if r.StatusCode < 400 {
		if nr != nil {
			if err != nil {
				e := common.NewErrJsonRpcException(
					0,
					common.JsonRpcErrorParseException,
					"could not parse json rpc response from upstream",
					err,
				)
				e.Details = map[string]interface{}{
					"upstream":   c.upstream.Config().Id,
					"statusCode": r.StatusCode,
					"headers":    r.Header,
					"body":       string(nr.Body()),
				}
				return e
			}
			log.Debug().
				Err(err).
				Interface("resp", jr).
				Msgf("received json rpc response status: %d", r.StatusCode)

			if jr != nil && jr.Error == nil {
				return nil
			}
		}
	}

	if e := extractJsonRpcError(r, nr, jr); e != nil {
		return e
	}

	e := common.NewErrJsonRpcException(0, common.JsonRpcErrorServerSideException, "unknown json-rpc response", nil)
	e.Details = map[string]interface{}{
		"upstream":   c.upstream.Config().Id,
		"statusCode": r.StatusCode,
		"headers":    r.Header,
		"body":       string(nr.Body()),
	}

	return e
}

func extractJsonRpcError(r *http.Response, nr common.NormalizedResponse, jr *common.JsonRpcResponse) error {
	if jr != nil && jr.Error != nil {
		err := jr.Error

		if ver := getVendorSpecificErrorIfAny(r, nr, jr); ver != nil {
			return ver
		}

		code := common.JsonRpcErrorNumber(err.OriginalCode())

		// Infer from known status codes
		if r.StatusCode == 401 || r.StatusCode == 403 {
			return common.NewErrEndpointUnauthorized(err)
		} else if r.StatusCode == 415 || code == common.JsonRpcErrorUnsupportedException {
			return common.NewErrEndpointUnsupported(err)
		} else if r.StatusCode == 429 || r.StatusCode == 408 {
			return common.NewErrEndpointCapacityExceeded(err)
		} else if r.StatusCode >= 500 {
			return common.NewErrEndpointServerSideException(err)

			// Wrap rpc exception with endpoint-specific errors (useful for erpc specialized handling)
		} else if code == common.JsonRpcErrorUnsupportedException {
			return common.NewErrEndpointUnsupported(err)
		} else if code == common.JsonRpcErrorServerSideException {
			return common.NewErrEndpointServerSideException(err)

			// Wrap stanard EIP-1474 rpc error codes with endpoint-specific errors (useful for erpc specialized handling)
		} else if code == -32004 || code == -32001 {
			return common.NewErrEndpointUnsupported(err)
		} else if code == -32005 {
			return common.NewErrEndpointCapacityExceeded(err)
		}

		// Text-based common errors
		if strings.Contains(err.Message, "missing trie node") {
			return common.NewErrEndpointNotSyncedYet(err)
		} else if code == -32600 && strings.Contains(err.Message, "not supported") {
			return common.NewErrEndpointUnsupported(err)
		}

		// By default we consider a problem on the server so that retry/failover mechanisms try other upstreams
		return common.NewErrEndpointServerSideException(err)
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

	ups := req.Upstream()
	if ups == nil {
		return nil
	}

	vn := ups.Vendor()
	if vn == nil {
		return nil
	}

	return vn.GetVendorSpecificErrorIfAny(rp, jr)
}
