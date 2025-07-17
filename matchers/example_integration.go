package matchers

import (
	"context"

	"github.com/erpc/erpc/common"
)

// Example of how to use matchers in upstream selection
func ExampleUpstreamMatching(req *common.NormalizedRequest, upstreamConfigs []*common.UpstreamConfig) []string {
	method, _ := req.Method()
	finality := req.Finality(context.Background())

	// Get params from JsonRpcRequest
	var params []interface{}
	jrpcReq, _ := req.JsonRpcRequest(context.Background())
	if jrpcReq != nil {
		params = jrpcReq.Params
	}

	networkId := req.NetworkId()

	var allowedUpstreams []string

	for _, upstream := range upstreamConfigs {
		// Check each failsafe config to see if it matches
		for _, failsafe := range upstream.Failsafe {
			// Convert legacy config if needed
			ConvertLegacyFailsafeConfig(failsafe)

			// Create matcher from failsafe config
			matcher := NewMatcher(failsafe.Matchers)

			// Check if request matches
			result := matcher.MatchRequest(networkId, method, params, finality)

			if result.Matched {
				if result.Action == common.MatcherInclude {
					// This upstream can handle this request
					allowedUpstreams = append(allowedUpstreams, upstream.Id)
				} else if result.Action == common.MatcherExclude {
					// This upstream should not handle this request
					// Skip to next upstream
					break
				}
			}
		}
	}

	return allowedUpstreams
}

// Example of how to use matchers for cache decisions
func ExampleCacheMatching(networkId, method string, params []interface{}, finality common.DataFinalityState, isEmptyish bool, cacheConfigs []*common.MatcherConfig) bool {
	matcher := NewMatcher(cacheConfigs)
	result := matcher.MatchForCache(networkId, method, params, finality, isEmptyish)

	// Only cache if matched with include action
	return result.Matched && result.Action == common.MatcherInclude
}

// Example of how to use matchers for authentication
func ExampleAuthMatching(method string, authConfigs []*common.MatcherConfig) bool {
	matcher := NewMatcher(authConfigs)
	result := matcher.MatchRequest("", method, nil, 0)

	// Require auth if matched with include action
	return result.Matched && result.Action == common.MatcherInclude
}
