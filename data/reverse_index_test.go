package data

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReverseIndexWildcardKey(t *testing.T) {
	t.Parallel()

	ctx := WithReverseIndexWildcard(context.Background(), "evm:998:systx:*")
	got, ok := reverseIndexWildcardKey(ctx, "evm:998:systx:foo:bar")
	require.True(t, ok)
	assert.Equal(t, "evm:998:systx:*", got, "explicit wildcard must be used even when the ref contains ':'")

	_, ok = reverseIndexWildcardKey(context.Background(), "evm:998:foo:bar")
	assert.False(t, ok, "without WithReverseIndexWildcard, do not infer from the opaque partition key")

	_, ok = reverseIndexWildcardKey(nil, "evm:998:64321354")
	assert.False(t, ok)

	ctx = WithReverseIndex(context.Background(), "evm:998", "systx")
	got, ok = reverseIndexWildcardKey(ctx, "evm:998:systx:64321354")
	require.True(t, ok)
	assert.Equal(t, "evm:998:systx:*", got)

	ctx = WithReverseIndex(context.Background(), "evm:998", "")
	got, ok = reverseIndexWildcardKey(ctx, "evm:998:64321354")
	require.True(t, ok)
	assert.Equal(t, "evm:998:*", got)
}
