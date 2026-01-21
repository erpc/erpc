package erpc

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHttpServer_BatchEthCall_MulticallAggregation(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	cfg := baseBatchConfig()
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	multicallAddr := strings.ToLower("0xcA11bde05977b3631167028862bE2a173976CA11")
	results := []evm.Multicall3Result{
		{Success: true, ReturnData: []byte{0xaa}},
		{Success: true, ReturnData: []byte{0xbb}},
	}
	resultHex := encodeAggregate3Results(results)

	gock.New("http://rpc1.localhost").
		Post("/").
		Times(1).
		Filter(func(request *http.Request) bool {
			body := strings.ToLower(util.SafeReadBody(request))
			return strings.Contains(body, "\"eth_call\"") && strings.Contains(body, multicallAddr)
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  resultHex,
		})

	batchBody := `[
		{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]},
		{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000002","data":"0x"},"latest"]}
	]`

	status, _, body := sendRequest(batchBody, nil, map[string]string{})
	assert.Equal(t, http.StatusOK, status)

	var responses []map[string]interface{}
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(body), &responses))
	require.Len(t, responses, 2)

	assert.Equal(t, float64(1), responses[0]["id"])
	assert.Equal(t, "0x"+hex.EncodeToString(results[0].ReturnData), responses[0]["result"])
	assert.Equal(t, float64(2), responses[1]["id"])
	assert.Equal(t, "0x"+hex.EncodeToString(results[1].ReturnData), responses[1]["result"])
}

func TestHttpServer_BatchEthCall_MulticallAggregationDisabled(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	// Create config with multicall3 aggregation disabled
	cfg := baseBatchConfig()
	cfg.Projects[0].Networks[0].Evm.Multicall3Aggregation = &common.Multicall3AggregationConfig{Enabled: false}

	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	// When multicall3 is disabled, each eth_call should be sent individually
	// So we mock 2 individual eth_call responses instead of 1 multicall
	gock.New("http://rpc1.localhost").
		Post("/").
		Times(2).
		Filter(func(request *http.Request) bool {
			body := util.SafeReadBody(request)
			return strings.Contains(body, "\"eth_call\"")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  "0xcc",
		})

	batchBody := `[
		{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000001","data":"0x"},"latest"]},
		{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":"0x0000000000000000000000000000000000000002","data":"0x"},"latest"]}
	]`

	status, _, body := sendRequest(batchBody, nil, map[string]string{})
	assert.Equal(t, http.StatusOK, status)

	var responses []map[string]interface{}
	require.NoError(t, common.SonicCfg.Unmarshal([]byte(body), &responses))
	require.Len(t, responses, 2)

	// Both responses should have the individual result
	assert.Equal(t, float64(1), responses[0]["id"])
	assert.Equal(t, "0xcc", responses[0]["result"])
	assert.Equal(t, float64(2), responses[1]["id"])
	assert.Equal(t, "0xcc", responses[1]["result"])
}
