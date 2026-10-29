package erpc

import (
	"encoding/hex"
	"strconv"

	"github.com/erpc/erpc/indexer"
)

// Session events a stream writes besides its data (streams spec §3.2).
// The append functions write each one's JSON object (§5.2) with its
// "type" first.

// streamHeartbeat is a heartbeat's content: the confirmed head and the
// lane's finalized and safe numbers, safe 0 while not computed.
type streamHeartbeat struct {
	head            indexer.Cursor
	finalized, safe uint64
}

// streamEnd is why a stream ended (§3.4): reason and detail label
// erpc_tap_end_total; oldest is the lowest pin for out_of_range, block the
// reverted block for final_reverted. An end with no reason is an error no
// row of §3.4 names (a tap panic, ...): err.
type streamEnd struct {
	reason, detail string
	oldest, block  *indexer.Cursor
	err            error
}

func appendQuantity(b []byte, n uint64) []byte {
	b = append(b, `"0x`...)
	b = strconv.AppendUint(b, n, 16)
	return append(b, '"')
}

func appendHash(b []byte, h [32]byte) []byte {
	b = append(b, `"0x`...)
	b = hex.AppendEncode(b, h[:])
	return append(b, '"')
}

func appendType(b []byte, typ string) []byte {
	b = append(b, '{')
	if typ != "" {
		b = append(b, `"type":"`...)
		b = append(b, typ...)
		b = append(b, `",`...)
	}
	return b
}

func appendRef(b []byte, number uint64, hash [32]byte) []byte {
	b = append(b, `{"number":`...)
	b = appendQuantity(b, number)
	b = append(b, `,"hash":`...)
	b = appendHash(b, hash)
	return append(b, '}')
}

// appendBlockEnd: {"type":"blockEnd","block":{number,hash,parentHash},"logs":n}.
func appendBlockEnd(b []byte, typ string, blk indexer.Block, logs int) []byte {
	b = appendType(b, typ)
	b = append(b, `"block":{"number":`...)
	b = appendQuantity(b, blk.Number)
	b = append(b, `,"hash":`...)
	b = appendHash(b, blk.Hash)
	b = append(b, `,"parentHash":`...)
	b = appendHash(b, blk.ParentHash)
	b = append(b, `},"logs":`...)
	b = strconv.AppendInt(b, int64(logs), 10)
	return append(b, '}')
}

// appendHeartbeat: {"type":"heartbeat","head":{number,hash},"finalized":n,"safe":n};
// safe is absent while Head.Safe is not computed (§5.2).
func appendHeartbeat(b []byte, typ string, hb streamHeartbeat) []byte {
	b = appendType(b, typ)
	b = append(b, `"head":`...)
	b = appendRef(b, hb.head.Number, hb.head.Hash)
	b = append(b, `,"finalized":`...)
	b = appendQuantity(b, hb.finalized)
	if hb.safe > 0 {
		b = append(b, `,"safe":`...)
		b = appendQuantity(b, hb.safe)
	}
	return append(b, '}')
}

// appendUndo: {"type":"undo","forkPoint":{number,hash}}.
func appendUndo(b []byte, typ string, forkPoint indexer.Cursor) []byte {
	b = appendType(b, typ)
	b = append(b, `"forkPoint":`...)
	b = appendRef(b, forkPoint.Number, forkPoint.Hash)
	return append(b, '}')
}

// appendStatus: {"type":"status","state":s,"reason":r}; no reason for ok.
func appendStatus(b []byte, typ, state, reason string) []byte {
	b = appendType(b, typ)
	b = append(b, `"state":"`...)
	b = append(b, state...)
	b = append(b, '"')
	if reason != "" {
		b = append(b, `,"reason":"`...)
		b = append(b, reason...)
		b = append(b, '"')
	}
	return append(b, '}')
}

// appendEnd: {"type":"end","reason":r,"detail":d,"oldest":{number,hash},"block":{number,hash}}.
func appendEnd(b []byte, typ string, e streamEnd) []byte {
	b = appendType(b, typ)
	b = append(b, `"reason":"`...)
	b = append(b, e.reason...)
	b = append(b, '"')
	if e.detail != "" {
		b = append(b, `,"detail":"`...)
		b = append(b, e.detail...)
		b = append(b, '"')
	}
	if e.oldest != nil {
		b = append(b, `,"oldest":`...)
		b = appendRef(b, e.oldest.Number, e.oldest.Hash)
	}
	if e.block != nil {
		b = append(b, `,"block":`...)
		b = appendRef(b, e.block.Number, e.block.Hash)
	}
	return append(b, '}')
}
