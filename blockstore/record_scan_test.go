package blockstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Oracle: the encoding/json implementations the single-scan parser replaced
// (as of 543b214c), kept verbatim to pin identical results.

func oldParseBlockHeader(raw json.RawMessage) (*rawBlock, int64, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, 0, fmt.Errorf("null block")
	}
	var b rawBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, 0, err
	}
	n, err := parseHexInt(b.Number)
	if err != nil {
		return nil, 0, fmt.Errorf("block number: %w", err)
	}
	if n < 0 {
		return nil, 0, fmt.Errorf("negative block number")
	}
	if !isHexOfLen(b.Hash, 64) || !isHexOfLen(b.ParentHash, 64) {
		return nil, 0, fmt.Errorf("block has invalid hash/parentHash")
	}
	return &b, n, nil
}

func oldTxHashesOf(b *rawBlock) (map[string]struct{}, bool, error) {
	if b.Transactions == nil {
		return nil, false, fmt.Errorf("block missing transactions array")
	}
	out := make(map[string]struct{}, len(b.Transactions))
	full := true
	add := func(h string) error {
		h = normHash(h)
		if _, dup := out[h]; dup {
			return fmt.Errorf("duplicate transaction %s", h)
		}
		out[h] = struct{}{}
		return nil
	}
	for _, t := range b.Transactions {
		t = bytes.TrimSpace(t)
		if len(t) > 0 && t[0] == '"' {
			full = false
			var h string
			if err := json.Unmarshal(t, &h); err != nil {
				return nil, false, err
			}
			if err := add(h); err != nil {
				return nil, false, err
			}
			continue
		}
		var tx struct {
			Hash string `json:"hash"`
		}
		if err := json.Unmarshal(t, &tx); err != nil {
			return nil, false, err
		}
		if tx.Hash == "" {
			return nil, false, fmt.Errorf("transaction without hash")
		}
		if err := add(tx.Hash); err != nil {
			return nil, false, err
		}
	}
	return out, full, nil
}

// syntheticBlock builds a realistic full block with n transactions.
func syntheticBlock(num int64, n int) json.RawMessage {
	h := func(tag string, i int) string {
		return fmt.Sprintf("0x%064x", uint64(i)*7919+uint64(len(tag))*1_000_003+uint64(num))
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `{"baseFeePerGas":"0x3b9aca00","difficulty":"0x0","extraData":"0x","gasLimit":"0x1c9c380","gasUsed":"0x1312d00",`+
		`"hash":"%s","logsBloom":"0x%0512x","miner":"0x95222290dd7278aa3ddd389cc1e1d165cc4bafe5","mixHash":"%s","nonce":"0x0000000000000000",`+
		`"number":"0x%x","parentHash":"%s","receiptsRoot":"%s","sha3Uncles":"%s","size":"0x2a1f3","stateRoot":"%s","timestamp":"0x6500%04x","transactions":[`,
		h("block", 0), 0, h("mix", 0), num, h("parent", 0), h("rcpt", 0), h("uncles", 0), h("state", 0), num%0xffff)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"accessList":[{"address":"0x%040x","storageKeys":["%s","%s"]}],"blockHash":"%s","blockNumber":"0x%x","chainId":"0x1",`+
			`"from":"0x%040x","gas":"0x5208","gasPrice":"0x4a817c800","hash":"%s","input":"0xa9059cbb000000000000000000000000%040x0000000000000000000000000000000000000000000000000de0b6b3a7640000",`+
			`"maxFeePerGas":"0x4a817c800","maxPriorityFeePerGas":"0x3b9aca00","nonce":"0x%x","r":"%s","s":"%s","to":"0x%040x","transactionIndex":"0x%x","type":"0x2","v":"0x1","value":"0x0","yParity":"0x1"}`,
			i, h("k1", i), h("k2", i), h("block", 0), num, i+1, h("tx", i), i+2, i, h("r", i), h("s", i), i+3, i)
	}
	sb.WriteString(`],"transactionsRoot":"` + h("txroot", 0) + `","uncles":[],"withdrawals":[],"withdrawalsRoot":"` + h("wroot", 0) + `"}`)
	return json.RawMessage(sb.String())
}

func parityFixtures(t testing.TB) map[string]json.RawMessage {
	ch := newFakeChain(4)
	ch.logless[3] = `[]`
	fullB := string(syntheticBlock(100, 3))
	const hA = "0x000000000000000000000000000000000000000000000000000000000000000a"
	const hB = "0x000000000000000000000000000000000000000000000000000000000000000b"
	base := `"number":"0x10","hash":"0x00000000000000000000000000000000000000000000000000000000000000b1","parentHash":"0x00000000000000000000000000000000000000000000000000000000000000b0"`
	f := map[string]json.RawMessage{
		"fakechain full":       fullBlockRaw(t, ch, 2),
		"fakechain empty txs":  fullBlockRaw(t, ch, 3),
		"synthetic 150":        syntheticBlock(1000, 150),
		"synthetic 1000":       syntheticBlock(2000, 1000),
		"synthetic indented":   indent(t, syntheticBlock(3000, 5)),
		"hashes only":          json.RawMessage(`{` + base + `,"transactions":["` + hA + `","` + hB + `"]}`),
		"hashes only upper":    json.RawMessage(`{` + base + `,"transactions":["` + strings.ToUpper(hA[2:]) + `"]}`),
		"tx hash escaped":      json.RawMessage(`{` + base + `,"transactions":[{"hash":"\u0030x0a"}]}`),
		"tx key case":          json.RawMessage(`{` + base + `,"transactions":[{"HASH":"` + hA + `"}]}`),
		"tx dup key":           json.RawMessage(`{` + base + `,"transactions":[{"hash":"` + hA + `","Hash":"` + hB + `"}]}`),
		"tx nested hash":       json.RawMessage(`{` + base + `,"transactions":[{"x":{"hash":"` + hB + `"},"hash":"` + hA + `"}]}`),
		"tx hash in value":     json.RawMessage(`{` + base + `,"transactions":[{"note":"hash","hash":"` + hA + `"}]}`),
		"block key case":       json.RawMessage(`{"NUMBER":"0x10","Hash":"0x00000000000000000000000000000000000000000000000000000000000000b1","parentHash":"0x00000000000000000000000000000000000000000000000000000000000000b0","Transactions":[{"hash":"` + hA + `"}]}`),
		"block dup tx key":     json.RawMessage(`{` + base + `,"transactions":[{"hash":"` + hA + `"}],"transactions":[{"hash":"` + hB + `"}]}`),
		"txs whitespace":       json.RawMessage(`{` + base + `,"transactions" :  [ {"hash":"` + hA + `"} , {"hash":"` + hB + `"} ] }`),
		"null block":           json.RawMessage(`null`),
		"null block spaced":    json.RawMessage("  null \n"),
		"empty":                json.RawMessage(``),
		"not object":           json.RawMessage(`[1,2]`),
		"truncated":            json.RawMessage(fullB[:len(fullB)/2]),
		"trailing garbage":     json.RawMessage(fullB + ` x`),
		"bad number":           json.RawMessage(`{"number":"16","hash":"0x00000000000000000000000000000000000000000000000000000000000000b1","parentHash":"0x00000000000000000000000000000000000000000000000000000000000000b0","transactions":[]}`),
		"numeric number":       json.RawMessage(`{"number":16,"hash":"0x00000000000000000000000000000000000000000000000000000000000000b1","parentHash":"0x00000000000000000000000000000000000000000000000000000000000000b0","transactions":[]}`),
		"negative number":      json.RawMessage(`{"number":"0x-1","hash":"0x00000000000000000000000000000000000000000000000000000000000000b1","parentHash":"0x00000000000000000000000000000000000000000000000000000000000000b0","transactions":[]}`),
		"short hash":           json.RawMessage(`{"number":"0x10","hash":"0x1234","parentHash":"0x00000000000000000000000000000000000000000000000000000000000000b0","transactions":[]}`),
		"missing parent":       json.RawMessage(`{"number":"0x10","hash":"0x00000000000000000000000000000000000000000000000000000000000000b1","transactions":[]}`),
		"missing txs":          json.RawMessage(`{` + base + `}`),
		"null txs":             json.RawMessage(`{` + base + `,"transactions":null}`),
		"txs not array":        json.RawMessage(`{` + base + `,"transactions":{}}`),
		"duplicate tx":         json.RawMessage(`{` + base + `,"transactions":[{"hash":"` + hA + `"},{"hash":"` + hB + `"},{"hash":"` + hA + `"}]}`),
		"duplicate tx case":    json.RawMessage(`{` + base + `,"transactions":["` + hA + `","` + strings.ToUpper(hA) + `"]}`),
		"mixed full and hash":  json.RawMessage(`{` + base + `,"transactions":[{"hash":"` + hA + `"},"` + hB + `"]}`),
		"tx without hash":      json.RawMessage(`{` + base + `,"transactions":[{"from":"0x1"}]}`),
		"tx empty hash":        json.RawMessage(`{` + base + `,"transactions":[{"hash":""}]}`),
		"tx null":              json.RawMessage(`{` + base + `,"transactions":[null]}`),
		"tx number":            json.RawMessage(`{` + base + `,"transactions":[5]}`),
		"tx numeric hash":      json.RawMessage(`{` + base + `,"transactions":[{"hash":5}]}`),
		"tx null hash":         json.RawMessage(`{` + base + `,"transactions":[{"hash":null}]}`),
		"tx bad json":          json.RawMessage(`{` + base + `,"transactions":[{"hash":tru}]}`),
		"control char":         json.RawMessage(`{` + base + `,"extra":"a` + "\x01" + `b","transactions":[]}`),
		"control char in tx":   json.RawMessage(`{` + base + `,"transactions":[{"input":"a` + "\x01" + `","hash":"` + hA + `"}]}`),
		"invalid utf8":         json.RawMessage(`{` + base + `,"extra":"` + "\xff" + `","transactions":[{"hash":"` + hA + `"}]}`),
		"escaped tx string":    json.RawMessage(`{` + base + `,"transactions":["\u0030x0a"]}`),
		"tx key escaped":       json.RawMessage(`{` + base + `,"transactions":[{"h\u0061sh":"` + hA + `"}]}`),
		"unicode in tx":        json.RawMessage(`{` + base + `,"transactions":[{"memo":"héllo","hash":"` + hA + `"}]}`),
		"empty tx array full":  json.RawMessage(`{` + base + `,"transactions":[]}`),
		"transactions in body": json.RawMessage(`{` + base + `,"note":"transactions","transactions":[{"hash":"` + hA + `"}]}`),
	}
	return f
}

func fullBlockRaw(t testing.TB, ch *fakeChain, n int64) json.RawMessage {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	raw, err := ch.blockLocked(n)
	require.NoError(t, err)
	return raw
}

func indent(t testing.TB, raw json.RawMessage) json.RawMessage {
	var out bytes.Buffer
	require.NoError(t, json.Indent(&out, raw, "", "  "))
	return out.Bytes()
}

func isDecodeErr(err error) bool {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	return errors.As(err, &se) || errors.As(err, &te) || !strings.Contains(err.Error(), " ") ||
		strings.Contains(err.Error(), "json") || strings.Contains(err.Error(), "Syntax") || strings.Contains(err.Error(), "Mismatch") ||
		strings.Contains(err.Error(), "invalid char") || strings.Contains(err.Error(), "unexpected end")
}

// The single-scan parser agrees with the encoding/json implementation on
// acceptance, identity, the transaction set and fullness, and validation
// errors (decode errors may be worded differently, but both reject).
func TestScanParity_ParseAndTxHashes(t *testing.T) {
	for name, raw := range parityFixtures(t) {
		t.Run(name, func(t *testing.T) {
			ob, on, oerr := oldParseBlockHeader(raw)
			nb, nn, nerr := parseBlockHeader(raw)
			require.Equal(t, oerr == nil, nerr == nil, "parse: old=%v new=%v", oerr, nerr)
			if oerr != nil {
				if !isDecodeErr(oerr) {
					require.EqualError(t, nerr, oerr.Error())
				}
				return
			}
			require.Equal(t, on, nn)
			require.Equal(t, ob.Number, nb.Number)
			require.Equal(t, ob.Hash, nb.Hash)
			require.Equal(t, ob.ParentHash, nb.ParentHash)
			require.Equal(t, ob.LogsBloom, nb.LogsBloom)
			require.Equal(t, ob.Transactions == nil, nb.Transactions == nil)
			require.Equal(t, len(ob.Transactions), len(nb.Transactions))
			oset, ofull, oterr := oldTxHashesOf(ob)
			nset, nfull, nterr := txHashesOf(nb)
			require.Equal(t, oterr == nil, nterr == nil, "txs: old=%v new=%v", oterr, nterr)
			if oterr != nil {
				if !isDecodeErr(oterr) {
					require.EqualError(t, nterr, oterr.Error())
				}
				return
			}
			require.Equal(t, ofull, nfull)
			require.Equal(t, oset, nset)
		})
	}
}

// The spliced hashes-only rendering is semantically equal to the map-based
// one, and keeps every other field's bytes in order.
func TestScanParity_HashesOnly(t *testing.T) {
	spliced := 0
	for name, raw := range parityFixtures(t) {
		t.Run(name, func(t *testing.T) {
			want, werr := legacyHashesOnly(raw)
			got, gerr := (&BlockRecord{Block: raw}).BlockJSON(false)
			require.Equal(t, werr == nil, gerr == nil, "old=%v new=%v", werr, gerr)
			if werr != nil {
				return
			}
			require.JSONEq(t, string(want), string(got))
			if sb, err := scanBlock(raw); err == nil {
				if out, hb, ok := sb.hashesOnly(); ok {
					spliced++
					// Everything before the transactions value is byte-identical.
					i := bytes.Index(raw, []byte(`"transactions"`))
					require.Equal(t, string(raw[:i]), string(out[:i]))
					// The derived header parses as the hashes-only block it renders.
					pb, _, err := parseBlockHeader(out)
					require.NoError(t, err)
					pset, pfull, err := txHashesOf(pb)
					require.NoError(t, err)
					hset, hfull, err := txHashesOf(hb)
					require.NoError(t, err)
					require.Equal(t, pset, hset)
					require.Equal(t, pfull, hfull)
					require.False(t, hfull)
					require.Len(t, hb.Transactions, len(pb.Transactions))
					for i := range pb.Transactions {
						require.Equal(t, string(pb.Transactions[i]), string(hb.Transactions[i]))
					}
				}
			}
		})
	}
	require.GreaterOrEqual(t, spliced, 8, "the common shapes take the splice path")
}

// AdoptBlock's derived header equals the one the old path built.
func TestScanParity_AdoptedHeader(t *testing.T) {
	for _, n := range []int{0, 1, 150, 1000} {
		raw := syntheticBlock(int64(5000+n), n)
		ob, num, err := oldParseBlockHeader(raw)
		require.NoError(t, err)
		var oldHeader json.RawMessage = raw
		if len(ob.Transactions) > 0 {
			oldHeader, err = legacyHashesOnly(raw)
			require.NoError(t, err)
		}
		want, err := parseHeader(oldHeader, num)
		require.NoError(t, err)

		sb, got, err := parseScannedBlock(raw)
		require.NoError(t, err)
		require.Equal(t, num, got)
		var h *header
		if n > 0 {
			hraw, hb, ok := sb.hashesOnly()
			require.True(t, ok)
			h, err = headerFromScan(hb, hraw, got, num)
		} else {
			h, err = parseHeader(raw, num)
		}
		require.NoError(t, err)
		require.Equal(t, want.n, h.n)
		require.Equal(t, want.b.Hash, h.b.Hash)
		require.Equal(t, want.b.ParentHash, h.b.ParentHash)
		require.JSONEq(t, string(want.raw), string(h.raw))
		ws, _, _ := txHashesOf(want.b)
		hs, _, _ := txHashesOf(h.b)
		require.Equal(t, ws, hs)
	}
}

func FuzzScanParity(f *testing.F) {
	for _, raw := range parityFixtures(f) {
		if len(raw) < 4096 {
			f.Add([]byte(raw))
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, on, oerr := oldParseBlockHeader(raw)
		nb, nn, nerr := parseBlockHeader(raw)
		if (oerr == nil) != (nerr == nil) {
			t.Fatalf("parse disagreement: old=%v new=%v", oerr, nerr)
		}
		if oerr != nil {
			return
		}
		if on != nn {
			t.Fatalf("number %d != %d", on, nn)
		}
		ob, _, _ := oldParseBlockHeader(raw)
		oset, ofull, oterr := oldTxHashesOf(ob)
		nset, nfull, nterr := txHashesOf(nb)
		if (oterr == nil) != (nterr == nil) || ofull != nfull || len(oset) != len(nset) {
			t.Fatalf("txs disagreement: old=%v,%v,%d new=%v,%v,%d", oterr, ofull, len(oset), nterr, nfull, len(nset))
		}
		for k := range oset {
			if _, ok := nset[k]; !ok {
				t.Fatalf("missing tx %q", k)
			}
		}
		want, werr := legacyHashesOnly(raw)
		got, gerr := (&BlockRecord{Block: raw}).BlockJSON(false)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("hashes-only disagreement: old=%v new=%v", werr, gerr)
		}
		if werr == nil {
			var a, b interface{}
			if json.Unmarshal(want, &a) != nil || json.Unmarshal(got, &b) != nil {
				t.Fatalf("hashes-only output not JSON")
			}
			aj, _ := json.Marshal(a)
			bj, _ := json.Marshal(b)
			if !bytes.Equal(aj, bj) {
				t.Fatalf("hashes-only mismatch:\n%s\n%s", want, got)
			}
		}
	})
}

func benchBlocks() map[string]json.RawMessage {
	return map[string]json.RawMessage{"txs=150": syntheticBlock(1, 150), "txs=1000": syntheticBlock(2, 1000)}
}

func BenchmarkParseBlockHeader(b *testing.B) {
	for name, raw := range benchBlocks() {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				blk, _, err := parseBlockHeader(raw)
				if err != nil {
					b.Fatal(err)
				}
				if _, _, err := txHashesOf(blk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkHashOnlyHeader(b *testing.B) {
	for name, raw := range benchBlocks() {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := hashOnlyHeader(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAdoptBlock adopts a block not yet held per iteration (the full
// parse path; held blocks skip parsing entirely).
func BenchmarkAdoptBlock(b *testing.B) {
	for name, n := range map[string]int{"txs=150": 150, "txs=1000": 1000} {
		b.Run(name, func(b *testing.B) {
			const pool = 64
			blocks := make([]json.RawMessage, pool)
			for i := range blocks {
				blocks[i] = syntheticBlock(int64(10+i), n)
			}
			b.SetBytes(int64(len(blocks[0])))
			b.ReportAllocs()
			o := pullOpts()
			o.MaxBlockSize, o.MaxBytes, o.Depth = 0, 0, 1<<40
			ch := newFakeChain(1)
			ctx := context.Background()
			var c *Cache
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if i%pool == 0 {
					b.StopTimer()
					c = New(o, newMapStore(), ch, func(context.Context) int64 { return -1 }, nil)
					b.StartTimer()
				}
				c.AdoptBlock(ctx, blocks[i%pool], true, true, false)
			}
			b.StopTimer()
			if c.adoptParses.Load() == 0 {
				b.Fatal("nothing parsed")
			}
		})
	}
}
