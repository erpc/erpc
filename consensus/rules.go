package consensus

import (
	"math/big"

	"github.com/erpc/erpc/common"

	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
)

// consensusRule represents a single consensus decision rule
type consensusRule struct {
	Description string
	Condition   func(a *consensusAnalysis) bool
	Action      func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse]
}

type shortCircuitRule struct {
	Description string
	Reason      string
	Condition   func(winner *failsafeCommon.PolicyResult[*common.NormalizedResponse], a *consensusAnalysis) bool
}

// consensusRules defines all consensus rules in priority order
// Rules are evaluated from most specific/nuanced to most generic, and ideally those that error must come before those that return a result.
var consensusRules = []consensusRule{
	// eth_sendRawTransaction: return first valid tx hash immediately
	// For transaction broadcasting, we don't need consensus - any single valid response is sufficient
	// because the transaction will propagate through the network.
	{
		Description: "eth_sendRawTransaction: return first valid tx hash response",
		Condition: func(a *consensusAnalysis) bool {
			if a.method != "eth_sendRawTransaction" {
				return false
			}
			// Check if we have any non-empty response (tx hash)
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty && g.Count >= 1 {
					return true
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Return the first valid non-empty response
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty && g.LargestResult != nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Result: g.LargestResult,
					}
				}
			}
			// Shouldn't reach here since condition already verified, but fallback to dispute
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute(
					"no valid tx hash response found",
					a.participants(),
					nil,
				),
			}
		},
	},
	// PreferHighestValueFor: when configured for this method, return the response with highest field values
	// that meets the agreementThreshold. This rule takes precedence over all other consensus rules.
	{
		Description: "prefer-highest-value-for: return highest value where at least agreementThreshold upstreams agree",
		Condition: func(a *consensusAnalysis) bool {
			// Check if this method has preferHighestValueFor configured
			if a.config.preferHighestValueFor == nil || a.method == "" {
				return false
			}
			fields, ok := a.config.preferHighestValueFor[a.method]
			if !ok || len(fields) == 0 {
				return false
			}
			// Only match if at least one valid group has extractable values
			for _, group := range a.getValidGroups() {
				if group.LargestResult != nil {
					if extractFieldValues(group.LargestResult, fields) != nil {
						return true
					}
				}
			}
			return false // No extractable values, fall through to other rules
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			fields := a.config.preferHighestValueFor[a.method]
			threshold := a.config.agreementThreshold
			if threshold < 1 {
				threshold = 1
			}

			// Group responses by their numeric value (not by hash).
			// This handles cases where same value has different JSON formatting.
			type valueGroup struct {
				values       []*big.Int
				count        int
				response     *common.NormalizedResponse
				responseSize int
			}
			valueGroups := make(map[string]*valueGroup) // key is string representation of values

			for _, group := range a.getValidGroups() {
				for _, result := range group.Results {
					if result == nil || result.Result == nil || result.Err != nil {
						continue
					}
					values := extractFieldValues(result.Result, fields)
					if values == nil {
						continue
					}
					// Create a key from the values for grouping
					key := valuesToKey(values)
					if vg, exists := valueGroups[key]; exists {
						vg.count++
						// Keep the largest response in case of ties
						if result.CachedResponseSize > vg.responseSize {
							vg.response = result.Result
							vg.responseSize = result.CachedResponseSize
						}
					} else {
						valueGroups[key] = &valueGroup{
							values:       values,
							count:        1,
							response:     result.Result,
							responseSize: result.CachedResponseSize,
						}
					}
				}
			}

			// Find the highest value that meets the threshold
			var best *valueGroup
			for _, vg := range valueGroups {
				if vg.count < threshold {
					continue // Doesn't meet minimum agreement
				}
				if best == nil || compareValueChains(vg.values, best.values) > 0 {
					best = vg
				}
			}

			if best != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Result: best.response,
				}
			}

			// No value met the threshold - return dispute error
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute(
					"no value met agreement threshold for highest-value comparison",
					a.participants(),
					nil,
				),
			}
		},
	},
	// BlockHeadLeader: OnlyBlockHeadLeader behavior for disputes
	{
		Description: "only-block-head-leader on dispute: prefer leader; non-error if available else leader error",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.disputeBehavior != common.ConsensusDisputeBehaviorOnlyBlockHeadLeader {
				return false
			}
			// Trigger only when we do not already have a valid group meeting threshold (i.e., dispute path)
			var bestValid *responseGroup
			for _, g := range a.getValidGroups() {
				if bestValid == nil || g.Count > bestValid.Count {
					bestValid = g
				}
			}
			if bestValid != nil && bestValid.Count >= a.config.agreementThreshold {
				return false
			}
			// Always handle in dispute mode; action decides whether to return leader result or leader error
			return true
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			if g := a.getLeaderGroupNonError(); g != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: g.LargestResult}
			}
			// If leader exists but only has an error, return that error (prefer leader strictly),
			// including infrastructure errors.
			if err := a.getLeaderFirstErrorIncludingInfra(); err != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: err}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// BlockHeadLeader: OnlyBlockHeadLeader behavior for low participants
	{
		Description: "only-block-head-leader on low participants: select leader's non-error result if available",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.lowParticipantsBehavior != common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader {
				return false
			}
			if !a.isLowParticipants(a.config.agreementThreshold) {
				return false
			}
			// Trigger for any low participants case under OnlyBlockHeadLeader; action will decide outcome
			return true
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			if g := a.getLeaderGroupNonError(); g != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: g.LargestResult}
			}
			// If leader only has an error, return that error; otherwise low participants
			if gAny := a.getLeaderGroupAny(); gAny != nil && gAny.FirstError != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: gAny.FirstError}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("not enough participants", a.participants(), nil),
			}
		},
	},
	// BlockHeadLeader: PreferBlockHeadLeader behavior for dispute/low participants – prefer leader group if exists, else fallback
	{
		Description: "prefer-block-head-leader: prefer leader's non-error group when no threshold winner",
		Condition: func(a *consensusAnalysis) bool {
			preferDispute := a.config.disputeBehavior == common.ConsensusDisputeBehaviorPreferBlockHeadLeader
			preferLow := a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader && a.isLowParticipants(a.config.agreementThreshold)
			if !(preferDispute || preferLow) {
				return false
			}
			// Apply only when there is no valid group meeting threshold
			var bestValid *responseGroup
			for _, g := range a.getValidGroups() {
				if bestValid == nil || g.Count > bestValid.Count {
					bestValid = g
				}
			}
			if bestValid != nil && bestValid.Count >= a.config.agreementThreshold {
				return false
			}
			// Prefer leader group if available; otherwise let subsequent rules (accept-most-common) handle
			return a.getLeaderGroupNonError() != nil
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			if g := a.getLeaderGroupNonError(); g != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: g.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// PreferLargerResponses + AcceptMostCommon: when no group meets threshold, choose the largest non-empty
	{
		Description: "prefer-larger + accept-most-common: choose largest non-empty below threshold",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferLargerResponses {
				return false
			}
			// Apply only when AcceptMostCommon is active in the current context
			isDisputeAcceptMost := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
			isLowParticipantsAcceptMost := a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult && a.isLowParticipants(a.config.agreementThreshold)
			if !(isDisputeAcceptMost || isLowParticipantsAcceptMost) {
				return false
			}
			best := a.getBestByCount()
			return best == nil || best.Count < a.config.agreementThreshold
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			largest := a.getBestBySize()
			if largest != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: largest.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// AcceptMostCommon + PreferNonEmpty: even if empty or consensus error meets threshold, choose best non-empty
	{
		Description: "accept-most-common + prefer-non-empty: choose non-empty even if empty or error meets threshold",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferNonEmpty {
				return false
			}
			// Active only when AcceptMostCommon is in effect for the current context
			isDisputeAcceptMost := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
			isLowParticipantsAcceptMost := a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult && a.isLowParticipants(a.config.agreementThreshold)
			if !(isDisputeAcceptMost || isLowParticipantsAcceptMost) {
				return false
			}
			best := a.getBestByCount()
			if best == nil {
				return false
			}
			// Apply when the leading group meets threshold and is empty OR consensus error, while any non-empty exists
			if best.Count >= a.config.agreementThreshold && (best.ResponseType == ResponseTypeEmpty || best.ResponseType == ResponseTypeConsensusError) {
				for _, g := range a.groups {
					if g.ResponseType == ResponseTypeNonEmpty {
						return true
					}
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			var bestNonEmpty *responseGroup
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty && (bestNonEmpty == nil || g.Count > bestNonEmpty.Count || (g.Count == bestNonEmpty.Count && g.ResponseSize > bestNonEmpty.ResponseSize)) {
					bestNonEmpty = g
				}
			}
			if bestNonEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// When multiple groups meet threshold but there is a tie and no explicit preference, dispute
	// This must come before the generic threshold winner rule. We consider ties only among non-empty
	// and empty groups; if any consensus-valid error group meets threshold, let the generic rule handle it.
	{
		Description: "dispute when there is a tie at or above threshold without preference",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.preferNonEmpty { // preference exists; let other rules handle
				return false
			}
			if a.config.preferLargerResponses {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.Count < a.config.agreementThreshold {
				return false
			}
			// If the best is an agreed error, allow generic threshold rule to return it
			if best.ResponseType == ResponseTypeConsensusError {
				return false
			}
			// Count how many valid groups share the best count
			countWithBest := 0
			for _, g := range a.getValidGroups() {
				if g.ResponseType == ResponseTypeConsensusError {
					continue
				}
				if g.Count == best.Count {
					countWithBest++
					if countWithBest > 1 {
						return true
					}
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// AcceptMostCommon: when below threshold, prefer non-empty over empty
	{
		Description: "accept-most-common below threshold prefers non-empty over empty",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferNonEmpty {
				return false
			}
			// Apply AcceptMostCommon only in the correct context:
			// - Dispute context when DisputeBehavior = AcceptMostCommon
			// - Low participants context when LowParticipantsBehavior = AcceptMostCommon
			isDisputeAcceptMost := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
			isLowParticipantsAcceptMost := a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult && a.isLowParticipants(a.config.agreementThreshold)
			if !(isDisputeAcceptMost || isLowParticipantsAcceptMost) {
				return false
			}
			// Apply only when below threshold, there exists at least one non-empty group and at least one empty group
			best := a.getBestByCount()
			if best == nil || best.Count >= a.config.agreementThreshold {
				return false
			}
			nonEmptyGroups := 0
			hasEmpty := false
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty {
					nonEmptyGroups++
				}
				if g.ResponseType == ResponseTypeEmpty {
					hasEmpty = true
				}
			}
			return hasEmpty && nonEmptyGroups == 1
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			var bestNonEmpty *responseGroup
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty && (bestNonEmpty == nil || g.Count > bestNonEmpty.Count || (g.Count == bestNonEmpty.Count && g.ResponseSize > bestNonEmpty.ResponseSize)) {
					bestNonEmpty = g
				}
			}
			if bestNonEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// AcceptMostCommon: when below threshold, prefer non-empty over consensus-valid error
	{
		Description: "accept-most-common below threshold prefers non-empty over consensus error",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferNonEmpty {
				return false
			}
			isDisputeAcceptMost := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
			isLowParticipantsAcceptMost := a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult && a.isLowParticipants(a.config.agreementThreshold)
			if !(isDisputeAcceptMost || isLowParticipantsAcceptMost) {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.Count >= a.config.agreementThreshold {
				return false
			}
			if best.ResponseType != ResponseTypeConsensusError {
				return false
			}
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty {
					return true
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			var bestNonEmpty *responseGroup
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty && (bestNonEmpty == nil || g.Count > bestNonEmpty.Count || (g.Count == bestNonEmpty.Count && g.ResponseSize > bestNonEmpty.ResponseSize)) {
					bestNonEmpty = g
				}
			}
			if bestNonEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// ReturnError: if empty meets threshold but any non-empty exists, dispute
	{
		Description: "return error when empty would win but non-empty exists (above threshold)",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.disputeBehavior != common.ConsensusDisputeBehaviorReturnError {
				return false
			}
			// Trigger this rule only when prefer-non-empty is enabled; otherwise, allow the generic
			// threshold-winner rule to select the empty result.
			if !a.config.preferNonEmpty {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.ResponseType != ResponseTypeEmpty || best.Count < a.config.agreementThreshold {
				return false
			}
			// any non-empty group present?
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty {
					return true
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// With ReturnError + preferNonEmpty, do not override the threshold winner; dispute instead
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil)}
		},
	},
	// AcceptMostCommon + PreferNonEmpty: when above threshold and both non-empty and consensus-error meet,
	// choose the best non-empty (by count then size). Applies only to non-empty (empty is not preferred here).
	{
		Description: "accept-most-common + prefer-non-empty: above threshold choose non-empty over consensus error",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferNonEmpty {
				return false
			}
			acceptMostCommon := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult ||
				a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
			if !acceptMostCommon {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.Count < a.config.agreementThreshold {
				return false
			}
			// Check presence of at least one non-empty group meeting threshold
			hasNonEmptyAbove := false
			hasConsensusErrAbove := false
			for _, g := range a.getValidGroups() {
				if g.Count >= a.config.agreementThreshold {
					switch g.ResponseType {
					case ResponseTypeNonEmpty:
						hasNonEmptyAbove = true
					case ResponseTypeConsensusError:
						hasConsensusErrAbove = true
					}
				}
			}
			return hasNonEmptyAbove && hasConsensusErrAbove
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			var bestNonEmpty *responseGroup
			for _, g := range a.getValidGroups() {
				if g.ResponseType == ResponseTypeNonEmpty && (bestNonEmpty == nil || g.Count > bestNonEmpty.Count || (g.Count == bestNonEmpty.Count && g.ResponseSize > bestNonEmpty.ResponseSize)) {
					bestNonEmpty = g
				}
			}
			if bestNonEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// Tie above threshold without preference -> dispute
	{
		Description: "dispute when there is a tie at or above threshold without preference",
		Condition: func(a *consensusAnalysis) bool {
			// If any preference is active (non-empty or larger), skip and let preference rules resolve
			if a.config.preferNonEmpty || a.config.preferLargerResponses {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.Count < a.config.agreementThreshold {
				return false
			}
			// Count how many groups share the best count
			countWithBest := 0
			for _, g := range a.getValidGroups() {
				if g.Count == best.Count {
					countWithBest++
					if countWithBest > 1 {
						return true
					}
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// PreferLargerResponses: any above-threshold scenario with multiple valid groups -> choose largest non-empty
	{
		Description: "prefer-larger: when above threshold and multiple valid groups, choose largest non-empty",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferLargerResponses {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.Count < a.config.agreementThreshold {
				return false
			}
			// Activate when there is more than one valid group above threshold (could have different counts)
			numValidAbove := 0
			for _, g := range a.getValidGroups() {
				if g.Count >= a.config.agreementThreshold {
					numValidAbove++
					if numValidAbove > 1 {
						return true
					}
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Choose the largest non-empty among all, regardless of count tie specifics
			largest := a.getBestBySize()
			if largest != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: largest.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// PreferLargerResponses: if a smaller result meets threshold but a larger non-empty exists, respect preference
	{
		Description: "prefer-larger + accept-most-common: choose largest when smaller meets threshold and larger exists",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferLargerResponses {
				return false
			}
			// Apply only for AcceptMostCommon in the current context
			isDisputeAcceptMost := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
			isLowParticipantsAcceptMost := a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult && a.isLowParticipants(a.config.agreementThreshold)
			if !(isDisputeAcceptMost || isLowParticipantsAcceptMost) {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.Count < a.config.agreementThreshold {
				return false
			}
			if best.ResponseType != ResponseTypeNonEmpty {
				return false
			}
			// Check if a larger non-empty exists
			for _, g := range a.getValidGroups() {
				if g.ResponseType == ResponseTypeNonEmpty && g.ResponseSize > best.ResponseSize {
					return true
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			largest := a.getBestBySize()
			if largest != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: largest.LargestResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil)}
		},
	},
	// ReturnError behavior: if a smaller non-empty meets threshold but a larger non-empty exists (below threshold), dispute.
	{
		Description: "return-error: smaller winner at threshold but larger non-empty exists with preference -> dispute",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.disputeBehavior != common.ConsensusDisputeBehaviorReturnError {
				return false
			}
			if !a.config.preferLargerResponses {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.Count < a.config.agreementThreshold || best.ResponseType != ResponseTypeNonEmpty {
				return false
			}
			// Trigger only when the larger non-empty exists but is below threshold (single or minority)
			for _, g := range a.getValidGroups() {
				if g.ResponseType == ResponseTypeNonEmpty && g.ResponseSize > best.ResponseSize && g.Count < a.config.agreementThreshold {
					return true
				}
			}
			return false
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil)}
		},
	},
	{
		Description: "accept-most-common valid group below threshold with a unique leader",
		Condition: func(a *consensusAnalysis) bool {
			// Only active when AcceptMostCommon is applicable for the present context
			isDisputeAcceptMost := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
			isLowParticipantsAcceptMost := a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult && a.isLowParticipants(a.config.agreementThreshold)
			if !(isDisputeAcceptMost || isLowParticipantsAcceptMost) {
				return false
			}
			// Determine best and second best among valid groups by count
			var best *responseGroup
			second := 0
			for _, g := range a.getValidGroups() {
				if best == nil || g.Count > best.Count {
					if best != nil {
						if best.Count > second {
							second = best.Count
						}
					}
					best = g
				} else if g.Count > second {
					second = g.Count
				}
			}
			if best == nil {
				return false
			}
			// Below threshold and strictly unique leader
			return best.Count < a.config.agreementThreshold && best.Count > second
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Pick the unique best-by-count valid group; resolve by response type
			var best *responseGroup
			second := 0
			for _, g := range a.getValidGroups() {
				if best == nil || g.Count > best.Count {
					if best != nil {
						if best.Count > second {
							second = best.Count
						}
					}
					best = g
				} else if g.Count > second {
					second = g.Count
				}
			}
			if best == nil || best.Count <= second {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
				}
			}
			if best.ResponseType == ResponseTypeConsensusError {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: best.FirstError}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: best.LargestResult}
		},
	},
	{
		Description: "low participants + accept-most-common: return best valid by priority and consider non-empty ties as dispute",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.lowParticipantsBehavior != common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult {
				return false
			}
			return a.validParticipants < a.config.agreementThreshold && len(a.getValidGroups()) > 0
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			if bestNonEmpty := a.getBestNonEmpty(); bestNonEmpty != nil {
				if bestNonEmpty.IsTie {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
						Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
					}
				}
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.LargestResult}
			}
			if bestEmpty := a.getBestEmpty(); bestEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestEmpty.LargestResult}
			}
			if bestError := a.getBestError(); bestError != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: bestError.FirstError}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("not enough participants", a.participants(), nil),
			}
		},
	},
	{
		Description: "consensus on result or error achieved if there is a valid group that meets the agreement threshold",
		Condition: func(a *consensusAnalysis) bool {
			// Consider only valid groups (non-empty, empty, and consensus-valid errors)
			var bestValid *responseGroup
			for _, g := range a.getValidGroups() {
				if bestValid == nil || g.Count > bestValid.Count {
					bestValid = g
				}
			}
			return bestValid != nil && bestValid.Count >= a.config.agreementThreshold
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			// Pick winner among valid groups only
			var bestValid *responseGroup
			for _, g := range a.getValidGroups() {
				if bestValid == nil || g.Count > bestValid.Count {
					bestValid = g
				}
			}
			if bestValid == nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
					Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
				}
			}
			if bestValid.ResponseType == ResponseTypeConsensusError {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: bestValid.FirstError}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestValid.LargestResult}
		},
	},
	{
		Description: "dispute when multiple valid groups exist and none meet the agreement threshold",
		Condition: func(a *consensusAnalysis) bool {
			best := a.getBestByCount()
			if best == nil {
				return false
			}
			return best.Count < a.config.agreementThreshold && len(a.getValidGroups()) > 1
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// All participants responded with infrastructure errors (no valid participants), but they agree
	// on the same error and meet threshold: return the agreed error
	{
		Description: "all participants have identical infrastructure errors meeting threshold -> return error",
		Condition: func(a *consensusAnalysis) bool {
			if a.validParticipants > 0 {
				return false
			}
			best := a.getBestByCount()
			if best == nil || best.ResponseType != ResponseTypeInfrastructureError {
				return false
			}
			return best.Count >= a.config.agreementThreshold
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			best := a.getBestByCount()
			if best != nil && best.FirstError != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: best.FirstError}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("not enough participants", a.participants(), nil),
			}
		},
	},
	// Low participants + ReturnError behavior
	{
		Description: "low participants with ReturnError behavior",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.lowParticipantsBehavior != common.ConsensusLowParticipantsBehaviorReturnError {
				return false
			}
			// Low participants when valid (non-infra-error) responses are fewer than threshold
			return a.validParticipants < a.config.agreementThreshold
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("not enough participants", a.participants(), nil),
			}
		},
	},
	{
		Description: "consider low participants when no responses are available",
		Condition: func(a *consensusAnalysis) bool {
			return len(a.groups) == 0
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("no responses available", nil, nil),
			}
		},
	},
	{
		Description: "none of the rules were able to resolve consensus",
		Condition: func(a *consensusAnalysis) bool {
			return true
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("none of the rules were able to resolve consensus", nil, nil),
			}
		},
	},
}

var shortCircuitRules = []shortCircuitRule{
	{
		Description: "eth_sendRawTransaction: short-circuit on first valid tx hash response",
		Reason:      "sendrawtx_first_success",
		Condition: func(w *failsafeCommon.PolicyResult[*common.NormalizedResponse], a *consensusAnalysis) bool {
			// Only applies to eth_sendRawTransaction
			if a.method != "eth_sendRawTransaction" {
				return false
			}
			// Short-circuit as soon as we have any valid non-empty response (tx hash)
			// For eth_sendRawTransaction, once a tx is accepted by any node, it will
			// propagate through the network, so we don't need to wait for consensus.
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty && g.Count >= 1 {
					return true
				}
			}
			return false
		},
	},
	{
		Description: "consensus-valid error meets agreement threshold -> short-circuit to error",
		Reason:      "consensus_error_threshold",
		Condition: func(w *failsafeCommon.PolicyResult[*common.NormalizedResponse], a *consensusAnalysis) bool {
			best := a.getBestByCount()
			if best == nil {
				return false
			}
			if best.ResponseType != ResponseTypeConsensusError {
				return false
			}
			if best.Count < a.config.agreementThreshold {
				return false
			}
			// Don't short-circuit when preferHighestValueFor is configured for this method;
			// we need all responses to find the truly highest value.
			if a.config.preferHighestValueFor != nil && a.method != "" {
				if _, ok := a.config.preferHighestValueFor[a.method]; ok {
					return false
				}
			}
			// Don't short-circuit to an error under AcceptMostCommon when a preference
			// could change the winner (PreferNonEmpty or PreferLargerResponses).
			// We intentionally avoid early termination even if a non-empty/larger has
			// not yet arrived, to allow preferred valid results to participate.
			acceptMostCommon := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult ||
				a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
			if acceptMostCommon && (a.config.preferNonEmpty || a.config.preferLargerResponses) {
				return false
			}
			return true
		},
	},
	{
		Description: "winner meets agreement threshold, is non-empty, and lead is unassailable (no possible tie with remaining)",
		Reason:      "unassailable_lead",
		Condition: func(w *failsafeCommon.PolicyResult[*common.NormalizedResponse], a *consensusAnalysis) bool {
			// With remaining participants, avoid short-circuiting when a preference could still
			// change the winner. In particular, when PreferLargerResponses is enabled, a later
			// larger response can override a smaller above-threshold winner regardless of counts.
			best := a.getBestByCount()
			if a.hasRemaining() {
				// Do not short-circuit when preferHighestValueFor is configured for this method;
				// we need all responses to find the truly highest value.
				if a.config.preferHighestValueFor != nil && a.method != "" {
					if _, ok := a.config.preferHighestValueFor[a.method]; ok {
						return false
					}
				}
				// Do not short-circuit while PreferLargerResponses is enabled; a larger result
				// arriving later may change the final decision even if the current leader is
				// above threshold.
				if a.config.preferLargerResponses {
					return false
				}
				// If PreferNonEmpty is enabled and the current leader is empty, allow more
				// responses to arrive, since the preference could change the winner.
				if a.config.preferNonEmpty && best != nil && best.ResponseType == ResponseTypeEmpty {
					return false
				}
			}
			if best == nil || best.ResponseType != ResponseTypeNonEmpty || best.Count < a.config.agreementThreshold {
				return false
			}
			// Compute second best among valid groups (non-infrastructure)
			secondCount := 0
			for _, g := range a.getValidGroups() {
				if g.Hash == best.Hash {
					continue
				}
				if g.Count > secondCount {
					secondCount = g.Count
				}
			}
			remaining := a.config.maxParticipants - a.totalParticipants
			// Without size preference, allow potential ties to short-circuit once threshold is met
			// and the lead is unassailable by the number of remaining responses.
			leadOverSecond := best.Count - secondCount
			return leadOverSecond >= remaining
		},
	},
}
