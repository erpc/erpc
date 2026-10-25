package realworld

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Bounds of the integrity scenarios (spec §6.9 suspension, §6.10 evidence
// quorum, §11 logs path, §13; §19 U8, U9, K5).
const (
	// u9VendorBLatency is harness, not spec: enough for the selection
	// policy to rank vendor-b's upstreams below vendor-a's.
	u9VendorBLatency = 100 * time.Millisecond
	// u9FaultBlocks: how long each U9 receipts fault lasts (harness).
	u9FaultBlocks = 50
	// bloomSuspendRun: blocks in a row whose bloom mismatches, from
	// vendors that meet the evidence quorum, suspend the bloom check;
	// bloomRearmRun matching blocks in a row re-arm it (§6.9).
	bloomSuspendRun = 8
	bloomRearmRun   = 8
	// logsFallbackAfter: receipts that fail on every upstream this long
	// switch the block to eth_getLogs by blockHash (§11).
	logsFallbackAfter = 5 * time.Second
	// bloomCheck and baseFeeCheck: the check ids metrics label (§13).
	bloomCheck   = "bloomMatch"
	baseFeeCheck = "baseFeeDerivation"
	// u8Sample: U8 reads every u8Sample-th forged hash (harness).
	u8Sample = 5
)

// vendorA and vendorB are baseline B's upstream keys by vendor.
var (
	vendorA = []string{"ws-a", "http-a"}
	vendorB = []string{"ws-b", "http-b"}
)

// U8: ws-a pushes headers whose hash the header does not hash to, for 100
// blocks. No forged hash is pinned or sent: no client receives one (the
// oracle rejects unknown hashes), and no pod answers one by hash. With
// integrity on, intake drops them and counts the violations on ws-a only.
// With integrity off, ws-b registers after the network started: its heads
// vote from the first one, so ws-a's never stand alone.
func u8(integrity bool) spec {
	name := "U8-off"
	var late []string
	if integrity {
		name = "U8-on"
	} else {
		late = []string{"ws-b"}
	}
	return spec{name: name, integrity: integrity, late: late, bound: settleDelivery, body: func(s *scenario) {
		s.blocks(25)
		susp := s.suspensions("")
		ws := s.vendors["ws-a"]
		from := s.chain.Tip().Number + 1
		ws.Update(func(f *chainsim.Faults) { f.WrongHashes = true })
		s.blocks(100)
		ws.Update(func(f *chainsim.Faults) { f.WrongHashes = false })
		to := ws.Head().Number
		if ws.Stats().ForgedHeads == 0 {
			s.violate("harness", "ws-a pushed no forged head")
		}
		for n := from; n <= to; n += u8Sample {
			b := s.chain.CanonAt(n)
			for pod := range s.h.pods {
				if got := s.blockAnswer(pod, "", "eth_getBlockByHash", chainsim.ForgedHashOf(b).Hex(), false); got != "null" {
					s.violate(name, "pod-%d: eth_getBlockByHash of ws-a's forged hash of block %d answered %s: it was pinned", pod+1, n, got)
				}
				if got := s.blockAnswer(pod, "", "eth_getBlockByNumber", hexutil.EncodeUint64(n), false); got != b.Hash.Hex() {
					s.violate(name, "pod-%d: eth_getBlockByNumber(%d) answered %s, not %s", pod+1, n, got, short(b.Hash))
				}
			}
		}
		s.blocks(25)
		if !integrity {
			return
		}
		s.requireNotSuspended(susp, "")
		up := s.upstream("ws-a")
		for pod := range s.h.pods {
			if v, _ := s.metric(pod, "erpc_chain_tracker_source_heads_total", map[string]string{"upstream": up, "outcome": "gated_integrity"}); v == 0 {
				s.violate(name, "pod-%d: no %s header counted gated_integrity", pod+1, up)
			}
			mine, _ := s.metric(pod, "erpc_integrity_violation_total", map[string]string{"upstream": up})
			all, _ := s.metric(pod, "erpc_integrity_violation_total", nil)
			if mine == 0 {
				s.violate(name, "pod-%d: erpc_integrity_violation_total counts nothing on %s", pod+1, up)
			}
			if all > mine {
				s.violate(name, "pod-%d: erpc_integrity_violation_total counts %v on upstreams other than %s", pod+1, all-mine, up)
			}
		}
	}}
}

// U9: vendor-a serves truncated receipts, then a forged log, then a zero
// bloom, u9FaultBlocks each, integrity on. Receipts are refetched from
// vendor-b: every client's logs equal the canonical logs, no forged log
// reaches a client, and violations count on vendor-a only.
func u9() spec {
	return spec{name: "U9", integrity: true, bound: healthyDelivery, body: func(s *scenario) {
		s.slowVendorB()
		s.blocks(25)
		susp := s.suspensions("")
		var during, duringB int64
		for _, fault := range []func(*chainsim.Faults, bool){
			func(f *chainsim.Faults, on bool) { f.TruncReceipts = on },
			func(f *chainsim.Faults, on bool) { f.ForgedLog = on },
			func(f *chainsim.Faults, on bool) { f.WrongBloom = on },
		} {
			a0, b0 := s.receiptsCalls(vendorA), s.receiptsCalls(vendorB)
			s.fault(vendorA, fault, u9FaultBlocks)
			during += s.receiptsCalls(vendorA) - a0
			duringB += s.receiptsCalls(vendorB) - b0
			s.blocks(10)
		}
		s.logf("under vendor-a's faults, vendor-a answered %d receipts calls, vendor-b %d", during, duringB)
		s.requireOnVendorA(during, nil)
		s.requireNotSuspended(susp, "")
		s.blocks(25)
	}}
}

// U9-trunc1, U9-trunc2: truncated receipts from vendor-a, then from both
// vendors, integrity on. With one vendor, receipts are refetched from
// vendor-b, violations count on vendor-a only, and nothing is suspended:
// one vendor of two never meets the evidence quorum (§6.10). With both,
// every receipts fetch fails, so after logsFallbackAfter each block's
// logs come from eth_getLogs by blockHash, held only where both vendors
// agree on them (§11 fallback, §6.10): both vendors are asked, and every
// client's logs equal the canonical logs.
func u9Trunc(vendors int) spec {
	trunc := func(f *chainsim.Faults, on bool) { f.TruncReceipts = on }
	if vendors == 1 {
		return spec{name: "U9-trunc1", integrity: true, bound: healthyDelivery, body: func(s *scenario) {
			s.slowVendorB()
			s.blocks(25)
			susp := s.suspensions("")
			a0 := s.receiptsCalls(vendorA)
			s.fault(vendorA, trunc, u9FaultBlocks)
			s.requireOnVendorA(s.receiptsCalls(vendorA)-a0, nil)
			s.requireNotSuspended(susp, "")
			s.blocks(25)
		}}
	}
	return spec{name: "U9-trunc2", integrity: true, bound: healthyDelivery + logsFallbackAfter + liveBound, body: func(s *scenario) {
		s.blocks(25)
		logs0 := map[string]int64{}
		for _, key := range append(vendorA, vendorB...) {
			logs0[key] = s.vendors[key].Stats().LogsCalls
		}
		s.fault(append(vendorA, vendorB...), trunc, u9FaultBlocks)
		for name, keys := range map[string][]string{"vendor-a": vendorA, "vendor-b": vendorB} {
			var n int64
			for _, key := range keys {
				n += s.vendors[key].Stats().LogsCalls - logs0[key]
			}
			s.logf("%s answered %d eth_getLogs calls while every vendor truncated receipts", name, n)
			if n == 0 {
				s.violate(s.name, "%s answered no eth_getLogs while every vendor truncated receipts: the fallback did not ask both vendors of the evidence quorum", name)
			}
		}
		s.blocks(25)
	}}
}

// U9-bloom1, U9-bloom2: receipts whose logsBloom is set but not the
// header's (bloomMatch, §11) from vendor-a, integrity on: refetched from
// vendor-b, violations count bloomMatch on vendor-a only, and the bloom
// check is never suspended (§6.10). U9-bloom2 first mints a chain whose
// header bloom is not its receipts' OR, on every vendor: the check is
// suspended after bloomSuspendRun blocks in a row and the blocks are
// fetched again, so every client's logs equal the canonical logs; after
// bloomRearmRun matching blocks it is re-armed, and vendor-a's wrong bloom
// alone counts against vendor-a again, with no new suspension (§6.9).
func u9Bloom(vendors int) spec {
	bloom := func(f *chainsim.Faults, on bool) { f.OffBloom = on }
	check := map[string]string{"check": bloomCheck}
	if vendors == 1 {
		return spec{name: "U9-bloom1", integrity: true, bound: healthyDelivery, body: func(s *scenario) {
			s.slowVendorB()
			s.blocks(25)
			susp := s.suspensions(bloomCheck)
			a0 := s.receiptsCalls(vendorA)
			s.fault(vendorA, bloom, u9FaultBlocks)
			s.requireOnVendorA(s.receiptsCalls(vendorA)-a0, check)
			s.requireNotSuspended(susp, bloomCheck)
			s.blocks(25)
		}}
	}
	return spec{name: "U9-bloom2", integrity: true, bound: healthyDelivery + bloomSuspendRun*blockTime + liveBound, body: func(s *scenario) {
		s.slowVendorB()
		s.blocks(25)
		susp := s.suspensions(bloomCheck)
		s.chain.OffHeaderBloom(true)
		s.blocks(3 * bloomSuspendRun)
		s.chain.OffHeaderBloom(false)
		for pod, n := range s.suspensions(bloomCheck) {
			if n <= susp[pod] {
				s.violate(s.name, "pod-%d: erpc_integrity_check_suspended_total{check=%s} stayed %v while the chain's header bloom was not its receipts' for %d blocks", pod+1, bloomCheck, n, 3*bloomSuspendRun)
			}
		}
		s.blocks(3 * bloomRearmRun)
		susp = s.suspensions(bloomCheck)
		va0, vb0 := s.integrityViolations(vendorA, check), s.integrityViolations(vendorB, check)
		a0 := s.receiptsCalls(vendorA)
		s.fault(vendorA, bloom, u9FaultBlocks)
		if s.receiptsCalls(vendorA) == a0 {
			s.violate("harness", "no receipts call reached vendor-a during its wrong bloom")
		}
		if va := s.integrityViolations(vendorA, check); va <= va0 {
			s.violate(s.name, "vendor-a's wrong bloom after %d matching blocks counted no %s violation (%v): the check was not re-armed", 3*bloomRearmRun, bloomCheck, va)
		}
		if vb := s.integrityViolations(vendorB, check); vb > vb0 {
			s.violate(s.name, "%s violations on vendor-b rose by %v while only vendor-a served a wrong bloom", bloomCheck, vb-vb0)
		}
		s.requireNotSuspended(susp, bloomCheck)
		s.blocks(25)
	}}
}

// slowVendorB runs vendor-b's links u9VendorBLatency slower on every pod,
// so the selection policy asks vendor-a first and its faults are
// exercised.
func (s *scenario) slowVendorB() {
	for _, key := range vendorB {
		for _, p := range s.h.pods {
			s.proxies[key].set(p, linkFaults{Latency: u9VendorBLatency})
		}
	}
}

// fault turns fault on for the vendors of keys for blocks, then off.
func (s *scenario) fault(keys []string, fault func(*chainsim.Faults, bool), blocks int) {
	for _, key := range keys {
		s.vendors[key].Update(func(f *chainsim.Faults) { fault(f, true) })
	}
	s.blocks(blocks)
	for _, key := range keys {
		s.vendors[key].Update(func(f *chainsim.Faults) { fault(f, false) })
	}
}

// receiptsCalls sums the receipts and logs calls the vendors of keys
// answered.
func (s *scenario) receiptsCalls(keys []string) (n int64) {
	for _, key := range keys {
		st := s.vendors[key].Stats()
		n += st.ReceiptsCalls + st.LogsCalls
	}
	return n
}

// integrityViolations sums erpc_integrity_violation_total over every pod
// and the upstreams of keys, plus labels.
func (s *scenario) integrityViolations(keys []string, labels map[string]string) (sum float64) {
	for pod := range s.h.pods {
		for _, key := range keys {
			l := map[string]string{"upstream": s.upstream(key)}
			for k, v := range labels {
				l[k] = v
			}
			v, _ := s.metric(pod, "erpc_integrity_violation_total", l)
			sum += v
		}
	}
	return sum
}

// requireOnVendorA: vendor-a answered during receipts calls under its
// fault, and violations (plus labels) count on vendor-a, never on vendor-b.
func (s *scenario) requireOnVendorA(during int64, labels map[string]string) {
	if during == 0 {
		s.violate("harness", "no receipts call reached vendor-a during its faults: nothing exercised them")
		return
	}
	if mine := s.integrityViolations(vendorA, labels); mine == 0 {
		s.violate(s.name, "vendor-a answered %d faulty receipts calls; erpc_integrity_violation_total%v counts none on its upstreams", during, labels)
	}
	if other := s.integrityViolations(vendorB, nil); other > 0 {
		s.violate(s.name, "erpc_integrity_violation_total counts %v on vendor-b, which served no fault", other)
	}
}

// suspensions is erpc_integrity_check_suspended_total per pod, for check
// ("" every check).
func (s *scenario) suspensions(check string) []float64 {
	var labels map[string]string
	if check != "" {
		labels = map[string]string{"check": check}
	}
	out := make([]float64, len(s.h.pods))
	for pod := range s.h.pods {
		out[pod], _ = s.metric(pod, "erpc_integrity_check_suspended_total", labels)
	}
	return out
}

// requireNotSuspended: no pod suspended check ("" any) since before.
func (s *scenario) requireNotSuspended(before []float64, check string) {
	for pod, n := range s.suspensions(check) {
		if n > before[pod] {
			s.violate(s.name, "pod-%d: erpc_integrity_check_suspended_total{check=%q} rose by %v while one vendor of two served the fault: it does not meet the evidence quorum", pod+1, check, n-before[pod])
		}
	}
}

// K5: one chain and one plan (1–3 deep reorgs, then a reorgWindow − 1 deep
// one), integrity on on pod-1 and off on pod-2, each pod with its own
// baseline clients that never move. Integrity also runs baseFeeDerivation,
// which every simulated header passes, as a node's headers do: nothing is
// suspended on either pod. Every pod-1 client ends with the same chain and
// the same live logs as its pod-2 twin (§19 K5).
func k5() spec {
	return spec{name: "K5", integrity: true, integrityOff: []int{1}, checks: []string{baseFeeCheck}, bound: healthyDelivery,
		clients: func(s *scenario) {
			for _, p := range s.h.pods {
				s.podClients(p.idx, 0)
			}
		},
		body: func(s *scenario) {
			s.blocks(25)
			for end := time.Now().Add(60 * time.Second); time.Now().Before(end); {
				s.blocks(30 + rand.IntN(20))
				s.reorg(1 + rand.IntN(3))
			}
			s.blocks(25)
			s.reorg(reorgWindow - 1)
			s.blocks(25)
			if v := s.suspensions(""); v[0] > 0 || v[1] > 0 {
				s.violate(s.name, "erpc_integrity_check_suspended_total: pod-1 (integrity on) %v, pod-2 (off) %v on a chain whose every header a node would serve", v[0], v[1])
			}
		},
		after: requireSameOnAndOff,
	}
}

// requireSameOnAndOff: each pod-1 client and its pod-2 twin (same name
// after the pod) hold the same block at every height both hold, and the
// same live logs over the blocks both covered.
func requireSameOnAndOff(s *scenario) {
	twin := func(name string) (string, bool) {
		rest, ok := strings.CutPrefix(name, "p1-")
		return "p2-" + rest, ok
	}
	sameChain := func(name string, a, b *headerChain, other string) {
		diff := 0
		var first string
		for n, h := range a.m {
			if g, ok := b.m[n]; ok && g != h {
				if diff++; diff == 1 {
					first = fmt.Sprintf("at %d %s vs %s", n, short(h), short(g))
				}
			}
		}
		if diff > 0 {
			s.violate(name, "integrity on and off disagree with %s at %d heights, first %s", other, diff, first)
		}
	}
	heads := map[string]*headsClient{}
	for _, c := range s.heads {
		heads[c.name] = c
	}
	for _, c := range s.heads {
		if o, ok := twin(c.name); ok && heads[o] != nil {
			sameChain(c.name, &c.hc, &heads[o].hc, o)
		}
	}
	grpcs := map[string]*grpcClient{}
	for _, g := range s.grpcs {
		grpcs[g.name] = g
	}
	for _, g := range s.grpcs {
		if o, ok := twin(g.name); ok && grpcs[o] != nil {
			sameChain(g.name, &g.hc, &grpcs[o].hc, o)
		}
	}
	logs := map[string]*logsClient{}
	for _, c := range s.logs {
		logs[c.name] = c
	}
	covered := func(c *logsClient) map[uint64]bool {
		out := map[uint64]bool{}
		for _, lc := range c.conns {
			for n := lc.from; n <= lc.to && lc.to > 0; n++ {
				out[n] = true
			}
		}
		return out
	}
	for _, a := range s.logs {
		o, ok := twin(a.name)
		b := logs[o]
		if !ok || b == nil {
			continue
		}
		ca, cb := covered(a), covered(b)
		var sample []string
		diff := 0
		onlyIn := func(x, y *logsClient, label string) {
			for k, l := range x.live {
				if _, ok := y.live[k]; !ok && ca[l.number] && cb[l.number] {
					if diff++; len(sample) < 5 {
						sample = append(sample, fmt.Sprintf("%s %d %s/%d", label, l.number, short(k.BlockHash), k.LogIndex))
					}
				}
			}
		}
		onlyIn(a, b, "only on")
		onlyIn(b, a, "only off")
		if diff > 0 {
			s.violate(a.name, "integrity on and off hold different live logs than %s over blocks both covered: %d differ: %s", o, diff, strings.Join(sample, ", "))
		}
	}
}
