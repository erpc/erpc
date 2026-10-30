package streams

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
)

// Hash is a 32-byte block or transaction hash.
type Hash [32]byte

// String is the hash as 0x-prefixed lowercase hex.
func (h Hash) String() string { return "0x" + hex.EncodeToString(h[:]) }

func hashOf(b []byte) (Hash, error) {
	if len(b) != 32 {
		return Hash{}, fmt.Errorf("hash of %d bytes, not 32", len(b))
	}
	return Hash(b), nil
}

// BlockRef names one block.
type BlockRef struct {
	Number uint64
	Hash   Hash
}

// Token is the resume token of the ref: <numberHex>:<hash>.
func (r BlockRef) Token() string {
	return "0x" + strconv.FormatUint(r.Number, 16) + ":" + r.Hash.String()
}

// ParseToken reads a resume token written by Token.
func ParseToken(token string) (BlockRef, error) {
	num, hx, ok := strings.Cut(strings.TrimSpace(token), ":")
	if !ok || !strings.HasPrefix(num, "0x") || !strings.HasPrefix(hx, "0x") {
		return BlockRef{}, fmt.Errorf("token %q: want <0xnumber>:<0xhash>", token)
	}
	n, err := strconv.ParseUint(num[2:], 16, 64)
	if err != nil {
		return BlockRef{}, fmt.Errorf("token %q: number: %w", token, err)
	}
	raw, err := hex.DecodeString(hx[2:])
	if err != nil {
		return BlockRef{}, fmt.Errorf("token %q: hash: %w", token, err)
	}
	h, err := hashOf(raw)
	if err != nil {
		return BlockRef{}, fmt.Errorf("token %q: %w", token, err)
	}
	return BlockRef{Number: n, Hash: h}, nil
}

// Block is what Apply gets. NewHeads fills Header; Logs fills Logs, every
// log the filter matched in the block, possibly none; NewPendingTransactions
// fills Hashes, or Transactions with FullTransactions, and leaves the
// block fields zero.
type Block struct {
	BlockRef
	ParentHash   Hash
	Header       *bdsevm.BlockHeader
	Logs         []Log
	Hashes       []Hash
	Transactions []*bdsevm.Transaction
}

// Log is one matched log.
type Log struct{ *bdsevm.Log }

// Key is the log's dedup key, stable across instances and reconnects:
// <blockHash>:<logIndex>.
func (l Log) Key() string {
	return "0x" + hex.EncodeToString(l.BlockHash) + ":" + strconv.FormatUint(uint64(l.LogIndex), 10)
}

// State is a stream's state as Status reports it.
type State string

const (
	StateOK           State = "ok"
	StateDegraded     State = "degraded"
	StateLagging      State = "lagging"
	StateReconnecting State = "reconnecting"
)

// Status is a state change of one endpoint's stream. Reason is the
// server's reason, or for StateReconnecting the gRPC code name of the end,
// or "watchdog".
type Status struct {
	State    State
	Reason   string
	Endpoint string
}

// OutOfRangeError ends Run when Endpoint holds no block the cursor can
// resume from and no other endpoint streams: Cursor is the cursor that
// endpoint's stream resumed from, Oldest the lowest block it could resume
// from, when it named one. Nothing older than the server's window is
// backfilled.
type OutOfRangeError struct {
	Endpoint string
	Cursor   string
	Reason   string
	Oldest   *BlockRef
}

func (e *OutOfRangeError) Error() string {
	s := fmt.Sprintf("%s: cursor %s out of range (%s)", e.Endpoint, e.Cursor, e.Reason)
	if e.Oldest != nil {
		s += "; oldest resumable " + e.Oldest.Token()
	}
	return s
}

// FinalRevertedError ends a finalized stream when a majority reverted a
// block it delivered.
type FinalRevertedError struct {
	Endpoint string
	Message  string
}

func (e *FinalRevertedError) Error() string { return e.Endpoint + ": " + e.Message }

// MessageTooLargeError ends Run when Endpoint cannot send one message of
// Block within its message bound (spec §3.4 message_too_large): a resume
// from the cursor meets the same block.
type MessageTooLargeError struct {
	Endpoint     string
	Block        *BlockRef
	MessageBytes int
	MaxBytes     int
}

func (e *MessageTooLargeError) Error() string {
	b := "unknown"
	if e.Block != nil {
		b = e.Block.Token()
	}
	return fmt.Sprintf("%s: block %s needs a %d-byte message, over the %d-byte bound", e.Endpoint, b, e.MessageBytes, e.MaxBytes)
}
