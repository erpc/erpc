package evm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
)

func upstreamPostForward_eth_getTransactionReceipt(
	ctx context.Context,
	n common.Network,
	u common.Upstream,
	rq *common.NormalizedRequest,
	rs *common.NormalizedResponse,
	re error,
) (*common.NormalizedResponse, error) {
	if re != nil || rs == nil || rs.IsObjectNull() || !rs.IsResultEmptyish() {
		return rs, re
	}

	if rq != nil {
		if rd := rq.Directives(); rd != nil && !rd.RetryEmpty {
			return rs, re
		}
	}

	jrReq, err := rq.JsonRpcRequest()
	if err != nil || jrReq == nil || len(jrReq.Params) == 0 {
		return rs, re
	}

	txHash, ok := jrReq.Params[0].(string)
	if !ok || txHash == "" {
		return rs, re
	}

	ctx, span := common.StartDetailSpan(ctx, "PostForward.eth_getTransactionReceipt.pendingCheck")
	defer span.End()
	span.SetAttributes(attribute.String("tx_hash", txHash))

	getTxReq := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_getTransactionByHash","params":[%q]}`,
		util.RandomID(),
		txHash,
	)))

	txResp, txErr := u.Forward(ctx, getTxReq, true)
	if txResp != nil {
		defer txResp.Release()
	}
	if txErr != nil || txResp == nil || txResp.IsResultEmptyish() {
		span.SetAttributes(attribute.String("result", "tx_not_found"))
		rs.SetEmptyAccepted(true)
		return rs, re
	}

	jrr, err := txResp.JsonRpcResponse()
	if err != nil || jrr == nil || jrr.Error != nil {
		return rs, re
	}

	var txObj map[string]interface{}
	if uerr := json.Unmarshal(jrr.GetResultBytes(), &txObj); uerr != nil {
		return rs, re
	}

	blockNumber, exists := txObj["blockNumber"]
	if !exists || blockNumber == nil {
		span.SetAttributes(attribute.String("result", "tx_pending"))
		rs.SetEmptyAccepted(true)
		return rs, re
	}

	span.SetAttributes(
		attribute.String("result", "tx_mined"),
		attribute.String("block_number", fmt.Sprintf("%v", blockNumber)),
	)
	return rs, common.NewErrEndpointMissingData(
		fmt.Errorf("receipt is null but tx is mined in block %v", blockNumber),
		u,
	)
}
