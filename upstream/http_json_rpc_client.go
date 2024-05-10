package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type HttpJsonRpcClient struct {
	Type string
	Url  *url.URL

	httpClient *http.Client
	jsonRpcId  int // TODO do we need atomic concurrency-safe counter?
}

type JsonRpcRequest struct {
	Method string        `json:"method"`
	Params []interface{} `json:"params"`
	Id     int           `json:"id"`
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
	client := &HttpJsonRpcClient{
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

	return client, nil
}

func (c *HttpJsonRpcClient) Call(method string, params []interface{}) (interface{}, error) {
	requestBody, err := json.Marshal(JsonRpcRequest{
		Method: method,
		Params: params,
		Id:     c.jsonRpcId,
	})
	c.jsonRpcId++

	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Post(c.Url.String(), "application/json", bytes.NewBuffer(requestBody))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server responded with status code %d", resp.StatusCode)
	}

	var jsonResponse JsonRpcResponse
	if err := json.NewDecoder(resp.Body).Decode(&jsonResponse); err != nil {
		return nil, err
	}

	if jsonResponse.Error != nil {
		return nil, fmt.Errorf("json rpc error (%d): %s", jsonResponse.Error.Code, jsonResponse.Error.Message)
	}

	return jsonResponse.Result, nil

}

func (c *HttpJsonRpcClient) GetType() string {
	return c.Type
}
