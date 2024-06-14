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

	httpClient *http.Client
}

func NewHttpJsonRpcClient(parsedUrl *url.URL) (*HttpJsonRpcClient, error) {
	var client *HttpJsonRpcClient

	if util.IsTest() {
		client = &HttpJsonRpcClient{
			Type:       "HttpJsonRpcClient",
			Url:        parsedUrl,
			httpClient: &http.Client{},
		}
	} else {
		client = &HttpJsonRpcClient{
			Type: "HttpJsonRpcClient",
			Url:  parsedUrl,
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

func (c *HttpJsonRpcClient) SendRequest(ctx context.Context, req *common.JsonRpcRequest) (*common.NormalizedResponse, error) {
	requestBody, err := json.Marshal(common.JsonRpcRequest{
		JSONRPC: req.JSONRPC,
		Method:  req.Method,
		Params:  req.Params,
		ID:      req.ID,
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
				"url":     c.Url.String(),
				"request": requestBody,
			},
		}
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}

	respStatusCode := resp.StatusCode
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Debug().Msgf("received json rpc response status: %d", respStatusCode)

	if respStatusCode >= 400 {
		defer resp.Body.Close()
		return nil, &common.BaseError{
			Code:    "ErrHttp",
			Message: "server responded with non-2xx status code",
			Details: map[string]interface{}{
				"statusCode": respStatusCode,
				"body":       string(respBody),
				"headers":    resp.Header,
			},
		}
	}

	return common.NewNormalizedResponseFromBody(respBody), nil
}

func (c *HttpJsonRpcClient) GetType() string {
	return c.Type
}
