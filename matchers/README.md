# Matchers Package

The matchers package provides a unified way to match and filter requests, responses, and upstreams throughout erpc.

## Features

- **Unified matching logic** for method, network, params, finality, and empty responses
- **Pattern matching** using wildcards (`*`), OR (`|`), and NOT (`!`) operators
- **Parameter matching** for complex request filtering
- **Backward compatibility** with legacy `matchMethod` and `matchFinality` fields
- **Memory efficient** design with minimal allocations

## Usage

```go
import (
    "github.com/erpc/erpc/matchers"
    "github.com/erpc/erpc/common"
)

// Create matcher configs
configs := []*common.MatcherConfig{
    {
        Method:   "eth_getLogs",
        Finality: common.DataFinalityStateUnfinalized,
        Action:   common.MatcherInclude,
    },
}

// Create a matcher
matcher := matchers.NewConfigMatcher(configs)

// Check if a request matches
result := matcher.MatchRequest("evm:1", "eth_getLogs", params, common.DataFinalityStateUnfinalized)
if result.Matched && result.Action == common.MatcherInclude {
    // Request matches and should be included
}
```

## Integration

The matchers package is integrated throughout erpc:

- **Failsafe policies**: Match requests to apply specific timeout, retry, and circuit breaker settings
- **Upstream selection**: Route requests to specific upstreams or exclude certain upstreams
- **Cache policies**: Determine which responses to cache based on method, params, and finality
- **Authentication**: Control which methods require authentication

## Configuration

See the [documentation](/config/matchers) for detailed configuration examples. 