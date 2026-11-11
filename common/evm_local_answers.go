package common

import (
	"context"
	"fmt"
	"iter"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// A local answer is eRPC's own reply to a method whose true answer does not
// depend on which upstream serves it, or is about eRPC itself, the node the
// client talks to. Routing such a method makes one upstream a single point of
// failure for a constant (upstreams often reject net_* methods) or returns one
// node's view where eRPC fronts several. ok=false leaves the request to the
// upstreams.
type localAnswer func(ctx context.Context, n Network) (result any, ok bool)

var localAnswers = map[string]localAnswer{
	"eth_chainId": func(_ context.Context, n Network) (any, bool) {
		id := evmChainId(n)
		if id <= 0 {
			return nil, false
		}
		return fmt.Sprintf("0x%x", id), true
	},
	"net_version": func(_ context.Context, n Network) (any, bool) {
		id := evmChainId(n)
		if id <= 0 {
			return nil, false
		}
		return strconv.FormatInt(id, 10), true
	},
	// eRPC is listening while it answers.
	"net_listening": func(context.Context, Network) (any, bool) { return true, true },
	// eRPC's peers are the upstreams it can route this network to now.
	"net_peerCount": func(ctx context.Context, n Network) (any, bool) {
		ups, ok := evmRoutableUpstreams(ctx, n)
		if !ok {
			return nil, false
		}
		count := 0
		for range ups {
			count++
		}
		return fmt.Sprintf("0x%x", count), true
	},
	"web3_clientVersion": func(context.Context, Network) (any, bool) {
		return fmt.Sprintf("erpc/%s/%s-%s/%s", ErpcVersion, runtime.GOOS, runtime.GOARCH, runtime.Version()), true
	},
	// eRPC serves from synced upstreams, so it is not syncing while one of its
	// routable upstreams is synced. Otherwise the upstreams answer.
	"eth_syncing": func(ctx context.Context, n Network) (any, bool) {
		ups, ok := evmRoutableUpstreams(ctx, n)
		if !ok {
			return nil, false
		}
		for u := range ups {
			if eu, ok := u.(EvmUpstream); ok && eu.EvmSyncingState() == EvmSyncingStateNotSyncing {
				return false, true
			}
		}
		return nil, false
	},
}

// LocalAnswerMethods lists the methods eRPC answers itself unless a network's
// evm.localAnswers sets them false.
func LocalAnswerMethods() []string {
	names := make([]string, 0, len(localAnswers))
	for name := range localAnswers {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// LocalAnswer returns eRPC's own answer to method on n. ok=false when the
// method has no local answer, the network turned it off, or eRPC cannot tell
// now; the request then goes to the upstreams.
func LocalAnswer(ctx context.Context, n Network, method string) (result any, ok bool) {
	name, answer := lookupLocalAnswer(method)
	if answer == nil {
		return nil, false
	}
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil {
		if on, set := cfg.Evm.LocalAnswers[name]; set && !on {
			return nil, false
		}
	}
	return answer(ctx, n)
}

func lookupLocalAnswer(method string) (string, localAnswer) {
	if answer, ok := localAnswers[method]; ok {
		return method, answer
	}
	for name, answer := range localAnswers {
		if strings.EqualFold(name, method) {
			return name, answer
		}
	}
	return "", nil
}

func evmChainId(n Network) int64 {
	if cfg := n.Config(); cfg != nil && cfg.Evm != nil {
		return cfg.Evm.ChainId
	}
	return 0
}

// EvmRoutableUpstreamsProvider is the optional surface a network exposes its
// routable upstreams through: the selection policy's eligible set in routing
// order, narrowed to the request's use-upstream selector. Asserted narrowly,
// like EvmStateProvenWriter, so EvmNetwork implementations need not change.
type EvmRoutableUpstreamsProvider interface {
	EvmRoutableUpstreams(ctx context.Context) iter.Seq[Upstream]
}

func evmRoutableUpstreams(ctx context.Context, n Network) (iter.Seq[Upstream], bool) {
	p, ok := n.(EvmRoutableUpstreamsProvider)
	if !ok {
		return nil, false
	}
	return p.EvmRoutableUpstreams(ctx), true
}
