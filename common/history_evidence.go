package common

import (
	"context"
	"strings"
)

// HistoryEvidence is what an architecture tells the consensus history waivers
// (waiveAgreementOnEmptyOutsideRetention, waiveAgreementOnMissingData) about
// where data sits on its chain. A block is the chain's height unit: an EVM
// block number, an SVM slot. Every method fails closed: it answers 0, false or
// "" when it cannot tell, and the waiver then holds the quota.
type HistoryEvidence interface {
	// RequestedBlock is the highest block the request's own params name. It
	// never reads an answer: agreeing upstreams could fabricate one.
	RequestedBlock(ctx context.Context, req *NormalizedRequest) (int64, bool)

	// CorroboratedHead is the network's latest block when a quorum of its
	// upstreams backs it, so one inflating upstream cannot raise it; else 0.
	CorroboratedHead(ctx context.Context, req *NormalizedRequest) int64

	// UpstreamLatestBlock is u's own polled latest block, capped by its
	// declared availability, or 0 when unknown.
	UpstreamLatestBlock(u Upstream) int64

	// UpstreamLowerBound is the lowest block u is declared to serve, or
	// math.MinInt64 when unbounded.
	UpstreamLowerBound(u Upstream) int64

	// LookupSubject is the id a lookup request names (a tx hash, a block hash,
	// a tx signature) when the method's answer names the block that holds
	// it, or "" otherwise.
	LookupSubject(ctx context.Context, req *NormalizedRequest) string

	// AnswerBlock is the block an answer to req names, and that block's own
	// id when the chain's answer carries one (else ""). number is 0 when the
	// answer names no block or is malformed.
	AnswerBlock(ctx context.Context, req *NormalizedRequest, resp *NormalizedResponse) (number int64, blockId string)

	// ProbeBlock fetches block n straight from u, never through consensus or
	// the network's gates: the block's id and the lookup ids it holds (tx
	// hashes, signatures). ok is false when u does not serve block n.
	ProbeBlock(ctx context.Context, u Upstream, n int64) (blockId string, subjects []string, ok bool)
}

var historyEvidence = map[NetworkArchitecture]HistoryEvidence{}

// RegisterHistoryEvidence installs arch's HistoryEvidence. Call it from init.
func RegisterHistoryEvidence(arch NetworkArchitecture, h HistoryEvidence) {
	historyEvidence[arch] = h
}

// HistoryEvidenceOfNetworkId returns the HistoryEvidence of the architecture
// a network id names by its prefix ("evm:1", "svm:mainnet"), or nil.
func HistoryEvidenceOfNetworkId(networkId string) HistoryEvidence {
	arch, _, ok := strings.Cut(networkId, ":")
	if !ok {
		return nil
	}
	return historyEvidence[NetworkArchitecture(arch)]
}
