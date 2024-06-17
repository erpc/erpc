package upstream

import (
	"encoding/json"
)

type NormalizedResponse struct {
	Request *NormalizedRequest
	body    []byte
	err     error

	jsonRpcResponse *JsonRpcResponse
}

func NewNormalizedResponse() *NormalizedResponse {
	return &NormalizedResponse{}
}

func (r *NormalizedResponse) WithBody(body []byte) *NormalizedResponse {
	r.body = body
	return r
}

func (r *NormalizedResponse) WithError(err error) *NormalizedResponse {
	r.err = err
	return r
}

func (r *NormalizedResponse) WithRequest(req *NormalizedRequest) *NormalizedResponse {
	r.Request = req
	return r
}

func (r *NormalizedResponse) WithJsonRpcResponse(jrr *JsonRpcResponse) *NormalizedResponse {
	r.jsonRpcResponse = jrr
	return r
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

func (r *NormalizedResponse) IsEmpty() bool {
	jrr, err := r.JsonRpcResponse()
	if err != nil {
		if jrr.Result == nil {
			return true
		}

		if s, ok := jrr.Result.(string); ok {
			return s == "" || s == "0x"
		}
	}

	return false
}
