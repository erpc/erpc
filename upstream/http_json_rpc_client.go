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
	jsonRpcId  int // TODO do we need atomic concurrency-safe counter?
}

type JsonRpcRequest struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id,omitempty"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type JsonRpcResponse struct {
	Result interface{}   `json:"result"`
	Error  *JsonRpcError `json:"error"`
	Id     int           `json:"id"`
}

type JsonRpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func NewHttpJsonRpcClient(parsedUrl *url.URL) (*HttpJsonRpcClient, error) {
	var client *HttpJsonRpcClient

	if util.IsTest() {
		client = &HttpJsonRpcClient{
			Type:       "HttpJsonRpcClient",
			Url:        parsedUrl,
			httpClient: &http.Client{},
			jsonRpcId:  1,
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
			jsonRpcId: 1,
		}

	}

	return client, nil
}

func (c *HttpJsonRpcClient) SendRequest(ctx context.Context, req *JsonRpcRequest) (interface{}, error) {
	requestBody, err := json.Marshal(JsonRpcRequest{
		JSONRPC: req.JSONRPC,
		Method:  req.Method,
		Params:  req.Params,
		ID:      c.jsonRpcId,
	})
	c.jsonRpcId++

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
	defer resp.Body.Close()

	respStatusCode := resp.StatusCode
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	log.Debug().Msgf("received json rpc response status: %d and body: %s", respStatusCode, respBody)

	if respStatusCode >= 400 {
		// return nil, fmt.Errorf("server responded with status code %d", respStatusCode)
		return nil, &common.BaseError{
			Code:    "ErrHttp",
			Message: "server responded with non-2xx status code",
			Details: map[string]interface{}{
				"statusCode": respStatusCode,
				"body":       string(respBody),
			},
		}
	}

	var jsonResponse JsonRpcResponse
	if err := json.Unmarshal(respBody, &jsonResponse); err != nil {
		return nil, err
	}

	if jsonResponse.Error != nil {
		// return nil, fmt.Errorf("json rpc error (%d): %s", jsonResponse.Error.Code, jsonResponse.Error.Message)
		return nil, &common.BaseError{
			Code:    "ErrJsonRpc",
			Message: "response has json-rpc error",
			Details: map[string]interface{}{
				"response": jsonResponse,
			},
		}
	}

	return jsonResponse.Result, nil

}

func (c *HttpJsonRpcClient) GetType() string {
	return c.Type
}
