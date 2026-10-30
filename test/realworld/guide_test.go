package realworld

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/erpc/erpc/test/chainsim"
)

// T7 (specs/streams/feature.md §13): every code example of the streams
// integration guide (docs/pages/operation/streams-integration.mdx; `make
// check-streams-guide` holds the page to these files) runs against the
// pods and exits 0, twice in one directory, so the second run resumes from
// the cursor the first saved. The Python, Rust and Java examples build on
// the manifesto's stubs in MANIFESTO_DIR.

// guideRunTimeout bounds one run of one example, its first blocks
// included.
const guideRunTimeout = 3 * time.Minute

// guideExample is one guide example: how to run it, from the dir its
// cursor file lives in.
type guideExample struct {
	name string
	argv []string
	env  []string
}

func t7() spec {
	return spec{name: "T7", bound: healthyDelivery, clients: func(*scenario) {}, body: func(s *scenario) {
		examples, err := s.buildGuideExamples()
		if err != nil {
			s.violate(s.name, "build: %v", err)
			return
		}
		s.blocks(10)
		for _, ex := range examples {
			dir := filepath.Join(s.dir, ex.name)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				s.t.Fatal(err)
			}
			for run := 1; run <= 2; run++ {
				if out, err := s.runGuideExample(ex, dir, run); err != nil {
					s.violate(s.name+"/"+ex.name, "run %d: %v\n%s", run, err, tail(out, 2048))
				}
			}
		}
	}}
}

// buildGuideExamples builds every example once and returns how to run
// each against pod-1 (the merges against both pods).
func (s *scenario) buildGuideExamples() ([]guideExample, error) {
	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	manifesto := os.Getenv("MANIFESTO_DIR")
	if manifesto == "" {
		return nil, fmt.Errorf("MANIFESTO_DIR unset: T7 needs a manifesto checkout with the python, rust and java stubs")
	}
	if err := buildNodeLib(); err != nil {
		return nil, err
	}
	bin := filepath.Join(s.dir, "bin")
	guide := filepath.Join(root, "docs", "examples", "streams")
	cargoTarget := filepath.Join(os.TempDir(), "erpc-realworld-guide-cargo")
	steps := [][]string{
		{filepath.Join(root, "sdk", "go", "streams"), "go", "build", "-o", bin + "/", "./examples/..."},
		{s.dir, "uv", "venv", "-q", filepath.Join(s.dir, "venv")},
		{s.dir, "uv", "pip", "install", "-q", "--python", filepath.Join(s.dir, "venv", "bin", "python"),
			"grpcio", "grpcio-status", filepath.Join(manifesto, "python")},
		{s.dir, "env", "CARGO_TARGET_DIR=" + cargoTarget, "cargo", "build", "-q",
			"--manifest-path", filepath.Join(guide, "rust", "Cargo.toml"),
			"--config", fmt.Sprintf(`patch."https://github.com/blockchain-data-standards/manifesto".bds.path=%q`, filepath.Join(manifesto, "rust"))},
		{s.dir, "mvn", "-q", "-f", filepath.Join(manifesto, "java", "pom.xml"), "install", "-DskipTests"},
		{s.dir, "mvn", "-q", "-f", filepath.Join(guide, "java", "pom.xml"), "compile"},
	}
	for _, st := range steps {
		cmd := exec.CommandContext(s.ctx, st[1], st[2:]...)
		cmd.Dir = st[0]
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("%v: %v\n%s", st[1:], err, tail(out, 2048))
		}
	}

	one := "ERPC_ENDPOINT=" + s.grpcEndpoint(0)
	both := "ERPC_ENDPOINTS=" + s.grpcEndpoint(0) + "," + s.grpcEndpoint(1)
	logs := []string{one, "BLOCKS=3"}
	for _, f := range chainsim.Filters {
		if f.Address != nil {
			logs = append(logs, "ERPC_ADDRESS="+f.Address.Hex())
			break
		}
	}
	ts := filepath.Join(root, "typescript", "streams", "examples")
	return []guideExample{
		{"go-logs", []string{filepath.Join(bin, "logs")}, logs},
		{"go-merge", []string{filepath.Join(bin, "merge")}, []string{both, "BLOCKS=3"}},
		{"go-pending", []string{filepath.Join(bin, "pending")}, []string{one, "BATCHES=1"}},
		{"ts-logs", []string{"node", filepath.Join(ts, "logs.mts")}, logs},
		{"ts-merge", []string{"node", filepath.Join(ts, "merge.mts")}, []string{both, "BLOCKS=3"}},
		{"ts-pending", []string{"node", filepath.Join(ts, "pending.mts")}, []string{one, "BATCHES=1"}},
		{"python", []string{filepath.Join(s.dir, "venv", "bin", "python"), filepath.Join(guide, "python", "stream_logs.py")}, logs},
		{"rust", []string{filepath.Join(cargoTarget, "debug", "stream-logs")}, logs},
		{"java", []string{"mvn", "-q", "-f", filepath.Join(guide, "java", "pom.xml"), "exec:java"}, logs},
		{"ws", []string{"node", filepath.Join(guide, "ws", "ws_extended.mjs")},
			append([]string{"ERPC_WS=" + s.h.pods[0].wsURL(s.project, s.chain.ID())}, logs[1:]...)},
	}, nil
}

// runGuideExample runs ex in dir until it exits, at most guideRunTimeout.
func (s *scenario) runGuideExample(ex guideExample, dir string, run int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(s.ctx, guideRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ex.argv[0], ex.argv[1:]...)
	cmd.Dir, cmd.Env = dir, append(os.Environ(), ex.env...)
	began := time.Now()
	out, err := cmd.CombinedOutput()
	if werr := os.WriteFile(filepath.Join(dir, fmt.Sprintf("run-%d.log", run)), out, 0o644); werr != nil {
		s.t.Fatal(werr)
	}
	s.logf("%s/%s run %d: %v in %s", s.name, ex.name, run, err, time.Since(began).Round(time.Millisecond))
	return out, err
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}
