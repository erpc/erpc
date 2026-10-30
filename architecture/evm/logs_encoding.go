package evm

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/erpc/erpc/architecture/evm/logfilter"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
)

// LogsEncoding is one transport's encoding of the logs a filter matches in
// a block (streams spec §6.2): each log is converted once per block and
// shared by every filter; each (block, filter) is encoded once from those
// conversions and shared by every tap with that filter. Conversions live
// on the held receipts; a filter's encoding lives in its filterState,
// within the tracker's FilterStateBytes.
type LogsEncoding[L, T any] struct {
	transport string
	convert   func(log json.RawMessage) (L, error)
	encode    func(b indexer.Block, logs []L) (T, int, error)
}

// NewLogsEncoding is transport's encoding: convert converts one log;
// encode encodes a block's matched logs, in order, and reports the bytes
// the encoding holds beyond the conversions it shares; transport labels
// panics.
func NewLogsEncoding[L, T any](transport string, convert func(log json.RawMessage) (L, error),
	encode func(b indexer.Block, logs []L) (T, int, error)) *LogsEncoding[L, T] {
	return &LogsEncoding[L, T]{transport: transport, convert: convert, encode: encode}
}

type logConverted[L any] struct {
	once sync.Once
	v    L
	err  error
}

// Of is the encoding of b's logs that f matches, from the receipts the
// tracker holds for b, else from b.Receipts, encoded for this call alone:
// a Read's own fetch below the window. A block without receipts, or whose
// logs do not parse, encodes no logs, as MatchedLogs reads none. A
// conversion's error, or panic, is the encoding's, for every later tap too.
func (e *LogsEncoding[L, T]) Of(t *ChainTracker, b indexer.Block, f *logfilter.LogsFilter) (T, error) {
	r := t.receiptsOf(b)
	var s *filterState
	if r != nil {
		s = t.filterOf(r, f)
	}
	if s == nil || len(s.pos) == 0 {
		v, _, err := e.encode(b, nil)
		return v, err
	}
	x, ok := s.encodings.Load(e)
	if !ok {
		x, _ = s.encodings.LoadOrStore(e, &headerEncoded[T]{})
	}
	enc := x.(*headerEncoded[T])
	enc.once.Do(func() {
		defer e.recover(t, &enc.err)
		logs := make([]L, len(s.pos))
		for i, p := range s.pos {
			if logs[i], enc.err = e.log(t, r, p); enc.err != nil {
				return
			}
		}
		var held int
		if enc.v, held, enc.err = e.encode(b, logs); enc.err == nil {
			t.grewFilter(s, held)
		}
	})
	return enc.v, enc.err
}

// log is the conversion of r's log p, once per receipts.
func (e *LogsEncoding[L, T]) log(t *ChainTracker, r *heldReceipts, p int32) (L, error) {
	x, ok := r.encodings.Load(e)
	if !ok {
		x, _ = r.encodings.LoadOrStore(e, make([]logConverted[L], len(r.logs)))
	}
	c := &x.([]logConverted[L])[p]
	c.once.Do(func() {
		defer e.recover(t, &c.err)
		c.v, c.err = e.convert(r.logs[p])
	})
	return c.v, c.err
}

func (e *LogsEncoding[L, T]) recover(t *ChainTracker, err *error) {
	if rec := recover(); rec != nil {
		telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
			"logs-encoding",
			fmt.Sprintf("network:%s transport:%s", t.networkLabel(), e.transport),
			common.ErrorFingerprint(rec),
		).Inc()
		*err = fmt.Errorf("encoding panicked: %v", rec)
	}
}
