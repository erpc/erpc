package erpc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func buildJSONRPCRequest(method string, params interface{}) json.RawMessage {
	body, _ := sonic.Marshal(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      util.RandomID(),
		"method":  method,
		"params":  params,
	})
	return body
}

func parseJSONRPCResult(ctx context.Context, resp *common.NormalizedResponse) (json.RawMessage, error) {
	if resp == nil {
		return nil, fmt.Errorf("nil response")
	}
	defer resp.Release()

	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		return nil, err
	}
	if jrr == nil {
		return nil, fmt.Errorf("missing json-rpc response")
	}
	if jrr.Error != nil {
		return nil, jrr.Error
	}
	return json.RawMessage(jrr.GetResultBytes()), nil
}
