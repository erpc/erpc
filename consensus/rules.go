package consensus

import (
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
	Condition   func(winner *failsafeCommon.PolicyResult[*common.NormalizedResponse], a *consensusAnalysis) bool
}

// consensusRules defines all consensus rules in priority order
// Rules are evaluated from most specific/nuanced to most generic, and ideally those that error must come before those that return a result.
var consensusRules = []consensusRule{
	// PreferLargerResponses + AcceptMostCommon: when no group meets threshold, choose the largest non-empty
	{
		Description: "prefer-larger + accept-most-common: choose largest non-empty below threshold",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferLargerResponses {
				return false
			}
			// Apply only when dispute behavior is AcceptMostCommon (not low participants behavior)
			acceptMostCommon := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
			if !acceptMostCommon {
				return false
			}
			best := a.getBestByCount()
			return best == nil || best.Count < a.config.agreementThreshold
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			largest := a.getBestBySize()
			if largest != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: largest.FirstResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// AcceptMostCommon + PreferNonEmpty: even if empty meets threshold, choose best non-empty
	{
		Description: "accept-most-common + prefer-non-empty: choose non-empty even if empty meets threshold",
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
			if best == nil {
				return false
			}
			// Only apply when the leading group meets threshold and is empty while any non-empty exists
			if best.Count >= a.config.agreementThreshold && best.ResponseType == ResponseTypeEmpty {
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
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.FirstResult}
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
			// Apply only when below threshold, the leading group is empty, and there exists at least one non-empty group
			best := a.getBestByCount()
			if best == nil || best.Count >= a.config.agreementThreshold {
				return false
			}
			if best.ResponseType != ResponseTypeEmpty {
				return false
			}
			// Do not apply if there are multiple distinct non-empty groups (tie among non-empty)
			nonEmptyGroups := 0
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty {
					nonEmptyGroups++
				}
			}
			return nonEmptyGroups == 1
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			var bestNonEmpty *responseGroup
			for _, g := range a.groups {
				if g.ResponseType == ResponseTypeNonEmpty && (bestNonEmpty == nil || g.Count > bestNonEmpty.Count || (g.Count == bestNonEmpty.Count && g.ResponseSize > bestNonEmpty.ResponseSize)) {
					bestNonEmpty = g
				}
			}
			if bestNonEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.FirstResult}
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
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: largest.FirstResult}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	// PreferLargerResponses: if a smaller result meets threshold but a larger non-empty exists, respect preference
	{
		Description: "prefer-larger: dispute or choose largest when smaller meets threshold and larger exists",
		Condition: func(a *consensusAnalysis) bool {
			if !a.config.preferLargerResponses {
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
			// AcceptMostCommon -> choose largest; ReturnError -> dispute
			if a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult {
				largest := a.getBestBySize()
				if largest != nil {
					return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: largest.FirstResult}
				}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	{
		Description: "accept-most-common valid group below threshold with a unique leader",
		Condition: func(a *consensusAnalysis) bool {
			acceptMostCommon := a.config.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult ||
				a.config.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
			if !acceptMostCommon {
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
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: best.FirstResult}
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
	// Low participants with AcceptMostCommon: choose a valid result by priority
	{
		Description: "low participants + accept-most-common: return best valid by priority",
		Condition: func(a *consensusAnalysis) bool {
			if a.config.lowParticipantsBehavior != common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult {
				return false
			}
			return a.validParticipants < a.config.agreementThreshold && len(a.getValidGroups()) > 0
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			var bestNonEmpty *responseGroup
			var bestEmpty *responseGroup
			var bestError *responseGroup
			for _, g := range a.getValidGroups() {
				switch g.ResponseType {
				case ResponseTypeNonEmpty:
					if bestNonEmpty == nil || g.Count > bestNonEmpty.Count || (g.Count == bestNonEmpty.Count && g.ResponseSize > bestNonEmpty.ResponseSize) {
						bestNonEmpty = g
					}
				case ResponseTypeEmpty:
					if bestEmpty == nil || g.Count > bestEmpty.Count {
						bestEmpty = g
					}
				case ResponseTypeConsensusError:
					if bestError == nil || g.Count > bestError.Count {
						bestError = g
					}
				}
			}
			if bestNonEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestNonEmpty.FirstResult}
			}
			if bestEmpty != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: bestEmpty.FirstResult}
			}
			if bestError != nil {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: bestError.FirstError}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("not enough participants", a.participants(), nil),
			}
		},
	},
	{
		Description: "consensus on result or error achieved if there is a group that meets the agreement threshold",
		Condition: func(a *consensusAnalysis) bool {
			best := a.getBestByCount()
			return best != nil && best.Count >= a.config.agreementThreshold
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			best := a.getBestByCount()
			if best.ResponseType == ResponseTypeConsensusError {
				return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Error: best.FirstError}
			}
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{Result: best.FirstResult}
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
		Description: "winner meets agreement threshold, is non-empty, and lead is unassailable (no possible tie with remaining)",
		Condition: func(w *failsafeCommon.PolicyResult[*common.NormalizedResponse], a *consensusAnalysis) bool {
			// With remaining participants, avoid short-circuiting when PreferLargerResponses is enabled,
			// or when PreferNonEmpty is enabled and the current leader is empty (preference could change winner).
			best := a.getBestByCount()
			if a.hasRemaining() {
				if a.config.preferLargerResponses {
					return false
				}
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
			// Short-circuit only if the lead is strictly greater than what remaining responses could close
			return (best.Count - secondCount) > remaining
		},
	},
}

// {
// 	Name: "prefer-larger-accept-most-common",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		if !e.preferLargerResponses || e.disputeBehavior != common.ConsensusDisputeBehaviorAcceptMostCommonValidResult ||
// 		e.lowParticipantsBehavior != common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult{
// 			return false
// 		}
// 		// Check if we're in dispute/low participants with AcceptMostCommon behavior
// 		hasConsensus := a.GetBestByCount() != nil && a.GetBestByCount().Count >= e.agreementThreshold
// 		if hasConsensus {
// 			return false // Let normal consensus handle it
// 		}
// 		return (e.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult ||
// 			e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult) &&
// 			a.GetBestBySize() != nil
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		// Return the largest non-empty response regardless of count
// 		largest := a.GetBestBySize()
// 		return &failsafeCommon.PolicyResult[any]{Result: largest.FirstResult}
// 	},
// 	Description: "PreferLargerResponses + AcceptMostCommon: return largest response",
// },

// {
// 	Name: "prefer-non-empty-insufficient",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		if !e.preferNonEmpty {
// 			return false
// 		}
// 		// Check if best candidate is empty/error and below threshold
// 		bestNonEmpty := a.GetBestNonEmpty()
// 		if bestNonEmpty != nil && bestNonEmpty.Count >= e.agreementThreshold {
// 			return false // Non-empty meets threshold
// 		}
// 		// Check if empty/error would win but is below threshold
// 		bestEmpty := a.GetBestEmpty()
// 		bestError := a.GetBestError()

// 		// Find what would be selected
// 		var selected *responseGroup[any]
// 		if bestNonEmpty != nil {
// 			selected = bestNonEmpty
// 		} else if bestEmpty != nil {
// 			selected = bestEmpty
// 		} else {
// 			selected = bestError
// 		}

// 		return selected != nil && selected.ResponseType != ResponseTypeNonEmpty &&
// 			selected.Count < e.agreementThreshold
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		return &failsafeCommon.PolicyResult[any]{
// 			Error: common.NewErrConsensusLowParticipants("no non-empty responses with sufficient agreement", nil, nil),
// 		}
// 	},
// 	Description: "PreferNonEmpty but only empty/error responses below threshold",
// },

// {
// 	Name: "unresolvable-tie",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		return a.HasTie()
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		return &failsafeCommon.PolicyResult[any]{
// 			Error: common.NewErrConsensusDispute("Unresolvable tie between responses", nil, nil),
// 		}
// 	},
// 	Description: "Tie between multiple responses with same count",
// },

// {
// 	Name: "consensus-non-empty-preferred",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		if !e.preferNonEmpty {
// 			return false
// 		}
// 		bestNonEmpty := a.GetBestNonEmpty()
// 		return bestNonEmpty != nil && bestNonEmpty.Count >= e.agreementThreshold
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		best := a.GetBestNonEmpty()
// 		return &failsafeCommon.PolicyResult[any]{Result: best.FirstResult}
// 	},
// 	Description: "consensus on non-empty response (preferNonEmpty enabled)",
// },

// {
// 	Name: "low-participants-error",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		return a.IsLowParticipants(e.agreementThreshold) &&
// 			e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorReturnError
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		return &failsafeCommon.PolicyResult[any]{
// 			Error: common.NewErrConsensusLowParticipants("Not enough participants", nil, nil),
// 		}
// 	},
// 	Description: "Low participants with ReturnError behavior",
// },

// {
// 	Name: "low-participants-accept-prefer-non-empty",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		return a.IsLowParticipants(e.agreementThreshold) &&
// 			e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult &&
// 			e.preferNonEmpty && a.GetBestNonEmpty() != nil
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		best := a.GetBestNonEmpty()
// 		return &failsafeCommon.PolicyResult[any]{Result: best.FirstResult}
// 	},
// 	Description: "LowParticipants + AcceptMostCommon + PreferNonEmpty: return best non-empty",
// },

// {
// 	Name: "dispute-error",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		isDispute := !a.IsLowParticipants(e.agreementThreshold) &&
// 			(a.GetBestByCount() == nil || a.GetBestByCount().Count < e.agreementThreshold)
// 		return isDispute && e.disputeBehavior == common.ConsensusDisputeBehaviorReturnError
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		return &failsafeCommon.PolicyResult[any]{
// 			Error: common.NewErrConsensusDispute("Not enough agreement among responses", nil, nil),
// 		}
// 	},
// 	Description: "Dispute with ReturnError behavior",
// },

// {
// 	Name: "accept-most-common-prefer-non-empty",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		acceptMostCommon := e.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult ||
// 			e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
// 		return acceptMostCommon && e.preferNonEmpty && a.GetBestNonEmpty() != nil
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		best := a.GetBestNonEmpty()
// 		return &failsafeCommon.PolicyResult[any]{Result: best.FirstResult}
// 	},
// 	Description: "AcceptMostCommon + PreferNonEmpty: return best non-empty",
// },

// {
// 	Name: "accept-most-common-generic",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		acceptMostCommon := e.disputeBehavior == common.ConsensusDisputeBehaviorAcceptMostCommonValidResult ||
// 			e.lowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
// 		return acceptMostCommon && a.GetBestByCount() != nil
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		best := a.GetBestByCount()
// 		if best.ResponseType == ResponseTypeConsensusError ||
// 			best.ResponseType == ResponseTypeInfrastructureError {
// 			return &failsafeCommon.PolicyResult[any]{Error: best.FirstError}
// 		}
// 		return &failsafeCommon.PolicyResult[any]{Result: best.FirstResult}
// 	},
// 	Description: "AcceptMostCommon: return highest count response",
// },

// {
// 	Name: "dispute-error",
// 	Condition: func(e *executor[any], a *consensusAnalysis[any]) bool {
// 		isDispute := !a.IsLowParticipants(e.agreementThreshold) &&
// 			(a.GetBestByCount() == nil || a.GetBestByCount().Count < e.agreementThreshold)
// 		return isDispute && e.disputeBehavior == common.ConsensusDisputeBehaviorReturnError
// 	},
// 	Action: func(e *executor[any], a *consensusAnalysis[any]) *failsafeCommon.PolicyResult[any] {
// 		return &failsafeCommon.PolicyResult[any]{
// 			Error: common.NewErrConsensusDispute("Not enough agreement among responses", nil, nil),
// 		}
// 	},
// 	Description: "Dispute with ReturnError behavior",
// },
