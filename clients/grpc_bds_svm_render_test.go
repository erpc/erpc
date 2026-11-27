package clients

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/svm"
	"github.com/blockchain-data-standards/manifesto/svm/parse"
	"github.com/bytedance/sonic"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// svmRenderFixture is a mainnet block whose parent is slot 473458461 (321
// transactions: 96 legacy, 80 v0, 145 v1), captured from a vendor's
// getBlock(encoding json, transactionDetails full, rewards false,
// maxSupportedTransactionVersion 1) and converted to the proto Prism serves.
// The getBlock result carries no slot; the fixture records parent+1, which no
// rendered field reads. Regenerate with SVM_RENDER_FIXTURE_SRC=<vendor
// block_json.json> go test -run TestSvmRenderFixtureRegen ./clients/.
const svmRenderFixture = "testdata/svm-block-parent-473458461.pb.gz"

type vendorIx struct {
	ProgramIDIndex uint32   `json:"programIdIndex"`
	Accounts       []uint32 `json:"accounts"`
	Data           string   `json:"data"`
	StackHeight    *uint32  `json:"stackHeight"`
}

type vendorTokenBalance struct {
	AccountIndex  uint32  `json:"accountIndex"`
	Mint          string  `json:"mint"`
	Owner         *string `json:"owner"`
	ProgramID     *string `json:"programId"`
	UiTokenAmount struct {
		Amount         string  `json:"amount"`
		Decimals       uint32  `json:"decimals"`
		UiAmountString *string `json:"uiAmountString"`
	} `json:"uiTokenAmount"`
}

type vendorReward struct {
	Pubkey      string  `json:"pubkey"`
	Lamports    int64   `json:"lamports"`
	PostBalance *uint64 `json:"postBalance"`
	RewardType  *string `json:"rewardType"`
	Commission  *uint32 `json:"commission"`
}

type vendorTx struct {
	Transaction struct {
		Signatures []string `json:"signatures"`
		Message    struct {
			Header *struct {
				NumRequiredSignatures       uint32 `json:"numRequiredSignatures"`
				NumReadonlySignedAccounts   uint32 `json:"numReadonlySignedAccounts"`
				NumReadonlyUnsignedAccounts uint32 `json:"numReadonlyUnsignedAccounts"`
			} `json:"header"`
			AccountKeys         []string   `json:"accountKeys"`
			RecentBlockhash     string     `json:"recentBlockhash"`
			Instructions        []vendorIx `json:"instructions"`
			AddressTableLookups []struct {
				AccountKey      string   `json:"accountKey"`
				WritableIndexes []uint32 `json:"writableIndexes"`
				ReadonlyIndexes []uint32 `json:"readonlyIndexes"`
			} `json:"addressTableLookups"`
			TransactionConfig *struct {
				PriorityFee                 *uint64 `json:"priorityFee"`
				ComputeUnitLimit            *uint32 `json:"computeUnitLimit"`
				LoadedAccountsDataSizeLimit *uint32 `json:"loadedAccountsDataSizeLimit"`
				HeapSize                    *uint32 `json:"heapSize"`
			} `json:"transactionConfig"`
		} `json:"message"`
	} `json:"transaction"`
	Meta *struct {
		Err               json.RawMessage `json:"err"`
		Fee               uint64          `json:"fee"`
		PreBalances       []uint64        `json:"preBalances"`
		PostBalances      []uint64        `json:"postBalances"`
		InnerInstructions *[]struct {
			Index        uint32     `json:"index"`
			Instructions []vendorIx `json:"instructions"`
		} `json:"innerInstructions"`
		LogMessages       *[]string             `json:"logMessages"`
		PreTokenBalances  *[]vendorTokenBalance `json:"preTokenBalances"`
		PostTokenBalances *[]vendorTokenBalance `json:"postTokenBalances"`
		Rewards           *[]vendorReward       `json:"rewards"`
		LoadedAddresses   *struct {
			Writable []string `json:"writable"`
			Readonly []string `json:"readonly"`
		} `json:"loadedAddresses"`
		ReturnData *struct {
			ProgramID string    `json:"programId"`
			Data      [2]string `json:"data"`
		} `json:"returnData"`
		ComputeUnitsConsumed *uint64 `json:"computeUnitsConsumed"`
		CostUnits            *uint64 `json:"costUnits"`
	} `json:"meta"`
	Version json.RawMessage `json:"version"`
}

type vendorBlock struct {
	Blockhash           string          `json:"blockhash"`
	PreviousBlockhash   string          `json:"previousBlockhash"`
	ParentSlot          uint64          `json:"parentSlot"`
	BlockHeight         *uint64         `json:"blockHeight"`
	BlockTime           *int64          `json:"blockTime"`
	Transactions        []vendorTx      `json:"transactions"`
	Rewards             *[]vendorReward `json:"rewards"`
	NumRewardPartitions *uint64         `json:"numRewardPartitions"`
}

// svmBlockFromVendorJSON converts a getBlock(encoding json) result into the
// ConfirmedBlock proto. encoding json is lossless for every proto field:
// keys, hashes and instruction data are base58, indexes are numbers, and
// meta carries loadedAddresses, returnData and the null-vs-empty lists.
func svmBlockFromVendorJSON(tb testing.TB, raw []byte) *svm.ConfirmedBlock {
	tb.Helper()
	var vb vendorBlock
	require.NoError(tb, json.Unmarshal(raw, &vb))
	b58 := func(s string) []byte {
		out, err := svm.Base58Decode(s)
		require.NoError(tb, err)
		return out
	}
	b58s := func(ss []string) [][]byte {
		out := make([][]byte, 0, len(ss))
		for _, s := range ss {
			out = append(out, b58(s))
		}
		return out
	}
	idx := func(is []uint32) []byte {
		out := make([]byte, len(is))
		for i, v := range is {
			out[i] = byte(v)
		}
		return out
	}
	ixs := func(vs []vendorIx) []*svm.CompiledInstruction {
		out := make([]*svm.CompiledInstruction, 0, len(vs))
		for _, v := range vs {
			out = append(out, &svm.CompiledInstruction{
				ProgramIdIndex: v.ProgramIDIndex,
				Accounts:       idx(v.Accounts),
				Data:           b58(v.Data),
				StackHeight:    v.StackHeight,
			})
		}
		return out
	}
	tbs := func(vs []vendorTokenBalance) []*svm.TokenBalance {
		out := make([]*svm.TokenBalance, 0, len(vs))
		for _, v := range vs {
			t := &svm.TokenBalance{
				AccountIndex: v.AccountIndex,
				Mint:         b58(v.Mint),
				UiTokenAmount: &svm.UiTokenAmount{
					Amount:         v.UiTokenAmount.Amount,
					Decimals:       v.UiTokenAmount.Decimals,
					UiAmountString: v.UiTokenAmount.UiAmountString,
				},
			}
			if v.Owner != nil {
				t.Owner = b58(*v.Owner)
			}
			if v.ProgramID != nil {
				t.ProgramId = b58(*v.ProgramID)
			}
			out = append(out, t)
		}
		return out
	}
	rewards := func(vs []vendorReward) []*svm.Reward {
		out := make([]*svm.Reward, 0, len(vs))
		for _, v := range vs {
			out = append(out, &svm.Reward{
				Pubkey: b58(v.Pubkey), Lamports: v.Lamports, PostBalance: v.PostBalance,
				RewardType: v.RewardType, Commission: v.Commission,
			})
		}
		return out
	}

	block := &svm.ConfirmedBlock{
		Slot:                vb.ParentSlot + 1,
		Blockhash:           b58(vb.Blockhash),
		PreviousBlockhash:   b58(vb.PreviousBlockhash),
		ParentSlot:          vb.ParentSlot,
		BlockHeight:         vb.BlockHeight,
		BlockTime:           vb.BlockTime,
		NumRewardPartitions: vb.NumRewardPartitions,
		Transactions:        make([]*svm.ConfirmedTransaction, 0, len(vb.Transactions)),
	}
	if vb.Rewards != nil {
		block.Rewards = rewards(*vb.Rewards)
	}
	for _, vt := range vb.Transactions {
		vm := vt.Transaction.Message
		msg := &svm.Message{
			AccountKeys:     b58s(vm.AccountKeys),
			RecentBlockhash: b58(vm.RecentBlockhash),
			Instructions:    ixs(vm.Instructions),
		}
		if vm.Header != nil {
			msg.Header = &svm.MessageHeader{
				NumRequiredSignatures:       vm.Header.NumRequiredSignatures,
				NumReadonlySignedAccounts:   vm.Header.NumReadonlySignedAccounts,
				NumReadonlyUnsignedAccounts: vm.Header.NumReadonlyUnsignedAccounts,
			}
		}
		for _, l := range vm.AddressTableLookups {
			msg.AddressTableLookups = append(msg.AddressTableLookups, &svm.AddressTableLookup{
				AccountKey: b58(l.AccountKey), WritableIndexes: idx(l.WritableIndexes), ReadonlyIndexes: idx(l.ReadonlyIndexes),
			})
		}
		if c := vm.TransactionConfig; c != nil {
			msg.TransactionConfig = &svm.TransactionConfig{
				PriorityFee: c.PriorityFee, ComputeUnitLimit: c.ComputeUnitLimit,
				LoadedAccountsDataSizeLimit: c.LoadedAccountsDataSizeLimit, HeapSize: c.HeapSize,
			}
		}
		ct := &svm.ConfirmedTransaction{
			Transaction: &svm.Transaction{Signatures: b58s(vt.Transaction.Signatures), Message: msg},
		}
		if len(vt.Version) > 0 && string(vt.Version) != "null" {
			var v string
			if json.Unmarshal(vt.Version, &v) != nil {
				v = string(vt.Version) // numeric version: 0, 1
			}
			ct.Version = &v
		}
		if vmeta := vt.Meta; vmeta != nil {
			meta := &svm.TransactionStatusMeta{
				Fee: vmeta.Fee, PreBalances: vmeta.PreBalances, PostBalances: vmeta.PostBalances,
				ComputeUnitsConsumed: vmeta.ComputeUnitsConsumed, CostUnits: vmeta.CostUnits,
			}
			if len(vmeta.Err) > 0 && string(vmeta.Err) != "null" {
				var compact bytes.Buffer
				require.NoError(tb, json.Compact(&compact, vmeta.Err))
				s := compact.String()
				meta.Err = &s
			}
			if vmeta.InnerInstructions == nil {
				meta.InnerInstructionsNone = true
			} else {
				meta.InnerInstructions = []*svm.InnerInstructions{}
				for _, ii := range *vmeta.InnerInstructions {
					meta.InnerInstructions = append(meta.InnerInstructions, &svm.InnerInstructions{Index: ii.Index, Instructions: ixs(ii.Instructions)})
				}
			}
			if vmeta.LogMessages == nil {
				meta.LogMessagesNone = true
			} else {
				meta.LogMessages = *vmeta.LogMessages
			}
			if vmeta.PreTokenBalances == nil {
				meta.PreTokenBalancesNone = true
			} else {
				meta.PreTokenBalances = tbs(*vmeta.PreTokenBalances)
			}
			if vmeta.PostTokenBalances == nil {
				meta.PostTokenBalancesNone = true
			} else {
				meta.PostTokenBalances = tbs(*vmeta.PostTokenBalances)
			}
			if vmeta.Rewards == nil {
				meta.RewardsNone = true
			} else {
				meta.Rewards = rewards(*vmeta.Rewards)
			}
			if la := vmeta.LoadedAddresses; la != nil {
				meta.LoadedWritableAddresses = b58s(la.Writable)
				meta.LoadedReadonlyAddresses = b58s(la.Readonly)
			}
			if rd := vmeta.ReturnData; rd != nil {
				data, err := base64.StdEncoding.DecodeString(rd.Data[0])
				require.NoError(tb, err)
				meta.ReturnData = &svm.ReturnData{ProgramId: b58(rd.ProgramID), Data: data}
			}
			ct.Meta = meta
		}
		block.Transactions = append(block.Transactions, ct)
	}
	return block
}

func TestSvmRenderFixtureRegen(t *testing.T) {
	src := os.Getenv("SVM_RENDER_FIXTURE_SRC")
	if src == "" {
		t.Skip("set SVM_RENDER_FIXTURE_SRC to a vendor getBlock(encoding json) result to regenerate " + svmRenderFixture)
	}
	raw, err := os.ReadFile(src)
	require.NoError(t, err)
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`{"jsonrpc"`)) {
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &env))
		raw = env.Result
	}
	pb, err := proto.Marshal(svmBlockFromVendorJSON(t, raw))
	require.NoError(t, err)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, err = w.Write(pb)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, os.WriteFile(svmRenderFixture, gz.Bytes(), 0o644))
	t.Logf("wrote %s: %d proto bytes, %d gzipped", svmRenderFixture, len(pb), gz.Len())
}

// loadSvmRenderFixture decodes the fixture exactly as the gRPC client would:
// proto.Unmarshal of the wire bytes.
func loadSvmRenderFixture(tb testing.TB) *svm.GetBlockResponse {
	tb.Helper()
	f, err := os.Open(filepath.FromSlash(svmRenderFixture))
	require.NoError(tb, err)
	defer f.Close()
	r, err := gzip.NewReader(f)
	require.NoError(tb, err)
	pb, err := io.ReadAll(r)
	require.NoError(tb, err)
	block := &svm.ConfirmedBlock{}
	require.NoError(tb, proto.Unmarshal(pb, block))
	return &svm.GetBlockResponse{SlotStatus: svm.SlotStatus_SLOT_PRESENT, Block: block}
}

// TestSvmRenderFixtureDump writes the streaming render of the fixture
// (jsonParsed and json) to SVM_RENDER_DUMP, for diffing against the vendor's
// own getBlock output of the same slot.
func TestSvmRenderFixtureDump(t *testing.T) {
	dir := os.Getenv("SVM_RENDER_DUMP")
	if dir == "" {
		t.Skip("set SVM_RENDER_DUMP to a directory to dump the fixture's rendered getBlock results")
	}
	block := loadSvmRenderFixture(t).Block
	require.NoError(t, os.WriteFile(filepath.Join(dir, "erpc_jsonParsed.json"), svmBlockJson(block, nil, true, false, false), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "erpc_json.json"), svmBlockJson(block, nil, false, false, false), 0o644))
}

// Stage benchmarks over the real block. Each stage runs alone so its ns/op
// and allocs/op are that stage's cost inside handleSvmGetBlock. cpu-ms/op is
// process CPU (user+sys, GC included) per op from getrusage: on a loaded
// host it moves far less than wall ns/op, and for the parallel encoder it is
// the total work rather than the latency.
func BenchmarkSvmRenderStages(b *testing.B) {
	block := loadSvmRenderFixture(b).Block
	parse.AttachToBlock(block)
	parsedTree := svm.BlockToJsonRpcParsed(block, nil)
	jsonTree := svm.BlockToJsonRpc(block, nil)

	stages := []struct {
		name string
		fn   func()
	}{
		{"AttachToBlock", func() { parse.AttachToBlock(block) }},
		{"BlockToJsonRpcParsed", func() { _ = svm.BlockToJsonRpcParsed(block, nil) }},
		{"BlockToJsonRpc", func() { _ = svm.BlockToJsonRpc(block, nil) }},
		{"MarshalParsed", func() { _, _ = sonic.Marshal(parsedTree) }},
		{"MarshalJson", func() { _, _ = sonic.Marshal(jsonTree) }},
		// Old render path end to end, in the same process as the streaming
		// stages so both see the same machine load.
		{"OldPathParsed", func() {
			parse.AttachToBlock(block)
			_, _ = sonic.Marshal(svm.BlockToJsonRpcParsed(block, nil))
		}},
		{"OldPathJson", func() { _, _ = sonic.Marshal(svm.BlockToJsonRpc(block, nil)) }},
		{"StreamParsed", func() { _ = svmBlockJson(block, nil, true, false, false) }},
		{"StreamJson", func() { _ = svmBlockJson(block, nil, false, false, false) }},
	}
	for _, s := range stages {
		b.Run(s.name, func(b *testing.B) {
			b.ReportAllocs()
			runtime.GC()
			cpu0 := svmProcessCPU(b)
			for b.Loop() {
				s.fn()
			}
			b.ReportMetric(float64(svmProcessCPU(b)-cpu0)/float64(time.Millisecond)/float64(b.N), "cpu-ms/op")
		})
	}
}

func svmProcessCPU(tb testing.TB) time.Duration {
	var ru syscall.Rusage
	require.NoError(tb, syscall.Getrusage(syscall.RUSAGE_SELF, &ru))
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// svmRenderReference is the pre-streaming render: attach, map tree, history
// nulls, sonic.Marshal — handleSvmGetBlock as it stood on origin/main.
func svmRenderReference(tb testing.TB, block *svm.ConfirmedBlock, sigs [][]byte, parsed, nullTime, nullHeight bool) []byte {
	tb.Helper()
	block = proto.Clone(block).(*svm.ConfirmedBlock)
	var result map[string]interface{}
	if parsed {
		parse.AttachToBlock(block)
		result = svm.BlockToJsonRpcParsed(block, sigs)
	} else {
		result = svm.BlockToJsonRpc(block, sigs)
	}
	if nullTime {
		result["blockTime"] = nil
	}
	if nullHeight {
		result["blockHeight"] = nil
	}
	out, err := sonic.Marshal(result)
	require.NoError(tb, err)
	return out
}

// svmCanonicalJSON re-encodes JSON with object keys sorted and numbers kept
// as their exact text, so two renders compare by value regardless of key
// order (the map-tree render's order is map-iteration order).
func svmCanonicalJSON(tb testing.TB, in []byte) []byte {
	tb.Helper()
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.UseNumber()
	var v interface{}
	require.NoError(tb, dec.Decode(&v))
	_, err := dec.Token()
	require.ErrorIs(tb, err, io.EOF, "trailing data after the JSON value")
	out, err := json.Marshal(v)
	require.NoError(tb, err)
	return out
}

// TestSvmBlockJsonMatchesReference renders the mainnet fixture through the
// streaming encoder and the map-tree reference and requires equal JSON
// values, for every encoding / detail / history-null combination.
func TestSvmBlockJsonMatchesReference(t *testing.T) {
	block := loadSvmRenderFixture(t).Block
	sigBlock := proto.Clone(block).(*svm.ConfirmedBlock)
	var sigs [][]byte
	for _, tx := range sigBlock.Transactions {
		sigs = append(sigs, tx.Transaction.Signatures[0])
	}
	sigBlock.Transactions = nil
	noneBlock := proto.Clone(sigBlock).(*svm.ConfirmedBlock)
	// The fixture was fetched with rewards false; add block rewards of every
	// optional-field shape so that path is compared too.
	rewardBlock := proto.Clone(block).(*svm.ConfirmedBlock)
	fee, voting := "Fee", "Voting"
	post, commission, partitions := uint64(51614126116), uint32(7), uint64(4)
	rewardBlock.Rewards = []*svm.Reward{
		{Pubkey: svmTestKey(1), Lamports: 2913029, PostBalance: &post, RewardType: &fee},
		{Pubkey: svmTestKey(2), Lamports: -5000, RewardType: &voting, Commission: &commission},
		{Pubkey: svmTestKey(3)},
	}
	rewardBlock.NumRewardPartitions = &partitions

	cases := []struct {
		name  string
		block *svm.ConfirmedBlock
		sigs  [][]byte
	}{
		{"full", block, nil},
		{"rewards", rewardBlock, nil},
		{"signatures", sigBlock, sigs},
		{"none", noneBlock, nil},
	}
	for _, c := range cases {
		for _, parsed := range []bool{true, false} {
			for _, nulls := range [][2]bool{{false, false}, {true, true}} {
				name := c.name + "/parsed=" + strconv.FormatBool(parsed) + "/nulls=" + strconv.FormatBool(nulls[0])
				t.Run(name, func(t *testing.T) {
					want := svmRenderReference(t, c.block, c.sigs, parsed, nulls[0], nulls[1])
					got := svmBlockJson(c.block, c.sigs, parsed, nulls[0], nulls[1])
					require.True(t, json.Valid(got), "streaming output is not valid JSON")
					wantC, gotC := svmCanonicalJSON(t, want), svmCanonicalJSON(t, got)
					if !bytes.Equal(wantC, gotC) {
						i := 0
						for i < len(wantC) && i < len(gotC) && wantC[i] == gotC[i] {
							i++
						}
						lo := max(0, i-300)
						t.Fatalf("first difference at canonical byte %d\nwant …%s\ngot  …%s", i,
							wantC[lo:min(len(wantC), i+200)], gotC[lo:min(len(gotC), i+200)])
					}
					t.Logf("%d bytes, canonical %d bytes, equal", len(got), len(gotC))
				})
			}
		}
	}
}

// TestSvmAppendBase58MatchesEncode checks the limb encoder against
// svm.Base58Encode on every length a block carries (keys 32, signatures 64,
// instruction data 0..1232) and on leading-zero runs.
func TestSvmAppendBase58MatchesEncode(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 0; n <= 1232; n++ {
		for rep := range 8 {
			in := make([]byte, n)
			for i := range in {
				in[i] = byte(rng.Uint32())
			}
			switch rep {
			case 1: // leading zeros
				for i := 0; i < n && i < rep+n/3; i++ {
					in[i] = 0
				}
			case 2: // all zeros
				clear(in)
			case 3: // all 0xff
				for i := range in {
					in[i] = 0xff
				}
			case 4: // a single low byte after zeros
				clear(in)
				if n > 0 {
					in[n-1] = 1
				}
			}
			want := svm.Base58Encode(in)
			if got := string(appendBase58(nil, in)); got != want {
				t.Fatalf("len %d rep %d: got %q want %q", n, rep, got, want)
			}
		}
	}
}

// BenchmarkSvmRenderHandler is the whole render path (handler minus the gRPC
// call) per encoding.
func BenchmarkSvmRenderHandler(b *testing.B) {
	resp := loadSvmRenderFixture(b)
	slot := strconv.FormatUint(resp.Block.Slot, 10)
	for _, enc := range []string{"jsonParsed", "json"} {
		b.Run(enc, func(b *testing.B) {
			params := `[` + slot + `,{"encoding":"` + enc + `","maxSupportedTransactionVersion":1}]`
			b.ReportAllocs()
			for b.Loop() {
				_, out, err := callSvmGetBlock(b, params, resp, nil)
				if err != nil || out == nil {
					b.Fatalf("getBlock(%s): %v", enc, err)
				}
			}
		})
	}
}
