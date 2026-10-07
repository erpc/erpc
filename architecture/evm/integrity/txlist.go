package integrity

import (
	"strings"

	"github.com/erpc/erpc/common"
)

// TxList is a block's "transactions" array: hash strings (fullTransactions =
// false) or full objects. Callers can read the list's shape (Len, whether an
// entry is a full object) without decoding anything. Contents are only
// available through the explicit accessors (object here, Decoded.Transactions
// for the typed view), which always decode the real entry. There are no
// placeholders, so no check can read data that was never decoded.
//
// The zero value is an absent/null list (Len 0).
type TxList struct {
	// anys is set when the list came from a generic decode (TxList's own
	// UnmarshalJSON, or the exact legacy fallback in Decoded.Header): every
	// entry is already materialised exactly as a []any decode yields it.
	anys []any
	// raws is set by the shared fast path: each entry's raw JSON, sliced
	// out of the (validated, plain) response document.
	raws []string
}

// Len returns the number of entries.
func (l TxList) Len() int {
	if l.raws != nil {
		return len(l.raws)
	}
	return len(l.anys)
}

// IsObject reports whether entry i is a full transaction object rather than
// a bare hash (or some other non-object entry).
func (l TxList) IsObject(i int) bool {
	if l.raws != nil {
		r := l.raws[i]
		return len(r) > 0 && r[0] == '{'
	}
	_, ok := l.anys[i].(map[string]any)
	return ok
}

// Object decodes entry i as a generic JSON object, exactly as a []any decode
// of the whole list would have. ok is false for non-object entries.
func (l TxList) Object(i int) (map[string]any, bool) {
	if l.raws == nil {
		m, ok := l.anys[i].(map[string]any)
		return m, ok
	}
	if !l.IsObject(i) {
		return nil, false
	}
	var m map[string]any
	if err := common.SonicCfg.UnmarshalFromString(l.raws[i], &m); err != nil {
		return nil, false
	}
	return m, true
}

// UnmarshalJSON decodes the list the way a []any field always did, so any
// code decoding a Header directly (e.g. the chain view's header fetches)
// gets the same accept/reject behaviour as before.
func (l *TxList) UnmarshalJSON(b []byte) error {
	var a []any
	if err := common.SonicCfg.Unmarshal(b, &a); err != nil {
		return err
	}
	*l = TxList{anys: a}
	return nil
}

// MarshalJSON re-emits the list as an array (or null when absent).
func (l TxList) MarshalJSON() ([]byte, error) {
	if l.raws != nil {
		return []byte("[" + strings.Join(l.raws, ",") + "]"), nil
	}
	if l.anys == nil {
		return []byte("null"), nil
	}
	return common.SonicCfg.Marshal(l.anys)
}
