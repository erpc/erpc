package erpc

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildErrorResponseBody_SvmRateLimitWireCodes(t *testing.T) {
	authRateLimitErr := common.NewErrAuthRateLimitRuleExceeded(
		"project-1", "ip", "requests-per-minute", "100/min", "user-1", "127.0.0.1",
	)
	projectRateLimitErr := common.NewErrProjectRateLimitRuleExceeded("project-1", "requests-per-minute", "method:getBalance")
	networkRateLimitErr := common.NewErrNetworkRateLimitRuleExceeded("project-1", "svm:mainnet-beta", "requests-per-minute", "method:getBalance")
	upstreamRateLimitErr := common.NewErrUpstreamRateLimitRuleExceeded("upstream-1", "requests-per-minute", "method:getBalance")
	nodeUnhealthyErr := common.NewErrEndpointServerSideException(
		common.NewErrJsonRpcExceptionInternal(
			-32005,
			common.JsonRpcErrorNumber(-32005),
			"Node is unhealthy",
			nil,
			nil,
		),
		nil,
		500,
	)

	for _, tc := range []struct {
		name        string
		networkID   string
		err         error
		wantCode    common.JsonRpcErrorNumber
		wantMessage string
		wantCause   common.ErrorCode
	}{
		{
			name:        "local auth rate limit on SVM uses generic server error code",
			networkID:   "svm:mainnet-beta",
			err:         authRateLimitErr,
			wantCode:    -32000,
			wantMessage: "rate-limit exceeded",
			wantCause:   common.ErrCodeAuthRateLimitRuleExceeded,
		},
		{
			name:        "local project rate limit on SVM uses generic server error code",
			networkID:   "svm:mainnet-beta",
			err:         projectRateLimitErr,
			wantCode:    -32000,
			wantMessage: "rate-limit exceeded",
			wantCause:   common.ErrCodeProjectRateLimitRuleExceeded,
		},
		{
			name:        "local network rate limit on SVM uses generic server error code",
			networkID:   "svm:mainnet-beta",
			err:         networkRateLimitErr,
			wantCode:    -32000,
			wantMessage: "rate-limit exceeded",
			wantCause:   common.ErrCodeNetworkRateLimitRuleExceeded,
		},
		{
			name:        "local upstream rate limit on SVM uses generic server error code",
			networkID:   "svm:mainnet-beta",
			err:         upstreamRateLimitErr,
			wantCode:    -32000,
			wantMessage: "rate-limit exceeded",
			wantCause:   common.ErrCodeUpstreamRateLimitRuleExceeded,
		},
		{
			name:        "local auth rate limit on EVM retains capacity exceeded code",
			networkID:   "evm:1",
			err:         authRateLimitErr,
			wantCode:    common.JsonRpcErrorCapacityExceeded,
			wantMessage: "rate-limit exceeded",
			wantCause:   common.ErrCodeAuthRateLimitRuleExceeded,
		},
		{
			name:        "native SVM NodeUnhealthy code passes through",
			networkID:   "svm:mainnet-beta",
			err:         nodeUnhealthyErr,
			wantCode:    -32005,
			wantMessage: "Node is unhealthy",
			wantCause:   common.ErrCodeEndpointServerSideException,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":[]}`))
			req.SetNetwork(&Network{networkId: tc.networkID})

			body := buildErrorResponseBody(req, tc.err, tc.err, nil)
			response, ok := body.(*HttpJsonRpcErrorResponse)
			require.Truef(t, ok, "expected HttpJsonRpcErrorResponse, got %T", body)
			errObject, ok := response.Error.(map[string]interface{})
			require.Truef(t, ok, "expected JSON-RPC error object, got %T", response.Error)

			assert.EqualValuesf(t, tc.wantCode, errObject["code"], "got %v want %v", errObject["code"], tc.wantCode)
			require.Equal(t, tc.wantMessage, errObject["message"])
			require.ErrorIs(t, response.Cause, tc.err)
			require.Truef(t, common.HasErrorCode(response.Cause, tc.wantCause), "response cause lost %s: %v", tc.wantCause, response.Cause)
		})
	}
}

func TestProcessErrorBody_AuthRateLimitBeforeNetworkResolution(t *testing.T) {
	authRateLimitErr := common.NewErrAuthRateLimitRuleExceeded(
		"project-1", "ip", "requests-per-minute", "100/min", "user-1", "127.0.0.1",
	)

	for _, tc := range []struct {
		name      string
		body      string
		networkID string
		hints     []common.NetworkArchitecture
		wantCode  common.JsonRpcErrorNumber
	}{
		{
			name:     "URL SVM hint",
			body:     `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":[]}`,
			hints:    []common.NetworkArchitecture{common.ArchitectureSvm},
			wantCode: -32000,
		},
		{
			name:     "body-routed SVM networkId",
			body:     `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":[],"networkId":"svm:mainnet-beta"}`,
			wantCode: -32000,
		},
		{
			name:     "URL EVM hint",
			body:     `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`,
			hints:    []common.NetworkArchitecture{common.ArchitectureEvm},
			wantCode: common.JsonRpcErrorCapacityExceeded,
		},
		{
			name:      "resolved EVM overrides contradictory body SVM networkId",
			body:      `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[],"networkId":"svm:mainnet-beta"}`,
			networkID: "evm:1",
			wantCode:  common.JsonRpcErrorCapacityExceeded,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := common.NewNormalizedRequest([]byte(tc.body))
			if tc.networkID == "" {
				require.Nil(t, req.Network(), "auth runs before network resolution")
			} else {
				req.SetNetwork(&Network{networkId: tc.networkID})
				require.NotNil(t, req.Network(), "test requires a resolved network")
			}

			logger := zerolog.Nop()
			startedAt := time.Now()
			body := processErrorBody(&logger, &startedAt, req, authRateLimitErr, nil, tc.hints...)

			response, ok := body.(*HttpJsonRpcErrorResponse)
			require.Truef(t, ok, "expected HttpJsonRpcErrorResponse, got %T", body)
			errObject, ok := response.Error.(map[string]interface{})
			require.Truef(t, ok, "expected JSON-RPC error object, got %T", response.Error)
			assert.EqualValues(t, tc.wantCode, errObject["code"])
			require.Equal(t, "rate-limit exceeded", errObject["message"])
			require.ErrorIs(t, response.Cause, authRateLimitErr)
			require.True(t, common.HasErrorCode(response.Cause, common.ErrCodeAuthRateLimitRuleExceeded))
		})
	}
}
