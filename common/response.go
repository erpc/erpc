package common

import (
	"encoding/json"
)

type NormalizedResponse struct {
	body            []byte
	jsonRpcResponse *JsonRpcResponse
}

func NewNormalizedResponseFromBody(body []byte) *NormalizedResponse {
	return &NormalizedResponse{
		body: body,
	}
}

func NewNormalizedJsonRpcResponse(jrr *JsonRpcResponse) *NormalizedResponse {
	return &NormalizedResponse{
		jsonRpcResponse: jrr,
	}
}

func (r *NormalizedResponse) JsonRpcResponse() (*JsonRpcResponse, error) {
	if r.jsonRpcResponse != nil {
		return r.jsonRpcResponse, nil
	}

	jrr := &JsonRpcResponse{}
	err := json.Unmarshal(r.body, jrr)
	if err != nil {
		return nil, err
	}
	r.jsonRpcResponse = jrr

	return jrr, nil
}

func (r *NormalizedResponse) Body() []byte {
	if r.body != nil {
		return r.body
	}

	jrr, err := r.JsonRpcResponse()
	if err != nil {
		return nil
	}

	r.body, err = json.Marshal(jrr)
	if err != nil {
		return nil
	}

	return r.body
}
