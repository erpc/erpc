package data

import (
	"sync"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
)

type DataRow struct {
	PK    string
	RK    string
	Value string

	mu               sync.Mutex
	parsedJsonRpcRes *common.JsonRpcResponse
}

func NewDataValue(v string) *DataRow {
	return &DataRow{Value: v}
}

func (d *DataRow) AsJsonRpcResponse() (*common.JsonRpcResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.parsedJsonRpcRes != nil {
		return d.parsedJsonRpcRes, nil
	}

	var result common.JsonRpcResponse
	err := sonic.Unmarshal([]byte(d.Value), &result)
	if err != nil {
		return nil, err
	}

	d.parsedJsonRpcRes = &result
	return d.parsedJsonRpcRes, nil
}
