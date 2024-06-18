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

	upstream   *PreparedUpstream
	httpClient *http.Client
}

func NewHttpJsonRpcClient(pu *PreparedUpstream, parsedUrl *url.URL) (*HttpJsonRpcClient, error) {
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
		return nil, common.NewErrUpstreamRequest(err, c.upstream.Id)
	}

	requestBody, err := json.Marshal(JsonRpcRequest{
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
				"upstream": c.upstream.Id,
				"request":  requestBody,
			},
		}
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	respStatusCode := resp.StatusCode
	respBody, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		return nil, err
	}
	log.Debug().Msgf("received json rpc response status: %d", respStatusCode)

	if respStatusCode >= 400 {
		return nil, &common.BaseError{
			Code:    "ErrHttp",
			Message: "server responded with non-2xx status code",
			Details: map[string]interface{}{
				"upstream":   c.upstream.Id,
				"statusCode": respStatusCode,
				"body":       string(respBody),
				"headers":    resp.Header,
			},
		}
	}

	return NewNormalizedResponse().
		WithRequest(req).
		WithBody(respBody), nil
}

func (c *HttpJsonRpcClient) GetType() string {
	return c.Type
}
