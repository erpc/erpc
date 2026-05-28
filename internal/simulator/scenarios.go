package simulator

import (
	"strings"
	"time"
)

// scenarioStep is one fired UI-visible mutation.
type scenarioStep struct {
	atSec int
	desc  string
	apply func(o *Orchestrator)
}

// scenarioDefs is the built-in scenario library exposed in the UI.
// Each step is fired at-or-after `atSec` seconds since the scenario
// started; once Duration elapses the scenario ends.
var scenarioDefs = map[string]struct {
	duration int
	steps    []scenarioStep
}{
	"gradual-degradation": {
		duration: 90,
		steps: []scenarioStep{
			{0, "alc-eth-1 errorRate → 0.05", patchOne("alc-eth-1", patchError(0.05))},
			{15, "alc-eth-1 base latency × 2", patchOne("alc-eth-1", patchLatencyMul(2))},
			{30, "alc-eth-1 errorRate → 0.18", patchOne("alc-eth-1", patchError(0.18))},
			{50, "alc-eth-1 errorRate → 0.40", patchOne("alc-eth-1", patchError(0.40))},
			{70, "alc-eth-1 errorRate → 0.70", patchOne("alc-eth-1", patchError(0.70))},
		},
	},
	"vendor-region-outage": {
		duration: 60,
		steps: []scenarioStep{
			{0, "Alchemy us-east latency × 4", patchTag("alchemy", "us-east", patchLatencyMul(4))},
			{8, "Alchemy us-east errors → 0.6", patchTag("alchemy", "us-east", patchError(0.6))},
			{22, "Alchemy us-east unavailable", patchTag("alchemy", "us-east", patchAvail(false))},
			{45, "Alchemy us-east restored", patchTag("alchemy", "us-east", func(o *Orchestrator, id string) {
				_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{
					Available:     boolPtr(true),
					ErrorRate:     float64Ptr(0.02),
					BaseLatencyMs: float64Ptr(50),
				})
			})},
		},
	},
	// "traffic-spike" simulates the *effects* of a spike on the
	// upstream pool: latency creeps up, jitter widens, error rates rise
	// as upstreams overload. The actual RPS knob lives in the browser
	// (it owns the traffic-gen rate), so we apply pool-wide degradation
	// instead of mutating the wire-load value here.
	"traffic-spike": {
		duration: 60,
		steps: []scenarioStep{
			{0, "Pool latency × 1.6 (overload begins)", patchAll(patchLatencyMul(1.6))},
			{15, "Pool jitter × 2 (queueing variance)", patchAll(patchJitterMul(2))},
			{35, "All upstreams errorRate +0.15 (peak load)", patchAll(patchError(0.15))},
			{50, "Recovery", patchAll(func(o *Orchestrator, id string) {
				_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{ErrorRate: float64Ptr(0.005)})
			})},
		},
	},
	"hedge-saturator": {
		duration: 60,
		steps: []scenarioStep{
			{0, "All upstreams jitter × 3", patchAll(patchJitterMul(3))},
			{12, "drpc-eth-1 latency × 4", patchOne("drpc-eth-1", patchLatencyMul(4))},
			{30, "chainstack-eth-1 latency × 4", patchOne("chainstack-eth-1", patchLatencyMul(4))},
		},
	},
	"policy-exclude-cascade": {
		duration: 50,
		steps: []scenarioStep{
			{0, "public-eth-1 errorRate → 0.95", patchOne("public-eth-1", patchError(0.95))},
			{8, "chainstack-eth-1 timeouts → 0.4", patchOne("chainstack-eth-1", patchTimeout(0.4))},
			{25, "drpc-eth-1 throttle → 0.6", patchOne("drpc-eth-1", patchThrottle(0.6))},
		},
	},
	"slow-network": {
		duration: 60,
		steps: []scenarioStep{
			{0, "All upstreams latency × 3", patchAll(patchLatencyMul(3))},
			{0, "All upstreams jitter × 2", patchAll(patchJitterMul(2))},
		},
	},
}

// StartScenario kicks off a named scenario. Unknown names are no-ops.
func (o *Orchestrator) StartScenario(name string) bool {
	if _, ok := scenarioDefs[name]; !ok {
		return false
	}
	o.scnMu.Lock()
	o.scnName = name
	o.scnStart = time.Now()
	o.scnIdx = 0
	o.scnEvents = nil
	o.scnMu.Unlock()
	o.dumper.LogScenarioStart(name)
	return true
}

// StopScenario clears any running scenario.
func (o *Orchestrator) StopScenario() {
	o.scnMu.Lock()
	o.scnName = ""
	o.scnIdx = 0
	o.scnEvents = nil
	o.scnMu.Unlock()
	o.dumper.LogScenarioStop()
}

// advanceScenario is called from the traffic loop; fires any due steps.
func (o *Orchestrator) advanceScenario() {
	o.scnMu.Lock()
	name := o.scnName
	if name == "" {
		o.scnMu.Unlock()
		return
	}
	def, ok := scenarioDefs[name]
	if !ok {
		o.scnName = ""
		o.scnMu.Unlock()
		return
	}
	elapsed := int(time.Since(o.scnStart).Seconds())
	if elapsed >= def.duration {
		o.scnName = ""
		o.scnMu.Unlock()
		return
	}
	var due []scenarioStep
	for o.scnIdx < len(def.steps) && def.steps[o.scnIdx].atSec <= elapsed {
		due = append(due, def.steps[o.scnIdx])
		o.scnEvents = append([]ScenarioEventBubble{{TSec: def.steps[o.scnIdx].atSec, Desc: def.steps[o.scnIdx].desc}}, o.scnEvents...)
		if len(o.scnEvents) > 12 {
			o.scnEvents = o.scnEvents[:12]
		}
		o.scnIdx++
	}
	o.scnMu.Unlock()
	for _, s := range due {
		s.apply(o)
	}
}

func (o *Orchestrator) scenarioFrame() *ScenarioFrame {
	o.scnMu.Lock()
	defer o.scnMu.Unlock()
	if o.scnName == "" {
		return nil
	}
	def := scenarioDefs[o.scnName]
	return &ScenarioFrame{
		Name:       o.scnName,
		ElapsedSec: time.Since(o.scnStart).Seconds(),
		DurationS:  float64(def.duration),
		Events:     append([]ScenarioEventBubble(nil), o.scnEvents...),
	}
}

// ---------- step helpers ----------------------------------------------

func patchOne(id string, fn func(o *Orchestrator, id string)) func(o *Orchestrator) {
	return func(o *Orchestrator) {
		for _, k := range o.hub.Snapshot() {
			if k.ID == id {
				fn(o, id)
				return
			}
		}
	}
}

func patchTag(vendor, tag string, fn func(o *Orchestrator, id string)) func(o *Orchestrator) {
	return func(o *Orchestrator) {
		for _, k := range o.hub.Snapshot() {
			if k.Vendor != vendor {
				continue
			}
			has := false
			for _, t := range k.Tags {
				if strings.EqualFold(t, tag) {
					has = true
					break
				}
			}
			if !has {
				continue
			}
			fn(o, k.ID)
		}
	}
}

func patchAll(fn func(o *Orchestrator, id string)) func(o *Orchestrator) {
	return func(o *Orchestrator) {
		for _, k := range o.hub.Snapshot() {
			fn(o, k.ID)
		}
	}
}

func patchError(v float64) func(*Orchestrator, string) {
	return func(o *Orchestrator, id string) {
		_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{ErrorRate: float64Ptr(v)})
	}
}

func patchTimeout(v float64) func(*Orchestrator, string) {
	return func(o *Orchestrator, id string) {
		_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{TimeoutRate: float64Ptr(v)})
	}
}

func patchThrottle(v float64) func(*Orchestrator, string) {
	return func(o *Orchestrator, id string) {
		_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{ThrottleRate: float64Ptr(v)})
	}
}

func patchAvail(v bool) func(*Orchestrator, string) {
	return func(o *Orchestrator, id string) {
		_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{Available: boolPtr(v)})
	}
}

func patchLatencyMul(mul float64) func(*Orchestrator, string) {
	return func(o *Orchestrator, id string) {
		for _, k := range o.hub.Snapshot() {
			if k.ID == id {
				newV := k.BaseLatencyMs * mul
				_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{BaseLatencyMs: float64Ptr(newV)})
				return
			}
		}
	}
}

func patchJitterMul(mul float64) func(*Orchestrator, string) {
	return func(o *Orchestrator, id string) {
		for _, k := range o.hub.Snapshot() {
			if k.ID == id {
				newV := k.JitterMs * mul
				_, _ = o.UpdateUpstream(id, UpstreamKnobPatch{JitterMs: float64Ptr(newV)})
				return
			}
		}
	}
}

func float64Ptr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool           { return &v }
