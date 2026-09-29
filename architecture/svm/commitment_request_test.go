package svm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

func init() {
	util.ConfigureTestLogger()
}

func TestExtractRequestedCommitment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name string
		body string
		want common.CommitmentLevel
	}{
		{
			name: "omitted params object",
			body: `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111"]}`,
			want: common.CommitmentNone,
		},
		{
			name: "options without commitment",
			body: `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111",{"encoding":"base64"}]}`,
			want: common.CommitmentNone,
		},
		{
			name: "object commitment finalized",
			body: `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111",{"commitment":"finalized"}]}`,
			want: common.CommitmentFinalized,
		},
		{
			name: "object commitment confirmed case-insensitive",
			body: `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"Confirmed"}]}`,
			want: common.CommitmentConfirmed,
		},
		{
			name: "object commitment processed",
			body: `{"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":["pk",{"commitment":"processed","encoding":"jsonParsed"}]}`,
			want: common.CommitmentProcessed,
		},
		{
			name: "unsupported commitment is unknown not none",
			body: `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111",{"commitment":"recent"}]}`,
			want: common.CommitmentUnknown,
		},
		{
			name: "non-string commitment is unknown",
			body: `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111",{"commitment":1}]}`,
			want: common.CommitmentUnknown,
		},
		{
			name: "empty commitment string is none",
			body: `{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111",{"commitment":""}]}`,
			want: common.CommitmentNone,
		},
		{
			name: "empty commitment stops positional override",
			body: `{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pk",{"commitment":""},"finalized"]}`,
			want: common.CommitmentNone,
		},
		{
			name: "null commitment stops positional override",
			body: `{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pk",{"commitment":null},"confirmed"]}`,
			want: common.CommitmentNone,
		},
		{
			name: "preflightCommitment alone is none",
			body: `{"jsonrpc":"2.0","id":1,"method":"sendTransaction","params":["tx",{"preflightCommitment":"confirmed","skipPreflight":false}]}`,
			want: common.CommitmentNone,
		},
		{
			name: "preflightCommitment does not override commitment",
			body: `{"jsonrpc":"2.0","id":1,"method":"simulateTransaction","params":["tx",{"commitment":"processed","preflightCommitment":"finalized"}]}`,
			want: common.CommitmentProcessed,
		},
		{
			name: "positional commitment on getSignaturesForAddress",
			body: `{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pk",{"limit":1000},"confirmed"]}`,
			want: common.CommitmentConfirmed,
		},
		{
			name: "positional commitment only",
			body: `{"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":["pk","finalized"]}`,
			want: common.CommitmentFinalized,
		},
		{
			name: "encoding string is not commitment",
			body: `{"jsonrpc":"2.0","id":1,"method":"getBlock","params":[42,"base64"]}`,
			want: common.CommitmentNone,
		},
		{
			name: "object commitment wins over trailing encoding string",
			body: `{"jsonrpc":"2.0","id":1,"method":"getTransaction","params":["sig",{"commitment":"confirmed","encoding":"json"}]}`,
			want: common.CommitmentConfirmed,
		},
		{
			name: "getSlot options at index 0",
			body: `{"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}`,
			want: common.CommitmentProcessed,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := common.NewNormalizedRequest([]byte(tc.body))
			got := ExtractRequestedCommitment(ctx, req)
			if got != tc.want {
				t.Fatalf("ExtractRequestedCommitment = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCaptureRequestedCommitment_SurvivesInjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111"]}`,
	))
	h := &SvmArchitectureHandler{}
	if _, _, err := h.HandleProjectPreForward(ctx, net, req); err != nil {
		t.Fatalf("HandleProjectPreForward: %v", err)
	}

	got, ok := req.RequestedCommitment()
	if !ok {
		t.Fatal("expected requested commitment to be memoized")
	}
	if got != common.CommitmentNone {
		t.Fatalf("omitted request must stay none after injection, got %q", got)
	}

	// Params were injected — extractor on mutated params would see confirmed.
	if after := ExtractRequestedCommitment(ctx, req); after != common.CommitmentConfirmed {
		t.Fatalf("post-injection extract should see injected confirmed, got %q", after)
	}
}

func TestCaptureRequestedCommitment_ExplicitWinsOverNetworkDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := &fakeNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureSvm,
		Svm:          &common.SvmNetworkConfig{Commitment: "confirmed"},
	}}
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"getBalance","params":["So1111",{"commitment":"finalized"}]}`,
	))
	h := &SvmArchitectureHandler{}
	if _, _, err := h.HandleProjectPreForward(ctx, net, req); err != nil {
		t.Fatalf("HandleProjectPreForward: %v", err)
	}
	got, ok := req.RequestedCommitment()
	if !ok || got != common.CommitmentFinalized {
		t.Fatalf("got (%q, %v), want (finalized, true)", got, ok)
	}
}
