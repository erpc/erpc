package erpc

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
)

// blockStreamMaxBackfill bounds how many blocks a single advance may backfill.
// A live subscriber is at most a few blocks behind, so a larger gap means a cold
// start (head still 0 / just observed) or a >tolerance deep reorg — in either
// case we resync to the current head rather than replay a long (or historical)
// range down a live tip stream. Reorg-correct replay is ChainView's job.
const blockStreamMaxBackfill = 256

// blockStreamManager owns one long-lived head-watch hub per (project, network).
// It is the reorg-unaware, poller-backed implementation of the head feed the
// StreamBlocks handler consumes; ChainView will later provide the same head
// signal (gap-free, reorg-aware) behind this same seam without touching the
// handler.
type blockStreamManager struct {
	mu   sync.Mutex
	hubs map[string]*blockStreamHub
}

func newBlockStreamManager() *blockStreamManager {
	return &blockStreamManager{hubs: make(map[string]*blockStreamHub)}
}

// hubFor returns the hub for a network, creating it (and wiring its head callback
// onto the network's upstream pollers) exactly once. The callback registration is
// permanent — OnLatestBlock cannot be undone — so it must happen once per network,
// not per subscriber, which is why the hub is cached here.
func (m *blockStreamManager) hubFor(ctx context.Context, network *Network) *blockStreamHub {
	key := network.projectId + "/" + network.networkId

	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.hubs[key]; ok {
		return h
	}

	h := &blockStreamHub{subs: make(map[*blockStreamSub]struct{})}
	// Wire the hub onto every upstream's latest-block callback and seed the head
	// with the highest value any of them already observed. The head is the max
	// across upstreams; advance() merges the independent callbacks monotonically.
	for _, up := range network.upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId) {
		sp := up.EvmStatePoller()
		if sp == nil || sp.IsObjectNull() {
			continue
		}
		if v := sp.LatestBlock(); v > h.head.Load() {
			h.head.Store(v)
		}
		if reg, ok := sp.(interface{ OnLatestBlock(func(int64)) }); ok {
			reg.OnLatestBlock(h.advance)
		}
	}
	m.hubs[key] = h
	return h
}

// blockStreamHub fans a network's head advances out to its live subscribers.
type blockStreamHub struct {
	head atomic.Int64

	mu   sync.Mutex
	subs map[*blockStreamSub]struct{}
}

// blockStreamSub is one subscriber's edge-triggered wake-up. The channel is
// buffered(1) and sends are non-blocking, so a burst of advances coalesces into a
// single pending signal; the subscriber then reads the current head and catches up.
type blockStreamSub struct {
	ch chan struct{}
}

// advance records a new head and wakes subscribers. It runs synchronously inside
// the poller's shared-variable update path, so it only does a monotonic CAS and
// non-blocking sends — never any blocking work.
func (h *blockStreamHub) advance(v int64) {
	for {
		cur := h.head.Load()
		if v <= cur {
			return
		}
		if h.head.CompareAndSwap(cur, v) {
			break
		}
	}

	h.mu.Lock()
	for s := range h.subs {
		select {
		case s.ch <- struct{}{}:
		default:
		}
	}
	h.mu.Unlock()
}

func (h *blockStreamHub) subscribe() *blockStreamSub {
	s := &blockStreamSub{ch: make(chan struct{}, 1)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

func (h *blockStreamHub) unsubscribe(s *blockStreamSub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// ProcessBlockStream subscribes to the network's head and pushes one header per
// new block, in ascending order, until ctx is done. It backfills every number
// between advances so no block number is skipped (forward-only — a reorg that
// replaces an already-sent number is not corrected here; see blockStreamMaxBackfill
// and the ChainView follow-up).
func (rp *RequestProcessor) ProcessBlockStream(
	ctx context.Context,
	input *RequestInput,
	onBlock func(*evm.BlockHeader) error,
) error {
	project, err := rp.erpc.GetProject(input.ProjectId)
	if err != nil {
		return err
	}
	networkID := fmt.Sprintf("%s:%s", input.Architecture, input.ChainId)
	network, err := project.GetNetwork(ctx, networkID)
	if err != nil {
		return err
	}

	// Authenticate once up front so an unauthorized subscriber is rejected
	// immediately rather than holding a subscription until the next block; each
	// header fetch below re-checks through the normal request path.
	nq := common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{}),
	)
	nq.SetClientIP(input.ClientIP)
	if _, err := project.AuthenticateConsumer(ctx, nq, "eth_getBlockByNumber", input.AuthPayload); err != nil {
		return err
	}

	hub := rp.blockStream.hubFor(ctx, network)
	sub := hub.subscribe()
	defer hub.unsubscribe(sub)

	// Tip subscription: start from the current head and emit only strictly-new
	// blocks (never backfill history for a fresh subscriber).
	lastSent := hub.head.Load()
	for {
		select {
		case <-ctx.Done():
			// Client cancelled / disconnected — a clean end of stream, not an error.
			return nil
		case <-sub.ch:
			head := hub.head.Load()
			if lastSent == 0 || head-lastSent > blockStreamMaxBackfill {
				// Cold start or a gap too large to be a live tail — resync forward.
				lastSent = head
				continue
			}
			for n := lastSent + 1; n <= head; n++ {
				header, err := rp.fetchBlockHeader(ctx, input, n)
				if err != nil {
					return err
				}
				if header == nil {
					// Head advanced but this block isn't retrievable yet; leave
					// lastSent behind it and retry on the next advance.
					break
				}
				if err := onBlock(header); err != nil {
					return err
				}
				lastSent = n
			}
		}
	}
}

// fetchBlockHeader resolves one block's header through the normal cached request
// path (eth_getBlockByNumber, hashes-only). Concurrent identical fetches across
// subscribers are coalesced by erpc, so a shared head yields ~one upstream call
// per block. Returns (nil, nil) when the block is not yet available.
func (rp *RequestProcessor) fetchBlockHeader(
	ctx context.Context,
	input *RequestInput,
	blockNumber int64,
) (*evm.BlockHeader, error) {
	resp, err := rp.ProcessUnary(
		ctx,
		input,
		buildJSONRPCRequest("eth_getBlockByNumber", []interface{}{fmt.Sprintf("0x%x", blockNumber), false}),
	)
	if err != nil {
		return nil, err
	}
	result, err := parseJSONRPCResult(ctx, resp)
	if err != nil {
		return nil, err
	}
	if string(result) == "null" {
		return nil, nil
	}
	var block evm.JsonRpcBlock
	if err := sonic.Unmarshal(result, &block); err != nil {
		return nil, err
	}
	protoBlock, err := block.ToProto()
	if err != nil {
		return nil, err
	}
	return protoBlock.Header, nil
}
