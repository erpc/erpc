package erpc

import (
	"net/http"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	consensusPolicySettlementToken = "settlement-token"
	consensusPolicyIndexerToken    = "indexer-token"
)

// consensusPolicyYAML is the operator-facing shape of the feature, loaded
// through the real config loader so the YAML keys themselves are covered —
// a Go-struct-only test would pass even if a `yaml:` tag were wrong.
//
// The deployment: one internal node and two external providers. Callers
// prefer an answer both kinds of node agree on, but rather than hard-fail
// when the internal node is absent or wrong, an answer two independent
// externals agree on is served — labelled `degraded`, and only to callers
// whose role permits it.
const consensusPolicyYAML = `
logLevel: warn
server:
  maxTimeout: 10s
projects:
  - id: test_project
    auth:
      strategies:
        - type: secret
          consensusPolicies: ["standard"]
          secret:
            id: settlement
            value: settlement-token
        - type: secret
          consensusPolicies: ["standard", "degraded"]
          secret:
            id: indexer
            value: indexer-token
    networks:
      - architecture: evm
        evm:
          chainId: 123
        failsafe:
          - consensus:
              maxParticipants: 3
              agreementThreshold: 2
              punishMisbehavior: null
              # Generous caps so the assertions describe grading, not a race
              # with the default ~5ms straggler cap.
              maxWaitOnResult: 3s
              maxWaitOnEmpty: 3s
              acceptancePolicies:
                - name: standard
                  requiredAgreement:
                    - tag: "type:internal"
                      minAgreement: 1
                    - tag: "type:external"
                      minAgreement: 1
                - name: degraded
                  requiredAgreement:
                    - tag: "type:external"
                      minAgreement: 2
    upstreams:
      - id: internal1
        type: evm
        endpoint: http://rpc1.localhost
        tags: ["type:internal"]
        evm:
          chainId: 123
        jsonRpc:
          supportsBatch: false
      - id: external1
        type: evm
        endpoint: http://rpc2.localhost
        tags: ["type:external"]
        evm:
          chainId: 123
        jsonRpc:
          supportsBatch: false
      - id: external2
        type: evm
        endpoint: http://rpc3.localhost
        tags: ["type:external"]
        evm:
          chainId: 123
        jsonRpc:
          supportsBatch: false
rateLimiters: {}
`

// startConsensusPolicyServer boots the server from the YAML above and pins a
// deterministic upstream order, so every consensus round draws all three
// nodes instead of racing the selection policy's background evaluation.
func startConsensusPolicyServer(t *testing.T) (
	func(body string, headers map[string]string, queryParams map[string]string) (int, map[string]string, string),
	func(),
) {
	t.Helper()
	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(consensusPolicyYAML), 0o644))
	cfg, err := common.LoadConfig(fs, "erpc.yaml", &common.DefaultOptions{})
	require.NoError(t, err, "the documented YAML shape must load and validate")

	sendRequest, _, _, shutdown, erpcInstance := createServerTestFixtures(cfg, t)
	prj, err := erpcInstance.GetProject("test_project")
	require.NoError(t, err)
	policy.OverrideAllForTest(prj.policyEngine, "internal1", "external1", "external2")
	return sendRequest, shutdown
}

// mockConsensusUpstream persists a canned eth_getBlockNumber result for one
// upstream. Persisted because consensus fans out and the state poller probes
// independently; single-use mocks would race.
func mockConsensusUpstream(host, result string) {
	gock.New("http://" + host + ".localhost").
		Post("/").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getBlockNumber")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result})
}

// TestHttpServer_ConsensusAcceptancePolicies drives the whole path — YAML
// load, auth, consensus fan-out, acceptance grading, header emission — for
// the two situations that motivated the feature.
func TestHttpServer_ConsensusAcceptancePolicies(t *testing.T) {
	const body = `{"jsonrpc":"2.0","method":"eth_getBlockNumber","params":[],"id":1}`

	// The happy path: all three nodes agree, so the strict grade is met and
	// even the strictest caller is served normally.
	t.Run("StrictGradeWhenInternalAgrees", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		mockConsensusUpstream("rpc1", "0xaaaaaa")
		mockConsensusUpstream("rpc2", "0xaaaaaa")
		mockConsensusUpstream("rpc3", "0xaaaaaa")

		sendRequest, shutdown := startConsensusPolicyServer(t)
		defer shutdown()

		status, headers, respBody := sendRequest(body, map[string]string{
			"X-ERPC-Secret-Token": consensusPolicySettlementToken,
		}, nil)

		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, respBody, "0xaaaaaa")
		assert.Equal(t, "standard", headers["X-Erpc-Consensus-Policy"],
			"a mixed internal+external agreement must be served and labelled as the strict grade")
	})

	// The case André raised: the internal node is UP but serving forked
	// data while the two externals agree. A dispute-rate circuit breaker
	// cannot tell this apart from the internal node being down; here it is
	// decided from the votes of this single round.
	t.Run("RelaxedGradeWhenInternalDissents", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		mockConsensusUpstream("rpc1", "0xbbbbbb") // forked: same payload size, so this tests grading not preferLargerResponses
		mockConsensusUpstream("rpc2", "0xaaaaaa")
		mockConsensusUpstream("rpc3", "0xaaaaaa")

		sendRequest, shutdown := startConsensusPolicyServer(t)
		defer shutdown()

		// A caller whose role permits the relaxed grade is served, and can
		// see from the header that the answer is relaxed.
		t.Run("AllowedCallerIsServedAndLabelled", func(t *testing.T) {
			status, headers, respBody := sendRequest(body, map[string]string{
				"X-ERPC-Secret-Token": consensusPolicyIndexerToken,
			}, nil)

			require.Equal(t, http.StatusOK, status)
			assert.Contains(t, respBody, "0xaaaaaa",
				"two agreeing externals must resolve the round")
			assert.Equal(t, "degraded", headers["X-Erpc-Consensus-Policy"],
				"the relaxed answer must be labelled, never indistinguishable from a strict one")
		})

		// The same round, same votes, a stricter caller: withheld rather
		// than downgraded. This is what removes the need for a second
		// endpoint per policy.
		t.Run("StrictCallerIsWithheldNotDowngraded", func(t *testing.T) {
			_, headers, respBody := sendRequest(body, map[string]string{
				"X-ERPC-Secret-Token": consensusPolicySettlementToken,
			}, nil)

			// erpc reports JSON-RPC level failures in the body, so the
			// error code is the assertion that matters, not the HTTP status.
			assert.Contains(t, respBody, "ErrConsensusCompositionDispute",
				"a settlement-grade caller must be refused, not downgraded")
			assert.NotContains(t, respBody, `"result"`,
				"the relaxed answer must not leak to a caller barred from that grade")
			assert.Empty(t, headers["X-Erpc-Consensus-Policy"],
				"no grade was served to this caller")
		})
	})

	// The availability case: the internal node is unreachable entirely.
	// Recovery needs no breaker reset — the grade is recomputed per round.
	t.Run("RelaxedGradeWhenInternalUnreachable", func(t *testing.T) {
		util.ResetGock()
		defer util.ResetGock()
		util.SetupMocksForEvmStatePoller()

		gock.New("http://rpc1.localhost").
			Post("/").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), "eth_getBlockNumber")
			}).
			Reply(500).
			JSON(map[string]interface{}{"error": "internal node down"})
		mockConsensusUpstream("rpc2", "0xaaaaaa")
		mockConsensusUpstream("rpc3", "0xaaaaaa")

		sendRequest, shutdown := startConsensusPolicyServer(t)
		defer shutdown()

		status, headers, respBody := sendRequest(body, map[string]string{
			"X-ERPC-Secret-Token": consensusPolicyIndexerToken,
		}, nil)

		require.Equal(t, http.StatusOK, status)
		assert.Contains(t, respBody, "0xaaaaaa")
		assert.Equal(t, "degraded", headers["X-Erpc-Consensus-Policy"])
	})
}

// TestHttpServer_ConsensusAcceptancePolicies_ConfigRejection covers the
// startup guard for the ordering mistake that would silently defeat the
// feature: listing the relaxed grade first makes the strict grade dead code,
// so every round would be served relaxed.
func TestHttpServer_ConsensusAcceptancePolicies_ConfigRejection(t *testing.T) {
	invertedYAML := strings.Replace(consensusPolicyYAML, `
              acceptancePolicies:
                - name: standard
                  requiredAgreement:
                    - tag: "type:internal"
                      minAgreement: 1
                    - tag: "type:external"
                      minAgreement: 1
                - name: degraded
                  requiredAgreement:
                    - tag: "type:external"
                      minAgreement: 2`, `
              acceptancePolicies:
                - name: degraded
                  requiredAgreement:
                    - tag: "type:external"
                      minAgreement: 1
                - name: standard
                  requiredAgreement:
                    - tag: "type:internal"
                      minAgreement: 1
                    - tag: "type:external"
                      minAgreement: 1`, 1)
	require.NotEqual(t, consensusPolicyYAML, invertedYAML, "replacement must have applied")

	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(invertedYAML), 0o644))
	_, err := common.LoadConfig(fs, "erpc.yaml", &common.DefaultOptions{})

	require.Error(t, err, "an unreachable grade must fail startup, not serve silently")
	assert.Contains(t, err.Error(), "unreachable")
}
