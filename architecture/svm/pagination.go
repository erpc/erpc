package svm

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
)

// The V2 scan methods page through accounts in pubkey order (32 bytes,
// big-endian), and providers disagree on what the cursor means:
//
//   - some answer the accounts AFTER paginationKey and return the last account
//     as the next key;
//   - others answer FROM paginationKey on and return the key after the last
//     account (the method's original semantics).
//
// A cursor one provider made therefore repeats an account on the other. eRPC
// serves one meaning on every provider — a page starts AT paginationKey — so a
// scan may move between upstreams on any page:
//
//   - the request carries pred(key), the key just before the cursor, so both
//     kinds answer from the cursor on, plus at most a leading pred(key);
//   - the response drops a leading pred(key), and a next key equal to the
//     page's last account becomes the key after it.
//
// scanOptionsIndex maps each V2 scan method to the param index of its config
// object, which carries paginationKey.
var scanOptionsIndex = map[string]int{
	"getProgramAccountsV2":      1,
	"getTokenAccountsByOwnerV2": 2,
}

// projectPreForward_scanCursor moves the request's paginationKey one key back.
// The zero key has nothing before it, so the request scans from the start
// instead. A key that is not a 32-byte base58 value goes out untouched: the
// upstream answers for it. Runs at the project layer, before the cache read,
// for the reason commitment injection does: it changes the CacheHash.
func projectPreForward_scanCursor(ctx context.Context, r *common.NormalizedRequest, method string) {
	idx, ok := scanOptionsIndex[method]
	if !ok {
		return
	}
	rpcReq, err := r.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return
	}
	rpcReq.Lock()
	defer rpcReq.Unlock()
	if idx >= len(rpcReq.Params) {
		return
	}
	cfg, ok := rpcReq.Params[idx].(map[string]interface{})
	if !ok {
		return
	}
	cursor, ok := cfg["paginationKey"].(string)
	if !ok {
		return
	}
	key, ok := decodePubkey(cursor)
	if !ok {
		return
	}
	if prev, ok := predPubkey(key); ok {
		cfg["paginationKey"] = encodePubkey(prev)
	} else {
		delete(cfg, "paginationKey")
	}
	rpcReq.InvalidateCacheHash()
}

// upstreamPostForward_scanCursor rewrites one upstream's page to the cursor
// meaning above. It runs per upstream, before consensus, cache and hedging see
// the page, so every provider's answer to one request is the same page.
func upstreamPostForward_scanCursor(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse, method string) {
	idx, ok := scanOptionsIndex[method]
	if !ok || resp == nil {
		return
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil {
		return
	}
	result := normalizeScanResult(jrr.GetResultBytes(), sentScanCursor(ctx, req, idx))
	if result == nil {
		return
	}
	normalized, err := common.NewJsonRpcResponseFromBytes(nil, result, nil)
	if err != nil || normalized.SetID(jrr.ID()) != nil {
		return
	}
	resp.WithJsonRpcResponse(normalized)
}

// sentScanCursor is the paginationKey the upstream was sent, or "".
func sentScanCursor(ctx context.Context, req *common.NormalizedRequest, idx int) string {
	rpcReq, err := req.JsonRpcRequest(ctx)
	if err != nil || rpcReq == nil {
		return ""
	}
	rpcReq.RLock()
	defer rpcReq.RUnlock()
	if idx >= len(rpcReq.Params) {
		return ""
	}
	cfg, ok := rpcReq.Params[idx].(map[string]interface{})
	if !ok {
		return ""
	}
	cursor, _ := cfg["paginationKey"].(string)
	return cursor
}

// normalizeScanResult returns the result with its page rewritten, or nil when
// the page needs no change or is not a V2 page. The page is the result itself,
// or its "value" when the caller asked withContext.
func normalizeScanResult(result []byte, sent string) []byte {
	var outer map[string]sonic.NoCopyRawMessage
	if common.SonicCfg.Unmarshal(result, &outer) != nil {
		return nil
	}
	if _, ok := outer["accounts"]; ok {
		if !normalizeScanPage(outer, sent) {
			return nil
		}
		return encodeRawObject(outer)
	}
	value, ok := outer["value"]
	if !ok {
		return nil
	}
	var page map[string]sonic.NoCopyRawMessage
	if common.SonicCfg.Unmarshal(value, &page) != nil {
		return nil
	}
	if _, ok := page["accounts"]; !ok || !normalizeScanPage(page, sent) {
		return nil
	}
	encoded := encodeRawObject(page)
	if encoded == nil {
		return nil
	}
	outer["value"] = encoded
	return encodeRawObject(outer)
}

// normalizeScanPage applies the two response rules to one page in place and
// reports whether it changed anything. A next key that is not the page's last
// account stays: a provider that answers after the key then names the last
// account its filter rejected, and a page that starts at that account rejects
// it again.
func normalizeScanPage(page map[string]sonic.NoCopyRawMessage, sent string) bool {
	var accounts []sonic.NoCopyRawMessage
	if common.SonicCfg.Unmarshal(page["accounts"], &accounts) != nil {
		return false
	}
	changed := false
	if sent != "" && len(accounts) > 0 && accountPubkey(accounts[0]) == sent {
		accounts = accounts[1:]
		page["accounts"] = encodeRawArray(accounts)
		// count, where a provider sends it, is the page's length.
		var count int64
		if raw, ok := page["count"]; ok && common.SonicCfg.Unmarshal(raw, &count) == nil && count > 0 {
			page["count"] = strconv.AppendInt(nil, count-1, 10)
		}
		changed = true
	}
	var next *string
	if common.SonicCfg.Unmarshal(page["paginationKey"], &next) != nil || next == nil || len(accounts) == 0 {
		return changed
	}
	if *next != accountPubkey(accounts[len(accounts)-1]) {
		return changed
	}
	key, ok := decodePubkey(*next)
	if !ok {
		return changed
	}
	if succ, ok := succPubkey(key); ok {
		page["paginationKey"] = strconv.AppendQuote(nil, encodePubkey(succ))
	} else {
		page["paginationKey"] = sonic.NoCopyRawMessage("null")
	}
	return true
}

func accountPubkey(raw sonic.NoCopyRawMessage) string {
	var a struct {
		Pubkey string `json:"pubkey"`
	}
	if common.SonicCfg.Unmarshal(raw, &a) != nil {
		return ""
	}
	return a.Pubkey
}

// encodeRawObject writes the fields in key order, so one page always encodes
// to the same bytes. nil when a key cannot be encoded.
func encodeRawObject(m map[string]sonic.NoCopyRawMessage) []byte {
	keys := make([]string, 0, len(m))
	size := 2
	for k, v := range m {
		keys = append(keys, k)
		size += len(k) + len(v) + 4
	}
	sort.Strings(keys)
	out := make([]byte, 0, size)
	out = append(out, '{')
	for i, k := range keys {
		if i > 0 {
			out = append(out, ',')
		}
		quoted, err := common.SonicCfg.Marshal(k)
		if err != nil {
			return nil
		}
		out = append(out, quoted...)
		out = append(out, ':')
		out = append(out, m[k]...)
	}
	return append(out, '}')
}

func encodeRawArray(items []sonic.NoCopyRawMessage) []byte {
	size := 2 + len(items)
	for _, it := range items {
		size += len(it)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, it := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, it...)
	}
	return append(out, ']')
}

// decodePubkey decodes a base58 string that is exactly 32 bytes.
func decodePubkey(s string) ([32]byte, bool) {
	var key [32]byte
	if s == "" || len(s) > 44 {
		return key, false
	}
	var b [33]byte // little-endian magnitude; one spare byte catches overflow
	size, zeros := 0, 0
	leading := true
	for i := range len(s) {
		d := strings.IndexByte(base58Alphabet, s[i])
		if d < 0 {
			return key, false
		}
		if leading && d == 0 {
			zeros++
			continue
		}
		leading = false
		carry := d
		for k := range size {
			carry += int(b[k]) * 58
			b[k] = byte(carry)
			carry >>= 8
		}
		for ; carry > 0; carry >>= 8 {
			if size == len(b) {
				return key, false
			}
			b[size] = byte(carry)
			size++
		}
	}
	if zeros+size != 32 {
		return key, false
	}
	for k := range size {
		key[31-k] = b[k]
	}
	return key, true
}

func encodePubkey(key [32]byte) string {
	zeros := 0
	for zeros < len(key) && key[zeros] == 0 {
		zeros++
	}
	var digits [44]byte // little-endian base58 digits; 58^44 > 2^256
	size := 0
	for _, v := range key[zeros:] {
		carry := int(v)
		for k := range size {
			carry += int(digits[k]) << 8
			digits[k] = byte(carry % 58)
			carry /= 58
		}
		for ; carry > 0; carry /= 58 {
			digits[size] = byte(carry % 58)
			size++
		}
	}
	out := make([]byte, zeros+size)
	for i := range zeros {
		out[i] = base58Alphabet[0]
	}
	for i := range size {
		out[zeros+i] = base58Alphabet[digits[size-1-i]]
	}
	return string(out)
}

// predPubkey is the key just before key, false for the zero key.
func predPubkey(key [32]byte) ([32]byte, bool) {
	for i := len(key) - 1; i >= 0; i-- {
		key[i]--
		if key[i] != 0xff {
			return key, true
		}
	}
	return key, false
}

// succPubkey is the key just after key, false for the largest key.
func succPubkey(key [32]byte) ([32]byte, bool) {
	for i := len(key) - 1; i >= 0; i-- {
		key[i]++
		if key[i] != 0 {
			return key, true
		}
	}
	return key, false
}
