package integrity

import "github.com/erpc/erpc/common"

// callTracer output. debug_trace* accepts several tracers whose result shapes
// have nothing in common — the struct logger returns {gas, failed, structLogs},
// prestateTracer returns an address→state map, callTracer returns a call tree —
// and the response alone does not say which ran. The discriminator used
// throughout is the ROOT frame's "type": a callTracer frame always carries one
// (measured across 12,400 frames on 5 chains, none missing), the other tracers
// never produce that shape. A response we cannot identify as callTracer is
// skipped, never judged.
type CallFrame struct {
	Type    string      `json:"type"`
	From    string      `json:"from"`
	To      string      `json:"to"`
	Gas     string      `json:"gas"`
	GasUsed string      `json:"gasUsed"`
	Error   string      `json:"error"`
	Calls   []CallFrame `json:"calls"`
}

// TraceEntry is one element of a debug_traceBlock* response: geth wraps each
// transaction's trace as {txHash, result}.
type TraceEntry struct {
	TxHash string     `json:"txHash"`
	Result *CallFrame `json:"result"`
	Error  string     `json:"error"`
}

// BlockTraces decodes a debug_traceBlock* response as a callTracer trace list.
// ok=false when the response is not a list of callTracer frames — a different
// tracer, an error shape, or anything unparseable — in which case the caller
// must skip.
func (d *Decoded) BlockTraces() ([]TraceEntry, bool) {
	if !d.blockTracesDone {
		d.blockTracesDone = true
		var entries []TraceEntry
		if err := common.SonicCfg.Unmarshal(d.raw, &entries); err != nil {
			return nil, false
		}
		// An empty list is a legitimate answer for an empty block, and it is
		// also the everything-dropped corruption shape; both are for the
		// reconciliation check to judge against the header, so it decodes ok.
		for _, e := range entries {
			if e.Result == nil || e.Result.Type == "" {
				return nil, false // not callTracer output
			}
		}
		d.blockTraces = entries
		d.blockTracesOK = true
	}
	return d.blockTraces, d.blockTracesOK
}

// CallTrace decodes a debug_traceTransaction response as a single callTracer
// frame. ok=false for any other tracer's output.
func (d *Decoded) CallTrace() (*CallFrame, bool) {
	if !d.callTraceDone {
		d.callTraceDone = true
		var f CallFrame
		if err := common.SonicCfg.Unmarshal(d.raw, &f); err != nil || f.Type == "" {
			return nil, false
		}
		d.callTrace = &f
	}
	return d.callTrace, d.callTrace != nil
}
