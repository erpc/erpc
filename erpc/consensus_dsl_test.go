package erpc

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
)

func init() { util.ConfigureTestLogger() }

func TestConsensusPolicy_DSLScenarios(t *testing.T) {
	// Natural language scenarios that define intent; the DSL is generated via LLM
	nlScenarios := []string{
		"Dispute behavior is AcceptMostCommon andPreferLargerResponses is enabled and one group returns larger response but not meet threshold and another group smaller response but meets threshold, it returns the larger response.",
		"Above threshold both error and non-empty groups; error has higher count; AcceptMostCommon + PreferNonEmpty still selects the non-empty.",
		"Above threshold empty has higher count and no non-empty preference; select empty.",
		"Above threshold error vs non-empty counts tie; with PreferNonEmpty choose the non-empty.",
		"2/3 agree with PreferLargerResponses disabled; short-circuit; return the agreed non-empty.",
		"Accept most common by count regardless of size: two smaller non-empty agree vs one larger different non-empty; expect smaller value.",
		"Accept most common on dispute when two match and one differs; expect the matching value.",
		"All non-empty results different under ReturnError; expect dispute.",
		"All participants return execution reverted (code 3) and it returns same execution reverted error.",
		"All participants return method not found; and it returns method not found error.",
		"All participants return missing data (-32014) and it returns missing data error.",
		"All participants return null; it returns null.",
		"All participants return the same invalid params error and it returns invalid params error.",
		"All participants return the same unknown error and it returns unknown error.",
		"At threshold two identical errors vs one success; expect the successful response.",
		"Below threshold AcceptMostCommon with tie between different non-empty results; expect dispute.",
		// "Below threshold different errors from participants; expect dispute.",
		// "Below threshold infra errors vs a single non-empty; PreferNonEmpty selects the non-empty.",
		// "Below threshold majority execution errors vs one non-empty; AcceptMostCommon + PreferNonEmpty selects the non-empty.",
		// "Below threshold mix: two identical errors, one empty, one non-empty; AcceptMostCommon + PreferNonEmpty selects the non-empty.",
		// "Below threshold mixed empty and two different non-empty with ReturnError; expect dispute.",
		// "Below threshold unique consensus-valid error vs single non-empty; PreferNonEmpty selects the non-empty.",
		// "Below threshold; empty uniquely leads; PreferNonEmpty selects the best non-empty.",
		// "Below threshold: unique empty leader vs one non-empty and one infra error; no non-empty preference; select empty.",
		// "Below threshold: unique empty leader vs single non-empty; with PreferNonEmpty select the non-empty.",
		// "Both empty and non-empty meet threshold; PreferNonEmpty selects the non-empty.",
		// "Both groups above threshold; empty has higher count; with PreferNonEmpty pick the non-empty.",
		// "Both groups meet threshold with equal counts and no preference; expect dispute.",
		// "Consensus-valid error meets threshold but a single non-empty exists; AcceptMostCommon + PreferNonEmpty selects the non-empty.",
		// "Delayed large response loses to fast smaller responses under accept-most-common below threshold; expect smaller.",
		// "Dispute with OnlyBlockHeadLeader; leader has highest block and a non-error result; expect leader’s result.",
		// "Dispute with PreferBlockHeadLeader; leader has a non-error result; expect leader’s result.",
		// "Empty array and empty object vs one non-empty below threshold; PreferNonEmpty selects the non-empty.",
		// "Empty leads early; with PreferLargerResponses + PreferNonEmpty do not short-circuit; select the larger non-empty when it arrives.",
		// "Empty meets threshold (3) but non-empty has 2; AcceptMostCommon + PreferNonEmpty selects the non-empty.",
		// "Empty meets threshold quickly; no non-empty preference; short-circuit to empty.",
		// "Empty responses arrive first and meet threshold; with PreferNonEmpty wait and select the later non-empty (no short-circuit).",
		// "Empty results meet threshold; expect empty as winner.",
		// "Errors meet threshold vs one empty and no non-empty exists; AcceptMostCommon (+ PreferNonEmpty) returns the agreed error.",
		// "Explicit low participants ReturnError; two successes below threshold; expect low participants error.",
		// "Five upstreams, threshold 3; only two agree; AcceptMostCommon returns the best group (the agreed non-empty).",
		// "Five upstreams, threshold 3; only two agree; ReturnError yields dispute.",
		// "Five upstreams; only two valid (empty + non-empty); low participants with AcceptMostCommon picks the non-empty.",
		// "For receipts: prefer non-empty logs over empty logs under AcceptMostCommon; expect the receipt with logs.",
		// "Ignore block timestamp differences to achieve consensus on matching block; expect the matching block.",
		// "Ignore timestamp but core fields differ; expect dispute.",
		// "Leader is upstream with highest block height; prefer leader non-error on dispute.",
		// "Low participants when only one valid response and others are infrastructure errors; expect low participants error.",
		// "Low participants when two no-responses and one valid response while agreement threshold is 3; expect low participants error.",
		// "Low participants with accept-most-common selects the only valid non-empty when threshold unmet; expect that value.",
		// "Low participants with AcceptMostCommon and only empty valid present; return empty.",
		// "Low participants with AcceptMostCommon: empty valid beats infra errors; return empty.",
		// "Low participants with only empty valid responses under ReturnError; expect low participants error.",
		// "Low participants with OnlyBlockHeadLeader; leader returns error and no leader non-error exists; expect low participants error.",
		// "Low participants with OnlyBlockHeadLeader; leader returns non-error; expect leader’s result.",
		// "Non-empty meets threshold and PreferLargerResponses disabled; short-circuit and return the agreed non-empty.",
		// "One infra error and two identical execution errors; return the execution error.",
		// "One upstream does not respond; accept-most-common still wins with two matching responses; expect that value.",
		// "One upstream only, threshold 1; succeed with that response.",
		// "Only block head leader on dispute; leader's non-error result should be returned regardless of others.",
		// "Prefer block head leader on dispute; leader has non-error result; expect leader's result.",
		// "Prefer larger even when a smaller meets threshold; expect the larger value.",
		// "Prefer non-empty over consensus-valid error when both meet counts above threshold; expect non-empty.",
		// "Prefer non-empty over empty when below threshold and one non-empty exists; expect the non-empty.",
		// "Prefer non-empty when accept-most-common, two non-empty agree and one error; expect non-empty win.",
		// "PreferBlockHeadLeader but leader returns error; fall back to accept-most-common; expect the agreed non-empty.",
		// "PreferLargerResponses disabled; both non-empty groups >= threshold with different counts; higher count wins (smaller payload wins).",
		// "PreferLargerResponses enabled below threshold; one large non-empty vs three small non-empties; choose the largest non-empty (no short-circuit).",
		// "PreferLargerResponses enabled with ReturnError: two empty vs one large error; empty wins (errors do not win by size).",
		// "PreferLargerResponses enabled; both groups above threshold but smaller has higher count; choose largest non-empty.",
		// "PreferLargerResponses enabled; non-empty and consensus-error both meet threshold equally; choose the largest non-empty.",
		// "PreferLargerResponses enabled; one large non-empty below threshold vs three small meeting threshold; under ReturnError dispute it must fail with error.",
		// "PreferLargerResponses enabled; two large meet threshold vs two small meet threshold under ReturnError; still choose the larger non-empty.",
		// "PreferLargerResponses enabled; two large meet threshold vs two small meet threshold; AcceptMostCommon chooses the larger non-empty.",
		// "PreferNonEmpty enabled but ReturnError behavior with empty meeting threshold; expect dispute error.",
		// "Retry enabled; one upstream errors; two succeed reaching 2/3; return the agreed non-empty.",
		// "Retry once on fully broken upstream; cannot reach consensus; expect low participants error.",
		// "Retry once on intermittently bad upstream; reach consensus when it succeeds; return the agreed non-empty.",
		// "Return error on dispute when three distinct non-empty disagree; expect dispute.",
		// "ReturnError with three different execution errors; return one of the execution errors.",
		// "Single participant responding with threshold 2; expect low participants error.",
		// "Three empty vs one non-empty; AcceptMostCommon + PreferNonEmpty selects the non-empty.",
		// "Three upstreams all different non-empty under ReturnError; expect dispute.",
		// "Three upstreams, threshold 2; two successes and one error; expect success.",
		// "Three upstreams, threshold 3; two successes and one error; expect low participants error.",
		// "Threshold requirement exceeds available valid participants; expect low participants error.",
		// "Two null vs one non-empty; AcceptMostCommon + PreferNonEmpty selects the non-empty.",
		// "Two participants, threshold 2, both empty; expect empty.",
		// "Two participants, threshold 2, both non-empty identical; expect success with that value.",
		// "Two participants, threshold 2, different non-empty; expect dispute.",
		// "Two upstreams only, threshold 3; AcceptMostCommon returns the only valid non-empty.",
		// "Two zero-hex meet threshold first; with PreferNonEmpty do not short-circuit and select the later non-empty.",
		// "When prefer larger is enabled and a larger non-empty group exists with lower count than a smaller non-empty group; expect larger value.",
	}

	// Generate or load DSL scenarios via LLM (OpenAI) using a cache file keyed by hash
	items := util.GenerateDslFromScenariosViaLLM(t, "consensus_policy", dslForConsensusTests, nlScenarios)

	for _, it := range items {
		name := strings.TrimSpace(it.NL)
		if name == "" {
			name = it.DSL // fallback
		}
		spec := it.DSL
		t.Run(util.SanitizeTestName(name), func(t *testing.T) {
			tc := buildConsensusTestCaseFromDSL(t, spec)
			runConsensusTest(t, tc)
		})
	}
}

const dslForConsensusTests = `# DSL for compact consensus policy tests

Configuration Tokens:
- MXP:N: Max participants (also number of upstreams U1..UN)
- ATSH:N: Agreement threshold
- PNE: Prefer Non-Empty (default: false)
- PLR: Prefer Larger Responses (default: false)

Dispute Behavior:
- DBRE: Dispute Behavior = ReturnError (default)
- DBAMC: Dispute Behavior = AcceptMostCommonValidResult
- DBPBL: Dispute Behavior = PreferBlockHeadLeader
- DBOBL: Dispute Behavior = OnlyBlockHeadLeader

Low Participants Behavior:
- LPRE: Low Participants = ReturnError
- LPAMC: Low Participants = AcceptMostCommonValidResult (default)
- LPPBL: Low Participants = PreferBlockHeadLeader
- LPOBL: Low Participants = OnlyBlockHeadLeader

 Upstream Response Types:
 - U{index}:{response}
   Modifiers for {response}:
   - Prefix '!' to indicate the upstream is NOT expected to be called (short-circuited).
     This sets expectedCalls=0 for that upstream while still allowing you to describe a
     hypothetical response for clarity. Example: U3:!RS100AD300
   - RS{size}[Seed]: Non-empty JSON-RPC success with deterministic string payload
     Example: RS100A -> 100-char string using seed 'A'. If seed omitted, defaults to 'A'.
   - RSEM: Empty JSON-RPC success (null or empty result)
   - RSJR{code}: JSON-RPC error with code (e.g., RSJR3, RSJR-32000)
   - RSER: Infrastructure/server error (non-consensus-valid, e.g., timeout, 500)
   - RSNE: No response (simulates timeout/network failure)
   - RS{size}D{ms}: Response with delay in milliseconds (e.g., RS100AD50)

 Leader Designation:
 - LEAD:{index}: Designate upstream {index} as the block head leader
   Example: LEAD:2 -> upstream 2 is the leader
 - BH:{index}:{height}: Set block height for upstream {index}. Use separate space-delimited tokens for multiple entries.
   Example: "BH:1:100 BH:2:200" -> U1 at height 100, U2 at height 200 (do NOT use '+').

  Expected Outcomes (right side of =>):
  - RS{size}[Seed]: Expected successful result (non-empty). Example: RS100A
  - RSEM: Expected empty result (null)
  - LOWPART: Expected low participants error
  - DISPUTE: Expected dispute error
  - RSJR{code}: Expected JSON-RPC error
  - RSEM: Expected empty result (null)
  - RSER: Expected infrastructure error (non-consensus-valid, e.g., timeout, 500)
  
  Generation Guidelines (must follow):
 - Emit the arrow as "=>" (ASCII). No other separator is allowed.
 `

// buildConsensusTestCaseFromDSL parses a single-line DSL spec into a consensusTestCase.
func buildConsensusTestCaseFromDSL(t *testing.T, spec string) consensusTestCase {
	parts := strings.Split(spec, "=>")
	if len(parts) != 2 {
		t.Fatalf("invalid DSL (missing =>): %s", spec)
	}
	lhs := strings.TrimSpace(parts[0])
	rhs := strings.TrimSpace(parts[1])

	tokens := strings.Split(lhs, " ")

	cfg := &common.ConsensusPolicyConfig{}
	// Sensible defaults to keep scenarios concise and deterministic
	cfg.LowParticipantsBehavior = common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
	// Explicitly override repo defaults so DSL controls behavior
	cfg.PreferNonEmpty = boolPtr(false)
	cfg.PreferLargerResponses = boolPtr(false)

	mxp := 0
	atsh := 0
	// Collect per-upstream response specs
	upstreamSpecs := map[int]string{}
	// Tracks upstreams that should not be called (short-circuited)
	notCalled := map[int]bool{}

	leaderIndex := -1
	blockHeights := map[int]int{}

	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		switch {
		// Preferences
		case tok == "PNE":
			cfg.PreferNonEmpty = boolPtr(true)
		case tok == "PLR":
			cfg.PreferLargerResponses = boolPtr(true)

		// Dispute behaviors
		case tok == "DBRE":
			cfg.DisputeBehavior = common.ConsensusDisputeBehaviorReturnError
		case tok == "DBAMC":
			cfg.DisputeBehavior = common.ConsensusDisputeBehaviorAcceptMostCommonValidResult
		case tok == "DBPBL":
			cfg.DisputeBehavior = common.ConsensusDisputeBehaviorPreferBlockHeadLeader
		case tok == "DBOBL":
			cfg.DisputeBehavior = common.ConsensusDisputeBehaviorOnlyBlockHeadLeader

		// Low participants behaviors
		case tok == "LPRE":
			cfg.LowParticipantsBehavior = common.ConsensusLowParticipantsBehaviorReturnError
		case tok == "LPAMC":
			cfg.LowParticipantsBehavior = common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult
		case tok == "LPPBL":
			cfg.LowParticipantsBehavior = common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader
		case tok == "LPOBL":
			cfg.LowParticipantsBehavior = common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader

		// Core settings
		case strings.HasPrefix(tok, "MXP:"):
			mxp = mustAtoi(t, strings.TrimPrefix(tok, "MXP:"))
		case strings.HasPrefix(tok, "ATSH:"):
			atsh = mustAtoi(t, strings.TrimPrefix(tok, "ATSH:"))

		// Leader designation
		case strings.HasPrefix(tok, "LEAD:"):
			leaderIndex = mustAtoi(t, strings.TrimPrefix(tok, "LEAD:"))

		// Block heights
		case strings.HasPrefix(tok, "BH:"):
			bhParts := strings.SplitN(strings.TrimPrefix(tok, "BH:"), ":", 2)
			if len(bhParts) != 2 {
				t.Fatalf("invalid block height token: %s", tok)
			}
			idx := mustAtoi(t, bhParts[0])
			height := mustAtoi(t, bhParts[1])
			blockHeights[idx] = height

			// Upstream responses
		case strings.HasPrefix(tok, "U"):
			// U{index}:{spec}
			uParts := strings.SplitN(strings.TrimPrefix(tok, "U"), ":", 2)
			if len(uParts) != 2 {
				t.Fatalf("invalid upstream token: %s", tok)
			}
			idx := mustAtoi(t, uParts[0])
			specStr := strings.TrimSpace(uParts[1])
			// Leading '!' marks this upstream as not expected to be called (short-circuited)
			if strings.HasPrefix(specStr, "!") {
				notCalled[idx] = true
				specStr = strings.TrimPrefix(specStr, "!")
			}
			upstreamSpecs[idx] = specStr
		default:
			t.Fatalf("unknown token: %s", tok)
		}
	}

	if mxp <= 0 {
		t.Fatalf("MXP must be > 0 in: %s", spec)
	}
	if atsh <= 0 {
		t.Fatalf("ATSH must be > 0 in: %s", spec)
	}

	cfg.MaxParticipants = mxp
	cfg.AgreementThreshold = atsh
	if cfg.DisputeBehavior == "" {
		// Default to ReturnError when not specified
		cfg.DisputeBehavior = common.ConsensusDisputeBehaviorReturnError
	}

	upstreams := createTestUpstreams(mxp)

	// Build mocks aligned with upstreams
	mocks := make([]mockResponse, mxp)
	calls := make([]int, mxp)
	for i := 1; i <= mxp; i++ {
		specStr, ok := upstreamSpecs[i]
		if !ok {
			// Default to infrastructure error
			mocks[i-1] = makeInfraErrorMock()
			calls[i-1] = 1
			// If explicitly marked not-called, override call expectation
			if notCalled[i] {
				calls[i-1] = 0
			}
			continue
		}
		mock := makeMockFromResponseSpec(t, specStr)
		// RSNE (no response) means don't set up mock
		if specStr == "RSNE" {
			calls[i-1] = 0 // no mock setup
		} else {
			mocks[i-1] = mock
			calls[i-1] = 1
		}
		// Override with not-called flag if present
		if notCalled[i] {
			calls[i-1] = 0
		}
	}

	// Parse expected outcome
	expResult, expError := parseExpectedOutcomeFromSpec(t, rhs, upstreamSpecs)

	// Setup function for leader/block heights if needed
	var setupFn func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry)
	if leaderIndex > 0 || len(blockHeights) > 0 {
		setupFn = func(t *testing.T, ctx context.Context, reg *upstream.UpstreamsRegistry) {
			ups := reg.GetNetworkUpstreams(ctx, util.EvmNetworkId(123))

			// Set block heights
			for idx, height := range blockHeights {
				if idx > 0 && idx <= len(ups) {
					ups[idx-1].EvmStatePoller().SuggestLatestBlock(int64(height))
					time.Sleep(50 * time.Millisecond)
				}
			}

			// Auto-set leader based on highest block if not explicitly set
			if leaderIndex <= 0 && len(blockHeights) > 0 {
				maxHeight := 0
				for idx, h := range blockHeights {
					if h > maxHeight {
						maxHeight = h
						leaderIndex = idx
					}
				}
			}

			// Set leader by giving it highest block
			if leaderIndex > 0 && leaderIndex <= len(ups) {
				// Ensure leader has highest block
				maxHeight := 0
				for i, u := range ups {
					if i+1 != leaderIndex {
						h := 100 // default height for non-leaders
						if bh, ok := blockHeights[i+1]; ok {
							h = bh
						}
						u.EvmStatePoller().SuggestLatestBlock(int64(h))
						time.Sleep(50 * time.Millisecond)
						if h > maxHeight {
							maxHeight = h
						}
					}
				}
				// Leader gets max + 100
				leaderHeight := maxHeight + 100
				if bh, ok := blockHeights[leaderIndex]; ok && bh > maxHeight {
					leaderHeight = bh
				}
				ups[leaderIndex-1].EvmStatePoller().SuggestLatestBlock(int64(leaderHeight))
				time.Sleep(50 * time.Millisecond)
			}
		}
	}

	return consensusTestCase{
		name:            util.SanitizeTestName(spec),
		description:     spec,
		upstreams:       upstreams,
		consensusConfig: cfg,
		mockResponses:   mocks,
		expectedCalls:   calls,
		expectedResult:  expResult,
		expectedError:   expError,
		setupFn:         setupFn,
	}
}

var (
	reRS       = regexp.MustCompile(`^RS(\d+)([A-Za-z])?(?:D(\d+))?$`) // RS{size}[Seed][D{delay}]
	reRSJR     = regexp.MustCompile(`^RSJR(\-?\d+)$`)
	reUpstream = regexp.MustCompile(`^U(\d+)$`)
)

func makeMockFromResponseSpec(t *testing.T, s string) mockResponse {
	s = strings.TrimSpace(s)

	// RS{size}[Seed][D{delay}] - non-empty success
	if m := reRS.FindStringSubmatch(s); m != nil {
		size := mustAtoi(t, m[1])
		seed := "A"
		if len(m) > 2 && m[2] != "" {
			seed = m[2]
		}
		mock := mockResponse{status: 200, body: jsonRpcSuccess(genDeterministicString(size, seed))}
		if len(m) > 3 && m[3] != "" {
			mock.delay = time.Duration(mustAtoi(t, m[3])) * time.Millisecond
		}
		return mock
	}

	// RSEM - empty success
	if s == "RSEM" {
		return mockResponse{status: 200, body: jsonRpcSuccess(nil)}
	}

	// RSJR{code} - JSON-RPC error
	if m := reRSJR.FindStringSubmatch(s); m != nil {
		code := mustAtoi(t, m[1])
		return mockResponse{status: 200, body: jsonRpcError(code, fmt.Sprintf("jsonrpc error %d", code))}
	}

	// RSER - infrastructure error
	if s == "RSER" {
		return makeInfraErrorMock()
	}

	// RSNE - no response (will not set up mock)
	if s == "RSNE" {
		return mockResponse{} // empty mock signals no setup
	}

	t.Fatalf("unknown response spec: %s", s)
	return mockResponse{}
}

func makeInfraErrorMock() mockResponse {
	// HTTP 500 with generic server error body
	return mockResponse{status: 500, body: map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"error": map[string]any{
			"code":    -32603,
			"message": "internal error",
		},
	}}
}

func parseExpectedOutcomeFromSpec(t *testing.T, s string, upstreamSpecs map[int]string) (*expectedResult, *expectedError) {
	s = strings.TrimSpace(s)

	// RS{size}[Seed] - expected successful result
	if m := reRS.FindStringSubmatch(s); m != nil {
		size := mustAtoi(t, m[1])
		seed := "A"
		if len(m) > 2 && m[2] != "" {
			seed = m[2]
		}
		payload := genDeterministicString(size, seed)
		return &expectedResult{jsonRpcResult: strconv.Quote(payload)}, nil
	}

	// RSEM - expected empty result
	if s == "RSEM" {
		return &expectedResult{jsonRpcResult: "null"}, nil
	}

	// RSJR{code} - expected JSON-RPC error
	if m := reRSJR.FindStringSubmatch(s); m != nil {
		code := m[1]
		// // Map common JSON-RPC codes to error codes
		// var errCode common.ErrorCode
		// switch code {
		// case 3, -32000:
		// 	errCode = common.ErrCodeEndpointServerSideException
		// default:
		// 	errCode = common.ErrCodeEndpointServerSideException
		// }
		return nil, &expectedError{contains: code}
	}

	// DISPUTE - expected dispute error
	if s == "DISPUTE" {
		return nil, &expectedError{code: common.ErrCodeConsensusDispute, contains: "not enough agreement"}
	}

	// LOWPART - expected low participants error
	if s == "LOWPART" {
		return nil, &expectedError{code: common.ErrCodeConsensusLowParticipants, contains: "not enough participants"}
	}

	// CONSENSUS - any successful consensus
	if s == "CONSENSUS" {
		return &expectedResult{}, nil // empty result means don't check specific value
	}

	// U{index} - expected result from specific upstream
	if m := reUpstream.FindStringSubmatch(s); m != nil {
		idx := mustAtoi(t, m[1])
		if spec, ok := upstreamSpecs[idx]; ok {
			// Parse the upstream's spec to determine expected result
			if specM := reRS.FindStringSubmatch(spec); specM != nil {
				size := mustAtoi(t, specM[1])
				seed := "A"
				if len(specM) > 2 && specM[2] != "" {
					seed = specM[2]
				}
				payload := genDeterministicString(size, seed)
				return &expectedResult{jsonRpcResult: strconv.Quote(payload)}, nil
			}
			if spec == "RSEM" {
				return &expectedResult{jsonRpcResult: "null"}, nil
			}
		}
		t.Fatalf("cannot determine expected result for upstream %d", idx)
	}

	t.Fatalf("unsupported expected outcome: %s", s)
	return nil, nil
}

func genDeterministicString(size int, seed string) string {
	if size <= 0 {
		return ""
	}
	ch := seed
	if ch == "" {
		ch = "A"
	}
	return strings.Repeat(ch[:1], size)
}

func mustAtoi(t *testing.T, s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatalf("invalid integer: %s", s)
	}
	return v
}

func boolPtr(v bool) *bool { return &v }
