package realworld

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Bounds of the config scenarios (§16; §19 K1, K3, K4).
const (
	// k1Refuse: a config the strict decoder rejects ends the process
	// within this; k1Boots: a valid one is still serving after it
	// (harness).
	k1Refuse = 30 * time.Second
	k1Boots  = 10 * time.Second

	// k3Depth: the reorg both K3 networks take (§19 K3), inside the 64
	// window, past the 32 one.
	k3Depth = 40
)

// k1Case is one deleted key (§16), placed where the old config held it.
type k1Case struct {
	key   string
	where string
	put   func(cfg map[string]interface{})
}

// k1Cases: every key §16 deletes, at each place it lived.
func k1Cases() []k1Case {
	type m = map[string]interface{}
	project := func(cfg m) m { return cfg["projects"].([]m)[0] }
	network := func(cfg m) m { return project(cfg)["networks"].([]m)[0] }
	return []k1Case{
		{"maxBlocksPerTick", "projects[].networks[].integrity.follow", func(cfg m) {
			network(cfg)["integrity"] = m{"follow": m{"maxBlocksPerTick": 10}}
		}},
		{"reorgWindow", "projects[].integrity", func(cfg m) {
			project(cfg)["integrity"] = m{"reorgWindow": 32}
		}},
		{"reorgWindow", "projects[].networks[].integrity", func(cfg m) {
			network(cfg)["integrity"] = m{"reorgWindow": 32}
		}},
		{"stripSubscribeFromBlockZero", "projects[].networks[].evm", func(cfg m) {
			network(cfg)["evm"].(m)["stripSubscribeFromBlockZero"] = true
		}},
		{"subscriptionBufferSize", "server.webSocket", func(cfg m) {
			cfg["server"].(m)["webSocket"].(m)["subscriptionBufferSize"] = 100
		}},
		{"indexer", "top level", func(cfg m) {
			cfg["indexer"] = m{"canonicalChainDepth": 64, "dedupWindowSize": 1000}
		}},
	}
}

// K1: for every key §16 deletes, at every place it lived, as YAML and as a
// TypeScript config: the binary refuses to boot and its output names the
// key. The same config without the key boots, so each refusal is the
// key's (§19 K1; §16 strict decoding, no aliases).
func k1() spec {
	return spec{name: "K1", pods: 1, memory: true, bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		for _, format := range []string{"yaml", "ts"} {
			if out, booted := s.bootConfig(format, "valid", nil); !booted {
				s.violate("harness", "%s config with no deleted key did not boot: %s", format, out)
			}
			for i, c := range k1Cases() {
				out, booted := s.bootConfig(format, fmt.Sprintf("deleted-%d", i), c.put)
				switch {
				case booted:
					s.violate(s.name, "%s config with %s.%s booted", format, c.where, c.key)
				case !strings.Contains(out, c.key):
					s.violate(s.name, "%s config with %s.%s refused without naming the key: %.300s", format, c.where, c.key, out)
				default:
					s.tr.logf("%s %s.%s refused: %.200s", format, c.where, c.key, lastLine(out))
				}
			}
		}
	}}
}

// lastLine is the last non-empty line of out.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

// bootConfig writes a minimal config for the scenario's network, edited
// by put, as format (yaml or ts), and runs the binary on it. booted: still
// running after k1Boots (then killed); otherwise out is its output once it
// exited, within k1Refuse.
func (s *scenario) bootConfig(format, name string, put func(map[string]interface{})) (out string, booted bool) {
	type m = map[string]interface{}
	cfg := m{
		"logLevel": "warn",
		"server": m{"listenV4": true, "httpHostV4": "127.0.0.1", "httpPortV4": freePort(s.t),
			"webSocket": m{"enabled": true}},
		"metrics": m{"enabled": false},
		"projects": []m{{"id": "k1",
			"networks":  []m{{"architecture": "evm", "evm": m{"chainId": s.chain.ID()}}},
			"upstreams": []m{{"id": "k1-up", "endpoint": s.proxies["http-a"].URL(), "evm": m{"chainId": s.chain.ID()}}},
		}},
	}
	if put != nil {
		put(cfg)
	}
	var body []byte
	var err error
	path := filepath.Join(s.dir, "k1-"+name+"."+format)
	if format == "yaml" {
		body, err = yaml.Marshal(cfg)
	} else {
		body, err = json.Marshal(cfg)
		body = append(append([]byte("export default "), body...), ";\n"...)
	}
	if err == nil {
		err = os.WriteFile(path, body, 0o644)
	}
	if err != nil {
		s.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(s.ctx, k1Refuse)
	defer cancel()
	cmd := exec.CommandContext(ctx, erpcBin, path)
	var b strings.Builder
	cmd.Stdout, cmd.Stderr = &b, &b
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		return b.String(), false
	case <-time.After(k1Boots):
		_ = cmd.Process.Kill()
		<-done
		return b.String(), true
	}
}

// K3: two networks take the same k3Depth reorg; their reorgWindow differs
// (§19 K3; §10.2; §15.1 reorg deeper than reorgWindow). K3-64 holds the
// reorg in its window: no re-anchor, every client told with Reverted (no
// ws logs end). K3-32 re-anchors, and every ws logs tap, which sent more
// than 32 of the reverted blocks, ends 1013 within the close spread.
// Both run in one phase, two networks on the same pods.
func k3(window int) spec {
	evm := map[string]interface{}{"chainTracker": map[string]interface{}{"reorgWindow": window, "maxBackfill": maxBackfill.String()}}
	held := window > k3Depth
	sp := spec{name: fmt.Sprintf("K3-%d", window), evm: evm, bound: healthyDelivery + k3Depth*blockTime}
	if !held {
		sp.allow = []string{"ws:logs-end"}
	}
	sp.body = func(s *scenario) {
		s.blocks(k3Depth + 25)
		before := make([]float64, len(s.h.pods))
		for pod := range s.h.pods {
			before[pod], _ = s.metric(pod, "erpc_chain_tracker_reanchor_total", nil)
		}
		r := s.reorg(k3Depth)
		if held {
			s.blocks(25)
		} else {
			time.Sleep(logsCloseSpread + reconnectSlack)
		}
		for pod := range s.h.pods {
			v, _ := s.waitMetric(pod, "erpc_chain_tracker_reanchor_total", nil, reconnectSlack, func(v float64) bool { return held || v > before[pod] })
			switch {
			case held && v > before[pod]:
				s.violate(s.name, "pod-%d: re-anchored %v times on a %d-deep reorg inside reorgWindow %d", pod+1, v-before[pod], k3Depth, window)
			case !held && v <= before[pod]:
				s.violate(s.name, "pod-%d: erpc_chain_tracker_reanchor_total stayed %v after a %d-deep reorg past reorgWindow %d", pod+1, v, k3Depth, window)
			}
		}
		if !held {
			for _, c := range s.logs {
				if !c.endedWith(r.at, r.at.Add(logsCloseSpread+reconnectSlack), func(e clientEnd) bool { return e.server && closedWith(e.reason, 1013) }) {
					s.violate(c.name, "no 1013 close within %s of the %d-deep reorg past reorgWindow %d", logsCloseSpread, k3Depth, window)
				}
			}
		}
		s.requireReorgsTold()
	}
	return sp
}

// K4: http-a sets subscriptionSource true, ws-a false. http-a is polled
// at its cadence as a poll source; ws-a is no source: it never gets a
// newHeads subscribe (§19 K4; §5.1; §16).
func k4() spec {
	return spec{name: "K4", bound: healthyDelivery,
		upEvm: map[string]map[string]interface{}{"http-a": {"subscriptionSource": true}, "ws-a": {"subscriptionSource": false}},
		body: func(s *scenario) {
			s.blocks(25)
			s.requirePolledAtCadence("http-a")
			if n := s.vendors["ws-a"].Stats().HeadsSubscribes; n > 0 {
				s.violate(s.name, "%s, subscriptionSource false, got %d newHeads subscribes: a source", s.upstream("ws-a"), n)
			}
			s.blocks(25)
		}}
}
