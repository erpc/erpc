package erpc

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/erpc/erpc/indexer"
)

// StreamOptions are one stream's options (streams spec §3.1), from the
// ws subscribe's erpc object and the connection's URL.
type StreamOptions struct {
	// Extended: the subscribe carries an erpc object, or the URL sets an
	// option for its kind (§5.1); the stream then sends heartbeats,
	// status and ends.
	Extended bool
	// From is the last block the client applied; nil starts after the
	// bound at subscribe.
	From *indexer.Cursor
	// Heartbeat is the idle interval; 0 is the server default.
	Heartbeat time.Duration
	// BlockEnd: a blockEnd after each applied block's logs.
	BlockEnd bool
}

// streamOption is one row of §3.1: its name, the kinds it applies to,
// whether a ws URL may set it, and how its value sets StreamOptions.
type streamOption struct {
	name  string
	kinds []string
	url   bool
	set   func(o *StreamOptions, v any) error
}

var streamOptions = []streamOption{
	{name: "from", kinds: []string{indexer.SubTypeNewHeads, indexer.SubTypeLogs}, set: func(o *StreamOptions, v any) error {
		c, err := parseStreamCursor(v)
		if err != nil {
			return err
		}
		o.From = &c
		return nil
	}},
	{name: "heartbeat", kinds: []string{indexer.SubTypeNewHeads, indexer.SubTypeLogs, indexer.SubTypeNewPendingTransactions}, url: true,
		set: func(o *StreamOptions, v any) error {
			n, ok := optionNumber(v)
			if !ok || n != math.Trunc(n) || n < 0 || n > heartbeatMax.Seconds() {
				return fmt.Errorf("must be whole seconds, 1 to %d, or 0 for the server default", int(heartbeatMax.Seconds()))
			}
			o.Heartbeat = time.Duration(n) * time.Second
			return nil
		}},
	{name: "blockEnd", kinds: []string{indexer.SubTypeLogs}, url: true, set: func(o *StreamOptions, v any) error {
		b, ok := v.(bool)
		if !ok {
			return fmt.Errorf("must be true or false")
		}
		o.BlockEnd = b
		return nil
	}},
}

// streamDefaults are a ws connection's URL options, by name.
type streamDefaults map[string]any

// streamOptionError is an option a client set wrong; it names the key
// (§3.1): -32602 on ws, 400 at a ws upgrade.
type streamOptionError struct{ name, reason string }

func (e streamOptionError) Error() string {
	return fmt.Sprintf("invalid erpc option %q: %s", e.name, e.reason)
}

func errStreamOption(name, reason string) error { return streamOptionError{name, reason} }

func streamOptionNamed(name string) *streamOption {
	for i := range streamOptions {
		if streamOptions[i].name == name {
			return &streamOptions[i]
		}
	}
	return nil
}

// parseStreamURL reads the stream options of a ws upgrade URL: each value
// is a JSON literal, else a string. Other query keys (auth, directives)
// are not options and pass.
func parseStreamURL(q url.Values) (streamDefaults, error) {
	var d streamDefaults
	for _, o := range streamOptions {
		vs, ok := q[o.name]
		if !ok {
			continue
		}
		if !o.url {
			return nil, errStreamOption(o.name, "set it per subscription, not in the URL")
		}
		if len(vs) != 1 {
			return nil, errStreamOption(o.name, "given more than once")
		}
		var v any
		if err := json.Unmarshal([]byte(vs[0]), &v); err != nil {
			v = vs[0]
		}
		if err := o.set(&StreamOptions{}, v); err != nil {
			return nil, errStreamOption(o.name, err.Error())
		}
		if d == nil {
			d = streamDefaults{}
		}
		d[o.name] = v
	}
	return d, nil
}

// splitStreamParams takes the erpc object off eth_subscribe params: the
// last param after the type, an object whose one key is erpc (§5.1).
// opts is nil when the params carry none.
func splitStreamParams(params []any) (rest []any, opts map[string]any, err error) {
	if len(params) < 2 {
		return params, nil, nil
	}
	last, ok := params[len(params)-1].(map[string]any)
	if !ok || len(last) != 1 {
		return params, nil, nil
	}
	raw, ok := last["erpc"]
	if !ok {
		return params, nil, nil
	}
	if opts, ok = raw.(map[string]any); !ok {
		return nil, nil, errStreamOption("erpc", "must be an object")
	}
	return params[:len(params)-1], opts, nil
}

// parseStreamOptions merges a subscription's own options over the
// connection's URL defaults, per key, for a subscription of kind. A
// subscription's unknown key, or a key with no row for kind, is -32602;
// a URL default with no row for kind does not apply to it.
func parseStreamOptions(kind string, own map[string]any, defaults streamDefaults) (StreamOptions, error) {
	o := StreamOptions{Extended: own != nil}
	for name := range own {
		opt := streamOptionNamed(name)
		if opt == nil {
			return o, errStreamOption(name, "unknown option")
		}
		if !slices.Contains(opt.kinds, kind) {
			return o, errStreamOption(name, "does not apply to "+kind)
		}
	}
	for _, opt := range streamOptions {
		v, ok := own[opt.name]
		if !ok {
			if v, ok = defaults[opt.name]; !ok || !slices.Contains(opt.kinds, kind) {
				continue
			}
		}
		if err := opt.set(&o, v); err != nil {
			return o, errStreamOption(opt.name, err.Error())
		}
		o.Extended = true
	}
	return o, nil
}

// parseStreamCursor reads a token, <numberHex>:<hash>, or an object
// {number, hash} (§3.5).
func parseStreamCursor(v any) (indexer.Cursor, error) {
	var number, hash string
	switch t := v.(type) {
	case string:
		var ok bool
		if number, hash, ok = strings.Cut(t, ":"); !ok {
			return indexer.Cursor{}, fmt.Errorf("must be <numberHex>:<hash>")
		}
	case map[string]any:
		h, ok := t["hash"].(string)
		if !ok || len(t) != 2 {
			return indexer.Cursor{}, fmt.Errorf("must be {number, hash}")
		}
		hash = h
		if n, ok := optionNumber(t["number"]); ok && n >= 0 && n == math.Trunc(n) && n <= 1<<53 {
			number = "0x" + strconv.FormatUint(uint64(n), 16)
		} else if s, ok := t["number"].(string); ok {
			number = s
		} else {
			return indexer.Cursor{}, fmt.Errorf("number must be a quantity")
		}
	default:
		return indexer.Cursor{}, fmt.Errorf("must be <numberHex>:<hash> or {number, hash}")
	}
	hexNum, ok := strings.CutPrefix(number, "0x")
	if !ok || hexNum == "" {
		return indexer.Cursor{}, fmt.Errorf("number must be a 0x quantity")
	}
	n, err := strconv.ParseUint(hexNum, 16, 64)
	if err != nil {
		return indexer.Cursor{}, fmt.Errorf("number must be a 0x quantity")
	}
	c := indexer.Cursor{Number: n}
	hexHash, ok := strings.CutPrefix(hash, "0x")
	if !ok || len(hexHash) != 64 {
		return indexer.Cursor{}, fmt.Errorf("hash must be 32 bytes of 0x hex")
	}
	if _, err := hex.Decode(c.Hash[:], []byte(hexHash)); err != nil {
		return indexer.Cursor{}, fmt.Errorf("hash must be 32 bytes of 0x hex")
	}
	return c, nil
}

// optionNumber reads a JSON number as decoded by encoding/json or sonic.
func optionNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
