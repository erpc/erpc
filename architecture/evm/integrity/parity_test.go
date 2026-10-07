package integrity

// Legacy-vs-new oracle. Every response here goes through the current
// Validate AND through internal/legacy (a frozen copy of this package from
// before the shared block decode), and the two must agree on every check's
// outcome, the rejecting check ID and the record list. The fuzzers extend the
// same oracle to arbitrary documents and transactions.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/erpc/erpc/architecture/evm/integrity/internal/legacy"
	"github.com/erpc/erpc/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
)

// verdict is everything about a Validate result the parity tests compare.
type verdict struct {
	Outcomes []string
	Rejected string
	Err      bool
	Recorded []string
}

func (v verdict) String() string {
	return fmt.Sprintf("rej=%q err=%v rec=%v outcomes=%v", v.Rejected, v.Err, v.Recorded, v.Outcomes)
}

type parityReq struct {
	method  string
	params  []any
	chainID int64
	level   Level
}

func blockReq(method string, ref any, full bool) parityReq {
	return parityReq{method: method, params: []any{ref, full}, chainID: 1, level: LevelIntrinsic}
}

func parityResponse(t testing.TB, r parityReq, raw []byte) *common.NormalizedResponse {
	params, err := json.Marshal(r.params)
	if err != nil {
		t.Fatal(err)
	}
	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, r.method, params)))
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte("1"), raw, nil)
	if err != nil {
		return nil
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func runNew(t testing.TB, r parityReq, raw []byte) verdict {
	cs := CheckSetForLevel(r.level)
	ApplyChainProfile(cs, r.chainID)
	rs := parityResponse(t, r, raw)
	if rs == nil {
		return verdict{Rejected: "<unparseable>"}
	}
	res := Validate(context.Background(), Input{
		Reorg: DefaultReorgPolicy(), Method: r.method, Upstream: common.NewFakeUpstream("u"),
		Response: rs, Checks: cs, Params: r.params,
	})
	v := verdict{Rejected: res.RejectedCheckID, Err: res.Err != nil}
	for _, o := range res.Outcomes {
		v.Outcomes = append(v.Outcomes, o.CheckID+"="+o.Outcome)
	}
	for _, rec := range res.Recorded {
		v.Recorded = append(v.Recorded, rec.CheckID+"/"+rec.Verdict)
	}
	return v
}

func runLegacy(t testing.TB, r parityReq, raw []byte) verdict {
	cs := legacy.CheckSetForLevel(legacy.Level(r.level))
	legacy.ApplyChainProfile(cs, r.chainID)
	rs := parityResponse(t, r, raw)
	if rs == nil {
		return verdict{Rejected: "<unparseable>"}
	}
	res := legacy.Validate(context.Background(), legacy.Input{
		Reorg: legacy.DefaultReorgPolicy(), Method: r.method, Upstream: common.NewFakeUpstream("u"),
		Response: rs, Checks: cs, Params: r.params,
	})
	v := verdict{Rejected: res.RejectedCheckID, Err: res.Err != nil}
	for _, o := range res.Outcomes {
		v.Outcomes = append(v.Outcomes, o.CheckID+"="+o.Outcome)
	}
	for _, rec := range res.Recorded {
		v.Recorded = append(v.Recorded, rec.CheckID+"/"+rec.Verdict)
	}
	return v
}

// assertParity runs raw through both implementations and returns the new
// verdict.
func assertParity(t testing.TB, name string, r parityReq, raw []byte) verdict {
	t.Helper()
	got, want := runNew(t, r, raw), runLegacy(t, r, raw)
	// The legacy tx views go through a map[string]any -> Marshal round trip
	// whose key order is Go's randomized map order, so a transaction object
	// with case-insensitively duplicate keys ("Hash" and "hash") makes the
	// legacy verdict itself vary between runs. The new code sends such
	// objects down that same original path. So agreement means: the new
	// verdict is one legacy can produce.
	for i := 0; i < 64 && got.String() != want.String(); i++ {
		want = runLegacy(t, r, raw)
	}
	if got.String() != want.String() {
		t.Errorf("%s: verdicts differ\n  legacy: %s\n  new:    %s", name, want, got)
	}
	return got
}

func outcomeIn(v verdict, check string) string {
	for _, o := range v.Outcomes {
		if id, out, _ := strings.Cut(o, "="); id == check {
			return out
		}
	}
	return ""
}

// blockVariants derives the tampering cases from an honest block document.
func blockVariants(t testing.TB, raw []byte) map[string][]byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	with := func(k string, v []byte) []byte {
		m2 := make(map[string]json.RawMessage, len(m)+1)
		for kk, vv := range m {
			m2[kk] = vv
		}
		if v == nil {
			delete(m2, k)
		} else {
			m2[k] = v
		}
		b, err := json.Marshal(m2)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var txs []map[string]any
	if err := json.Unmarshal(m["transactions"], &txs); err != nil {
		t.Fatal(err)
	}
	marshal := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tampered := make([]map[string]any, len(txs))
	for i, tx := range txs {
		c := map[string]any{}
		for k, v := range tx {
			c[k] = v
		}
		tampered[i] = c
	}
	tampered[3]["nonce"] = "0x99999" // the claimed hash no longer recomputes
	hashes := make([]any, len(txs))
	for i, tx := range txs {
		hashes[i] = tx["hash"]
	}
	// A transaction substituted wholesale (hash recomputes, root does not).
	// Indices are swapped too, so the positional txBlockInfo check passes and
	// only the root recompute can notice.
	swapped := make([]map[string]any, len(txs))
	copy(swapped, tampered)
	swapped[3] = txs[3]
	a, b := map[string]any{}, map[string]any{}
	for k, v := range txs[0] {
		a[k] = v
	}
	for k, v := range txs[1] {
		b[k] = v
	}
	a["transactionIndex"], b["transactionIndex"] = b["transactionIndex"], a["transactionIndex"]
	swapped[0], swapped[1] = b, a

	return map[string][]byte{
		"honest":           raw,
		"tampered-tx":      with("transactions", marshal(tampered)),
		"reordered-txs":    with("transactions", marshal(swapped)),
		"tampered-header":  with("gasUsed", []byte(`"0x1"`)),
		"custom-field":     with("customField", []byte(`"0x1"`)),
		"hash-only":        with("transactions", marshal(hashes)),
		"no-transactions":  with("transactions", nil),
		"null-txs":         with("transactions", []byte(`null`)),
		"empty-txs":        with("transactions", []byte(`[]`)),
		"empty-root-txs":   with("transactionsRoot", []byte(`"`+emptyTrieRoot+`"`)),
		"zero-root-hashes": bytes.Replace(with("transactions", marshal(hashes)), m["transactionsRoot"], []byte(`"`+zeroHash32+`"`), 1),
		"pretty-printed":   indentJSON(t, raw),
		"escaped-key":      bytes.Replace(raw, []byte(`"gasUsed"`), []byte(`"gas\u0055sed"`), 1),
		"upper-key":        bytes.Replace(raw, []byte(`"gasUsed"`), []byte(`"GASUSED"`), 1),
		"dup-key":          append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"gasUsed":"0x2"}`)...),
		"bad-utf8-extra":   spliceMember(raw, "\"x\":\"\xff\""),
		"ctl-char-extra":   spliceMember(raw, "\"extraNote\":\"0x\x01\""),
		"float-overflow":   spliceMember(raw, `"size2":1e400`),
		"number-hash":      with("hash", []byte(`12`)),
		"truncated":        raw[:len(raw)/2],
	}
}

// spliceMember appends a member to the top-level object verbatim, so the
// member may hold bytes json.Marshal refuses to emit.
func spliceMember(raw []byte, member string) []byte {
	out := append([]byte{}, raw[:bytes.LastIndexByte(raw, '}')]...)
	return append(append(out, ','), member+"}"...)
}

func indentJSON(t testing.TB, raw []byte) []byte {
	var b bytes.Buffer
	if err := json.Indent(&b, raw, "", "  "); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestValidateParityWithLegacy: identical outcomes and rejecting check, legacy
// vs shared decode, on signed 150- and 1000-tx blocks and their tampered
// variants, for both block methods.
func TestValidateParityWithLegacy(t *testing.T) {
	for _, n := range []int{150, 1000} {
		raw := realisticSignedBlock(t, 0x1234, n)
		var hdr struct{ Hash string }
		if err := json.Unmarshal(raw, &hdr); err != nil {
			t.Fatal(err)
		}
		variants := blockVariants(t, raw)
		names := make([]string, 0, len(variants))
		for k := range variants {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			body := variants[name]
			for _, r := range []parityReq{
				blockReq("eth_getBlockByNumber", "0x1234", true),
				blockReq("eth_getBlockByHash", hdr.Hash, true),
			} {
				v := assertParity(t, fmt.Sprintf("n=%d %s %s", n, name, r.method), r, body)
				t.Logf("n=%d %-16s %-21s rej=%q root=%s hash=%s", n, name, r.method, v.Rejected,
					outcomeIn(v, "transactionsRootRecompute"), outcomeIn(v, "blockHashRecompute"))
			}
		}

		// The honest block must actually exercise both recomputes, or the
		// parity above proves little.
		v := runNew(t, blockReq("eth_getBlockByNumber", "0x1234", true), raw)
		if outcomeIn(v, "transactionsRootRecompute") != "pass" || outcomeIn(v, "blockHashRecompute") != "pass" {
			t.Fatalf("n=%d honest block did not run the recomputes: %s", n, v)
		}
		if tv := runNew(t, blockReq("eth_getBlockByNumber", "0x1234", true), variants["reordered-txs"]); tv.Rejected != "transactionsRootRecompute" {
			t.Fatalf("n=%d reordered txs not rejected by the root recompute: %s", n, tv)
		}
	}
}

// TestValidateParityPhantomBlocks covers the one path that reads transaction
// contents from the header list: transactionsRootConsistency on an empty
// root (system/phantom transactions).
func TestValidateParityPhantomBlocks(t *testing.T) {
	const blk = `{"hash":"0x%064x","number":"0x10","parentHash":"0x%064x","transactionsRoot":"%s","receiptsRoot":"0x%064x","stateRoot":"0x%064x","logsBloom":"0x%0512x","transactions":%s}`
	sys := `{"hash":"0x%064x","from":"0x0000000000000000000000000000000000000000","gas":"0x0","blockNumber":"0x10","blockHash":"0x%064x","transactionIndex":"0x%x"}`
	hyper := `{"hash":"0x%064x","from":"0x2222222222222222222222222222222222222222","gas":"0x5208","r":"0x1","gasPrice":"0x0","blockNumber":"0x10","blockHash":"0x%064x","transactionIndex":"0x%x"}`
	real := `{"hash":"0x%064x","from":"0x1111111111111111111111111111111111111111","gas":"0x5208","r":"0xabc","gasPrice":"0x3b9aca00","blockNumber":"0x10","blockHash":"0x%064x","transactionIndex":"0x%x"}`
	for name, txs := range map[string]string{
		"all-system":  "[" + fmt.Sprintf(sys, 1, 0xb, 0) + "," + fmt.Sprintf(hyper, 2, 0xb, 1) + "]",
		"system+real": "[" + fmt.Sprintf(sys, 1, 0xb, 0) + "," + fmt.Sprintf(real, 2, 0xb, 1) + "]",
		"hash-only":   fmt.Sprintf(`["0x%064x"]`, 1),
		"mixed":       "[" + fmt.Sprintf(sys, 1, 0xb, 0) + fmt.Sprintf(`,"0x%064x"]`, 2),
		"bad-index":   "[" + fmt.Sprintf(sys, 1, 0xb, 7) + "]",
		"wrong-block": "[" + fmt.Sprintf(real, 1, 0xc, 0) + "]",
		"dup-hash":    "[" + fmt.Sprintf(real, 1, 0xb, 0) + "," + fmt.Sprintf(real, 1, 0xb, 1) + "]",
		"num-gas":     `[{"hash":"0x01","from":"0x0","gas":0}]`,
		"empty-obj":   `[{}]`,
		"nested":      `[{"from":"0x0","gas":"0x0","x":[[[{"y":null}]]]}]`,
	} {
		for _, root := range []string{emptyTrieRoot, zeroHash32, "0x" + strings.Repeat("ab", 32)} {
			raw := []byte(fmt.Sprintf(blk, 0xb, 0xa, root, 0xc, 0xd, 0, txs))
			assertParity(t, name+"/"+root[:6], blockReq("eth_getBlockByNumber", "0x10", true), raw)
		}
	}
}

// TestValidateParityPathological: documents on which sonic and encoding/json
// disagree, or that sit at decoder limits. All must take the exact path.
func TestValidateParityPathological(t *testing.T) {
	deep := func(d int) string { return strings.Repeat("[", d) + strings.Repeat("]", d) }
	docs := []string{
		``, `null`, `[]`, `{}`, `"x"`, `5`, `true`, ` {} `, `{"hash":"0x1"} trailing`,
		`{"hash":5}`, `{"transactions":"x"}`, `{"transactions":{}}`, `{"transactions":[1e400]}`,
		`{"transactions":[{"v":1e400}]}`, `{"size":1e400,"transactions":[]}`,
		`{"hash":"0x1","hash":"0x2"}`, `{"HASH":"0x1","hash":"0x2"}`, `{"transactions":[],"Transactions":null}`,
		`{"hash":"\u0030x1"}`, "{\"hash\":\"0x\x01\"}", "{\"hash\":\"\xff\"}", "{\"h\u017fash\":\"1\"}",
		`{"foo":[tru],"hash":"0x1"}`, `{"transactions":[{"hash":"0x1","x":[tru]}]}`,
		`{"transactions":` + deep(4094) + `}`, `{"transactions":` + deep(4096) + `}`,
		`{"transactions":[{"x":` + deep(4095) + `}]}`, `{"foo":` + deep(5000) + `,"transactions":[]}`,
		`{"transactions":[{"x":` + deep(70) + `}]}`, `{"transactionsRoot":"` + emptyTrieRoot + `","transactions":[{"from":"0x0","gas":"0x0","n":1e5}]}`,
		`{"transactions":[` + strings.Repeat("9", 400) + `]}`,
		`{"transactionsRoot":"` + emptyTrieRoot + `","transactions":[{"from":"0x0","gas":"0x0","x":1e400}]}`,
	}
	for i, doc := range docs {
		for _, m := range []string{"eth_getBlockByNumber", "eth_getBlockByHash"} {
			assertParity(t, fmt.Sprintf("doc %d %.40q", i, doc), blockReq(m, "0x1", true), []byte(doc))
		}
	}
}

// TestValidateParityFixtures: the captured Arbitrum responses (every method
// they cover) plus, when INTEGRITY_MAINNET_FIXTURES points at a directory of
// {"result":...} block captures, real Ethereum mainnet blocks.
func TestValidateParityFixtures(t *testing.T) {
	for name, fx := range loadRealFixtures(t, "arbitrum") {
		for _, lvl := range []Level{LevelIntrinsic, LevelAuthoritative} {
			r := parityReq{method: fx.Method, params: fx.Params, chainID: 42161, level: lvl}
			assertParity(t, name+"/"+string(lvl), r, fx.Result)
			r.chainID = 1
			assertParity(t, name+"/eth/"+string(lvl), r, fx.Result)
		}
	}

	dir := os.Getenv("INTEGRITY_MAINNET_FIXTURES")
	if dir == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(dir, "blk-*.json"))
	if len(files) == 0 {
		t.Fatalf("INTEGRITY_MAINNET_FIXTURES=%s has no blk-*.json", dir)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatal(err)
		}
		var hdr struct{ Number, Hash string }
		_ = json.Unmarshal(env.Result, &hdr)
		for _, r := range []parityReq{
			blockReq("eth_getBlockByNumber", hdr.Number, true),
			blockReq("eth_getBlockByHash", hdr.Hash, true),
		} {
			v := assertParity(t, filepath.Base(f)+" "+r.method, r, env.Result)
			t.Logf("%s %s rej=%q root=%s hash=%s", filepath.Base(f), r.method, v.Rejected,
				outcomeIn(v, "transactionsRootRecompute"), outcomeIn(v, "blockHashRecompute"))
		}
	}
}

// FuzzValidateBlockParity mutates whole block documents and requires the
// shared decode to agree with the legacy decoders on every check.
func FuzzValidateBlockParity(f *testing.F) {
	raw := realisticSignedBlock(f, 0x1234, 6)
	for _, v := range blockVariants(f, raw) {
		f.Add(v)
	}
	f.Add([]byte(`{"transactionsRoot":"` + emptyTrieRoot + `","transactions":[{"from":"0x0","gas":"0x0"}]}`))
	f.Fuzz(func(t *testing.T, doc []byte) {
		assertParity(t, "fuzz", blockReq("eth_getBlockByNumber", "0x1234", true), doc)
	})
}

// --- per-transaction decoder parity ---

func gethDecode(b []byte) (*gethtypes.Transaction, string, error) {
	var g gethtypes.Transaction
	if err := g.UnmarshalJSON(b); err != nil {
		return nil, "", err
	}
	var meta struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return nil, "", err
	}
	return &g, meta.Hash, nil
}

func checkTxParity(t testing.TB, label string, b []byte) bool {
	t.Helper()
	g, gClaim, gErr := gethDecode(b)
	s, sClaim, sErr := decodeTxSonic(b)
	if (gErr == nil) != (sErr == nil) {
		t.Errorf("%s: accept/reject differs: geth err=%v sonic err=%v\n%s", label, gErr, sErr, b)
		return false
	}
	if gErr != nil {
		return true
	}
	if g.Hash() != s.Hash() {
		t.Errorf("%s: hash differs: geth %s sonic %s\n%s", label, g.Hash(), s.Hash(), b)
		return false
	}
	if gClaim != sClaim {
		t.Errorf("%s: claimed hash differs: %q vs %q", label, gClaim, sClaim)
		return false
	}
	return true
}

var txMutations = []func(m map[string]json.RawMessage){
	func(m map[string]json.RawMessage) {},
	func(m map[string]json.RawMessage) { delete(m, "nonce") },
	func(m map[string]json.RawMessage) { delete(m, "to") },
	func(m map[string]json.RawMessage) { m["to"] = json.RawMessage(`null`) },
	func(m map[string]json.RawMessage) { m["value"] = json.RawMessage(`"0x00"`) },
	func(m map[string]json.RawMessage) { m["value"] = json.RawMessage(`123`) },
	func(m map[string]json.RawMessage) { m["gas"] = json.RawMessage(`"0x"`) },
	func(m map[string]json.RawMessage) { m["input"] = json.RawMessage(`"0xabc"`) },
	func(m map[string]json.RawMessage) { m["yParity"] = json.RawMessage(`"0x2"`) },
	func(m map[string]json.RawMessage) { delete(m, "yParity") },
	func(m map[string]json.RawMessage) { delete(m, "v") },
	func(m map[string]json.RawMessage) { m["v"] = json.RawMessage(`"0x25"`) },
	func(m map[string]json.RawMessage) { m["r"] = json.RawMessage(`"0x0"`) },
	func(m map[string]json.RawMessage) {
		m["s"] = json.RawMessage(`"0xffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"`)
	},
	func(m map[string]json.RawMessage) { m["type"] = json.RawMessage(`"0x7e"`) },
	func(m map[string]json.RawMessage) { m["type"] = json.RawMessage(`"0x1"`) },
	func(m map[string]json.RawMessage) { m["accessList"] = json.RawMessage(`[]`) },
	func(m map[string]json.RawMessage) { m["accessList"] = json.RawMessage(`null`) },
	func(m map[string]json.RawMessage) { m["accessList"] = json.RawMessage(`[{"address":"0x01"}]`) },
	func(m map[string]json.RawMessage) { m["chainId"] = json.RawMessage(`"0x89"`) },
	func(m map[string]json.RawMessage) { delete(m, "chainId") },
	func(m map[string]json.RawMessage) { m["hash"] = json.RawMessage(`"0x1234"`) },
	func(m map[string]json.RawMessage) { m["maxFeePerGas"] = json.RawMessage(`"0x0001"`) },
	func(m map[string]json.RawMessage) { m["extra"] = json.RawMessage(`{"x":[1,2]}`) },
}

// TestDecodeTxSonicParity: decodeTxSonic must accept/reject exactly like
// geth's Transaction.UnmarshalJSON and produce the same hash (and claimed
// hash), across 40 signed transactions x 24 field mutations.
func TestDecodeTxSonicParity(t *testing.T) {
	raw := realisticSignedBlock(t, 0x1234, 40)
	var blk struct{ Transactions []map[string]json.RawMessage }
	if err := json.Unmarshal(raw, &blk); err != nil {
		t.Fatal(err)
	}
	checked, accepted := 0, 0
	for i, tm := range blk.Transactions {
		for j, mut := range txMutations {
			m := make(map[string]json.RawMessage, len(tm))
			for k, v := range tm {
				m[k] = v
			}
			mut(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if checkTxParity(t, fmt.Sprintf("tx %d mut %d", i, j), b) {
				if _, _, err := decodeTxSonic(b); err == nil {
					accepted++
				}
			}
			checked++
		}
	}
	if checked != 960 {
		t.Fatalf("expected 960 cases, ran %d", checked)
	}
	if accepted == 0 || accepted == checked {
		t.Fatalf("mutation set does not exercise both outcomes (accepted %d of %d)", accepted, checked)
	}
	t.Logf("checked %d tx/mutation pairs (%d accepted)", checked, accepted)
}

// FuzzDecodeTxSonic compares decodeTxSonic against geth's
// Transaction.UnmarshalJSON on arbitrary input: same accept/reject, and the
// same transaction hash and claimed hash whenever accepted.
func FuzzDecodeTxSonic(f *testing.F) {
	raw := realisticSignedBlock(f, 0x1234, 40)
	var blk struct{ Transactions []map[string]json.RawMessage }
	if err := json.Unmarshal(raw, &blk); err != nil {
		f.Fatal(err)
	}
	for i, tm := range blk.Transactions {
		if i%3 != 0 && i < 34 {
			continue
		}
		for _, mut := range txMutations {
			m := make(map[string]json.RawMessage, len(tm))
			for k, v := range tm {
				m[k] = v
			}
			mut(m)
			b, _ := json.Marshal(m)
			f.Add(b)
		}
	}
	for _, s := range []string{
		`{}`, `null`, `[]`, `{"type":null}`, `{"type":"0x3"}`, `{"type":"0x4"}`,
		`{"type":"0x0","nonce":"0x1","gas":"0x1","gasPrice":"0x1","value":"0x1","input":"0x","v":"0x0","r":"0x0","s":"0x0"}`,
		`{"type":"0x2","chainId":"0x1","nonce":"0x0","gas":"0x0","maxPriorityFeePerGas":"0x0","maxFeePerGas":"0x0","value":"0x0","input":"0x","r":"0x0","s":"0x0","yParity":"0x0","v":"0x1"}`,
		`{"nonce":"0x1","NONCE":"0x2"}`, `{"Hash":"0x1","hash":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		checkTxParity(t, "fuzz", b)
	})
}
