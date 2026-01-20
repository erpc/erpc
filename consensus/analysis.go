package consensus

import (
	"errors"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/failsafe-go/failsafe-go"
	"github.com/rs/zerolog"
)

// responseGroup holds all responses that share the same hash.
type responseGroup struct {
	Hash    string
	Results []*execResult
	Count   int
	IsTie   bool // Flag to indicate a tie with another group

	ResponseType ResponseType
	ResponseSize int // Size of the largest result in the group

	// Cached largest result/error for quick access (prefer largest response even within same consensus group)
	LargestResult *common.NormalizedResponse
	FirstError    error
	HasResult     bool
}

func (g *responseGroup) participants() []common.ParticipantInfo {
	participants := make([]common.ParticipantInfo, 0, len(g.Results))
	for _, r := range g.Results {
		upsId := "n/a"
		if r.Upstream != nil {
			upsId = r.Upstream.Id()
		}
		participants = append(participants, common.ParticipantInfo{
			Upstream:   upsId,
			ResultHash: r.CachedHash,
			ErrSummary: common.ErrorSummary(r.Err),
		})
	}
	return participants
}

// consensusAnalysis contains the complete, one-time analysis of all collected responses.
type consensusAnalysis struct {
	config            *config
	groups            map[string]*responseGroup
	totalParticipants int
	validParticipants int
	originalRequest   *common.NormalizedRequest
	leaderUpstream    common.Upstream
	method            string // The RPC method being called (e.g., "eth_getTransactionCount")

	// Cached computed values
	cachedBestNonEmpty *responseGroup
	cachedBestEmpty    *responseGroup
	cachedBestError    *responseGroup
	cachedBestByCount  *responseGroup
	cachedBestBySize   *responseGroup
	cachedValidGroups  []*responseGroup
}

func newConsensusAnalysis(lg *zerolog.Logger, exec failsafe.Execution[*common.NormalizedResponse], config *config, responses []*execResult) *consensusAnalysis {
	analysis := &consensusAnalysis{
		config:            config,
		groups:            make(map[string]*responseGroup),
		totalParticipants: len(responses),
	}

	// Try to extract original request and compute leader upstream once
	if req, ok := exec.Context().Value(common.RequestContextKey).(*common.NormalizedRequest); ok && req != nil {
		analysis.originalRequest = req
		if method, err := req.Method(); err == nil {
			analysis.method = method
		}
		if net := req.Network(); net != nil && net.Architecture() == common.ArchitectureEvm {
			// Use the executor context; leader selection is read-only and fast
			analysis.leaderUpstream = net.EvmLeaderUpstream(exec.Context())
		}
	}

	// Classify, hash, and group all responses
	for _, r := range responses {
		classifyAndHashResponse(r, exec, config)

		if r.CachedResponseType != ResponseTypeInfrastructureError {
			analysis.validParticipants++
		}

		group, exists := analysis.groups[r.CachedHash]
		if !exists {
			group = &responseGroup{
				Hash:         r.CachedHash,
				ResponseType: r.CachedResponseType,
				ResponseSize: r.CachedResponseSize,
			}
			analysis.groups[r.CachedHash] = group
		}

		group.Count++
		group.Results = append(group.Results, r)
		// Track the largest successful response in the group
		if r.Err == nil {
			if !group.HasResult {
				// First successful response in this group
				group.LargestResult = r.Result
				group.ResponseSize = r.CachedResponseSize
				group.FirstError = nil
				group.HasResult = true
			} else if r.CachedResponseSize > group.ResponseSize {
				// Found a larger response in the same consensus group
				group.LargestResult = r.Result
				group.ResponseSize = r.CachedResponseSize
			}
		} else if group.FirstError == nil && r.Err != nil {
			group.FirstError = r.Err
		}
	}

	// After grouping, compute tie flags among valid groups by response type (exclude infrastructure errors)
	if len(analysis.groups) > 1 {
		countsByType := make(map[ResponseType]map[int]int)
		for _, g := range analysis.groups {
			if g.ResponseType == ResponseTypeInfrastructureError {
				continue
			}
			if _, ok := countsByType[g.ResponseType]; !ok {
				countsByType[g.ResponseType] = make(map[int]int)
			}
			countsByType[g.ResponseType][g.Count]++
		}
		for _, g := range analysis.groups {
			if g.ResponseType == ResponseTypeInfrastructureError {
				g.IsTie = false
				continue
			}
			g.IsTie = countsByType[g.ResponseType][g.Count] > 1
		}
	}

	return analysis
}

func (a *consensusAnalysis) hasRemaining() bool {
	return a.config.maxParticipants > a.totalParticipants
}

func (a *consensusAnalysis) participants() []common.ParticipantInfo {
	participants := make([]common.ParticipantInfo, 0, len(a.groups))
	for _, group := range a.groups {
		participants = append(participants, group.participants()...)
	}
	return participants
}

// getValidGroups returns all groups of non-empty, empty, or valid consensus errors, skipping infrastructure errors
func (a *consensusAnalysis) getValidGroups() []*responseGroup {
	if a.cachedValidGroups == nil {
		a.cachedValidGroups = make([]*responseGroup, 0, len(a.groups))
		for _, group := range a.groups {
			if group.ResponseType != ResponseTypeInfrastructureError {
				a.cachedValidGroups = append(a.cachedValidGroups, group)
			}
		}
	}
	return a.cachedValidGroups
}

// getBestNonEmpty returns the best non-empty group (cached)
func (a *consensusAnalysis) getBestNonEmpty() *responseGroup {
	if a.cachedBestNonEmpty == nil {
		var best *responseGroup
		for _, group := range a.groups {
			if group.ResponseType != ResponseTypeNonEmpty {
				continue
			}
			if best == nil || group.Count > best.Count || (group.Count == best.Count && group.ResponseSize > best.ResponseSize) {
				best = group
			}
		}
		a.cachedBestNonEmpty = best
	}
	return a.cachedBestNonEmpty
}

// getBestEmpty returns the best empty group (cached)
func (a *consensusAnalysis) getBestEmpty() *responseGroup {
	if a.cachedBestEmpty == nil {
		var best *responseGroup
		for _, group := range a.groups {
			if group.ResponseType == ResponseTypeEmpty {
				if best == nil {
					best = group
					continue
				}
				if group.Count > best.Count {
					best = group
				}
			}
		}
		a.cachedBestEmpty = best
	}
	return a.cachedBestEmpty
}

// getBestError returns the best error group (cached)
func (a *consensusAnalysis) getBestError() *responseGroup {
	if a.cachedBestError == nil {
		var best *responseGroup
		for _, group := range a.groups {
			if group.ResponseType == ResponseTypeConsensusError || group.ResponseType == ResponseTypeInfrastructureError {
				if best == nil {
					best = group
					continue
				}
				if group.Count > best.Count {
					best = group
				}
			}
		}
		a.cachedBestError = best
	}
	return a.cachedBestError
}

// getBestByCount returns the group with highest count (cached)
func (a *consensusAnalysis) getBestByCount() *responseGroup {
	if a.cachedBestByCount == nil && len(a.groups) > 0 {
		var best *responseGroup
		for _, group := range a.groups {
			if best == nil || group.Count > best.Count {
				best = group
			}
		}
		a.cachedBestByCount = best
	}
	return a.cachedBestByCount
}

// getBestBySize returns the largest non-empty group (cached)
func (a *consensusAnalysis) getBestBySize() *responseGroup {
	if a.cachedBestBySize == nil {
		var best *responseGroup
		for _, group := range a.groups {
			if group.ResponseType == ResponseTypeNonEmpty && (best == nil || group.ResponseSize > best.ResponseSize) {
				best = group
			}
		}
		a.cachedBestBySize = best
	}
	return a.cachedBestBySize
}

// isLowParticipants checks if we have insufficient participants
func (a *consensusAnalysis) isLowParticipants(threshold int) bool {
	return a.validParticipants < threshold
}

// getLeaderGroupNonError returns the response group that contains the leader upstream with a non-error response (non-empty or empty).
func (a *consensusAnalysis) getLeaderGroupNonError() *responseGroup {
	if a.leaderUpstream == nil {
		return nil
	}
	for _, group := range a.groups {
		if group.ResponseType == ResponseTypeConsensusError || group.ResponseType == ResponseTypeInfrastructureError {
			continue
		}
		for _, r := range group.Results {
			if r != nil && r.Upstream != nil && r.Err == nil {
				if r.Upstream == a.leaderUpstream {
					return group
				}
			}
		}
	}
	return nil
}

// getLeaderGroupAny returns the response group that contains the leader upstream for any valid response
// (non-empty, empty, or consensus-valid error). Infrastructure errors are ignored since they are not
// meaningful consensus responses.
func (a *consensusAnalysis) getLeaderGroupAny() *responseGroup {
	if a.leaderUpstream == nil {
		return nil
	}
	// search valid groups only (exclude infrastructure errors)
	for _, group := range a.getValidGroups() {
		for _, r := range group.Results {
			if r != nil && r.Upstream != nil && r.Upstream == a.leaderUpstream {
				return group
			}
		}
	}
	return nil
}

// getLeaderFirstErrorIncludingInfra returns the leader's first error from any group,
// including infrastructure errors. Used by leader-preferring behaviors that must
// return the leader's error even when it isn't a consensus-valid error.
func (a *consensusAnalysis) getLeaderFirstErrorIncludingInfra() error {
	if a.leaderUpstream == nil {
		return nil
	}
	for _, group := range a.groups {
		if group == nil || group.FirstError == nil {
			// Fast path: skip groups without cached error
			// Still scan results in case FirstError isn't populated
		}
		for _, r := range group.Results {
			if r != nil && r.Upstream != nil && r.Upstream == a.leaderUpstream && r.Err != nil {
				return r.Err
			}
		}
	}
	return nil
}

// --- Helper and Utility Functions ---

// isConsensusValidError checks if an error can be part of consensus (e.g., EVM revert).
func isConsensusValidError(err error) bool {
	return common.HasErrorCode(err, common.ErrCodeEndpointExecutionException)
}

// isAgreedUponError checks for errors that are not execution-related but can be agreed upon (e.g., invalid params).
func isAgreedUponError(err error) bool {
	return common.HasErrorCode(err,
		common.ErrCodeEndpointClientSideException,
		common.ErrCodeEndpointUnsupported,
		common.ErrCodeEndpointMissingData,
	)
}

// errorToConsensusHash generates a comparable hash from an error.
func errorToConsensusHash(err error) string {
	if err == nil {
		return ""
	}
	var jre *common.ErrJsonRpcExceptionInternal
	if errors.As(err, &jre) {
		return fmt.Sprintf("jsonrpc:%d", jre.NormalizedCode())
	}
	if se, ok := err.(common.StandardError); ok {
		if base := se.Base(); base != nil {
			return string(base.Code)
		}
	}
	return "error:generic"
}

// resultToJsonRpcResponse safely converts a result to a JsonRpcResponse.
func resultToJsonRpcResponse(result *common.NormalizedResponse, exec failsafe.Execution[*common.NormalizedResponse]) *common.JsonRpcResponse {
	if result == nil {
		return nil
	}
	jr, _ := result.JsonRpcResponse(exec.Context())
	return jr
}

// classifyAndHashResponse computes and caches the response type, hash, and size for a result.
func classifyAndHashResponse(r *execResult, exec failsafe.Execution[*common.NormalizedResponse], config *config) {
	if r.Err != nil {
		// Classify agreed-upon JSON-RPC errors and execution exceptions as consensus-valid errors.
		// Only true infrastructure issues (like timeouts, network/server failures) are infrastructure errors.
		if isConsensusValidError(r.Err) || isAgreedUponError(r.Err) {
			r.CachedResponseType = ResponseTypeConsensusError
		} else {
			r.CachedResponseType = ResponseTypeInfrastructureError
		}
		r.CachedHash, _ = resultOrErrorToHash(r, exec, config)
		if r.CachedHash == "" {
			r.CachedHash = "error:generic"
		}
		r.CachedResponseSize = 0
		return
	}

	// Successful response
	jr := resultToJsonRpcResponse(r.Result, exec)
	if jr == nil {
		r.CachedResponseType = ResponseTypeInfrastructureError
		r.CachedHash = "error:generic"
		return
	}

	// Use NormalizedResponse-aware emptyish check to capture method-specific semantics (e.g., EVM logs)
	if r.Result != nil && r.Result.IsResultEmptyish(exec.Context()) {
		r.CachedResponseType = ResponseTypeEmpty
	} else {
		r.CachedResponseType = ResponseTypeNonEmpty
	}

	if size, err := jr.Size(exec.Context()); err == nil {
		r.CachedResponseSize = size
	}

	r.CachedHash, _ = resultOrErrorToHash(r, exec, config)
	if r.CachedHash == "" {
		r.CachedResponseType = ResponseTypeInfrastructureError
		r.CachedHash = "error:generic"
	}
}

// resultOrErrorToHash computes a hash for a result, considering both success and error cases.
func resultOrErrorToHash(r *execResult, exec failsafe.Execution[*common.NormalizedResponse], config *config) (string, error) {
	if r.Err != nil {
		if isConsensusValidError(r.Err) || isAgreedUponError(r.Err) {
			return errorToConsensusHash(r.Err), nil
		}
		return "", errNotConsensusValid // Infrastructure errors are not hashed for consensus
	}

	// Successful result
	jr := resultToJsonRpcResponse(r.Result, exec)
	if jr == nil {
		return "", errNoJsonRpcResponse
	}

	if config.ignoreFields != nil {
		if originalReq, ok := exec.Context().Value(common.RequestContextKey).(*common.NormalizedRequest); ok {
			if method, err := originalReq.Method(); err == nil {
				if fields, ok := config.ignoreFields[method]; ok {
					return jr.CanonicalHashWithIgnoredFields(fields, exec.Context())
				}
			}
		}
	}
	return jr.CanonicalHash()
}
