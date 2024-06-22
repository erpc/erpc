package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/util"
	"github.com/rs/zerolog/log"
)

type HttpJsonRpcClient struct {
	Type string
	Url  *url.URL

	upstream   *Upstream
	httpClient *http.Client
}

func NewHttpJsonRpcClient(pu *Upstream, parsedUrl *url.URL) (*HttpJsonRpcClient, error) {
	var client *HttpJsonRpcClient

	if util.IsTest() {
		client = &HttpJsonRpcClient{
			Type:       "HttpJsonRpcClient",
			Url:        parsedUrl,
			upstream:   pu,
			httpClient: &http.Client{},
		}
	} else {
		client = &HttpJsonRpcClient{
			Type:     "HttpJsonRpcClient",
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

func (c *HttpJsonRpcClient) SendRequest(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error) {
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

func (c *HttpJsonRpcClient) GetType() string {
	return c.Type
}

func (c *HttpJsonRpcClient) normalizeJsonRpcError(r *http.Response, nr *NormalizedResponse) error {
	jr, err := nr.JsonRpcResponse()

	if r.StatusCode < 400 {
		if nr != nil {
			if err != nil {
				return common.NewErrJsonRpcException(
					0,
					common.JsonRpcErrorClientSideException,
					"could not parse json rpc response from upstream",
					err,
				)
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

		if r.StatusCode == 401 || r.StatusCode == 403 {
			return common.NewErrEndpointUnauthorized(err)
		} else if r.StatusCode == 415 {
			return common.NewErrEndpointUnsupported(err)
		} else if r.StatusCode == 429 || r.StatusCode == 408 {
			return common.NewErrEndpointCapacityExceeded(err)
		} else if r.StatusCode >= 500 {
			return common.NewErrEndpointServerSideException(err)
		}

		return err
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
