package upstream

import (
	"encoding/json"

	"github.com/flair-sdk/erpc/common"
	"github.com/rs/zerolog"
)

type JsonRpcRequest struct {
	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id,omitempty"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type JsonRpcResponse struct {
	JSONRPC string                     `json:"jsonrpc,omitempty"`
	ID      interface{}                `json:"id"`
	Result  interface{}                `json:"result"`
	Error   *common.ErrJsonRpcException `json:"error,omitempty"`
}

func (r *JsonRpcRequest) MarshalZerologObject(e *zerolog.Event) {
	e.Str("method", r.Method).Interface("params", r.Params).Interface("id", r.ID)
}

func (r *JsonRpcResponse) MarshalZerologObject(e *zerolog.Event) {
	e.Interface("id", r.ID).Interface("result", r.Result).Interface("error", r.Error)
}

// Custom unmarshal method for JsonRpcResponse
func (r *JsonRpcResponse) UnmarshalJSON(data []byte) error {
	type Alias JsonRpcResponse
	aux := &struct {
		Error json.RawMessage `json:"error,omitempty"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	if aux.Error != nil {
		var customError map[string]interface{}
		if err := json.Unmarshal(aux.Error, &customError); err != nil {
			return err
		}
		r.Error = common.NewErrJsonRpcException(
			nil,
			common.JsonRpcErrorNumber(customError["code"].(float64)),
			customError["message"].(string),
		)
	}

	return nil
}
