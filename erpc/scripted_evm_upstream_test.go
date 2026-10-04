package erpc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// scriptedEvmUpstream is a deterministic local JSON-RPC EVM node used by the
// block store end-to-end tests. It serves a scripted chain where every block
// has one transaction emitting one log.
//
// Limits (honest fixture scope): no real EVM execution, no receipts/traces,
// no uncles/withdrawals; only the methods eRPC's state poller and the block
// store use are implemented. Unknown methods return -32601.
type scriptedEvmUpstream struct {
	mu            sync.Mutex
	chainId       int64
	tip           int64
	forks         map[int64]string
	srv           *httptest.Server
	rangeLogCalls atomic.Int64
	// Block store knobs: unfiltered range calls (no address/topics) are
	// counted separately, can be delayed, failed, or marked removed.
	unfilteredLogCalls atomic.Int64
	unfilteredLogDelay atomic.Int64
	failUnfilteredLogs atomic.Bool
	removedUnfiltered  atomic.Bool
	emptyLogHeights    map[int64]bool
}

func (u *scriptedEvmUpstream) UnfilteredLogCalls() int64 { return u.unfilteredLogCalls.Load() }
func (u *scriptedEvmUpstream) SetEmptyLogs(heights ...int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, h := range heights {
		u.emptyLogHeights[h] = true
	}
}

var scriptedEmitter = "0x5fbdb2315678afecb367f032d93f642f64180aa3"
var scriptedTopicEven = "0x1111111111111111111111111111111111111111111111111111111111111111"
var scriptedTopicOdd = "0x2222222222222222222222222222222222222222222222222222222222222222"

func newScriptedEvmUpstream(chainId, tip int64) *scriptedEvmUpstream {
	u := &scriptedEvmUpstream{chainId: chainId, tip: tip, forks: map[int64]string{}, emptyLogHeights: map[int64]bool{}}
	for i := int64(0); i <= tip; i++ {
		u.forks[i] = "a"
	}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	return u
}

func (u *scriptedEvmUpstream) Close()      { u.srv.Close() }
func (u *scriptedEvmUpstream) URL() string { return u.srv.URL }

// RangeLogCalls counts eth_getLogs calls that used fromBlock/toBlock.
func (u *scriptedEvmUpstream) RangeLogCalls() int64 { return u.rangeLogCalls.Load() }

func (u *scriptedEvmUpstream) HashAt(n int64) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return scriptedHash(n, u.forks[n])
}

func scriptedHash(n int64, fork string) string {
	return ethcommon.BytesToHash([]byte(fmt.Sprintf("blk-%d-%s", n, fork))).Hex()
}
func scriptedTx(n int64, fork string) string {
	return ethcommon.BytesToHash([]byte(fmt.Sprintf("tx-%d-%s", n, fork))).Hex()
}

func (u *scriptedEvmUpstream) logLocked(n int64) map[string]interface{} {
	f := u.forks[n]
	topic := scriptedTopicEven
	if n%2 == 1 {
		topic = scriptedTopicOdd
	}
	return map[string]interface{}{
		"address": scriptedEmitter, "topics": []string{topic}, "data": "0x",
		"blockNumber": fmt.Sprintf("0x%x", n), "blockHash": scriptedHash(n, f),
		"transactionHash": scriptedTx(n, f), "transactionIndex": "0x0", "logIndex": "0x0", "removed": false,
	}
}

func (u *scriptedEvmUpstream) blockLocked(n int64, full bool) interface{} {
	f, ok := u.forks[n]
	if !ok || n > u.tip || n < 0 {
		return nil
	}
	l := u.logLocked(n)
	var bloom types.Bloom
	bloom.Add(ethcommon.HexToAddress(scriptedEmitter).Bytes())
	for _, t := range l["topics"].([]string) {
		bloom.Add(ethcommon.HexToHash(t).Bytes())
	}
	parent := "0x0000000000000000000000000000000000000000000000000000000000000000"
	if n > 0 {
		parent = scriptedHash(n-1, u.forks[n-1])
	}
	var txs interface{} = []string{scriptedTx(n, f)}
	if full {
		txs = []map[string]interface{}{{
			"hash": scriptedTx(n, f), "from": scriptedEmitter, "to": scriptedEmitter,
			"blockHash": scriptedHash(n, f), "blockNumber": fmt.Sprintf("0x%x", n), "transactionIndex": "0x0",
		}}
	}
	return map[string]interface{}{
		"number": fmt.Sprintf("0x%x", n), "hash": scriptedHash(n, f), "parentHash": parent,
		"logsBloom": "0x" + ethcommon.Bytes2Hex(bloom.Bytes()), "timestamp": fmt.Sprintf("0x%x", 1_700_000_000+n),
		"gasLimit": "0x1c9c380", "gasUsed": "0x5208", "miner": scriptedEmitter, "extraData": "0x",
		"transactions": txs, "uncles": []string{},
	}
}

func (u *scriptedEvmUpstream) resolveLocked(ref string) int64 {
	switch ref {
	case "latest", "pending", "safe", "finalized", "":
		if ref == "finalized" || ref == "safe" {
			if u.tip > 64 {
				return u.tip - 64
			}
			return 0
		}
		return u.tip
	case "earliest":
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(ref, "0x"), 16, 64)
	if err != nil {
		return -1
	}
	return n
}

func (u *scriptedEvmUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "[") {
		var reqs []json.RawMessage
		_ = json.Unmarshal(body, &reqs)
		out := make([]json.RawMessage, 0, len(reqs))
		for _, rq := range reqs {
			out = append(out, u.handle(rq))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(u.handle(body))
}

func (u *scriptedEvmUpstream) handle(raw []byte) json.RawMessage {
	var req struct {
		Id     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(raw, &req)
	unfiltered := false
	if req.Method == "eth_getLogs" && len(req.Params) > 0 {
		var flt map[string]interface{}
		_ = json.Unmarshal(req.Params[0], &flt)
		_, hasAddr := flt["address"]
		_, hasTopics := flt["topics"]
		_, hasHash := flt["blockHash"]
		if !hasAddr && !hasTopics && !hasHash {
			unfiltered = true
			u.unfilteredLogCalls.Add(1)
			time.Sleep(time.Duration(u.unfilteredLogDelay.Load()))
			if u.failUnfilteredLogs.Load() {
				b, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.Id,
					"error": map[string]interface{}{"code": -32000, "message": "scripted unfiltered logs failure"}})
				return b
			}
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	var result interface{}
	var rpcErr map[string]interface{}
	switch req.Method {
	case "eth_chainId":
		result = fmt.Sprintf("0x%x", u.chainId)
	case "net_version":
		result = fmt.Sprintf("%d", u.chainId)
	case "eth_blockNumber":
		result = fmt.Sprintf("0x%x", u.tip)
	case "eth_syncing":
		result = false
	case "eth_getBlockByNumber":
		var ref string
		var full bool
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &ref)
		}
		if len(req.Params) > 1 {
			_ = json.Unmarshal(req.Params[1], &full)
		}
		result = u.blockLocked(u.resolveLocked(ref), full)
	case "eth_getBlockByHash":
		var h string
		var full bool
		_ = json.Unmarshal(req.Params[0], &h)
		if len(req.Params) > 1 {
			_ = json.Unmarshal(req.Params[1], &full)
		}
		for n, f := range u.forks {
			if strings.EqualFold(scriptedHash(n, f), h) && n <= u.tip {
				result = u.blockLocked(n, full)
			}
		}
	case "eth_getLogs":
		var flt map[string]interface{}
		_ = json.Unmarshal(req.Params[0], &flt)
		logs := []interface{}{}
		if bh, ok := flt["blockHash"].(string); ok {
			for n, f := range u.forks {
				if strings.EqualFold(scriptedHash(n, f), bh) && n <= u.tip {
					logs = append(logs, u.logLocked(n))
				}
			}
		} else {
			u.rangeLogCalls.Add(1)
			from := u.resolveLocked(fmt.Sprint(flt["fromBlock"]))
			to := u.resolveLocked(fmt.Sprint(flt["toBlock"]))
			for n := from; n <= to && n <= u.tip; n++ {
				if u.emptyLogHeights[n] {
					continue
				}
				l := u.logLocked(n)
				if unfiltered && u.removedUnfiltered.Load() {
					l["removed"] = true
				}
				if topics, ok := flt["topics"].([]interface{}); ok && len(topics) > 0 {
					if t0, ok := topics[0].(string); ok && !strings.EqualFold(t0, l["topics"].([]string)[0]) {
						continue
					}
				}
				logs = append(logs, l)
			}
		}
		result = logs
	default:
		rpcErr = map[string]interface{}{"code": -32601, "message": "the method " + req.Method + " does not exist/is not available"}
	}
	resp := map[string]interface{}{"jsonrpc": "2.0", "id": req.Id}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	b, _ := json.Marshal(resp)
	return b
}
