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

// anyPolicyQuotasCovered reports whether the collected results already cover
// the tag quotas of at least one configured grade — i.e. some grade could
// still be satisfied by what has arrived. Used by the wait-cap arming path,
// which passes the RAW pre-dedup slice, so distinctness is enforced inside
// resultsSatisfyQuotas rather than assumed.
func anyPolicyQuotasCovered(results []*execResult, policies []*acceptancePolicy) bool {
	for _, p := range policies {
		if p != nil && resultsSatisfyQuotas(results, p.quotas) {
			return true
		}
	}
	return false
}

// defaultAcceptancePolicyName is the grade reported when composition is
// configured through the single-grade shorthand
// (`requiredParticipants[].minAgreement`) rather than a named list.
const defaultAcceptancePolicyName = "standard"

// acceptancePolicy is the normalized runtime form of one acceptance grade.
// Both config shapes — the ordered `acceptancePolicies` list and the
// single-grade `requiredParticipants[].minAgreement` shorthand — are compiled
// into this, so the executor has exactly one code path.
type acceptancePolicy struct {
	name string
	// threshold is the number of DISTINCT agreeing upstreams this grade
	// requires, already resolved (explicit > sum of quotas > policy-level).
	threshold int
	quotas    []*common.ConsensusAgreementQuota
}

// compile derives the ordered acceptance grades from whichever config shape
// the operator used, and lowers the rules-engine threshold to the least
// demanding grade.
//
// Keeping this on config (rather than in the builder) means derived state is
// produced in exactly one place: any code path holding a config gets the same
// grades, and the compiled list can never drift from the requiredParticipants
// it was derived from. Idempotent.
//
// On the threshold: the rules engine only NOMINATES a candidate winner; each
// grade's own threshold is enforced by the acceptance gate. Running the rules
// at the lowest bar is what makes a relaxed grade reachable on a round too
// thin for the strict grade to ever win — the case where internal nodes are
// absent and only a couple of externals answered — without ever serving a
// result below the bar of the grade it is served under.
func (c *config) compile() {
	if c == nil || c.acceptancePolicies != nil {
		return
	}
	c.acceptancePolicies = buildAcceptancePolicies(c.acceptancePolicyConfigs, c.requiredParticipants, c.agreementThreshold)
	if lowest := lowestAcceptanceThreshold(c.acceptancePolicies); lowest > 0 && lowest < c.agreementThreshold {
		c.agreementThreshold = lowest
	}
}

// buildAcceptancePolicies compiles the configured grades into evaluation
// order. Returns nil when no composition requirement is configured at all,
// which leaves the gate a no-op — the feature stays opt-in.
func buildAcceptancePolicies(
	policies []*common.ConsensusAcceptancePolicy,
	required []*common.ConsensusRequiredParticipant,
	agreementThreshold int,
) []*acceptancePolicy {
	if len(policies) > 0 {
		out := make([]*acceptancePolicy, 0, len(policies))
		for _, p := range policies {
			if p == nil {
				continue
			}
			out = append(out, &acceptancePolicy{
				name:      p.Name,
				threshold: p.EffectiveAgreementThreshold(agreementThreshold),
				quotas:    p.RequiredAgreement,
			})
		}
		return out
	}
	if !anyAgreementQuota(required) {
		return nil
	}
	// Shorthand: one implicit grade carrying the minAgreement quotas, gated
	// by the policy-level threshold exactly as before named grades existed.
	quotas := make([]*common.ConsensusAgreementQuota, 0, len(required))
	for _, rp := range required {
		if rp == nil || rp.MinAgreement <= 0 {
			continue
		}
		quotas = append(quotas, &common.ConsensusAgreementQuota{Tag: rp.Tag, MinAgreement: rp.MinAgreement})
	}
	return []*acceptancePolicy{{
		name:      defaultAcceptancePolicyName,
		threshold: agreementThreshold,
		quotas:    quotas,
	}}
}

// lowestAcceptanceThreshold is the smallest agreement count any configured
// grade would accept. Zero when no grade is configured, leaving the caller's
// policy-level threshold untouched.
func lowestAcceptanceThreshold(policies []*acceptancePolicy) int {
	lowest := 0
	for _, p := range policies {
		if p == nil || p.threshold <= 0 {
			continue
		}
		if lowest == 0 || p.threshold < lowest {
			lowest = p.threshold
		}
	}
	return lowest
}

// distinctUpstreams counts unique upstream IDs in a result set. The agreeing
// set can contain the same upstream twice (hedge/retry duplicates survive on
// the raw pre-dedup path), and a node corroborating itself must not count as
// two votes toward any threshold.
func distinctUpstreams(results []*execResult) int {
	var seen map[string]struct{}
	n := 0
	for _, r := range results {
		if r == nil || r.Upstream == nil {
			continue
		}
		id := r.Upstream.Id()
		if seen == nil {
			seen = make(map[string]struct{}, len(results))
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		n++
	}
	return n
}

// satisfiedBy reports whether an agreeing set meets this grade: enough
// distinct upstreams overall, and enough distinct upstreams per tag quota.
func (p *acceptancePolicy) satisfiedBy(agreeing []*execResult) bool {
	if p == nil {
		return false
	}
	if p.threshold > 0 && distinctUpstreams(agreeing) < p.threshold {
		return false
	}
	return resultsSatisfyQuotas(agreeing, p.quotas)
}

// resultsSatisfyQuotas reports whether the result set contains at least
// MinAgreement DISTINCT upstreams matching each quota's tag.
func resultsSatisfyQuotas(results []*execResult, quotas []*common.ConsensusAgreementQuota) bool {
	for _, q := range quotas {
		if q == nil || q.MinAgreement <= 0 {
			continue
		}
		matched := 0
		var seen map[string]struct{}
		for _, r := range results {
			if r == nil || r.Upstream == nil || !upstreamMatchesTag(r.Upstream, q.Tag) {
				continue
			}
			id := r.Upstream.Id()
			if seen == nil {
				seen = make(map[string]struct{}, q.MinAgreement)
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			matched++
			if matched >= q.MinAgreement {
				break
			}
		}
		if matched < q.MinAgreement {
			return false
		}
	}
	return true
}

// anyQuotaTag reports whether any configured grade constrains composition at
// all. A pure count-only cascade needs no tag-coverage handling in the
// wait-cap path.
func anyQuotaTag(policies []*acceptancePolicy) bool {
	for _, p := range policies {
		if p != nil && len(p.quotas) > 0 {
			return true
		}
	}
	return false
}

// isStrictestPolicy reports whether name is the first configured grade —
// the one no other grade is preferred over. Only a win at this grade is
// final while responses are still outstanding.
func (e *executor) isStrictestPolicy(name string) bool {
	policies := e.config.acceptancePolicies
	return len(policies) == 0 || policies[0].name == name
}
