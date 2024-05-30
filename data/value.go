package data

import (
	"encoding/json"
	"sync"

	"github.com/flair-sdk/erpc/common"
)

type DataValue struct {
	raw string
	mu  sync.Mutex

	parsedJsonRpcRes *common.JsonRpcResponse
}

func NewDataValue(raw string) *DataValue {
	return &DataValue{raw: raw}
}

func (d *DataValue) AsJsonRpcResponse() (*common.JsonRpcResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.parsedJsonRpcRes != nil {
		return d.parsedJsonRpcRes, nil
	}

	var result common.JsonRpcResponse
	err := json.Unmarshal([]byte(d.raw), &result)
	if err != nil {
		return nil, err
	}

	d.parsedJsonRpcRes = &result
	return d.parsedJsonRpcRes, nil
}
