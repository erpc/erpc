package consensus

import "github.com/erpc/erpc/common"

// reorderForParticipantQuota returns a reordering of `ups` that front-loads
// enough tag-matching upstreams to satisfy each `requiredParticipants`
// entry, so that when the executor draws its first `maxParticipants`
// participants they include at least `minParticipants` from each required
// tag group.
//
// Semantics:
//   - Best-effort: if a required group has fewer matching upstreams than
//     requested (or several quotas can't all fit within maxParticipants),
//     it promotes everything it can and leaves the shortfall to the
//     existing lowParticipantsBehavior / agreementThreshold handling —
//     consensus is not aware this happened, it just sees fewer/uneven
//     participants like any organic low-participation tick.
//   - Minimal disturbance: non-required upstreams keep their incoming
//     (selection-policy) order in the remaining slots, so ranking/quality
//     is preserved wherever the quota doesn't force a change. Order WITHIN
//     the participant set doesn't affect voting — only set membership does.
//   - A single upstream can satisfy multiple entries it matches (we never
//     double-promote the same upstream).
//
// Returns the input slice unchanged when there are no upstreams or no
// requirements (the feature is opt-in and off by default).
func reorderForParticipantQuota(ups []common.Upstream, reqs []*common.ConsensusRequiredParticipant) []common.Upstream {
	if len(ups) == 0 || len(reqs) == 0 {
		return ups
	}

	promoted := make([]common.Upstream, 0, len(ups))
	promotedIDs := make(map[string]struct{}, len(ups))

	for _, r := range reqs {
		if r == nil || r.MinParticipants <= 0 || r.Tag == "" {
			continue
		}
		// Count matches already promoted by an earlier requirement — an
		// upstream that matches several tags counts toward each of them.
		have := 0
		for _, u := range promoted {
			if upstreamMatchesTag(u, r.Tag) {
				have++
			}
		}
		// Promote more matching upstreams, in incoming (quality) order,
		// until the minimum is met or we run out of candidates.
		for _, u := range ups {
			if have >= r.MinParticipants {
				break
			}
			if _, ok := promotedIDs[u.Id()]; ok {
				continue
			}
			if upstreamMatchesTag(u, r.Tag) {
				promoted = append(promoted, u)
				promotedIDs[u.Id()] = struct{}{}
				have++
			}
		}
	}

	if len(promoted) == 0 {
		return ups
	}

	// promoted (quota-required, in priority/quality order) first, then the
	// rest in their original order.
	out := make([]common.Upstream, 0, len(ups))
	out = append(out, promoted...)
	for _, u := range ups {
		if _, ok := promotedIDs[u.Id()]; ok {
			continue
		}
		out = append(out, u)
	}
	return out
}

// upstreamMatchesTag reports whether any of the upstream's tags matches the
// given glob pattern (`*`, `?`). Falls back to exact equality first so a
// plain tag like "tier:paid" matches without invoking the glob engine.
func upstreamMatchesTag(u common.Upstream, pattern string) bool {
	if u == nil {
		return false
	}
	cfg := u.Config()
	if cfg == nil {
		return false
	}
	for _, t := range cfg.Tags {
		if t == pattern {
			return true
		}
		if m, err := common.WildcardMatch(pattern, t); err == nil && m {
			return true
		}
	}
	return false
}

// anyAgreementQuota reports whether any requiredParticipants entry carries a
// winner-composition quota (minAgreement > 0). When false the composition
// gate is a no-op — the feature is opt-in and off by default.
func anyAgreementQuota(reqs []*common.ConsensusRequiredParticipant) bool {
	for _, r := range reqs {
		if r != nil && r.MinAgreement > 0 {
			return true
		}
	}
	return false
}

// resultsSatisfyAgreementQuotas reports whether the result set satisfies
// every minAgreement entry: for each entry, at least minAgreement DISTINCT
// upstreams must match the entry's tag. Distinctness is enforced here (not
// assumed from dedupeByUpstream) because the wait-cap arming path passes the
// raw pre-dedup response slice — a single tagged upstream answering twice
// via hedge/retry must not satisfy a minAgreement of 2 by itself. Takes a
// result slice (not a group) because the agreeing set can span multiple
// hash groups — preferHighestValueFor counts agreement by numeric value,
// and the same value with different encodings lands in different groups.
func resultsSatisfyAgreementQuotas(results []*execResult, reqs []*common.ConsensusRequiredParticipant) bool {
	for _, req := range reqs {
		if req == nil || req.MinAgreement <= 0 {
			continue
		}
		matched := 0
		var seen map[string]struct{}
		for _, r := range results {
			if r == nil || r.Upstream == nil || !upstreamMatchesTag(r.Upstream, req.Tag) {
				continue
			}
			id := r.Upstream.Id()
			if seen == nil {
				seen = make(map[string]struct{}, req.MinAgreement)
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			matched++
			if matched >= req.MinAgreement {
				break
			}
		}
		if matched < req.MinAgreement {
			return false
		}
	}
	return true
}
