package upstream

import (
	"sort"
	"strings"
)

// ReorderUpstreams is a test (or dev) utility function that enforces a preferred
// order of upstream IDs across all networks and methods in the given UpstreamsRegistry,
// including the wildcard network "*" and wildcard method "*". Any upstream IDs not
// listed in preferredOrder will appear at the end of the resulting slice in their
// original relative order.
func ReorderUpstreams(reg *UpstreamsRegistry, preferredOrder ...string) {
	reg.upstreamsMu.Lock()
	defer reg.upstreamsMu.Unlock()

	// If preferredOrder is empty, collect all upstream IDs from the registry in sorted order.
	if len(preferredOrder) == 0 {
		allUpstreamIDsMap := make(map[string]struct{})

		// Gather from sortedUpstreams
		for _, methodMap := range reg.sortedUpstreams {
			for _, upsSlice := range methodMap {
				for _, upstream := range upsSlice {
					allUpstreamIDsMap[upstream.Id()] = struct{}{}
				}
			}
		}

		// Gather from networkUpstreams
		for _, upsSlice := range reg.networkUpstreams {
			for _, upstream := range upsSlice {
				allUpstreamIDsMap[upstream.Id()] = struct{}{}
			}
		}

		// Convert to slice and sort
		allUpstreamIDs := make([]string, 0, len(allUpstreamIDsMap))
		for id := range allUpstreamIDsMap {
			allUpstreamIDs = append(allUpstreamIDs, id)
		}
		sort.Strings(allUpstreamIDs)
		preferredOrder = allUpstreamIDs
	}

	// 1) Reorder in registry.sortedUpstreams.
	for _, methodMap := range reg.sortedUpstreams {
		for method, upsSlice := range methodMap {
			newSlice := reorderSliceOfUpstreams(upsSlice, preferredOrder)
			methodMap[method] = newSlice
		}
	}

	// 2) Reorder in registry.networkUpstreams.
	for networkId, upsSlice := range reg.networkUpstreams {
		newSlice := reorderSliceOfUpstreams(upsSlice, preferredOrder)
		reg.networkUpstreams[networkId] = newSlice
		// Refresh atomic snapshot for fast-path reads
		cp := make([]*Upstream, len(newSlice))
		copy(cp, newSlice)
		reg.networkUpstreamsAtomic.Store(networkId, cp)
	}
}

// reorderSliceOfUpstreams returns a copy of upsSlice reordered so that upstream IDs
// appear in an order matching preferredOrder (if present). All upstreams not listed
// in preferredOrder preserve their original relative positions and are placed at the end.
func reorderSliceOfUpstreams(
	upsSlice []*Upstream,
	preferredOrder []string,
) []*Upstream {
	if len(upsSlice) < 2 || len(preferredOrder) == 0 {
		// No need to reorder
		// or no meaningful preference was provided
		return upsSlice
	}

	// Create a map of upstreamId -> position in preferredOrder
	preferredIndex := make(map[string]int, len(preferredOrder))
	for i, upId := range preferredOrder {
		preferredIndex[strings.TrimSpace(upId)] = i
	}

	// We'll create a stable sort:
	//  - upstreams that appear in the preferredIndex come first in ascending order by index
	//  - otherwise, they appear after in original relative order
	type upsPlusOrigIdx struct {
		Up      *Upstream
		OrigIdx int
		Pos     int  // derived from preferredIndex; -1 if not in preferredOrder
		InOrder bool // indicates if the upstream was found in preferredIndex
	}

	withMeta := make([]upsPlusOrigIdx, len(upsSlice))
	for i, ups := range upsSlice {
		id := ups.Id()
		pos, ok := preferredIndex[id]
		if !ok {
			withMeta[i] = upsPlusOrigIdx{Up: ups, OrigIdx: i, Pos: -1, InOrder: false}
		} else {
			withMeta[i] = upsPlusOrigIdx{Up: ups, OrigIdx: i, Pos: pos, InOrder: true}
		}
	}

	sort.SliceStable(withMeta, func(i, j int) bool {
		// Both are in the preferredOrder list
		if withMeta[i].InOrder && withMeta[j].InOrder {
			return withMeta[i].Pos < withMeta[j].Pos
		}
		// One is in the preferred list, the other is not
		if withMeta[i].InOrder && !withMeta[j].InOrder {
			return true
		}
		if !withMeta[i].InOrder && withMeta[j].InOrder {
			return false
		}
		// Neither is in the preferred list, preserve original order
		return withMeta[i].OrigIdx < withMeta[j].OrigIdx
	})

	newSlice := make([]*Upstream, len(upsSlice))
	for i := range withMeta {
		newSlice[i] = withMeta[i].Up
	}
	return newSlice
}
