package erpc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/ethereum/go-ethereum/accounts/abi"
	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	mc3TestEcho   = "0x1111111111111111111111111111111111111111" // returns its calldata
	mc3TestRevert = "0x2222222222222222222222222222222222222222" // always reverts
	mc3TestEmpty  = "0x3333333333333333333333333333333333333333" // returns nothing
	mc3TestSender = "0x4444444444444444444444444444444444444444" // reverts only when the caller is Multicall3
	mc3TestDeploy = 0x10                                         // Multicall3 has no code at or below this block
)

var mc3TestABI = func() abi.ABI {
	a, err := abi.JSON(strings.NewReader(`[{"name":"aggregate3","type":"function","stateMutability":"payable",
		"inputs":[{"name":"calls","type":"tuple[]","components":[{"name":"target","type":"address"},{"name":"allowFailure","type":"bool"},{"name":"callData","type":"bytes"}]}],
		"outputs":[{"name":"returnData","type":"tuple[]","components":[{"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}]}]}]`))
	if err != nil {
		panic(err)
	}
	return a
}()

// mc3TestNode is a fake EVM vendor: it runs eth_calls (single and through a real
// aggregate3 ABI codec) and counts what it was sent.
type mc3TestNode struct {
	mode       atomic.Value // "ok" | "error" | "garbage"
	mu         sync.Mutex
	multicalls int
	gasOfPacks map[string]int
	singles    map[string]int
}

func (f *mc3TestNode) reset(mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode.Store(mode)
	f.multicalls = 0
	f.gasOfPacks = map[string]int{}
	f.singles = map[string]int{}
}

func (f *mc3TestNode) counts() (int, map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := map[string]int{}
	for k, v := range f.singles {
		s[k] = v
	}
	return f.multicalls, s
}

func (f *mc3TestNode) run(to string, data []byte, viaMulticall bool) ([]byte, bool) {
	switch strings.ToLower(to) {
	case mc3TestEcho:
		return data, true
	case mc3TestEmpty:
		return nil, true
	case mc3TestSender:
		return data, !viaMulticall
	default:
		return []byte{0xde, 0xad, 0xbe, 0xef}, false
	}
}

func (f *mc3TestNode) answer(method string, params []interface{}) (interface{}, map[string]interface{}) {
	switch method {
	case "eth_chainId":
		return "0x7b", nil
	case "eth_blockNumber":
		return "0x100", nil
	case "eth_syncing":
		return false, nil
	case "eth_getBlockByNumber":
		return map[string]interface{}{"number": "0x100", "hash": "0x" + strings.Repeat("ab", 32), "timestamp": "0x1"}, nil
	case "eth_call":
	default:
		return nil, nil
	}
	call := params[0].(map[string]interface{})
	to := call["to"].(string)
	data, _ := hex.DecodeString(strings.TrimPrefix(call["data"].(string), "0x"))
	block, _ := params[1].(string)

	if !strings.EqualFold(to, "0xcA11bde05977b3631167028862bE2a173976CA11") {
		f.mu.Lock()
		f.singles[to]++
		f.mu.Unlock()
		out, ok := f.run(to, data, false)
		if !ok {
			return nil, map[string]interface{}{"code": 3, "message": "execution reverted", "data": "0x" + hex.EncodeToString(out)}
		}
		return "0x" + hex.EncodeToString(out), nil
	}

	f.mu.Lock()
	f.multicalls++
	gas, _ := call["gas"].(string)
	f.gasOfPacks[gas]++
	f.mu.Unlock()
	switch f.mode.Load().(string) {
	case "error":
		return nil, map[string]interface{}{"code": -32000, "message": "header not found"}
	case "garbage":
		return "0x1234", nil
	}
	if bn, err := common.HexToInt64(block); err == nil && bn <= mc3TestDeploy {
		return "0x", nil
	}
	method3 := mc3TestABI.Methods["aggregate3"]
	if len(data) < 4 || hex.EncodeToString(data[:4]) != hex.EncodeToString(method3.ID) {
		return nil, map[string]interface{}{"code": -32000, "message": "bad selector"}
	}
	args, err := method3.Inputs.Unpack(data[4:])
	if err != nil {
		return nil, map[string]interface{}{"code": -32000, "message": err.Error()}
	}
	type call3 struct {
		Target       ethcommon.Address
		AllowFailure bool
		CallData     []byte
	}
	type result3 struct {
		Success    bool
		ReturnData []byte
	}
	calls := *abi.ConvertType(args[0], new([]call3)).(*[]call3)
	results := make([]result3, len(calls))
	for i, c := range calls {
		out, ok := f.run(strings.ToLower(c.Target.Hex()), c.CallData, true)
		results[i] = result3{Success: ok, ReturnData: out}
	}
	packed, err := method3.Outputs.Pack(results)
	if err != nil {
		return nil, map[string]interface{}{"code": -32000, "message": err.Error()}
	}
	return "0x" + hex.EncodeToString(packed), nil
}

func (f *mc3TestNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     interface{}   `json:"id"`
		Method string        `json:"method"`
		Params []interface{} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	result, rpcErr := f.answer(req.Method, req.Params)
	resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func TestMulticall3Packing(t *testing.T) {
	node := &mc3TestNode{}
	node.reset("ok")
	srv := httptest.NewServer(node)
	defer srv.Close()

	cfg := &common.Config{
		Server: &common.ServerConfig{MaxTimeout: common.Duration(10 * time.Second).Ptr()},
		Projects: []*common.ProjectConfig{{
			Id: "test_project",
			Networks: []*common.NetworkConfig{{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
			}},
			Upstreams: []*common.UpstreamConfig{{
				Id:       "vendor",
				Type:     common.UpstreamTypeEvm,
				Endpoint: srv.URL,
				Evm: &common.EvmUpstreamConfig{
					ChainId:    123,
					Multicall3: &common.EvmMulticall3Config{MaxWait: common.Duration(300 * time.Millisecond), MaxCalls: 50},
				},
			}},
		}},
		RateLimiters: &common.RateLimiterConfig{},
	}
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	var seq atomic.Int64
	type call struct {
		to, block, from, gas string
		data                 string
	}
	newCall := func(to, block string) call {
		return call{to: to, block: block, data: fmt.Sprintf("0x%08x", seq.Add(1))}
	}
	type answer struct {
		Result string                 `json:"result"`
		Error  map[string]interface{} `json:"error"`
	}
	sendAll := func(calls []call) []answer {
		out := make([]answer, len(calls))
		var wg sync.WaitGroup
		for i, c := range calls {
			wg.Add(1)
			go func(i int, c call) {
				defer wg.Done()
				obj := fmt.Sprintf(`"to":"%s","data":"%s"`, c.to, c.data)
				if c.from != "" {
					obj += fmt.Sprintf(`,"from":"%s"`, c.from)
				}
				if c.gas != "" {
					obj += fmt.Sprintf(`,"gas":"%s"`, c.gas)
				}
				obj = "{" + obj + "}"
				status, _, body := sendRequest(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[%s,"%s"]}`, i+1, obj, c.block), nil, nil)
				require.Equal(t, http.StatusOK, status, body)
				require.NoError(t, json.Unmarshal([]byte(body), &out[i]), body)
			}(i, c)
		}
		wg.Wait()
		return out
	}
	assertEchoed := func(t *testing.T, calls []call, answers []answer) {
		for i, c := range calls {
			if c.to == mc3TestEcho || c.to == mc3TestSender {
				assert.Equal(t, c.data, answers[i].Result, "call %d to %s", i, c.to)
				assert.Nil(t, answers[i].Error, "call %d to %s", i, c.to)
			}
		}
	}

	t.Run("packs concurrent calls and sends alone each call the pack cannot answer exactly", func(t *testing.T) {
		node.reset("ok")
		calls := []call{
			newCall(mc3TestEcho, "latest"), newCall(mc3TestEcho, "latest"), newCall(mc3TestEcho, "latest"),
			newCall(mc3TestEcho, "latest"), newCall(mc3TestEcho, "latest"),
			newCall(mc3TestRevert, "latest"), newCall(mc3TestEmpty, "latest"), newCall(mc3TestSender, "latest"),
		}
		answers := sendAll(calls)

		assertEchoed(t, calls, answers)
		assert.Equal(t, "", answers[5].Result)
		require.NotNil(t, answers[5].Error)
		assert.Contains(t, fmt.Sprint(answers[5].Error["message"]), "execution reverted")
		assert.Equal(t, "0x", answers[6].Result)

		multicalls, singles := node.counts()
		assert.Equal(t, 1, multicalls)
		assert.Equal(t, map[string]int{mc3TestRevert: singles[mc3TestRevert], mc3TestEmpty: 1, mc3TestSender: 1}, singles)
		assert.GreaterOrEqual(t, singles[mc3TestRevert], 1)
	})

	for _, mode := range []string{"error", "garbage"} {
		t.Run("multicall "+mode+" sends every call alone", func(t *testing.T) {
			node.reset(mode)
			calls := []call{newCall(mc3TestEcho, "latest"), newCall(mc3TestEcho, "latest"), newCall(mc3TestEcho, "latest"), newCall(mc3TestEcho, "latest")}
			answers := sendAll(calls)

			assertEchoed(t, calls, answers)
			multicalls, singles := node.counts()
			assert.GreaterOrEqual(t, multicalls, 1)
			assert.Equal(t, map[string]int{mc3TestEcho: 4}, singles)
		})
	}

	t.Run("calls with a gas limit pack only with calls of the same limit", func(t *testing.T) {
		node.reset("ok")
		withGas := func() call {
			c := newCall(mc3TestEcho, "latest")
			c.gas = "0x2faf080"
			return c
		}
		calls := []call{withGas(), withGas(), withGas(), newCall(mc3TestEcho, "latest"), newCall(mc3TestEcho, "latest")}
		answers := sendAll(calls)

		assertEchoed(t, calls, answers)
		node.mu.Lock()
		defer node.mu.Unlock()
		assert.Equal(t, map[string]int{"0x2faf080": 1, "": 1}, node.gasOfPacks)
		assert.Empty(t, node.singles)
	})

	t.Run("lone and unpackable calls are sent alone without a multicall", func(t *testing.T) {
		node.reset("ok")
		withSender := newCall(mc3TestEcho, "latest")
		withSender.from = "0x5555555555555555555555555555555555555555"
		calls := []call{withSender, newCall(mc3TestEcho, "latest")}
		answers := sendAll(calls)

		assertEchoed(t, calls, answers)
		multicalls, singles := node.counts()
		assert.Equal(t, 0, multicalls)
		assert.Equal(t, map[string]int{mc3TestEcho: 2}, singles)
	})

	t.Run("blocks before Multicall3 existed are learned and then sent alone", func(t *testing.T) {
		node.reset("ok")
		calls := []call{newCall(mc3TestEcho, "0x10"), newCall(mc3TestEcho, "0x10"), newCall(mc3TestEcho, "0x10")}
		assertEchoed(t, calls, sendAll(calls))
		multicalls, singles := node.counts()
		assert.Equal(t, 1, multicalls)
		assert.Equal(t, map[string]int{mc3TestEcho: 3}, singles)

		node.reset("ok")
		calls = []call{newCall(mc3TestEcho, "0xf"), newCall(mc3TestEcho, "0xf"), newCall(mc3TestEcho, "0xf")}
		assertEchoed(t, calls, sendAll(calls))
		multicalls, singles = node.counts()
		assert.Equal(t, 0, multicalls)
		assert.Equal(t, map[string]int{mc3TestEcho: 3}, singles)

		node.reset("ok")
		calls = []call{newCall(mc3TestEcho, "0x20"), newCall(mc3TestEcho, "0x20")}
		assertEchoed(t, calls, sendAll(calls))
		multicalls, singles = node.counts()
		assert.Equal(t, 1, multicalls)
		assert.Empty(t, singles)
	})
}
