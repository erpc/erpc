package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSvm_V2Scan_OneCursorAcrossVendors walks V2 scans whose pages alternate
// between two vendors with opposite cursor meanings, every page carrying a
// minContextSlot past both vendors' last poll. A third, fresher vendor does not
// serve V2 scans. The client must get every account once, in key order, and
// never the fresher vendor's "Method not found". Only the after-key vendor
// serves getTokenAccountsByOwnerV2, and it puts the page's accounts in "value",
// beside paginationKey.
func TestSvm_V2Scan_OneCursorAcrossVendors(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	const (
		fresh = "svm-scan-fresh.localhost" // past minContextSlot, no V2 scans
		after = "svm-scan-after.localhost" // page after the key, next key = last account
		from  = "svm-scan-from.localhost"  // page from the key, next key = last account + 1

		gpa      = "getProgramAccountsV2"
		tabo     = "getTokenAccountsByOwnerV2"
		notFound = `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`
	)
	util.SetupMocksForSvmStatePoller(fresh, 1000, 990)
	util.SetupMocksForSvmStatePoller(after, 990, 980)
	util.SetupMocksForSvmStatePoller(from, 990, 980)

	// Adjacent keys (0x00ff/0x0100, 0x03ff/0x0400) put a real account at the
	// key before a cursor, and carry across bytes in both directions.
	var keys [][]byte
	var want []string
	for _, suffix := range []uint16{0x0010, 0x00ff, 0x0100, 0x01fe, 0x0300, 0x03ff, 0x0400} {
		key := bytes.Repeat([]byte{0x42}, 32)
		key[30], key[31] = byte(suffix>>8), byte(suffix)
		keys = append(keys, key)
		want = append(want, scanB58Encode(key))
	}

	asks := func(host string, methods ...string) func(*http.Request) bool {
		return func(r *http.Request) bool {
			body := util.SafeReadBody(r)
			return r.URL.Host == host && slices.ContainsFunc(methods, func(m string) bool {
				return strings.Contains(body, `"method":"`+m+`"`)
			})
		}
	}
	var freshCalls atomic.Int32
	gock.New("http://" + fresh).Post("").Persist().
		Filter(asks(fresh, gpa, tabo)).
		Reply(200).
		Map(func(res *http.Response) *http.Response {
			freshCalls.Add(1)
			return res
		}).
		BodyString(notFound)
	gock.New("http://" + from).Post("").Persist().
		Filter(asks(from, tabo)).
		Reply(200).
		BodyString(notFound)
	served := map[string]*atomic.Int32{after: {}, from: {}}
	for host, startsAfterKey := range map[string]bool{after: true, from: false} {
		methods := []string{gpa}
		if startsAfterKey {
			methods = append(methods, tabo)
		}
		gock.New("http://" + host).Post("").Persist().
			Filter(asks(host, methods...)).
			Reply(200).
			Map(func(res *http.Response) *http.Response {
				served[host].Add(1)
				body := scanVendorPage(t, keys, startsAfterKey, util.SafeReadBody(res.Request))
				res.Header.Set("Content-Type", "application/json")
				res.ContentLength = int64(len(body))
				res.Body = io.NopCloser(bytes.NewReader(body))
				return res
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	net := setupTestSvmNetwork(t, ctx, []*common.UpstreamConfig{
		svmUpstreamConfig("fresh", fresh),
		svmUpstreamConfig("after", after),
		svmUpstreamConfig("from", from),
	})

	type account struct {
		Pubkey string `json:"pubkey"`
	}
	scan := func(method string, params func(cfg map[string]interface{}) []interface{}) []string {
		var got []string
		var cursor *string
		for page := range 2 * len(keys) {
			if page%2 == 0 {
				net.PinUpstreamOrderForTest("fresh", "after", "from")
			} else {
				net.PinUpstreamOrderForTest("fresh", "from", "after")
			}
			cfg := map[string]interface{}{"encoding": "base64", "limit": 2, "minContextSlot": 995}
			if cursor != nil {
				cfg["paginationKey"] = *cursor
			}
			body, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": page + 1, "method": method, "params": params(cfg)})
			require.NoError(t, err)
			resp, err := svmProjectForward(ctx, net, common.NewNormalizedRequest(body))
			require.NoError(t, err, "%s page %d", method, page)
			jrr, err := resp.JsonRpcResponse(ctx)
			require.NoError(t, err)
			var result struct {
				Accounts      []account `json:"accounts"`
				Value         []account `json:"value"`
				Count         *int      `json:"count"`
				PaginationKey *string   `json:"paginationKey"`
			}
			require.NoError(t, json.Unmarshal(jrr.GetResultBytes(), &result), "%s page %d: %s", method, page, jrr.GetResultBytes())
			accounts := append(result.Accounts, result.Value...)
			if result.Count != nil {
				assert.Equal(t, len(accounts), *result.Count, "%s page %d: count is the page's length", method, page)
			}
			for _, a := range accounts {
				got = append(got, a.Pubkey)
			}
			if result.PaginationKey == nil {
				break
			}
			cursor = result.PaginationKey
		}
		return got
	}

	const tokenProgram = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	assert.Equal(t, want, scan(gpa, func(cfg map[string]interface{}) []interface{} {
		return []interface{}{tokenProgram, cfg}
	}), "%s: each account once, in key order", gpa)
	assert.Positive(t, served[after].Load(), "the after-key vendor served pages")
	assert.Positive(t, served[from].Load(), "the from-key vendor served pages")

	assert.Equal(t, want, scan(tabo, func(cfg map[string]interface{}) []interface{} {
		return []interface{}{"5Q544fKrFoe6tsEbD7S8EmxGTJYAKtTVhAW5Q5pge4j1", map[string]interface{}{"programId": tokenProgram}, cfg}
	}), "%s: each account once, in key order", tabo)
	assert.Positive(t, freshCalls.Load(), "the fresher vendor is still asked first")
}

// scanVendorPage answers one V2 scan request with 2 accounts the way one vendor
// dialect does.
func scanVendorPage(t *testing.T, keys [][]byte, startsAfterKey bool, reqBody string) []byte {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal([]byte(reqBody), &req))
	cfgIdx := 1
	if req.Method == "getTokenAccountsByOwnerV2" {
		cfgIdx = 2
	}
	var cfg struct {
		PaginationKey *string `json:"paginationKey"`
	}
	require.NoError(t, json.Unmarshal(req.Params[cfgIdx], &cfg))
	start := 0
	if cfg.PaginationKey != nil {
		cursor := scanB58Decode32(*cfg.PaginationKey)
		start = sort.Search(len(keys), func(i int) bool {
			c := bytes.Compare(keys[i], cursor)
			return c > 0 || (c == 0 && !startsAfterKey)
		})
	}
	end := min(start+2, len(keys))
	type account struct {
		Pubkey  string                 `json:"pubkey"`
		Account map[string]interface{} `json:"account"`
	}
	accounts := []account{}
	for _, k := range keys[start:end] {
		accounts = append(accounts, account{Pubkey: scanB58Encode(k), Account: map[string]interface{}{
			"lamports": 2039280, "owner": "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA",
			"data": []string{"", "base64"}, "executable": false, "rentEpoch": 0, "space": 0,
		}})
	}
	var next interface{}
	if end < len(keys) {
		last := new(big.Int).SetBytes(keys[end-1])
		if !startsAfterKey {
			last.Add(last, big.NewInt(1))
		}
		next = scanB58Encode(last.FillBytes(make([]byte, 32)))
	}
	result := map[string]interface{}{"accounts": accounts, "count": len(accounts), "paginationKey": next}
	if req.Method == "getTokenAccountsByOwnerV2" {
		// Alchemy without withContext: the accounts sit in "value", beside paginationKey.
		result = map[string]interface{}{"value": accounts, "paginationKey": next}
	}
	body, err := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": req.ID, "result": result})
	require.NoError(t, err)
	return body
}

const scanB58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func scanB58Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	base, mod := big.NewInt(58), new(big.Int)
	var out []byte
	for n.Sign() > 0 {
		n.DivMod(n, base, mod)
		out = append(out, scanB58Alphabet[mod.Int64()])
	}
	for _, c := range b {
		if c != 0 {
			break
		}
		out = append(out, '1')
	}
	slices.Reverse(out)
	return string(out)
}

func scanB58Decode32(s string) []byte {
	n := new(big.Int)
	base := big.NewInt(58)
	for i := range len(s) {
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(strings.IndexByte(scanB58Alphabet, s[i]))))
	}
	return n.FillBytes(make([]byte, 32))
}
