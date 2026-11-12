package policy

import (
	"github.com/erpc/erpc/common"
)

// methodBlockIndex is one tick's `blockMethods` result, indexed for the
// request path: method -> blocked upstream IDs, plus the blocked upstreams
// the prober may mirror that method to.
type methodBlockIndex struct {
	blocked map[string]map[string]struct{}
	probe   map[string][]common.Upstream
}

func newMethodBlockIndex(ups []common.Upstream, blocks map[string]map[string]MethodBlock) *methodBlockIndex {
	if len(blocks) == 0 {
		return nil
	}
	byID := make(map[string]common.Upstream, len(ups))
	for _, u := range ups {
		byID[u.Id()] = u
	}
	idx := &methodBlockIndex{
		blocked: make(map[string]map[string]struct{}),
		probe:   make(map[string][]common.Upstream),
	}
	for id, perMethod := range blocks {
		u, ok := byID[id]
		if !ok {
			continue
		}
		for method, b := range perMethod {
			set := idx.blocked[method]
			if set == nil {
				set = make(map[string]struct{})
				idx.blocked[method] = set
			}
			set[id] = struct{}{}
			if b.Probe {
				idx.probe[method] = append(idx.probe[method], u)
			}
		}
	}
	return idx
}

// filter drops the upstreams blocked for `method`. The input is never
// mutated and is returned as-is when nothing is blocked. When the blocks
// would leave no upstream, the input is returned unchanged: a method keeps
// being served by every upstream rather than by none.
func (idx *methodBlockIndex) filter(ups []common.Upstream, method string) []common.Upstream {
	if idx == nil {
		return ups
	}
	set := idx.blocked[method]
	if len(set) == 0 {
		return ups
	}
	kept := 0
	for _, u := range ups {
		if _, ok := set[u.Id()]; !ok {
			kept++
		}
	}
	if kept == len(ups) || kept == 0 {
		return ups
	}
	out := make([]common.Upstream, 0, kept)
	for _, u := range ups {
		if _, ok := set[u.Id()]; !ok {
			out = append(out, u)
		}
	}
	return out
}

// probeCandidates returns the upstreams blocked for `method` that the
// prober may mirror it to.
func (idx *methodBlockIndex) probeCandidates(method string) []common.Upstream {
	if idx == nil {
		return nil
	}
	return idx.probe[method]
}
