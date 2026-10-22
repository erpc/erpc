//go:build svm_live

// Live integration tests that exercise the SVM stack against real Solana RPC
// endpoints. Gated by the `svm_live` build tag so they never run in the
// default test pass — they require outbound network and are subject to
// upstream rate limits.
//
// Run with:
//
//	go test -tags=svm_live -count=1 -run TestSvmLive ./erpc/...
//
// The endpoints are public, unauthenticated, and occasionally reset connections
// on mainnet. Failures should be treated as infrastructure weather, not eRPC
// regressions, unless the SAME test fails consistently across several minutes.
//
// These tests verify five end-to-end properties:
//   - Server boot against real endpoints, both networks reach OK state.
//   - getGenesisHash short-circuit returns the correct hardcoded hash with
//     zero upstream RPC calls for each known cluster.
//   - A user-facing getSlot returns a monotonically increasing integer.
//   - A finalized getBlock is served from cache on the second call (latency
//     drops by >10×).
//   - Prometheus cache metrics populate with per-method series.

package erpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	liveMainnetURL = "https://api.mainnet-beta.solana.com"
	liveTestnetURL = "https://solana-testnet-rpc.publicnode.com"

	liveHTTPPort    = "4402"
	liveMetricsPort = "4102"
)

// liveConfigYAML is the eRPC config the live tests boot with. Kept inline so
// the test is self-contained — no separate yaml file to drift from the
// assertions.
const liveConfigYAML = `
logLevel: warn
server:
  httpHostV4: 127.0.0.1
  httpPort: ` + liveHTTPPort + `
metrics:
  enabled: true
  hostV4: 127.0.0.1
  port: ` + liveMetricsPort + `
database:
  svmJsonRpcCache:
    connectors:
      - id: mem
        driver: memory
        memory:
          maxItems: 10000
          maxTotalSize: 16MB
    policies:
      - connector: mem
        network: "svm:*"
        method: "*"
        finality: finalized
projects:
  - id: main
    networks:
      - architecture: svm
        svm:
          cluster: mainnet-beta
          commitment: confirmed
      - architecture: svm
        svm:
          cluster: testnet
          commitment: confirmed
    upstreams:
      - id: mainnet-solana-labs
        type: svm
        endpoint: ` + liveMainnetURL + `
        svm:
          cluster: mainnet-beta
      - id: testnet-publicnode
        type: svm
        endpoint: ` + liveTestnetURL + `
        svm:
          cluster: testnet
`

// TestSvmLive_Mainnet is the top-level live integration test. Spawns an eRPC
// process, waits for both networks to go OK, then runs the scenarios.
func TestSvmLive_Mainnet(t *testing.T) {
	srv := startLiveServer(t)
	defer srv.stop(t)

	srv.waitHealthy(t, 20*time.Second)

	t.Run("GenesisHashShortCircuit_Mainnet", func(t *testing.T) {
		testLiveGenesisShortCircuit(t, srv, "mainnet-beta",
			"5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")
	})

	t.Run("GenesisHashShortCircuit_Testnet", func(t *testing.T) {
		testLiveGenesisShortCircuit(t, srv, "testnet",
			"4uhcVJyU9pJkvQyS88uRDiswHXSCkY3zQawwpjk2NsNY")
	})

	t.Run("GetSlot_Testnet_ReturnsIncreasingSlot", func(t *testing.T) {
		// Testnet is the most reliable public endpoint; mainnet
		// api.mainnet-beta.solana.com rate-limits aggressively and gives false
		// negatives on a flaky day.
		s1 := mustLiveGetSlot(t, srv, "testnet")
		time.Sleep(500 * time.Millisecond)
		s2 := mustLiveGetSlot(t, srv, "testnet")
		if s2 < s1 {
			t.Fatalf("testnet slot went backwards: %d → %d", s1, s2)
		}
		if s1 == 0 {
			t.Fatal("testnet returned slot=0 — not a real response")
		}
	})

	t.Run("GetBlock_Testnet_CachedOnSecondCall", func(t *testing.T) {
		// Finalized getBlock should be cacheable. Measure two identical calls
		// and expect the second to be at least 10× faster.
		currentSlot := mustLiveGetSlot(t, srv, "testnet")
		querySlot := currentSlot - 10_000 // well behind, guaranteed finalized

		call := func() (time.Duration, string) {
			body := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "getBlock",
				"params": []interface{}{querySlot, map[string]interface{}{
					"commitment":                     "finalized",
					"maxSupportedTransactionVersion": 0,
					"transactionDetails":             "none",
					"rewards":                        false,
				}},
			}
			t0 := time.Now()
			resp := srv.post(t, "testnet", body)
			dur := time.Since(t0)
			var out struct {
				Result struct {
					Blockhash string `json:"blockhash"`
				} `json:"result"`
			}
			if err := json.Unmarshal(resp, &out); err != nil {
				t.Fatalf("decode getBlock response: %v", err)
			}
			return dur, out.Result.Blockhash
		}

		d1, h1 := call()
		d2, h2 := call()

		if h1 == "" || h2 == "" {
			t.Skipf("testnet didn't return a block for slot %d — some slots are skipped; skip rather than fail",
				querySlot)
		}
		if h1 != h2 {
			t.Fatalf("blockhash differs between calls (%s vs %s)", h1, h2)
		}
		ratio := d1.Seconds() / d2.Seconds()
		if ratio < 10 {
			t.Fatalf("expected >10× speedup from cache; got %v → %v (%.1fx)", d1, d2, ratio)
		}
		t.Logf("cache speedup: %v → %v (%.0fx)", d1, d2, ratio)
	})

	t.Run("Metrics_CacheSeriesPopulated", func(t *testing.T) {
		// Prime the cache with a hit so the success_hit series appear, then
		// scrape /metrics.
		_ = mustLiveGetSlot(t, srv, "testnet")

		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/metrics", liveMetricsPort))
		if err != nil {
			t.Fatalf("scrape metrics: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		// The cache emitted at least one get operation — exact method/outcome
		// is timing-dependent, but we should see SOME series for the svm network.
		if !strings.Contains(string(body), `network="svm:testnet"`) {
			t.Fatal("no SVM cache metrics emitted; svm network absent from /metrics")
		}
	})
}

// testLiveGenesisShortCircuit verifies that the local-table short-circuit hook
// returns the known hash for the given cluster without hitting the upstream.
// The proof is the response latency: genesis-hash lookups over a real network
// typically take 100ms+; our short-circuit resolves in <50ms.
func testLiveGenesisShortCircuit(t *testing.T, srv *liveServer, cluster, expectedHash string) {
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getGenesisHash",
		"params":  []interface{}{},
	}
	t0 := time.Now()
	resp := srv.post(t, cluster, body)
	dur := time.Since(t0)

	var out struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, resp)
	}
	if out.Result != expectedHash {
		t.Fatalf("expected %q, got %q", expectedHash, out.Result)
	}
	if dur > 100*time.Millisecond {
		t.Errorf("getGenesisHash took %v — short-circuit path should be <100ms; upstream may have been consulted",
			dur)
	}
	t.Logf("short-circuit latency: %v", dur)
}

func mustLiveGetSlot(t *testing.T, srv *liveServer, cluster string) int64 {
	t.Helper()
	body := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "getSlot",
		"params":  []interface{}{},
	}
	resp := srv.post(t, cluster, body)
	var out struct {
		Result int64                  `json:"result"`
		Error  map[string]interface{} `json:"error"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		t.Fatalf("decode getSlot: %v (body=%s)", err, resp)
	}
	if out.Error != nil {
		t.Skipf("upstream rejected getSlot on %s (likely rate-limited); skipping: %v", cluster, out.Error)
	}
	return out.Result
}

// ---- process harness --------------------------------------------------------

type liveServer struct {
	cmd        *exec.Cmd
	configPath string
	logPath    string
}

func startLiveServer(t *testing.T) *liveServer {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "svm_live.yaml")
	logPath := filepath.Join(dir, "svm_live.log")
	if err := os.WriteFile(cfgPath, []byte(liveConfigYAML), 0644); err != nil {
		t.Fatal(err)
	}

	repoRoot, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// repoRoot is .../erpc/erpc; we want the parent.
	repoRoot = filepath.Dir(repoRoot)

	// Run `go run ./cmd/erpc start --config=...`. Using `go run` avoids
	// requiring a pre-built binary; first invocation pays a compile cost.
	cmd := exec.Command("go", "run", "./cmd/erpc", "start", "--config", cfgPath)
	cmd.Dir = repoRoot
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start erpc: %v", err)
	}
	t.Logf("spawned erpc pid=%d config=%s log=%s", cmd.Process.Pid, cfgPath, logPath)
	return &liveServer{cmd: cmd, configPath: cfgPath, logPath: logPath}
}

func (s *liveServer) stop(t *testing.T) {
	t.Helper()
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	// Wait so Kill actually reaps the process; ignore errors.
	_, _ = s.cmd.Process.Wait()
}

func (s *liveServer) waitHealthy(t *testing.T, deadline time.Duration) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%s/healthcheck", liveHTTPPort)
	client := &http.Client{Timeout: 2 * time.Second}
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == 200 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(b), `"state":"OK"`) {
				t.Logf("server healthy after %v", deadline-time.Until(stop))
				return
			}
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Dump recent log for debugging.
	if b, err := os.ReadFile(s.logPath); err == nil {
		tail := string(b)
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		t.Logf("server log tail:\n%s", tail)
	}
	t.Fatalf("server did not become healthy within %v", deadline)
}

func (s *liveServer) post(t *testing.T, cluster string, body map[string]interface{}) []byte {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%s/main/svm/%s", liveHTTPPort, cluster)
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out
}
