package evm

import (
	"strings"

	"github.com/erpc/erpc/data"
)

// Immutable GET-policy lookup index.
// Policies are grouped by exact/wildcard network+method buckets, then merged
// back in original declaration order to preserve selection semantics.
type cachePolicySnapshot struct {
	policies  []*data.CachePolicy
	getLookup *cacheGetPolicyLookup
}

func newCachePolicySnapshot(policies []*data.CachePolicy) *cachePolicySnapshot {
	copied := append([]*data.CachePolicy(nil), policies...)
	return &cachePolicySnapshot{
		policies:  copied,
		getLookup: newCacheGetPolicyLookup(copied),
	}
}

type indexedCachePolicy struct {
	order  int
	policy *data.CachePolicy
}

type cacheGetPolicyLookup struct {
	byExactNetworkAndMethod map[string]map[string][]indexedCachePolicy
	byExactNetwork          map[string][]indexedCachePolicy
	byExactMethod           map[string][]indexedCachePolicy
	wildcard                []indexedCachePolicy
}

func newCacheGetPolicyLookup(policies []*data.CachePolicy) *cacheGetPolicyLookup {
	lookup := &cacheGetPolicyLookup{
		byExactNetworkAndMethod: make(map[string]map[string][]indexedCachePolicy),
		byExactNetwork:          make(map[string][]indexedCachePolicy),
		byExactMethod:           make(map[string][]indexedCachePolicy),
	}

	for i, policy := range policies {
		if !policy.AppliesToGet() {
			continue
		}

		entry := indexedCachePolicy{
			order:  i,
			policy: policy,
		}

		networkPattern := policy.NetworkPattern()
		methodPattern := policy.MethodPattern()
		networkExact := isExactCachePolicyPattern(networkPattern)
		methodExact := isExactCachePolicyPattern(methodPattern)

		switch {
		case networkExact && methodExact:
			networkBucket, ok := lookup.byExactNetworkAndMethod[networkPattern]
			if !ok {
				networkBucket = make(map[string][]indexedCachePolicy)
				lookup.byExactNetworkAndMethod[networkPattern] = networkBucket
			}
			networkBucket[methodPattern] = append(networkBucket[methodPattern], entry)
		case networkExact:
			lookup.byExactNetwork[networkPattern] = append(lookup.byExactNetwork[networkPattern], entry)
		case methodExact:
			lookup.byExactMethod[methodPattern] = append(lookup.byExactMethod[methodPattern], entry)
		default:
			lookup.wildcard = append(lookup.wildcard, entry)
		}
	}

	return lookup
}

func (l *cacheGetPolicyLookup) candidatesFor(networkId, method string) []indexedCachePolicy {
	if l == nil {
		return nil
	}

	var exactNetworkAndMethod []indexedCachePolicy
	if methodBuckets, ok := l.byExactNetworkAndMethod[networkId]; ok {
		exactNetworkAndMethod = methodBuckets[method]
	}

	return mergeOrderedIndexedPolicies(
		exactNetworkAndMethod,
		l.byExactNetwork[networkId],
		l.byExactMethod[method],
		l.wildcard,
	)
}

func mergeOrderedIndexedPolicies(
	list0 []indexedCachePolicy,
	list1 []indexedCachePolicy,
	list2 []indexedCachePolicy,
	list3 []indexedCachePolicy,
) []indexedCachePolicy {
	total := len(list0) + len(list1) + len(list2) + len(list3)
	if total == 0 {
		return nil
	}

	merged := make([]indexedCachePolicy, 0, total)
	i0 := 0
	i1 := 0
	i2 := 0
	i3 := 0

	for len(merged) < total {
		pick := -1
		pickOrder := 0

		if i0 < len(list0) {
			pick = 0
			pickOrder = list0[i0].order
		}
		if i1 < len(list1) && (pick == -1 || list1[i1].order < pickOrder) {
			pick = 1
			pickOrder = list1[i1].order
		}
		if i2 < len(list2) && (pick == -1 || list2[i2].order < pickOrder) {
			pick = 2
			pickOrder = list2[i2].order
		}
		if i3 < len(list3) && (pick == -1 || list3[i3].order < pickOrder) {
			pick = 3
		}

		switch pick {
		case 0:
			merged = append(merged, list0[i0])
			i0++
		case 1:
			merged = append(merged, list1[i1])
			i1++
		case 2:
			merged = append(merged, list2[i2])
			i2++
		case 3:
			merged = append(merged, list3[i3])
			i3++
		default:
			return merged
		}
	}

	return merged
}

func isExactCachePolicyPattern(pattern string) bool {
	if pattern == "" {
		return false
	}
	return !strings.ContainsAny(pattern, "*?|&!()<>=") && !strings.ContainsAny(pattern, " \t")
}
