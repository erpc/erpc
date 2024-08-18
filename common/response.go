package common

import (
	"fmt"

	"github.com/bytedance/sonic"
)

type NormalizedResponse struct {
	request *NormalizedRequest
	body    []byte
	err     error

	jsonRpcResponse *JsonRpcResponse
}

func NewNormalizedResponse() *NormalizedResponse {
	return &NormalizedResponse{}
}

func (r *NormalizedResponse) WithRequest(req *NormalizedRequest) *NormalizedResponse {
	r.request = req
	return r
}

func (r *NormalizedResponse) WithBody(body []byte) *NormalizedResponse {
	r.body = body
	return r
}

func (r *NormalizedResponse) WithError(err error) *NormalizedResponse {
	r.err = err
	return r
}

func (r *NormalizedResponse) WithJsonRpcResponse(jrr *JsonRpcResponse) *NormalizedResponse {
	r.jsonRpcResponse = jrr
	return r
}

func (r *NormalizedResponse) Request() *NormalizedRequest {
	if r == nil {
		return nil
	}
	return r.request
}

func (r *NormalizedResponse) Body() []byte {
	if r == nil {
		return nil
	}
	if r.body != nil {
		return r.body
	}

	jrr, err := r.JsonRpcResponse()
	if err != nil {
		return nil
	}

	r.body, err = sonic.Marshal(jrr)
	if err != nil {
		return nil
	}

	return r.body
}

func (r *NormalizedResponse) Error() error {
	if r.err != nil {
		return r.err
	}

	return nil
}

func (r *NormalizedResponse) IsResultEmptyish() bool {
	jrr, err := r.JsonRpcResponse()
	if err == nil {
		if jrr == nil {
			return true
		}

		// Use raw result to avoid json unmarshalling for performance reasons
		if jrr.Result == nil ||
			len(jrr.Result) == 0 ||
			(jrr.Result[0] == '0' && jrr.Result[1] == 'x' && len(jrr.Result) == 2) ||
			(jrr.Result[0] == '[' && jrr.Result[1] == ']' && len(jrr.Result) == 2) ||
			(jrr.Result[0] == '{' && jrr.Result[1] == '}' && len(jrr.Result) == 2) {
			return true
		}
	}

	return false
}

func (r *NormalizedResponse) JsonRpcResponse() (*JsonRpcResponse, error) {
	if r == nil {
		return nil, nil
	}

	if r.jsonRpcResponse != nil {
		return r.jsonRpcResponse, nil
	}

	jrr := &JsonRpcResponse{}
	err := sonic.Unmarshal(r.body, jrr)
	if err != nil {
		jrr.Error = NewErrJsonRpcExceptionExternal(
			int(JsonRpcErrorServerSideException),
			fmt.Sprintf("%s -> %s", err, string(r.body)),
			"",
		)
	}
	r.jsonRpcResponse = jrr

	return jrr, nil
}

func (r *NormalizedResponse) IsObjectNull() bool {
	if r == nil {
		return true
	}

	jrr, _ := r.JsonRpcResponse()
	if jrr == nil && r.body == nil {
		return true
	}

	return false
}

func (r *NormalizedResponse) String() string {
	if r == nil {
		return "<nil>"
	}
	if r.body != nil && len(r.body) > 0 {
		return string(r.body)
	}
	if r.err != nil {
		return r.err.Error()
	}
	if r.jsonRpcResponse != nil {
		b, _ := sonic.Marshal(r.jsonRpcResponse)
		return string(b)
	}
	return "<nil>"
}

// Custom unmarsheller for json.NewEncoder(w).Encode
func (r *NormalizedResponse) MarshalJSON() ([]byte, error) {
	if r.body != nil {
		return r.body, nil
	}

	if r.jsonRpcResponse != nil {
		return sonic.Marshal(r.jsonRpcResponse)
	}

	return nil, nil
}
