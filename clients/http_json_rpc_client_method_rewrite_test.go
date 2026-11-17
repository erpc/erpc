package clients

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoMethod answers a single request with the method it received as result.
func echoMethod(w http.ResponseWriter, body []byte) {
	var req map[string]interface{}
	if err := sonic.Unmarshal(body, &req); err != nil {
		http.Error(w, "not a single request", http.StatusBadRequest)
		return
	}
	out, _ := sonic.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req["id"], "result": req["method"]})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func TestHttpJsonRpcClient_MethodRewrite_Single(t *testing.T) {
	srv := newRecordingUpstream(t, echoMethod)
	c := newHttpTestClient(t, srv.URL, nil, nil, nil)
	c.upstream.Config().MethodRewrite = map[string]string{
		"custom_sendMevProtected": "eth_sendRawTransaction",
	}

	ctx := context.Background()

	req := rpcReq(1, "custom_sendMevProtected")
	resp, err := c.SendRequest(ctx, req)
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"eth_sendRawTransaction"`, jrr.GetResultString(), "upstream must receive the rewritten method")
	method, _ := req.Method()
	assert.Equal(t, "custom_sendMevProtected", method, "the request keeps the client's method")

	// A method without a mapping goes out unchanged.
	resp, err = c.SendRequest(ctx, rpcReq(2, "eth_chainId"))
	require.NoError(t, err)
	jrr, err = resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"eth_chainId"`, jrr.GetResultString())

	reqs := srv.requests()
	require.Len(t, reqs, 2)
	assert.Contains(t, string(reqs[0].body), `"method":"eth_sendRawTransaction"`)
	assert.NotContains(t, string(reqs[0].body), "custom_sendMevProtected")
	assert.Contains(t, string(reqs[1].body), `"method":"eth_chainId"`)
}

func TestHttpJsonRpcClient_MethodRewrite_Batch(t *testing.T) {
	srv := newRecordingUpstream(t, echoBatch)
	c := newHttpTestClient(t, srv.URL, batchCfg(10, 50*time.Millisecond), nil, nil)
	c.upstream.Config().MethodRewrite = map[string]string{
		"custom_sendMevProtected": "eth_sendRawTransaction",
	}

	ctx := context.Background()
	done := make(chan error, 2)
	for i, m := range []string{"custom_sendMevProtected", "eth_blockNumber"} {
		go func(id int, m string) {
			_, err := c.SendRequest(ctx, rpcReq(id, m))
			done <- err
		}(i+1, m)
	}
	for i := 0; i < 2; i++ {
		require.NoError(t, <-done)
	}

	var sent []map[string]interface{}
	for _, r := range srv.requests() {
		var batch []map[string]interface{}
		require.NoError(t, sonic.Unmarshal(r.body, &batch))
		sent = append(sent, batch...)
	}
	methods := map[string]bool{}
	for _, r := range sent {
		methods[r["method"].(string)] = true
	}
	assert.Equal(t, map[string]bool{"eth_sendRawTransaction": true, "eth_blockNumber": true}, methods)
}
