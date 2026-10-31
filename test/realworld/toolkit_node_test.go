package realworld

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/erpc/erpc/sdk/go/streams"
)

// The TS library in the toolkit rows (T2, T3, T5, T6 TS): a subprocess,
// typescript/streams/test/driver.mjs, that appends the same records as
// startGoClient to the client's file.

var nodeLib struct {
	once sync.Once
	err  error
}

// buildNodeLib checks node ≥ 22 and builds the library once per run.
func buildNodeLib() error {
	nodeLib.once.Do(func() {
		out, err := exec.Command("node", "--version").Output()
		if err != nil {
			nodeLib.err = fmt.Errorf("node: %v (the TS toolkit rows need node ≥ 22)", err)
			return
		}
		v := strings.TrimSpace(string(out))
		if major, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(v, "v"), ".", 2)[0]); major < 22 {
			nodeLib.err = fmt.Errorf("node %s: the TS toolkit rows need node ≥ 22", v)
			return
		}
		root, err := repoRoot()
		if err != nil {
			nodeLib.err = err
			return
		}
		build := exec.Command("pnpm", "--dir", filepath.Join(root, "typescript", "streams"), "run", "build")
		if out, err := build.CombinedOutput(); err != nil {
			nodeLib.err = fmt.Errorf("build typescript/streams (run pnpm install first): %v\n%s", err, out)
		}
	})
	return nodeLib.err
}

// startNodeClient is the TS toolkitStarter: it runs the Node library with
// cfg until stop, which ends it with SIGTERM and waits, or until the
// scenario ends. The driver has no kill hook, so killEvery must be 0.
func (s *scenario) startNodeClient(c *toolkitClient, cfg streams.Config, cursorPath string, killEvery int) (stop func()) {
	if killEvery != 0 {
		s.t.Fatalf("%s: the node driver cannot kill the app between apply and save", c.name)
	}
	if err := buildNodeLib(); err != nil {
		s.t.Fatal(err)
	}
	root, err := repoRoot()
	if err != nil {
		s.t.Fatal(err)
	}
	ts := map[string]interface{}{"endpoints": cfg.Endpoints, "kind": cfg.Kind, "heartbeatSeconds": cfg.HeartbeatSeconds,
		"recordsPath": c.path}
	if cfg.Kind == streams.Logs {
		ts["filter"] = map[string]interface{}{"address": cfg.Filter.Address, "topics": cfg.Filter.Topics}
	}
	if cfg.Finality != "" {
		ts["finality"] = cfg.Finality
	}
	if cfg.FullTransactions {
		ts["fullTransactions"] = true
	}
	if len(cfg.Metadata) > 0 {
		ts["metadata"] = cfg.Metadata
	}
	if cursorPath != "" {
		ts["cursorPath"] = cursorPath
	}
	in, err := json.Marshal(ts)
	if err != nil {
		s.t.Fatal(err)
	}
	stderr, err := os.Create(c.path + ".stderr.log")
	if err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command("node", filepath.Join(root, "typescript", "streams", "test", "driver.mjs"))
	cmd.Stdin, cmd.Stderr = bytes.NewReader(in), stderr
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	s.logf("%s: node driver pid %d: %s", c.name, cmd.Process.Pid, in)
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = stderr.Close()
		close(exited)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				s.violate(c.name, "node driver ignored SIGTERM for 10s; see %s", stderr.Name())
				_ = cmd.Process.Kill()
				<-exited
			}
		})
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-s.ctx.Done():
			stop()
		case <-exited:
		}
	}()
	return stop
}
