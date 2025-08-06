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
	{
		Description: "dispute when multiple groups and no group meets the agreement threshold",
		Condition: func(a *consensusAnalysis) bool {
			if a.hasRemaining() {
				return false
			}
			best := a.getBestByCount()
			if best == nil {
				return false
			}
			return best.Count < a.config.agreementThreshold && len(a.groups) > 1
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusDispute("not enough agreement among responses", a.participants(), nil),
			}
		},
	},
	{
		Description: "low participants when valid participants are less than the agreement threshold",
		Condition: func(a *consensusAnalysis) bool {
			if a.hasRemaining() {
				return false
			}
			best := a.getBestByCount()
			if best == nil {
				return false
			}
			return a.validParticipants < a.config.agreementThreshold
		},
		Action: func(a *consensusAnalysis) *failsafeCommon.PolicyResult[*common.NormalizedResponse] {
			return &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
				Error: common.NewErrConsensusLowParticipants("not enough participants", a.getBestByCount().Participants(), nil),
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
		Description: "winner meets agreement threshold and is non-empty + prefer larger responses is disabled",
		Condition: func(w *failsafeCommon.PolicyResult[*common.NormalizedResponse], a *consensusAnalysis) bool {
			if a.config.preferLargerResponses && a.hasRemaining() {
				return false
			}
			best := a.getBestByCount()
			if best == nil {
				return false
			}
			return best.Count >= a.config.agreementThreshold && best.ResponseType == ResponseTypeNonEmpty
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
