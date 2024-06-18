package upstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/flair-sdk/erpc/common"
	"github.com/rs/zerolog"
)

type NormalizedRequest struct {
	Network  common.Network
	Upstream *PreparedUpstream

	body []byte

	jsonRpcRequest *JsonRpcRequest

	DirectiveRetryEmpty bool
}

func NewNormalizedRequest(body []byte) *NormalizedRequest {
	return &NormalizedRequest{
		body: body,
	}
}

func (r *NormalizedRequest) Clone() *NormalizedRequest {
	return &NormalizedRequest{
		Network:             r.Network,
		Upstream:            r.Upstream,
		DirectiveRetryEmpty: r.DirectiveRetryEmpty,

		body:           r.body,
		jsonRpcRequest: r.jsonRpcRequest,
	}
}

func (r *NormalizedRequest) WithNetwork(network common.Network) *NormalizedRequest {
	r.Network = network
	return r
}

func (r *NormalizedRequest) WithUpstream(upstream *PreparedUpstream) *NormalizedRequest {
	r.Upstream = upstream
	return r
}

func (r *NormalizedRequest) ApplyDirectivesFromHttpHeaders(headers http.Header) *NormalizedRequest {
	r.DirectiveRetryEmpty = headers.Get("x-erpc-retry-empty") != "false"
	return r
}

// Extract and prepare the request for forwarding.
func (n *NormalizedRequest) JsonRpcRequest() (*JsonRpcRequest, error) {
	if n.jsonRpcRequest != nil {
		return n.jsonRpcRequest, nil
	}

	rpcReq := new(JsonRpcRequest)
	if err := json.Unmarshal(n.body, rpcReq); err != nil {
		return nil, common.NewErrJsonRpcRequestUnmarshal(err)
	}

	method := rpcReq.Method
	if method == "" {
		return nil, common.NewErrJsonRpcRequestUnresolvableMethod(rpcReq)
	}

	if rpcReq.JSONRPC == "" {
		rpcReq.JSONRPC = "2.0"
	}

	if rpcReq.ID == nil {
		rpcReq.ID = rand.Intn(math.MaxInt32)
	}

	n.jsonRpcRequest = rpcReq

	return rpcReq, nil
}

func (n *NormalizedRequest) Method() (string, error) {
	rpcReq, err := n.JsonRpcRequest()
	if err != nil {
		return "", err
	}

	return rpcReq.Method, nil
}

func (n *NormalizedRequest) MarshalZerologObject(e *zerolog.Event) {
	e.Str("body", string(n.body))
}

func (n *NormalizedRequest) EvmBlockNumber() (uint64, error) {
	rpcReq, err := n.JsonRpcRequest()
	if err != nil {
		return 0, err
	}

	_, bn, err := rpcReq.EvmBlockReference()
	if err != nil {
		return 0, err
	}

	return bn, nil
}

func (r *JsonRpcRequest) EvmBlockReference() (string, uint64, error) {
	if r == nil {
		return "", 0, errors.New("cannot extract block reference when json-rpc request is nil")
	}

	switch r.Method {
	case "eth_getBlockByNumber",
		"eth_getUncleByBlockNumberAndIndex",
		"eth_getTransactionByBlockNumberAndIndex",
		"eth_getUncleCountByBlockNumber",
		"eth_getBlockTransactionCountByNumber":
		if len(r.Params) > 0 {
			if bns, ok := r.Params[0].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := hexutil.DecodeUint64(bns)
					if err != nil {
						return "", 0, err
					}
					return bns, bni, nil
				} else {
					return "", 0, nil
				}
			}
		} else {
			return "", 0, fmt.Errorf("unexpected no parameters for method %s", r.Method)
		}

	case "eth_getBalance",
		"eth_getStorageAt",
		"eth_getCode",
		"eth_getTransactionCount",
		"eth_call",
		"eth_estimateGas":
		if len(r.Params) > 1 {
			if bns, ok := r.Params[1].(string); ok {
				if strings.HasPrefix(bns, "0x") {
					bni, err := hexutil.DecodeUint64(bns)
					if err != nil {
						return "", 0, err
					}
					return bns, bni, nil
				} else {
					return "", 0, nil
				}
			}
		} else {
			return "", 0, fmt.Errorf("unexpected missing 2nd parameter for method %s: %+v", r.Method, r.Params)
		}

	case "eth_getBlockByHash":
		if len(r.Params) > 0 {
			if blockHash, ok := r.Params[0].(string); ok {
				return blockHash, 0, nil
			}
			return "", 0, fmt.Errorf("first parameter is not a string for method %s it is %+v", r.Method, r.Params)
		}

	default:
		return "", 0, nil
	}

	return "", 0, nil
}
