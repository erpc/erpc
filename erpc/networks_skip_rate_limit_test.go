package erpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShouldSkipNetworkRateLimit(t *testing.T) {
	assert.False(t, shouldSkipNetworkRateLimit(nil))
	assert.False(t, shouldSkipNetworkRateLimit(context.Background()))

	skipCtx := withSkipNetworkRateLimit(context.Background())
	assert.True(t, shouldSkipNetworkRateLimit(skipCtx))

	wrongTypeCtx := context.WithValue(context.Background(), skipNetworkRateLimitKey{}, "no")
	assert.False(t, shouldSkipNetworkRateLimit(wrongTypeCtx))
}
