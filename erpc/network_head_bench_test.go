package erpc

import (
	"context"
	"fmt"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// headBenchShapes are the lane and upstream counts of one network the head
// benchmarks run on: a lane is a use-upstream group (plus the network-wide
// lane), and a group needs a distinct proper subset of the upstreams.
var headBenchShapes = []struct{ lanes, ups int }{{2, 4}, {8, 8}, {16, 10}}

// headBenchNetwork is a network of ups upstreams at block 1000 with lanes
// group lanes; lane k is selected by the tag "lane<k>", which a window of
// 2 to ups-1 consecutive upstreams carries, the windows spread over every
// size. The head loop runs on wakes only.
func headBenchNetwork(b *testing.B, lanes, ups int) (*Network, []*upstream.Upstream, []*servedTipLane, [][]int) {
	b.Helper()
	util.ResetGock()
	b.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()
	var windows [][]int
	for size := 2; size < ups; size++ {
		for start := range ups {
			w := make([]int, size)
			for i := range w {
				w[i] = (start + i) % ups
			}
			windows = append(windows, w)
		}
	}
	require.GreaterOrEqual(b, len(windows), lanes)
	fx := make([]servedTipFixture, ups)
	for i := range fx {
		fx[i] = servedTipFixture{id: fmt.Sprintf("u%d", i), chainID: 123, latestBlock: 1000}
	}
	members := make([][]int, lanes)
	for k := range members {
		members[k] = windows[k*len(windows)/lanes]
		for _, i := range members[k] {
			fx[i].tags = append(fx[i].tags, fmt.Sprintf("lane%d", k))
		}
	}
	n, ordered := setupServedTipNetworkEvery(b, context.Background(), fx,
		&common.EvmServedTipConfig{Quorum: common.ServedTipQuorumMajority}, time.Hour)
	ls := make([]*servedTipLane, lanes)
	for k := range ls {
		ls[k] = n.laneFor(fmt.Sprintf("lane%d", k))
		require.NotNil(b, ls[k], "lane %d", k)
		require.EqualValues(b, 1000, ls[k].published().Latest, "lane %d", k)
	}
	require.EqualValues(b, lanes, n.servedTipLaneCount.Load())
	return n, ordered, ls, members
}

// BenchmarkHeadRecompute is one recompute of every lane's Head with no
// counter moved. cpu-ns/op is the process's CPU time per op, which host
// load does not stretch as it does ns/op.
func BenchmarkHeadRecompute(b *testing.B) {
	for _, s := range headBenchShapes {
		b.Run(fmt.Sprintf("lanes=%d/ups=%d", s.lanes, s.ups), func(b *testing.B) {
			n, _, _, _ := headBenchNetwork(b, s.lanes, s.ups)
			b.ReportAllocs()
			cpu := processCPU()
			for b.Loop() {
				n.refreshHeads()
			}
			b.ReportMetric(float64((processCPU()-cpu).Nanoseconds())/float64(b.N), "cpu-ns/op")
		})
	}
}

// processCPU is the user and system CPU time this process used so far.
func processCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// BenchmarkHeadChangeToPublish is the time from a counter move to the
// published Latest of the lane it moves: each op raises every member of
// one lane, in turn, one block above the lane's highest member and waits
// for that lane's Latest to change. p50 and max are per op.
func BenchmarkHeadChangeToPublish(b *testing.B) {
	for _, s := range headBenchShapes {
		b.Run(fmt.Sprintf("lanes=%d/ups=%d", s.lanes, s.ups), func(b *testing.B) {
			_, ups, ls, members := headBenchNetwork(b, s.lanes, s.ups)
			heads := make([]int64, len(ups))
			for i := range heads {
				heads[i] = 1000
			}
			var took []time.Duration
			b.ReportAllocs()
			op := 0
			for b.Loop() {
				k := op % len(ls)
				op++
				var target int64
				for _, i := range members[k] {
					target = max(target, heads[i]+1)
				}
				moved := ls[k].latestMoved()
				start := time.Now()
				for _, i := range members[k] {
					heads[i] = target
					ups[i].EvmStatePoller().SuggestLatestBlock(target, start)
				}
				select {
				case <-moved:
				case <-time.After(5 * time.Second):
					b.Fatalf("lane %d never published %d", k, target)
				}
				took = append(took, time.Since(start))
			}
			slices.Sort(took)
			b.ReportMetric(float64(took[len(took)/2].Nanoseconds()), "p50-ns")
			b.ReportMetric(float64(took[len(took)*99/100].Nanoseconds()), "p99-ns")
			b.ReportMetric(float64(took[len(took)-1].Nanoseconds()), "max-ns")
		})
	}
}
