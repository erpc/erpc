package evm

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/architecture/evm/integrity"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"go.opentelemetry.io/otel/attribute"
)

// aggregate3((address,bool,bytes)[])
var aggregate3Selector = []byte{0x82, 0xad, 0x56, 0xcb}

const (
	multicall3Packed     = "packed"
	multicall3Alone      = "alone"
	multicall3Reverted   = "reverted"
	multicall3Empty      = "empty"
	multicall3Failed     = "failed"
	multicall3Ineligible = "ineligible"
	multicall3Bypassed   = "bypassed"
	multicall3Canceled   = "canceled"
)

// Multicall3Batcher packs the eth_calls one upstream receives for the same block
// into a single aggregate3 eth_call. It is best-effort: every call it cannot
// answer exactly goes back to the caller, which sends it alone.
type Multicall3Batcher struct {
	maxWait  time.Duration
	maxCalls int

	mu   sync.Mutex
	open map[string]*multicall3Batch

	// Highest block at which Multicall3 had no code on this upstream; calls at
	// or below it are not packed.
	absentAtOrBelow atomic.Int64

	// Contracts whose calls are never packed: configured (bypassContracts), or
	// learned when a call to them reverted inside a pack and then succeeded alone
	// (typically a contract that rejects a contract caller). Learned entries last
	// for the process; they only ever send calls alone, as without packing.
	bypass        map[[20]byte]struct{}
	learnedBypass sync.Map // [20]byte -> struct{}
	// Calls that reverted inside a pack and are being sent alone, by request,
	// until the upstream post-forward hook sees how the call alone ended.
	awaitingAlone sync.Map // *common.NormalizedRequest -> [20]byte
}

// A pack shares one block, one gas limit and one sender (none, or the zero
// address). With the callers' own gas limit on the multicall, an inner call
// gets less gas than it would alone: one that runs out fails inside the pack
// and is sent alone, so packing never adds gas.
type multicall3Batch struct {
	block   interface{}
	gas     string
	from    string
	entries []*multicall3Entry
	timer   *time.Timer
}

type multicall3Entry struct {
	ctx    context.Context
	req    *common.NormalizedRequest
	target []byte
	data   []byte
	result chan multicall3Result
}

type multicall3Result struct {
	returnData []byte
	outcome    string
}

type aggregate3Result struct {
	success bool
	data    []byte
}

func NewMulticall3Batcher(cfg *common.EvmMulticall3Config) *Multicall3Batcher {
	b := &Multicall3Batcher{
		maxWait:  cfg.MaxWait.Duration(),
		maxCalls: cfg.MaxCalls,
		open:     make(map[string]*multicall3Batch),
		bypass:   make(map[[20]byte]struct{}, len(cfg.BypassContracts)),
	}
	for _, a := range cfg.BypassContracts {
		// Validated as 20-byte hex addresses at config load.
		if raw, err := common.HexToBytes(a); err == nil && len(raw) == 20 {
			b.bypass[[20]byte(raw)] = struct{}{}
		}
	}
	return b
}

func (b *Multicall3Batcher) bypassed(target []byte) bool {
	addr := [20]byte(target)
	if _, ok := b.bypass[addr]; ok {
		return true
	}
	_, ok := b.learnedBypass.Load(addr)
	return ok
}

type multicall3Upstream interface {
	EvmMulticall3Batcher() *Multicall3Batcher
}

func upstreamPreForward_eth_call(ctx context.Context, n common.Network, u common.Upstream, nrq *common.NormalizedRequest) (bool, *common.NormalizedResponse, error) {
	mu, ok := u.(multicall3Upstream)
	if !ok || nrq.CompositeType() == common.CompositeTypeMulticall3 {
		return false, nil, nil
	}
	b := mu.EvmMulticall3Batcher()
	if b == nil {
		return false, nil, nil
	}

	ctx, span := common.StartDetailSpan(ctx, "Upstream.PreForwardHook.eth_call")
	defer span.End()

	returnData, outcome := b.submit(ctx, n, u, nrq)
	var resp *common.NormalizedResponse
	if outcome == multicall3Packed {
		if jrr, err := common.NewJsonRpcResponse(nrq.ID(), "0x"+hex.EncodeToString(returnData), nil); err == nil {
			resp = common.NewNormalizedResponse().WithRequest(nrq).WithJsonRpcResponse(jrr)
			resp.SetUpstream(u)
			nrq.SetLastUpstream(u)
			nrq.SetLastValidResponse(ctx, resp)
		} else {
			outcome = multicall3Failed
		}
	}
	span.SetAttributes(attribute.String("multicall3.outcome", outcome))
	telemetry.MetricUpstreamMulticall3CallsTotal.WithLabelValues(n.ProjectId(), nrq.NetworkLabel(), u.Id(), u.VendorName(), outcome).Inc()
	return resp != nil, resp, nil
}

// upstreamPostForward_eth_call learns a bypass when a call that reverted inside
// a pack succeeded alone: the contract answers differently to Multicall3.
func upstreamPostForward_eth_call(u common.Upstream, rq *common.NormalizedRequest, rs *common.NormalizedResponse, re error) {
	mu, ok := u.(multicall3Upstream)
	if !ok {
		return
	}
	b := mu.EvmMulticall3Batcher()
	if b == nil {
		return
	}
	v, ok := b.awaitingAlone.LoadAndDelete(rq)
	if !ok || re != nil || rs == nil {
		return
	}
	if jrr, err := rs.JsonRpcResponse(); err != nil || jrr == nil || jrr.Error != nil {
		return
	}
	addr := v.([20]byte)
	if _, known := b.learnedBypass.LoadOrStore(addr, struct{}{}); !known && u.Logger() != nil {
		u.Logger().Info().Str("contract", "0x"+hex.EncodeToString(addr[:])).Msg("multicall3: contract reverts inside a pack but not alone; its calls are no longer packed")
	}
}

// submit waits for the pack nrq joins and returns its return data when the pack
// answered it, else the outcome that sends it alone.
func (b *Multicall3Batcher) submit(ctx context.Context, n common.Network, u common.Upstream, nrq *common.NormalizedRequest) ([]byte, string) {
	target, data, block, gas, from, ok := multicall3CallOf(ctx, nrq)
	if !ok {
		return nil, multicall3Ineligible
	}
	if b.bypassed(target) {
		return nil, multicall3Bypassed
	}
	if bn := multicall3BlockNumber(block); bn > 0 && bn <= b.absentAtOrBelow.Load() {
		return nil, multicall3Ineligible
	}
	keyBytes, err := json.Marshal([]interface{}{block, gas, from})
	if err != nil {
		return nil, multicall3Ineligible
	}
	key := string(keyBytes)

	e := &multicall3Entry{ctx: ctx, req: nrq, target: target, data: data, result: make(chan multicall3Result, 1)}
	b.mu.Lock()
	bt := b.open[key]
	if bt == nil {
		bt = &multicall3Batch{block: block, gas: gas, from: from}
		b.open[key] = bt
		bt.timer = time.AfterFunc(b.maxWait, func() { b.flush(key, bt, n, u) })
	}
	bt.entries = append(bt.entries, e)
	full := len(bt.entries) >= b.maxCalls
	if full {
		delete(b.open, key)
		bt.timer.Stop()
	}
	b.mu.Unlock()
	if full {
		go b.send(bt, n, u)
	}

	select {
	case r := <-e.result:
		if r.outcome == multicall3Reverted {
			b.awaitingAlone.Store(nrq, [20]byte(target))
		}
		return r.returnData, r.outcome
	case <-ctx.Done():
		return nil, multicall3Canceled
	}
}

func (b *Multicall3Batcher) flush(key string, bt *multicall3Batch, n common.Network, u common.Upstream) {
	b.mu.Lock()
	if b.open[key] != bt {
		// Sent already because it filled up.
		b.mu.Unlock()
		return
	}
	delete(b.open, key)
	b.mu.Unlock()
	b.send(bt, n, u)
}

func (b *Multicall3Batcher) send(bt *multicall3Batch, n common.Network, u common.Upstream) {
	live := make([]*multicall3Entry, 0, len(bt.entries))
	for _, e := range bt.entries {
		if e.ctx.Err() == nil {
			live = append(live, e)
		}
	}
	if len(live) < 2 {
		for _, e := range live {
			e.result <- multicall3Result{outcome: multicall3Alone}
		}
		return
	}

	telemetry.MetricUpstreamMulticall3BatchSize.WithLabelValues(n.ProjectId(), live[0].req.NetworkLabel(), u.Id(), u.VendorName()).Observe(float64(len(live)))
	results, ok := b.aggregate(live, bt, n, u)
	for i, e := range live {
		switch {
		case !ok:
			e.result <- multicall3Result{outcome: multicall3Failed}
		case !results[i].success:
			e.result <- multicall3Result{outcome: multicall3Reverted}
		case len(results[i].data) == 0:
			// The node may answer "0x" differently from a contract that returned
			// nothing (retries, empty-result handling), so let it answer alone.
			e.result <- multicall3Result{outcome: multicall3Empty}
		default:
			e.result <- multicall3Result{returnData: results[i].data, outcome: multicall3Packed}
		}
	}
}

// aggregate sends the pack as one eth_call through the upstream, so its rate
// limits, retries and credit accounting count it once. A pack at the
// "latest" tag carries one more call, the per-architecture block-number call
// (integrity.ChainStateContextProbe): free evidence of the block u executes
// "latest" at, recorded as u's tag-state sample (spec §6.11) and stripped
// before the results meet the entries.
func (b *Multicall3Batcher) aggregate(entries []*multicall3Entry, bt *multicall3Batch, n common.Network, u common.Upstream) ([]aggregate3Result, bool) {
	packed := entries
	tag := bt.block == "latest"
	if tag {
		if probe := multicall3TagProbe(n); probe != nil {
			packed = append(entries[:len(entries):len(entries)], probe)
		} else {
			tag = false
		}
	}
	call := map[string]interface{}{
		"to":   integrity.Multicall3Address,
		"data": "0x" + hex.EncodeToString(encodeAggregate3(packed)),
	}
	if bt.gas != "" {
		call["gas"] = bt.gas
	}
	if bt.from != "" {
		call["from"] = bt.from
	}
	jrq := common.NewJsonRpcRequest("eth_call", []interface{}{call, bt.block})
	if err := jrq.SetID(util.RandomID()); err != nil {
		return nil, false
	}
	first := entries[0].req
	mrq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	mrq.SetCompositeType(common.CompositeTypeMulticall3)
	mrq.SetNetwork(n)
	mrq.SetDirectives(first.Directives().Clone())
	mrq.SetParentRequestId(first.ID())
	mrq.CopyHttpContextFrom(first)
	// The aggregate runs at the Head its entries took (spec §7): a "latest"
	// it still carries resolves to their Latest, never a newer one.
	if h := first.EvmHead(); h != nil {
		mrq.TakeEvmHead(h)
	}

	ctx, cancel := multicall3Context(entries)
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, mrq)

	// The tag-state reference is the network-wide Latest at send, not the
	// entries' Head.
	var latest int64
	if tag {
		latest = common.EvmHeadOf(n, context.Background(), nil).Latest
	}
	sentAt := time.Now()
	resp, err := u.Forward(ctx, mrq, false, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil || resp == nil {
		return nil, false
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil || jrr.Error != nil {
		return nil, false
	}
	var resultHex string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &resultHex); err != nil {
		return nil, false
	}
	raw, err := common.HexToBytes(resultHex)
	if err != nil {
		return nil, false
	}
	if len(raw) == 0 {
		// No code at the Multicall3 address: the block predates its deployment.
		if bn := multicall3BlockNumber(bt.block); bn > 0 {
			for cur := b.absentAtOrBelow.Load(); bn > cur && !b.absentAtOrBelow.CompareAndSwap(cur, bn); cur = b.absentAtOrBelow.Load() {
			}
		}
		return nil, false
	}
	results, ok := decodeAggregate3(raw)
	if !ok || len(results) != len(packed) {
		return nil, false
	}
	if tag {
		if executed, ok := multicall3Word(results[len(entries)]); ok && latest > 0 {
			if p := stateProberFor(n); p != nil {
				p.recordTagSample(u.Id(), executed, latest, sentAt)
			}
		}
	}
	return results[:len(entries)], true
}

// multicall3TagProbe is the pack entry for n's per-architecture block-number
// call; nil when its target cannot be encoded.
func multicall3TagProbe(n common.Network) *multicall3Entry {
	var chainId int64
	if c := n.Config(); c != nil && c.Evm != nil {
		chainId = c.Evm.ChainId
	}
	probe := integrity.ChainStateContextProbe(chainId)
	target, err := common.HexToBytes(probe.To)
	if err != nil || len(target) != 20 {
		return nil
	}
	data, err := common.HexToBytes(probe.Data)
	if err != nil {
		return nil
	}
	return &multicall3Entry{target: target, data: data}
}

// multicall3Word decodes a successful call's single uint64 return word.
func multicall3Word(r aggregate3Result) (int64, bool) {
	if !r.success || len(r.data) != 32 {
		return 0, false
	}
	for _, c := range r.data[:24] {
		if c != 0 {
			return 0, false
		}
	}
	v := binary.BigEndian.Uint64(r.data[24:])
	if v > math.MaxInt64 {
		return 0, false
	}
	return int64(v), true
}

// multicall3Context outlives any one caller, so a caller that gives up costs
// the others nothing; it ends at the last caller's deadline.
func multicall3Context(entries []*multicall3Entry) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(entries[0].ctx)
	var last time.Time
	for _, e := range entries {
		d, ok := e.ctx.Deadline()
		if !ok {
			return context.WithCancel(base)
		}
		if d.After(last) {
			last = d
		}
	}
	return context.WithDeadline(base, last)
}

// multicall3CallOf returns the target, calldata, wire block, gas limit and
// sender of an eth_call that Multicall3 can run exactly: {to, data|input, gas?,
// from?} with no value or state override, since Multicall3 cannot carry those.
// The only sender allowed is the zero address, the one a node assumes when
// none is given: any other sender is what msg.sender would be, and inside the
// pack msg.sender is Multicall3. It is returned so the pack is sent from it
// too, keeping tx.origin exact on nodes whose default sender differs.
func multicall3CallOf(ctx context.Context, nrq *common.NormalizedRequest) (target, data []byte, block interface{}, gas, from string, ok bool) {
	jrq, err := nrq.JsonRpcRequest(ctx)
	if err != nil || jrq == nil {
		return nil, nil, nil, "", "", false
	}
	jrq.RLock()
	defer jrq.RUnlock()

	params := common.WireParams(ctx, nrq, jrq.Params)
	if len(params) != 2 || params[1] == nil {
		return nil, nil, nil, "", "", false
	}
	call, isMap := params[0].(map[string]interface{})
	if !isMap {
		return nil, nil, nil, "", "", false
	}
	var to, input string
	for k, v := range call {
		if v == nil {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return nil, nil, nil, "", "", false
		}
		switch k {
		case "to":
			to = s
		case "gas":
			gas = s
		case "from":
			sender, err := common.HexToBytes(s)
			if err != nil || len(sender) != 20 || [20]byte(sender) != [20]byte{} {
				return nil, nil, nil, "", "", false
			}
			from = "0x" + hex.EncodeToString(sender)
		case "data", "input":
			if input != "" && !strings.EqualFold(input, s) {
				return nil, nil, nil, "", "", false
			}
			input = s
		default:
			return nil, nil, nil, "", "", false
		}
	}
	target, err = common.HexToBytes(to)
	if err != nil || len(target) != 20 {
		return nil, nil, nil, "", "", false
	}
	if input != "" {
		if data, err = common.HexToBytes(input); err != nil {
			return nil, nil, nil, "", "", false
		}
	}
	return target, data, params[1], gas, from, true
}

func multicall3BlockNumber(block interface{}) int64 {
	s, _, err := util.ParseBlockParameter(block)
	if err != nil || !strings.HasPrefix(s, "0x") {
		return 0
	}
	bn, err := common.HexToInt64(s)
	if err != nil {
		return 0
	}
	return bn
}

func encodeAggregate3(entries []*multicall3Entry) []byte {
	size := 4 + 64 + 32*len(entries)
	for _, e := range entries {
		size += 128 + padded32(len(e.data))
	}
	out := make([]byte, 0, size)
	out = append(out, aggregate3Selector...)
	out = appendWord(out, 32)
	out = appendWord(out, uint64(len(entries)))
	offset := uint64(32 * len(entries))
	for _, e := range entries {
		out = appendWord(out, offset)
		offset += uint64(128 + padded32(len(e.data)))
	}
	for _, e := range entries {
		out = append(out, make([]byte, 12)...)
		out = append(out, e.target...)
		out = appendWord(out, 1)  // allowFailure
		out = appendWord(out, 96) // callData offset inside the tuple
		out = appendWord(out, uint64(len(e.data)))
		out = append(out, e.data...)
		out = append(out, make([]byte, padded32(len(e.data))-len(e.data))...)
	}
	return out
}

// decodeAggregate3 decodes (bool success, bytes returnData)[]. Every offset and
// length is bounded by len(raw) before use, so malformed input fails, never panics.
func decodeAggregate3(raw []byte) ([]aggregate3Result, bool) {
	word := func(at int) (int, bool) {
		if at < 0 || at+32 > len(raw) {
			return 0, false
		}
		for _, c := range raw[at : at+24] {
			if c != 0 {
				return 0, false
			}
		}
		v := binary.BigEndian.Uint64(raw[at+24 : at+32])
		if v > uint64(len(raw)) {
			return 0, false
		}
		return int(v), true
	}
	array, ok := word(0)
	if !ok {
		return nil, false
	}
	count, ok := word(array)
	if !ok {
		return nil, false
	}
	head := array + 32
	if count > (len(raw)-head)/32 {
		return nil, false
	}
	results := make([]aggregate3Result, count)
	for i := range results {
		rel, ok := word(head + 32*i)
		if !ok {
			return nil, false
		}
		elem := head + rel
		success, ok := word(elem)
		if !ok || success > 1 {
			return nil, false
		}
		dataRel, ok := word(elem + 32)
		if !ok {
			return nil, false
		}
		length, ok := word(elem + dataRel)
		if !ok {
			return nil, false
		}
		start := elem + dataRel + 32
		if start+length > len(raw) {
			return nil, false
		}
		results[i] = aggregate3Result{success: success == 1, data: raw[start : start+length]}
	}
	return results, true
}

func padded32(n int) int {
	return (n + 31) / 32 * 32
}

func appendWord(out []byte, v uint64) []byte {
	var w [32]byte
	binary.BigEndian.PutUint64(w[24:], v)
	return append(out, w[:]...)
}
