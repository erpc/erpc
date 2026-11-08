package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	bdsevm "github.com/blockchain-data-standards/manifesto/evm"
	gethcommon "github.com/ethereum/go-ethereum/common"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/erpc/erpc/test/chainsim"
)

// The gRPC metadata each pod's Prism clients send: its index, and whether
// the cache connector (not the prism upstream) sent the call.
const (
	prismPodHeader   = "x-realworld-pod"
	prismCacheHeader = "x-realworld-cache"
)

// prismKeyName keys a spec's Prism upstream in upEvm and up, as a vendor
// key does.
const prismKeyName = "prism"

// prismFinalityDepth is the finality depth sp's vendors serve, which its
// Prism serves too: the deepest any vendor's faults set, else
// chainsim.FinalizedLag.
func prismFinalityDepth(sp spec) uint64 {
	depth := uint64(0)
	for _, f := range sp.faults {
		depth = max(depth, f.FinalityDepth)
	}
	if depth == 0 {
		return chainsim.FinalizedLag
	}
	return depth
}

// prismConnector is the cache connector id of n's Prism.
func prismConnector(n *netSpec) string { return fmt.Sprintf("prism-%d", n.chainID) }

// prism is a BDS reader of a scenario's chain at one address, as Prism is
// to a network: every pod's cache connector and its prism upstream dial
// it. It learns each canonical block lag after its mint and serves it by
// number, hash and transaction. It counts each pod's calls by role and
// kind: anyRead, and "latest" for a latest block by tag.
type prism struct {
	chain *chainsim.Chain
	depth uint64
	lag   time.Duration
	ln    net.Listener
	srv   *grpc.Server

	mu    sync.Mutex
	calls map[prismKey]int64
}

type prismKey struct {
	pod   int
	cache bool
	kind  string
}

// startPrism serves chain until close, its finalized block depth below
// the tip, each block lag after its mint.
func startPrism(chain *chainsim.Chain, depth uint64, lag time.Duration) (*prism, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &prism{chain: chain, depth: depth, lag: lag, ln: ln, calls: map[prismKey]int64{}}
	p.srv = grpc.NewServer(grpc.UnaryInterceptor(p.count))
	bdsevm.RegisterRPCQueryServiceServer(p.srv, prismRPC{p: p})
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// tip is the newest canonical block prism learned: minted lag ago or
// earlier.
func (p *prism) tip() *chainsim.Block {
	b := p.chain.Tip()
	for at := time.Now().Add(-p.lag); p.lag > 0 && b.Parent != nil && b.MintedAt.After(at); {
		b = b.Parent
	}
	return b
}

// learned is b when prism has learned it, else nil.
func (p *prism) learned(b *chainsim.Block) *chainsim.Block {
	if b == nil || p.lag > 0 && b.Number > p.tip().Number {
		return nil
	}
	return b
}

// addr is the host:port every pod's connector and upstream name.
func (p *prism) addr() string { return p.ln.Addr().String() }

func (p *prism) close() { p.srv.Stop() }

// reads counts pod's calls of kind by its cache connector (cache) or its
// prism upstream.
func (p *prism) reads(pod int, cache bool, kind string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[prismKey{pod, cache, kind}]
}

func (p *prism) count(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get(prismPodHeader); len(v) > 0 {
		if pod, err := strconv.Atoi(v[0]); err == nil {
			cache := len(md.Get(prismCacheHeader)) > 0
			p.mu.Lock()
			p.calls[prismKey{pod, cache, anyRead}]++
			if r, ok := req.(*bdsevm.GetBlockByNumberRequest); ok && r.GetBlockNumber() == "latest" {
				p.calls[prismKey{pod, cache, "latest"}]++
			}
			p.mu.Unlock()
		}
	}
	return handler(ctx, req)
}

type prismRPC struct {
	bdsevm.UnimplementedRPCQueryServiceServer
	p *prism
}

func (r prismRPC) ChainId(context.Context, *bdsevm.ChainIdRequest) (*bdsevm.ChainIdResponse, error) {
	return &bdsevm.ChainIdResponse{ChainId: uint64(r.p.chain.ID())}, nil
}

func (r prismRPC) GetBlockByNumber(_ context.Context, req *bdsevm.GetBlockByNumberRequest) (*bdsevm.GetBlockResponse, error) {
	tip := r.p.tip()
	var b *chainsim.Block
	switch tag := req.GetBlockNumber(); tag {
	case "latest", "pending":
		b = tip
	case "finalized", "safe":
		b = r.p.chain.CanonAt(r.p.chain.Finalized(tip.Number, r.p.depth))
	case "earliest":
	default:
		if n, err := strconv.ParseUint(strings.TrimPrefix(tag, "0x"), 16, 64); err == nil && n <= tip.Number {
			b = r.p.chain.CanonAt(n)
		}
	}
	return blockResponse(b)
}

func (r prismRPC) GetBlockByHash(_ context.Context, req *bdsevm.GetBlockByHashRequest) (*bdsevm.GetBlockResponse, error) {
	b := r.p.chain.Block(gethcommon.BytesToHash(req.GetBlockHash()))
	if b != nil && r.p.chain.CanonAt(b.Number) != b {
		b = nil
	}
	return blockResponse(r.p.learned(b))
}

func (r prismRPC) GetTransactionReceipt(_ context.Context, req *bdsevm.GetTransactionReceiptRequest) (*bdsevm.GetTransactionReceiptResponse, error) {
	b, i := r.p.chain.Tx(gethcommon.BytesToHash(req.GetTransactionHash()))
	if r.p.learned(b) == nil {
		return &bdsevm.GetTransactionReceiptResponse{}, nil
	}
	raw, err := json.Marshal(b.Receipt(i))
	if err != nil {
		return nil, err
	}
	var jr bdsevm.JsonRpcReceipt
	if err := json.Unmarshal(raw, &jr); err != nil {
		return nil, err
	}
	rc, err := jr.ToProto()
	if err != nil {
		return nil, err
	}
	return &bdsevm.GetTransactionReceiptResponse{Receipt: rc}, nil
}

// blockResponse is b's header and transaction hashes; no block for nil.
func blockResponse(b *chainsim.Block) (*bdsevm.GetBlockResponse, error) {
	if b == nil {
		return &bdsevm.GetBlockResponse{}, nil
	}
	pb, err := bdsevm.ParseJsonRpcBlock(b.JSON(), json.Unmarshal)
	if err != nil {
		return nil, err
	}
	return &bdsevm.GetBlockResponse{Block: pb.Header, Transactions: pb.TransactionHashes}, nil
}
