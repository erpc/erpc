package data

import (
	"encoding/json"
	"sync"

	"github.com/flair-sdk/erpc/upstream"
)

type DataRow struct {
	PK    string
	RK    string
	Value string

	mu               sync.Mutex
	parsedJsonRpcRes *upstream.JsonRpcResponse
}

func NewDataValue(v string) *DataRow {
	return &DataRow{Value: v}
}

func (d *DataRow) AsJsonRpcResponse() (*upstream.JsonRpcResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.parsedJsonRpcRes != nil {
		return d.parsedJsonRpcRes, nil
	}

	var result upstream.JsonRpcResponse
	err := json.Unmarshal([]byte(d.Value), &result)
	if err != nil {
		return nil, err
	}

	d.parsedJsonRpcRes = &result
	return d.parsedJsonRpcRes, nil
}
