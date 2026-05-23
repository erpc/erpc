package clients

import (
	"context"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// TestBuildTopicFiltersPreservesNullPositions guards against a regression where
// a null wildcard at a non-terminal topic position was silently dropped, which
// collapsed later filters into earlier positions and caused eth_getLogs to
// return zero results for valid queries (e.g. viem's
// getLogs({event, args:{to:[addr]}}) which encodes as [selector, null, to]).
// Empty TopicFilter Values is the proto-level wildcard at that position.
func TestBuildTopicFiltersPreservesNullPositions(t *testing.T) {
	const (
		transferSig = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
		from        = "0x000000000000000000000000b92fe925dc43a0ecde6c8b1a2709c170ec4fff4f"
		to          = "0x0000000000000000000000008ca997c0e5d38cf34ecb061f5374aead7728d86b"
		otherTo     = "0x0000000000000000000000001111111111111111111111111111111111111111"
	)

	tests := []struct {
		name        string
		topics      interface{}
		wantLengths []int // expected len(Values) per position; -1 means must be present (wildcard)
	}{
		{
			name:        "nil topics param",
			topics:      nil,
			wantLengths: nil,
		},
		{
			name:        "trailing null is preserved as wildcard",
			topics:      []interface{}{transferSig, nil},
			wantLengths: []int{1, 0},
		},
		{
			name:        "leading null is preserved as wildcard",
			topics:      []interface{}{nil, to},
			wantLengths: []int{0, 1},
		},
		{
			name:        "null in middle does not collapse subsequent positions",
			topics:      []interface{}{transferSig, nil, to},
			wantLengths: []int{1, 0, 1},
		},
		{
			name:        "null in middle followed by array value",
			topics:      []interface{}{transferSig, nil, []interface{}{to, otherTo}},
			wantLengths: []int{1, 0, 2},
		},
		{
			name:        "array of OR values at position",
			topics:      []interface{}{transferSig, []interface{}{from, to}},
			wantLengths: []int{1, 2},
		},
		{
			name:        "all nulls preserved",
			topics:      []interface{}{nil, nil, nil},
			wantLengths: []int{0, 0, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			filters, err := buildTopicFilters(tc.topics)
			require.NoError(t, err)
			require.Equal(t, len(tc.wantLengths), len(filters),
				"positional alignment lost: a null entry was silently dropped")
			for i, want := range tc.wantLengths {
				require.NotNil(t, filters[i], "position %d must be present (wildcard or filter)", i)
				require.Equal(t, want, len(filters[i].Values),
					"position %d: expected %d values, got %d", i, want, len(filters[i].Values))
			}
		})
	}
}

func TestBuildTopicFiltersRejectsInvalidHex(t *testing.T) {
	_, err := buildTopicFilters([]interface{}{"not-hex"})
	require.Error(t, err)
}

// TestGrpcBdsClientQueryMethodsDoNotShortCircuit verifies that query methods
// are routed to the streaming QueryService handlers rather than being
// rejected outright by SendRequest. With no live queryClient wired in, the
// handler surfaces a clear error — but critically NOT ErrEndpointUnsupported
// which would disqualify the upstream from carrying eth_query* traffic.
func TestGrpcBdsClientQueryMethodsDoNotShortCircuit(t *testing.T) {
	parsedURL, err := url.Parse("grpc://localhost:0")
	require.NoError(t, err)

	client := &GenericGrpcBdsClient{Url: parsedURL}
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_queryBlocks","params":[{"fromBlock":"0x1","toBlock":"0x2","limit":1}]}`))

	_, err = client.SendRequest(context.Background(), req)
	require.Error(t, err)
	require.False(
		t,
		common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
		"query methods must not be short-circuited as unsupported at SendRequest level; error was: %v",
		err,
	)
}
