package streams

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	"google.golang.org/grpc/metadata"
)

// Kind is what a stream delivers.
type Kind string

const (
	NewHeads               Kind = "newHeads"
	Logs                   Kind = "logs"
	NewPendingTransactions Kind = "newPendingTransactions"
)

// Finality is the highest block a stream delivers: Latest, Safe,
// Finalized, or Depth(n) blocks below the confirmed head.
type Finality string

const (
	Latest    Finality = "latest"
	Safe      Finality = "safe"
	Finalized Finality = "finalized"
)

// Depth is the finality n blocks below the confirmed head; n is 1 to the
// server's reorg window.
func Depth(n uint32) Finality { return Finality("depth:" + strconv.FormatUint(uint64(n), 10)) }

// Filter selects logs as eth_getLogs does: hex addresses, and per topic
// position the hex values any of which matches; an empty position matches
// any topic.
type Filter struct {
	Address []string
	Topics  [][]string
}

// Cursor stores the resume token of the last block the handler applied.
// Load returns "" when none is stored.
type Cursor interface {
	Load() (string, error)
	Save(token string) error
}

// Config is one stream. Endpoints are grpc(s)://host:port/<project>/evm/<chainId>;
// two endpoints are merged into one chain. Cursor nil keeps the cursor in
// memory only: a new Run starts at the server's bound. HeartbeatSeconds 0
// is 5, the server's lowest default.
type Config struct {
	Endpoints        []string
	Kind             Kind
	Filter           Filter
	Finality         Finality
	Cursor           Cursor
	Metadata         map[string]string
	HeartbeatSeconds uint32
	FullTransactions bool
}

// Handler receives the stream. Apply is called once per block in chain
// order (NewPendingTransactions: once per batch); Undo with the fork point
// when blocks above it were reverted, before the new branch. Both must be
// idempotent: the cursor is saved after they return nil, so a crash in
// between repeats the call. An error from either ends Run with it. Status
// is optional.
type Handler struct {
	Apply  func(ctx context.Context, b Block) error
	Undo   func(ctx context.Context, forkPoint BlockRef) error
	Status func(Status)
}

// endpoint is one parsed Config endpoint.
type endpoint struct {
	raw    string
	target string
	tls    bool
	md     metadata.MD
}

// plan is a validated Config.
type plan struct {
	eps       []endpoint
	kind      Kind
	finality  bdsevm.Finality
	depth     uint32
	reorg     bdsevm.Reorg
	filter    *bdsevm.LogFilter
	heartbeat uint32
	full      bool
	watchdog  time.Duration
}

// defaultHeartbeatSeconds is the heartbeat asked for when Config leaves it
// 0 (spec §9.5): the library always sends one, so its watchdog runs on the
// server's actual interval.
const defaultHeartbeatSeconds = 5

func (c Config) plan(h Handler) (*plan, error) {
	if n := len(c.Endpoints); n < 1 || n > 2 {
		return nil, fmt.Errorf("streams: %d endpoints, want 1 or 2", n)
	}
	if h.Apply == nil {
		return nil, errors.New("streams: Handler.Apply is required")
	}
	if c.HeartbeatSeconds > 30 {
		return nil, fmt.Errorf("streams: HeartbeatSeconds %d, want 0 to 30", c.HeartbeatSeconds)
	}
	p := &plan{kind: c.Kind, heartbeat: c.HeartbeatSeconds, full: c.FullTransactions}
	if p.heartbeat == 0 {
		p.heartbeat = defaultHeartbeatSeconds
	}
	p.watchdog = 3 * time.Duration(p.heartbeat) * time.Second
	for _, raw := range c.Endpoints {
		ep, err := parseEndpoint(raw, c.Metadata)
		if err != nil {
			return nil, err
		}
		p.eps = append(p.eps, ep)
	}
	switch c.Kind {
	case NewHeads, Logs:
	case NewPendingTransactions:
		if c.Finality != "" || c.Cursor != nil || len(c.Filter.Address) > 0 || len(c.Filter.Topics) > 0 {
			return nil, errors.New("streams: newPendingTransactions takes no Finality, Cursor or Filter")
		}
		return p, nil
	default:
		return nil, fmt.Errorf("streams: unknown Kind %q", c.Kind)
	}
	if c.FullTransactions {
		return nil, errors.New("streams: FullTransactions applies to newPendingTransactions only")
	}
	switch f := c.Finality; {
	case f == "" || f == Latest:
		p.finality = bdsevm.Finality_FINALITY_LATEST
	case f == Safe:
		p.finality = bdsevm.Finality_FINALITY_SAFE
	case f == Finalized:
		p.finality = bdsevm.Finality_FINALITY_FINALIZED
		p.reorg = bdsevm.Reorg_REORG_NONE
	case strings.HasPrefix(string(f), "depth:"):
		n, err := strconv.ParseUint(strings.TrimPrefix(string(f), "depth:"), 10, 32)
		if err != nil || n == 0 {
			return nil, fmt.Errorf("streams: Finality %q: want depth:N, N ≥ 1", f)
		}
		p.finality, p.depth = bdsevm.Finality_FINALITY_DEPTH, uint32(n)
	default:
		return nil, fmt.Errorf("streams: unknown Finality %q", f)
	}
	if h.Undo == nil && p.reorg != bdsevm.Reorg_REORG_NONE {
		return nil, errors.New("streams: Handler.Undo is required below finalized")
	}
	if c.Kind == NewHeads {
		if len(c.Filter.Address) > 0 || len(c.Filter.Topics) > 0 {
			return nil, errors.New("streams: Filter applies to logs only")
		}
		return p, nil
	}
	f, err := c.Filter.proto()
	if err != nil {
		return nil, err
	}
	p.filter = f
	return p, nil
}

func (f Filter) proto() (*bdsevm.LogFilter, error) {
	out := &bdsevm.LogFilter{}
	for _, a := range f.Address {
		b, err := hexBytes(a, 20)
		if err != nil {
			return nil, fmt.Errorf("streams: Filter.Address %q: %w", a, err)
		}
		out.Address = append(out.Address, b)
	}
	for i, pos := range f.Topics {
		tf := &bdsevm.TopicFilter{}
		for _, t := range pos {
			b, err := hexBytes(t, 32)
			if err != nil {
				return nil, fmt.Errorf("streams: Filter.Topics[%d] %q: %w", i, t, err)
			}
			tf.Values = append(tf.Values, b)
		}
		out.Topics = append(out.Topics, tf)
	}
	return out, nil
}

func hexBytes(s string, n int) ([]byte, error) {
	b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X"))
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		return nil, fmt.Errorf("%d bytes, want %d", len(b), n)
	}
	return b, nil
}

// parseEndpoint reads grpc(s)://host:port/<project>/evm/<chainId>: the
// library dials host:port and names the path in metadata, as eRPC's gRPC
// server reads it.
func parseEndpoint(raw string, extra map[string]string) (endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return endpoint{}, fmt.Errorf("streams: endpoint %q: %w", raw, err)
	}
	ep := endpoint{raw: raw}
	port := ""
	switch u.Scheme {
	case "grpc":
		port = "80"
	case "grpcs":
		ep.tls, port = true, "443"
	default:
		return endpoint{}, fmt.Errorf("streams: endpoint %q: scheme must be grpc or grpcs", raw)
	}
	if u.Port() != "" {
		port = u.Port()
	}
	if u.Hostname() == "" {
		return endpoint{}, fmt.Errorf("streams: endpoint %q: no host", raw)
	}
	ep.target = net.JoinHostPort(u.Hostname(), port)
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] != "evm" {
		return endpoint{}, fmt.Errorf("streams: endpoint %q: path must be /<project>/evm/<chainId>", raw)
	}
	if _, err := strconv.ParseUint(parts[2], 10, 64); err != nil {
		return endpoint{}, fmt.Errorf("streams: endpoint %q: chain id %q", raw, parts[2])
	}
	ep.md = metadata.New(extra)
	ep.md.Set("x-erpc-project", parts[0])
	ep.md.Set("x-erpc-chain-id", parts[2])
	return ep, nil
}
