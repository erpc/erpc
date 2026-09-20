package svm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
)

func matchKeyFor(t *testing.T, rawReq string) string {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(rawReq))
	resp := common.NewNormalizedResponse().WithRequest(req)
	return CommitmentMatchKey(context.Background(), resp)
}

func TestCommitmentClassForMethod(t *testing.T) {
	t.Parallel()
	cases := []struct {
		method string
		want   CommitmentClass
	}{
		// Standard: full three-level commitment.
		{"getAccountInfo", CommitmentClassStandard},
		{"getBalance", CommitmentClassStandard},
		{"getBlockHeight", CommitmentClassStandard},
		{"getEpochInfo", CommitmentClassStandard},
		{"getLatestBlockhash", CommitmentClassStandard},
		{"getProgramAccounts", CommitmentClassStandard},
		{"getSlot", CommitmentClassStandard},
		{"getTokenAccountBalance", CommitmentClassStandard},
		{"getVoteAccounts", CommitmentClassStandard},
		{"simulateTransaction", CommitmentClassStandard},
		// At-least-confirmed: agave rejects processed (-32602).
		{"getBlock", CommitmentClassAtLeastConfirmed},
		{"getConfirmedBlock", CommitmentClassAtLeastConfirmed},
		{"getBlocks", CommitmentClassAtLeastConfirmed},
		{"getBlocksWithLimit", CommitmentClassAtLeastConfirmed},
		{"getSignaturesForAddress", CommitmentClassAtLeastConfirmed},
		{"getTransaction", CommitmentClassAtLeastConfirmed},
		// None: no commitment parameter on the wire.
		{"getBlockCommitment", CommitmentClassNone},
		{"getBlockTime", CommitmentClassNone},
		{"getClusterNodes", CommitmentClassNone},
		{"getEpochSchedule", CommitmentClassNone},
		{"getGenesisHash", CommitmentClassNone},
		{"getHealth", CommitmentClassNone},
		{"getIdentity", CommitmentClassNone},
		{"getInflationRate", CommitmentClassNone},
		{"getMinimumLedgerSlot", CommitmentClassNone},
		{"getRecentPerformanceSamples", CommitmentClassNone},
		{"getSignatureStatuses", CommitmentClassNone},
		{"getVersion", CommitmentClassNone},
		{"sendTransaction", CommitmentClassNone},
		{"sendRawTransaction", CommitmentClassNone},
		// Unknown methods default to the conservative standard class.
		{"getSomethingNew", CommitmentClassStandard},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()
			if got := CommitmentClassForMethod(tc.method); got != tc.want {
				t.Fatalf("CommitmentClassForMethod(%q) = %v, want %v", tc.method, got, tc.want)
			}
		})
	}
}

func TestCommitmentMatchKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  string
		want string
	}{
		{
			name: "standard method with explicit finalized",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pk",{"commitment":"finalized"}]}`,
			want: "finalized",
		},
		{
			name: "standard method with explicit processed",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pk",{"commitment":"processed"}]}`,
			want: "processed",
		},
		{
			name: "standard method unpinned gets its own bucket",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getEpochInfo","params":[]}`,
			want: "unpinned",
		},
		{
			name: "standard method with unrelated options stays unpinned",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pk",{"encoding":"base64"}]}`,
			want: "unpinned",
		},
		{
			name: "legacy alias recent normalizes to processed",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["pk",{"commitment":"recent"}]}`,
			want: "processed",
		},
		{
			name: "legacy alias root normalizes to finalized",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"root"}]}`,
			want: "finalized",
		},
		{
			name: "legacy alias singleGossip normalizes to confirmed",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"singleGossip"}]}`,
			want: "confirmed",
		},
		{
			name: "at-least-confirmed clamps processed to confirmed",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[123,{"commitment":"processed"}]}`,
			want: "confirmed",
		},
		{
			name: "at-least-confirmed keeps finalized",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getTransaction","params":["sig",{"commitment":"finalized"}]}`,
			want: "finalized",
		},
		{
			name: "at-least-confirmed unpinned floors at confirmed",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[123]}`,
			want: "confirmed",
		},
		{
			name: "no-commitment method returns empty key",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getBlockTime","params":[123]}`,
			want: "",
		},
		{
			name: "no-commitment method ignores smuggled commitment",
			req:  `{"jsonrpc":"2.0","id":1,"method":"getVersion","params":[{"commitment":"finalized"}]}`,
			want: "",
		},
		{
			name: "broadcast returns empty key",
			req:  `{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["tx",{"preflightCommitment":"processed"}]}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := matchKeyFor(t, tc.req); got != tc.want {
				t.Fatalf("CommitmentMatchKey(%s) = %q, want %q", tc.req, got, tc.want)
			}
		})
	}
}

func TestCommitmentMatchKey_NilSafety(t *testing.T) {
	t.Parallel()
	if got := CommitmentMatchKey(context.Background(), nil); got != "" {
		t.Fatalf("nil response must yield empty key, got %q", got)
	}
	if got := CommitmentMatchKey(context.Background(), common.NewNormalizedResponse()); got != "" {
		t.Fatalf("response without request must yield empty key, got %q", got)
	}
	if got := CommitmentMatchKey(context.Background(), common.NewNormalizedResponse().WithRequest(common.NewNormalizedRequest([]byte(`not json`)))); got != "" {
		t.Fatalf("unparseable request must yield empty key, got %q", got)
	}
}

func TestNormalizeCommitmentLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"processed":    "processed",
		"PROCESSED":    "processed",
		"recent":       "processed",
		"confirmed":    "confirmed",
		"single":       "confirmed",
		"singleGossip": "confirmed",
		"singlegossip": "confirmed",
		"finalized":    "finalized",
		"root":         "finalized",
		"max":          "finalized",
		"bogus":        "bogus",
	}
	for in, want := range cases {
		if got := normalizeCommitmentLevel(in); got != want {
			t.Fatalf("normalizeCommitmentLevel(%q) = %q, want %q", in, got, want)
		}
	}
}
