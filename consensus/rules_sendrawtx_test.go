package consensus

import (
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	failsafeCommon "github.com/failsafe-go/failsafe-go/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// testError is a simple error for testing purposes
var testError = errors.New("test error")

// TestSendRawTransaction_ConsensusRule tests that eth_sendRawTransaction
// returns the first valid tx hash response immediately without waiting for consensus.
func TestSendRawTransaction_ConsensusRule(t *testing.T) {
	t.Run("returns first valid tx hash without requiring consensus threshold", func(t *testing.T) {
		// Create a response with a valid tx hash
		txHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
		jrpc, err := common.NewJsonRpcResponse(1, txHash, nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		// Create analysis with single non-empty response
		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2, // Normally would require 2 upstreams to agree
			},
			groups: map[string]*responseGroup{
				"hash1": {
					Hash:          "hash1",
					Count:         1, // Only 1 upstream responded
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
					Results: []*execResult{
						{Result: resp},
					},
				},
			},
			totalParticipants: 1,
			validParticipants: 1,
			method:            "eth_sendRawTransaction",
		}

		// Find and apply the eth_sendRawTransaction rule
		var matchedRule *consensusRule
		for i := range consensusRules {
			if consensusRules[i].Condition(analysis) {
				matchedRule = &consensusRules[i]
				break
			}
		}

		require.NotNil(t, matchedRule, "eth_sendRawTransaction rule should match")
		assert.Contains(t, matchedRule.Description, "eth_sendRawTransaction")

		result := matchedRule.Action(analysis)
		require.NotNil(t, result)
		require.Nil(t, result.Error, "should not return error")
		require.NotNil(t, result.Result, "should return the tx hash response")

		// Verify the response contains the tx hash
		jrr, err := result.Result.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), txHash)
	})

	t.Run("does not match for other methods", func(t *testing.T) {
		jrpc, err := common.NewJsonRpcResponse(1, "0x5", nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"hash1": {
					Hash:          "hash1",
					Count:         1,
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
				},
			},
			totalParticipants: 1,
			validParticipants: 1,
			method:            "eth_getTransactionCount", // Different method
		}

		// The eth_sendRawTransaction rule should NOT match
		ruleMatched := consensusRules[0].Condition(analysis)
		assert.False(t, ruleMatched, "eth_sendRawTransaction rule should not match for eth_getTransactionCount")
	})

	t.Run("does not match when only errors present", func(t *testing.T) {
		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"error_hash": {
					Hash:         "error_hash",
					Count:        1,
					ResponseType: ResponseTypeConsensusError, // Error, not non-empty
					FirstError:   testError,
				},
			},
			totalParticipants: 1,
			validParticipants: 0,
			method:            "eth_sendRawTransaction",
		}

		ruleMatched := consensusRules[0].Condition(analysis)
		assert.False(t, ruleMatched, "eth_sendRawTransaction rule should not match when only errors present")
	})

	t.Run("matches when there is at least one valid response among errors", func(t *testing.T) {
		txHash := "0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
		jrpc, err := common.NewJsonRpcResponse(1, txHash, nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"hash1": {
					Hash:          "hash1",
					Count:         1,
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
					Results: []*execResult{
						{Result: resp},
					},
				},
				"error_hash": {
					Hash:         "error_hash",
					Count:        2, // More errors than successes
					ResponseType: ResponseTypeConsensusError,
					FirstError:   testError,
				},
			},
			totalParticipants: 3,
			validParticipants: 3,
			method:            "eth_sendRawTransaction",
		}

		// Rule should match because we have at least one non-empty response
		ruleMatched := consensusRules[0].Condition(analysis)
		assert.True(t, ruleMatched, "eth_sendRawTransaction rule should match when there's a valid tx hash")

		// And it should return the valid response
		result := consensusRules[0].Action(analysis)
		require.NotNil(t, result)
		require.Nil(t, result.Error)
		require.NotNil(t, result.Result)
	})
}

// TestSendRawTransaction_ShortCircuitRule tests that eth_sendRawTransaction
// short-circuits as soon as one valid response is received.
func TestSendRawTransaction_ShortCircuitRule(t *testing.T) {
	t.Run("short-circuits on first valid tx hash", func(t *testing.T) {
		txHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
		jrpc, err := common.NewJsonRpcResponse(1, txHash, nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"hash1": {
					Hash:          "hash1",
					Count:         1,
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
				},
			},
			totalParticipants: 1,
			validParticipants: 1,
			method:            "eth_sendRawTransaction",
		}

		winner := &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
			Result: resp,
		}

		// Find and test the eth_sendRawTransaction short-circuit rule (should be first)
		shortCircuitRule := shortCircuitRules[0]
		assert.Contains(t, shortCircuitRule.Description, "eth_sendRawTransaction")
		assert.Equal(t, "sendrawtx_first_success", shortCircuitRule.Reason)

		shouldShortCircuit := shortCircuitRule.Condition(winner, analysis)
		assert.True(t, shouldShortCircuit, "should short-circuit on first valid tx hash")
	})

	t.Run("does not short-circuit for other methods", func(t *testing.T) {
		jrpc, err := common.NewJsonRpcResponse(1, "0x5", nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"hash1": {
					Hash:          "hash1",
					Count:         1,
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
				},
			},
			totalParticipants: 1,
			validParticipants: 1,
			method:            "eth_getTransactionCount",
		}

		winner := &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
			Result: resp,
		}

		// The eth_sendRawTransaction short-circuit rule should NOT match
		shortCircuitRule := shortCircuitRules[0]
		shouldShortCircuit := shortCircuitRule.Condition(winner, analysis)
		assert.False(t, shouldShortCircuit, "should not short-circuit for non-sendRawTransaction methods")
	})

	t.Run("does not short-circuit when only errors present", func(t *testing.T) {
		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"error_hash": {
					Hash:         "error_hash",
					Count:        1,
					ResponseType: ResponseTypeInfrastructureError,
					FirstError:   testError,
				},
			},
			totalParticipants: 1,
			validParticipants: 0,
			method:            "eth_sendRawTransaction",
		}

		winner := &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
			Error: testError,
		}

		shortCircuitRule := shortCircuitRules[0]
		shouldShortCircuit := shortCircuitRule.Condition(winner, analysis)
		assert.False(t, shouldShortCircuit, "should not short-circuit when only errors present")
	})

	t.Run("short-circuits even with mixed responses (valid + errors)", func(t *testing.T) {
		txHash := "0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
		jrpc, err := common.NewJsonRpcResponse(1, txHash, nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"hash1": {
					Hash:          "hash1",
					Count:         1,
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
				},
				"error_hash": {
					Hash:         "error_hash",
					Count:        1,
					ResponseType: ResponseTypeConsensusError,
					FirstError:   testError,
				},
			},
			totalParticipants: 2,
			validParticipants: 2,
			method:            "eth_sendRawTransaction",
		}

		winner := &failsafeCommon.PolicyResult[*common.NormalizedResponse]{
			Result: resp,
		}

		shortCircuitRule := shortCircuitRules[0]
		shouldShortCircuit := shortCircuitRule.Condition(winner, analysis)
		assert.True(t, shouldShortCircuit, "should short-circuit when there's at least one valid tx hash")
	})
}

// TestSendRawTransaction_RulePriority tests that the eth_sendRawTransaction
// rule takes precedence over other consensus rules.
func TestSendRawTransaction_RulePriority(t *testing.T) {
	t.Run("eth_sendRawTransaction rule is evaluated first", func(t *testing.T) {
		// Verify the eth_sendRawTransaction consensus rule is first
		assert.Contains(t, consensusRules[0].Description, "eth_sendRawTransaction",
			"eth_sendRawTransaction consensus rule should be first in the rules list")

		// Verify the eth_sendRawTransaction short-circuit rule is first
		assert.Contains(t, shortCircuitRules[0].Description, "eth_sendRawTransaction",
			"eth_sendRawTransaction short-circuit rule should be first in the rules list")
	})

	t.Run("returns tx hash even when error consensus would normally win", func(t *testing.T) {
		// Scenario: 2 upstreams return error, 1 returns valid tx hash
		// Normal consensus would pick the error (2 vs 1)
		// But eth_sendRawTransaction should return the valid tx hash

		txHash := "0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
		jrpc, err := common.NewJsonRpcResponse(1, txHash, nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"tx_hash": {
					Hash:          "tx_hash",
					Count:         1, // Minority
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: resp,
					Results: []*execResult{
						{Result: resp},
					},
				},
				"error_hash": {
					Hash:         "error_hash",
					Count:        2, // Majority (meets threshold)
					ResponseType: ResponseTypeConsensusError,
					FirstError:   testError,
					Results: []*execResult{
						{Err: testError},
						{Err: testError},
					},
				},
			},
			totalParticipants: 3,
			validParticipants: 3,
			method:            "eth_sendRawTransaction",
		}

		// The eth_sendRawTransaction rule should match first
		assert.True(t, consensusRules[0].Condition(analysis),
			"eth_sendRawTransaction rule should match")

		// And return the tx hash, not the error
		result := consensusRules[0].Action(analysis)
		require.NotNil(t, result)
		assert.Nil(t, result.Error, "should not return error")
		assert.NotNil(t, result.Result, "should return the tx hash response")
	})
}

// TestSendRawTransaction_EmptyResponse tests edge cases with empty responses.
func TestSendRawTransaction_EmptyResponse(t *testing.T) {
	t.Run("does not match for empty responses", func(t *testing.T) {
		// Empty response (e.g., null result)
		jrpc, err := common.NewJsonRpcResponse(1, nil, nil)
		require.NoError(t, err)

		resp := common.NewNormalizedResponse()
		resp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"empty_hash": {
					Hash:          "empty_hash",
					Count:         1,
					ResponseType:  ResponseTypeEmpty, // Empty, not non-empty
					LargestResult: resp,
				},
			},
			totalParticipants: 1,
			validParticipants: 1,
			method:            "eth_sendRawTransaction",
		}

		ruleMatched := consensusRules[0].Condition(analysis)
		assert.False(t, ruleMatched, "eth_sendRawTransaction rule should not match for empty responses")
	})
}

// TestSendRawTransaction_Integration tests the complete flow from analysis to result.
func TestSendRawTransaction_Integration(t *testing.T) {
	t.Run("complete flow: single success among multiple participants", func(t *testing.T) {
		// Setup: 3 participants, only 1 returns success
		txHash := "0xdeadbeef1234567890abcdef1234567890abcdef1234567890abcdef12345678"
		jrpc, err := common.NewJsonRpcResponse(1, txHash, nil)
		require.NoError(t, err)

		successResp := common.NewNormalizedResponse()
		successResp.WithJsonRpcResponse(jrpc)

		analysis := &consensusAnalysis{
			config: &config{
				maxParticipants:    3,
				agreementThreshold: 2,
			},
			groups: map[string]*responseGroup{
				"success": {
					Hash:          "success_hash",
					Count:         1,
					ResponseType:  ResponseTypeNonEmpty,
					LargestResult: successResp,
					Results: []*execResult{
						{Result: successResp},
					},
				},
				"infra_error": {
					Hash:         "infra_error_hash",
					Count:        2,
					ResponseType: ResponseTypeInfrastructureError,
					FirstError:   testError,
				},
			},
			totalParticipants: 3,
			validParticipants: 1, // Only 1 valid (non-infra-error)
			method:            "eth_sendRawTransaction",
		}

		// Run through rules to find match
		var result *failsafeCommon.PolicyResult[*common.NormalizedResponse]
		for _, rule := range consensusRules {
			if rule.Condition(analysis) {
				result = rule.Action(analysis)
				break
			}
		}

		require.NotNil(t, result, "should find a matching rule")
		require.Nil(t, result.Error, "should succeed")
		require.NotNil(t, result.Result, "should have result")

		jrr, err := result.Result.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), txHash)
	})
}
