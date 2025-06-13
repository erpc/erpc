package erpc

import (
	"context"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
)

// runConsensus executes the consensus flow when it is configured at network level.
// It receives the pre-sorted upstream list (highest score first).  It returns a
// response that satisfies the configured agreement threshold, or an error if no
// consensus could be reached.
func (n *Network) runConsensus(ctx context.Context, req *common.NormalizedRequest, upsList []common.Upstream) (*common.NormalizedResponse, error) {
	if n.cfg == nil || n.cfg.Failsafe == nil || n.cfg.Failsafe.Consensus == nil {
		return nil, nil // consensus not configured – caller should continue normal flow
	}

	cfg := n.cfg.Failsafe.Consensus

	required := cfg.RequiredParticipants
	if required <= 0 {
		required = 2
	}
	threshold := cfg.AgreementThreshold
	if threshold <= 0 {
		threshold = required // unanimous by default
	}

	// Determine how many upstreams we can actually use based on configured behaviour.
	available := len(upsList)
	if available < required {
		switch cfg.LowParticipantsBehavior {
		case common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
			common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader:
			// Proceed with all available upstreams – consensus will later fall back to
			// block-head leader logic if agreement cannot be achieved.
			required = available
		default:
			return nil, common.NewErrConsensusLowParticipants("not enough upstreams available")
		}
	}

	selected := upsList[:required]

	// For behaviours that care about block-head leader, ensure it is included in the set.
	if cfg.LowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader || cfg.LowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader {
		leader := findBlockHeadLeader(upsList)
		if leader != nil {
			// Ensure leader is part of selected slice
			found := false
			for _, u := range selected {
				if u == leader {
					found = true
					break
				}
			}
			if !found {
				if cfg.LowParticipantsBehavior == common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader {
					selected = []common.Upstream{leader}
					required = 1
				} else { // prefer leader -> swap last
					selected[len(selected)-1] = leader
				}
			}
		}
	}

	// Collect results in parallel
	type result struct {
		resp *common.NormalizedResponse
		err  error
		hash string
		up   common.Upstream
	}
	results := make([]*result, required)
	wg := sync.WaitGroup{}
	wg.Add(required)

	for i, up := range selected {
		i, up := i, up // capture
		go func() {
			defer wg.Done()

			// Clone request and pin upstream
			clone := common.NewNormalizedRequest(req.Body())
			clone.SetNetwork(n)
			clone.SetCacheDal(req.CacheDal())
			if dr := req.Directives(); dr != nil {
				clone.SetDirectives(dr.Clone())
			}
			dirs := clone.Directives()
			if dirs == nil {
				dirs = &common.RequestDirectives{}
				clone.SetDirectives(dirs)
			}
			dirs.UseUpstream = up.Id()

			// Skip cache read so each upstream is queried directly for consensus accuracy
			resp, err := n.doForward(ctx, up.(*upstream.Upstream), clone, true)
			var hash string
			if err == nil && resp != nil {
				if jrr, e := resp.JsonRpcResponse(ctx); e == nil {
					hash, _ = jrr.CanonicalHash()
				}
			}
			results[i] = &result{resp: resp, err: err, hash: hash, up: up}
		}()
	}

	wg.Wait()

	// Tally hashes
	counts := map[string]int{}
	var bestResp *common.NormalizedResponse
	for _, r := range results {
		if r == nil || r.err != nil || r.hash == "" {
			continue
		}
		counts[r.hash]++
		if counts[r.hash] >= threshold {
			bestResp = r.resp
			break
		}
	}

	if bestResp != nil {
		return bestResp, nil
	}

	// No agreement
	return nil, common.NewErrConsensusDispute("not enough agreement among upstreams")
}

// findBlockHeadLeader returns the upstream with the highest latest block (best head).
func findBlockHeadLeader(ups []common.Upstream) common.Upstream {
	var leader common.Upstream
	var highest int64
	for _, u := range ups {
		if evmUp, ok := u.(common.EvmUpstream); ok {
			sp := evmUp.EvmStatePoller()
			if sp == nil {
				continue
			}
			lb := sp.LatestBlock()
			if lb > highest {
				highest = lb
				leader = u
			}
		}
	}
	return leader
}
