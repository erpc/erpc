package realworld

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/erpc/erpc/data"
	"gopkg.in/yaml.v3"
)

// harness is one phase's real components: a redis-server (none for a lone
// pod, which keeps its shared state in memory), the pods that share it,
// and the fault proxies between the pods and the vendors.
type harness struct {
	t     *testing.T
	dir   string
	redis *redisServer
	pods  []*pod
	start time.Time
	cpu0  time.Duration
	// host labels latency verdicts taken while the host was overloaded.
	host *hostLoad
	// dumped are the pods that got a goroutine dump this phase.
	dumped map[int]bool
	// vars are the eRPC duration variables the pods run with, by name
	// (testdata/realworld_vars.go).
	vars map[string]time.Duration
	// webSocket is merged into every pod's server.webSocket config.
	webSocket map[string]interface{}
	// lookups time how long the proxies take to tell pods apart.
	lookups *lookups
	// owners tells the proxies which pod a connection comes from.
	owners *ownerTable
	// series ends the per-second record of the pods' vote gauges;
	// seriesDone, set once it runs, closes when it ended.
	series     chan struct{}
	seriesDone chan struct{}

	mu      sync.Mutex
	pids    map[int]*pod
	proxies []*proxy

	// headers is when each block header first reached each pod, by pod
	// index (headerScan); lasts, when a push of it last did (N2: pushes
	// reordered); latests, when it first reached the pod as an
	// eth_getBlockByNumber("latest") answer.
	headersMu sync.Mutex
	headers   map[int]map[hash]time.Time
	lasts     map[int]map[hash]time.Time
	latests   map[int]map[hash]time.Time
}

// sawHeader notes that a header of block bh reached pod at at, as a latest
// answer, a push, or neither; the first sighting stays, and a push's last.
func (h *harness) sawHeader(pod int, bh hash, at time.Time, latest, push bool) {
	h.headersMu.Lock()
	defer h.headersMu.Unlock()
	note := func(byPod map[int]map[hash]time.Time, keep bool) {
		m := byPod[pod]
		if m == nil {
			m = make(map[hash]time.Time)
			byPod[pod] = m
		}
		if _, ok := m[bh]; !ok || !keep {
			m[bh] = at
		}
	}
	note(h.headers, true)
	if push {
		note(h.lasts, false)
	}
	if latest {
		note(h.latests, true)
	}
}

// headerLastAt is when a push of block bh last reached pod.
func (h *harness) headerLastAt(pod int, bh hash) (time.Time, bool) {
	h.headersMu.Lock()
	defer h.headersMu.Unlock()
	at, ok := h.lasts[pod][bh]
	return at, ok
}

// headerAt is when a header of block bh first reached pod.
func (h *harness) headerAt(pod int, bh hash) (time.Time, bool) {
	h.headersMu.Lock()
	defer h.headersMu.Unlock()
	at, ok := h.headers[pod][bh]
	return at, ok
}

// latestAt is when block bh first reached pod as a latest answer.
func (h *harness) latestAt(pod int, bh hash) (time.Time, bool) {
	h.headersMu.Lock()
	defer h.headersMu.Unlock()
	at, ok := h.latests[pod][bh]
	return at, ok
}

func newHarness(t *testing.T, dir string, withRedis bool) *harness {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, pids: make(map[int]*pod), dumped: make(map[int]bool), start: time.Now(), cpu0: selfCPU(), lookups: &lookups{}, owners: newOwnerTable(),
		series: make(chan struct{}), headers: make(map[int]map[hash]time.Time), lasts: make(map[int]map[hash]time.Time),
		latests: make(map[int]map[hash]time.Time)}
	if withRedis {
		h.redis = startRedis(t, dir)
	}
	t.Cleanup(h.stop)
	return h
}

// selfCPU is the CPU time this test process used so far.
func selfCPU() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// stop kills every pod and redis, closes the proxies, and fails the phase
// if a pod's output holds a data race or a panic.
func (h *harness) stop() {
	close(h.series)
	if h.seriesDone != nil {
		<-h.seriesDone
	}
	for _, p := range h.pods {
		p.kill()
	}
	h.mu.Lock()
	proxies := h.proxies
	h.mu.Unlock()
	for _, px := range proxies {
		px.close()
	}
	if h.redis != nil {
		h.redis.stop()
	}
	wall := time.Since(h.start)
	h.t.Logf("the test process (harness and simulators of every phase running beside this one) used %s CPU over this phase's %s wall; proxies: %s", (selfCPU() - h.cpu0).Round(time.Second), wall.Round(time.Second), h.lookups.summary())
	for _, p := range h.pods {
		p.mu.Lock()
		h.t.Logf("%s used %s CPU (%.1f cores)", p.name, p.cpu.Round(time.Second), p.cpu.Seconds()/wall.Seconds())
		p.mu.Unlock()
	}
	for _, p := range h.pods {
		for _, f := range p.logs {
			for _, hit := range scanFatal(f) {
				h.t.Errorf("%s: %s", filepath.Base(f), hit)
			}
		}
	}
}

// podOfPid is the pod that runs process pid, if any.
func (h *harness) podOfPid(pid int) *pod {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pids[pid]
}

// podPids are the pids of the running pods.
func (h *harness) podPids() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	pids := make([]int, 0, len(h.pids))
	for pid := range h.pids {
		pids = append(pids, pid)
	}
	return pids
}

// forget drops an exited pod process from the pid table.
func (h *harness) forget(pid int) {
	h.mu.Lock()
	delete(h.pids, pid)
	h.mu.Unlock()
}

// claimDump reports whether pod may get this phase's one goroutine dump.
func (h *harness) claimDump(pod int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.dumped[pod] {
		return false
	}
	h.dumped[pod] = true
	return true
}

// freePort is a port no listener holds on any address and no phase of this
// run was handed before: phases run concurrently, and a port probed free
// stays unbound until its pod or redis boots. Pods bind HTTP and gRPC on
// 127.0.0.1 and metrics on every interface. On macOS a ":0" probe (the
// IPv6 dual-stack wildcard) may return a port an IPv4 listener holds, and
// the pod's wildcard bind then succeeds beside it while that listener
// takes every 127.0.0.1 connection (N6: pod-1's scrapes got EOF all
// phase), so the port is probed on the IPv4 addresses too.
func freePort(t *testing.T) int {
	for {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatal(err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
		if ipv4Free(port) && handedPorts.claim(port) {
			return port
		}
	}
}

// handedPorts are the ports freePort returned in this process.
var handedPorts = &portSet{ports: map[int]bool{}}

type portSet struct {
	mu    sync.Mutex
	ports map[int]bool
}

// claim reports whether port was not handed out before, and marks it.
func (ps *portSet) claim(port int) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.ports[port] {
		return false
	}
	ps.ports[port] = true
	return true
}

func ipv4Free(port int) bool {
	for _, host := range []string{"0.0.0.0", "127.0.0.1"} {
		ln, err := net.Listen("tcp4", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		_ = ln.Close()
	}
	return true
}

// bootAttempts: a pod whose port another process took between freePort
// and its boot is booted again on new ports (harness).
const bootAttempts = 3

// bindFailed reports whether the pod's last boot failed on a port another
// process holds.
func (p *pod) bindFailed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.logs) == 0 {
		return false
	}
	b, err := os.ReadFile(p.logs[len(p.logs)-1])
	return err == nil && strings.Contains(string(b), "address already in use")
}

// scanFatal returns the lines of a pod log that fail a run: a data race
// report or a Go panic.
func scanFatal(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var hits []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "WARNING: DATA RACE") || strings.HasPrefix(line, "panic: ") || strings.HasPrefix(line, "fatal error: ") {
			hits = append(hits, line)
		}
	}
	return hits
}

//
// --- pods ---
//

// pod is one eRPC process: erpcBin with its own YAML config, or
// the old image (X7).
type pod struct {
	h    *harness
	name string
	idx  int
	old  bool
	// counters carries each counter series across the pod's restarts
	// (pod.counted).
	counters                        counterCarry
	httpPort, grpcPort, metricsPort int
	cfgPath                         string
	logs                            []string
	cpu                             time.Duration // CPU time of every exited process of the pod

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
}

// startPods writes each pod's config and boots it, except cold pods, which
// a scenario boots later (pod.start); pods in old run the old image.
func (h *harness) startPods(n int, old, cold []int, nets []*netSpec) {
	for i := range n {
		p := &pod{h: h, idx: i, name: fmt.Sprintf("pod-%d", i+1), old: slices.Contains(old, i)}
		p.cfgPath = filepath.Join(h.dir, p.name+".yaml")
		h.pods = append(h.pods, p)
	}
	configure := func(p *pod) {
		p.httpPort, p.grpcPort, p.metricsPort = freePort(h.t), freePort(h.t), freePort(h.t)
		if err := os.WriteFile(p.cfgPath, podConfig(p, h.redis, nets), 0o644); err != nil {
			h.t.Error(err)
		}
	}
	for _, p := range h.pods {
		configure(p)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, p := range h.pods {
		if slices.Contains(cold, i) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 1; ; attempt++ {
				if errs[i] = p.start(); errs[i] == nil || attempt == bootAttempts || !p.bindFailed() {
					return
				}
				configure(p)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			h.t.Fatal(err)
		}
	}
	h.seriesDone = make(chan struct{})
	go h.recordSeries()
}

// seriesNames are the gauges and counters recordSeries keeps: who votes,
// how old each report is, the selection policy's order, and the tracker's re-anchors,
// resets and tap ends (triage, not a bound).
var seriesNames = []string{"erpc_upstream_report_age_seconds{", "erpc_chain_tracker_source_heads_total{", "erpc_selection_position{",
	"erpc_chain_tracker_reanchor_total{", "erpc_chain_tracker_reset_total{", "erpc_tap_end_total{"}

const seriesEvery = 2 * time.Second

// recordSeries appends seriesNames' samples of every running pod to
// series.log every seriesEvery, so a vote or selection change can be
// dated. A scrape renders about 1 MB, so it runs no faster.
func (h *harness) recordSeries() {
	defer close(h.seriesDone)
	f, err := os.Create(filepath.Join(h.dir, "series.log"))
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	tick := time.NewTicker(seriesEvery)
	defer tick.Stop()
	for {
		select {
		case <-h.series:
			return
		case <-tick.C:
		}
		for _, p := range h.pods {
			if !p.running() {
				continue
			}
			gen := p.counters.generation()
			body, err := p.metricsText()
			if err != nil {
				continue
			}
			// Each read keeps a crashed process's counters as of its
			// last seriesEvery.
			_, _ = p.counted(gen, body)
			at := time.Now().Format("15:04:05.000")
			for _, line := range strings.Split(string(body), "\n") {
				for _, name := range seriesNames {
					if strings.HasPrefix(line, name) {
						fmt.Fprintf(w, "%s %s %s\n", at, p.name, line)
					}
				}
			}
		}
	}
}

// start boots the pod from its config and waits until its HTTP, gRPC and
// metrics ports accept connections.
func (p *pod) start() error {
	p.counters.restart()
	p.mu.Lock()
	gen := len(p.logs)/2 + 1
	base := filepath.Join(p.h.dir, fmt.Sprintf("%s.%d", p.name, gen))
	stdout, err := os.Create(base + ".stdout.log")
	if err != nil {
		p.mu.Unlock()
		return err
	}
	stderr, err := os.Create(base + ".stderr.log")
	if err != nil {
		p.mu.Unlock()
		return err
	}
	p.logs = append(p.logs, stdout.Name(), stderr.Name())
	bin := erpcBin
	if p.old {
		bin = oldBin
	}
	cmd := exec.Command(bin, p.cfgPath)
	cmd.Dir = p.h.dir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The old image has no realworld_vars.go to read them.
	if len(p.h.vars) > 0 && !p.old {
		var kvs []string
		for name, d := range p.h.vars {
			kvs = append(kvs, name+"="+d.String())
		}
		cmd.Env = append(os.Environ(), "REALWORLD_VARS="+strings.Join(kvs, ","))
	}
	if err := cmd.Start(); err != nil {
		p.mu.Unlock()
		return err
	}
	exited := make(chan struct{})
	p.cmd, p.exited = cmd, exited
	p.mu.Unlock()
	p.h.mu.Lock()
	p.h.pids[cmd.Process.Pid] = p
	p.h.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		p.mu.Lock()
		p.cpu += cmd.ProcessState.UserTime() + cmd.ProcessState.SystemTime()
		p.mu.Unlock()
		_ = stdout.Close()
		_ = stderr.Close()
		close(exited)
	}()
	deadline := time.Now().Add(podBootWait)
	for _, port := range []int{p.httpPort, p.grpcPort, p.metricsPort} {
		for {
			select {
			case <-exited:
				return fmt.Errorf("%s exited while booting; see %s", p.name, stderr.Name())
			default:
			}
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
			if err == nil {
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: port %d not open after %s; see %s", p.name, port, podBootWait, stderr.Name())
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// kill saves the pod's metrics, then sends SIGKILL to the pod's process
// group and waits for it.
func (p *pod) kill() {
	p.saveMetrics()
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.cmd = nil
	p.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-exited
	p.h.forget(cmd.Process.Pid)
}

// quit saves the pod's metrics, then sends SIGQUIT to the pod: the Go
// runtime writes every goroutine's stack to the pod's stderr log, whose
// path quit returns, and exits.
func (p *pod) quit() string {
	p.saveMetrics()
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.cmd = nil
	log := p.logs[len(p.logs)-1]
	p.mu.Unlock()
	if cmd == nil {
		return ""
	}
	_ = cmd.Process.Signal(syscall.SIGQUIT)
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}
	p.h.forget(cmd.Process.Pid)
	return log
}

// saveMetrics writes the running process's /metrics next to its logs, so
// counters such as selection exclusions survive the process (triage), and
// keeps its counters' last values for the next process's reads.
func (p *pod) saveMetrics() {
	p.mu.Lock()
	if p.cmd == nil {
		p.mu.Unlock()
		return
	}
	path := strings.TrimSuffix(p.logs[len(p.logs)-1], ".stderr.log") + ".metrics.prom"
	p.mu.Unlock()
	gen := p.counters.generation()
	if body, err := p.metricsText(); err == nil {
		_ = os.WriteFile(path, body, 0o644)
		_, _ = p.counted(gen, body)
	}
}

// metricsText is the pod's /metrics body.
func (p *pod) metricsText() ([]byte, error) {
	resp, err := scrapeClient.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", p.metricsPort))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (p *pod) running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd != nil
}

func (p *pod) httpURL(project string, chainID int64) string {
	return fmt.Sprintf("http://127.0.0.1:%d/%s/evm/%d", p.httpPort, project, chainID)
}

func (p *pod) wsURL(project string, chainID int64) string {
	return fmt.Sprintf("ws://127.0.0.1:%d/%s/evm/%d", p.httpPort, project, chainID)
}

func (p *pod) grpcAddr() string { return fmt.Sprintf("127.0.0.1:%d", p.grpcPort) }

//
// --- metrics ---
//

type series struct {
	name   string
	labels map[string]string
	value  float64
}

var scrapeClient = &http.Client{Timeout: 5 * time.Second}

// scrape reads the pod's /metrics; a counter adds its values before each
// restart of the pod (counted).
func (p *pod) scrape() ([]series, error) {
	gen := p.counters.generation()
	body, err := p.metricsText()
	if err != nil {
		return nil, err
	}
	return p.counted(gen, body)
}

// counterCarry holds a pod's counters across its process restarts, as
// Prometheus increase() treats a counter reset: carried sums each series'
// last value in every ended process, latest is the running process's, and
// gen counts the processes, so a body read from an ended one is told apart.
type counterCarry struct {
	mu      sync.Mutex
	gen     int
	latest  map[string]series
	carried map[string]series
}

func (c *counterCarry) generation() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// restart ends the running process's generation: its last read counters
// join carried.
func (c *counterCarry) restart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.carried == nil {
		c.carried = map[string]series{}
	}
	for k, s := range c.latest {
		if prev, ok := c.carried[k]; ok {
			s.value += prev.value
		}
		c.carried[k] = s
	}
	c.latest = nil
	c.gen++
}

// carriedSum adds the carried values of name's series whose labels include
// match: what the pod's ended processes counted.
func (c *counterCarry) carriedSum(name string, match map[string]string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	ss := make([]series, 0, len(c.carried))
	for _, s := range c.carried {
		ss = append(ss, s)
	}
	v, _ := sumOf(ss, name, match)
	return v
}

// counted parses a /metrics body the pod's process of generation gen
// rendered. Each counter sample adds the carried value of its series, and
// a carried series the process has not exported yet is added as carried;
// gauges, histograms and summaries stay as read.
func (p *pod) counted(gen int, body []byte) ([]series, error) {
	isCounter := map[string]bool{}
	var out []series
	for _, line := range strings.Split(string(body), "\n") {
		if name, ok := strings.CutPrefix(line, "# TYPE "); ok {
			if name, ok = strings.CutSuffix(name, " counter"); ok {
				isCounter[name] = true
			}
		}
		if line == "" || line[0] == '#' {
			continue
		}
		s, ok := parseSeries(line)
		if ok {
			out = append(out, s)
		}
	}
	c := &p.counters
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return nil, fmt.Errorf("%s restarted during the scrape", p.name)
	}
	latest := map[string]series{}
	for i, s := range out {
		if !isCounter[s.name] {
			continue
		}
		k := s.key()
		latest[k] = s
		if prev, ok := c.carried[k]; ok {
			out[i].value += prev.value
		}
	}
	c.latest = latest
	for k, s := range c.carried {
		if _, ok := latest[k]; !ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// key names a series by its name and sorted labels.
func (s series) key() string {
	keys := make([]string, 0, len(s.labels))
	for k := range s.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.name)
	for _, k := range keys {
		fmt.Fprintf(&b, "|%s=%s", k, s.labels[k])
	}
	return b.String()
}

// parseSeries parses one text-format sample: name{k="v",...} value.
func parseSeries(line string) (series, bool) {
	s := series{labels: map[string]string{}}
	i := strings.IndexAny(line, "{ ")
	if i < 0 {
		return s, false
	}
	s.name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		rest = rest[1:]
		for len(rest) > 0 && rest[0] != '}' {
			eq := strings.IndexByte(rest, '=')
			if eq < 0 || eq+1 >= len(rest) || rest[eq+1] != '"' {
				return s, false
			}
			key := rest[:eq]
			rest = rest[eq+2:]
			var val strings.Builder
			for len(rest) > 0 && rest[0] != '"' {
				if rest[0] == '\\' && len(rest) > 1 {
					rest = rest[1:]
				}
				val.WriteByte(rest[0])
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return s, false
			}
			s.labels[key] = val.String()
			rest = strings.TrimPrefix(rest[1:], ",")
		}
		if len(rest) == 0 {
			return s, false
		}
		rest = rest[1:]
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return s, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, false
	}
	s.value = v
	return s, true
}

// sumOf adds the samples of name whose labels include match.
func sumOf(ss []series, name string, match map[string]string) (float64, bool) {
	var sum float64
	found := false
	for _, s := range ss {
		if s.name != name {
			continue
		}
		ok := true
		for k, v := range match {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			sum += s.value
			found = true
		}
	}
	return sum, found
}

//
// --- redis ---
//

type redisServer struct {
	port   int
	dir    string
	cmd    *exec.Cmd
	exited chan struct{}
}

func startRedis(t *testing.T, dir string) *redisServer {
	r := &redisServer{port: freePort(t), dir: dir}
	if err := r.launch(); err != nil {
		t.Fatal(err)
	}
	return r
}

// launch starts redis-server on r's port, with no data, and waits until it
// answers PING.
func (r *redisServer) launch() error {
	log, err := os.OpenFile(filepath.Join(r.dir, "redis.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command("redis-server", "--port", strconv.Itoa(r.port), "--bind", "127.0.0.1", "--save", "", "--appendonly", "no", "--dir", r.dir, "--protected-mode", "no")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
		close(exited)
	}()
	r.cmd, r.exited = cmd, exited
	// Harness slack: redis-server took 7.5s to initialize beside 18 phases
	// booting at load 120.
	deadline := time.Now().Add(30 * time.Second)
	for !r.ping() {
		if time.Now().After(deadline) {
			r.stop()
			return fmt.Errorf("redis-server on %d does not answer PING; see %s", r.port, log.Name())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// down stops redis-server for d, then starts it again on its port with no
// data, as a redis without persistence comes back (§19 N6).
func (r *redisServer) down(d time.Duration) error {
	r.stop()
	time.Sleep(d)
	return r.launch()
}

func (r *redisServer) ping() bool {
	reply, err := r.do("PING")
	return err == nil && reply == "+PONG"
}

// errRedisNil is a GET of a key redis does not hold.
var errRedisNil = errors.New("redis: nil")

// do sends one command and returns its reply's first line, or a bulk
// reply's value.
func (r *redisServer) do(args ...string) (string, error) {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", r.port), time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(c, b.String()); err != nil {
		return "", err
	}
	rd := bufio.NewReader(c)
	line, err := rd.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "-"):
		return "", fmt.Errorf("redis %s: %s", args[0], line[1:])
	case line == "$-1":
		return "", errRedisNil
	case strings.HasPrefix(line, "$"):
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return "", fmt.Errorf("redis %s: bulk reply %q", args[0], line)
		}
		body := make([]byte, n+2)
		if _, err := io.ReadFull(rd, body); err != nil {
			return "", err
		}
		return string(body[:n]), nil
	}
	return line, nil
}

// upstreamCounterKey is upstream u's shared counter of kind (latestBlock,
// finalizedBlock), without the cluster key, as
// architecture/evm/evm_state_poller.go names it from
// common.UniqueUpstreamKey: <schema>/<kind>/<id>/sha256(id, endpoint).
func upstreamCounterKey(u upSpec, kind string) string {
	sum := sha256.Sum256([]byte(u.id + u.endpoint))
	return fmt.Sprintf("%s/%s/%s/%s", data.CounterValueSchemaVersion, kind, u.id, hex.EncodeToString(sum[:]))
}

// counter is the state shared counter key (without the cluster key) holds,
// as data/shared_state_variable.go stores it at "<cluster>/<key>:value".
func (r *redisServer) counter(key string) (data.CounterInt64State, error) {
	var st data.CounterInt64State
	raw, err := r.do("GET", clusterKey+"/"+key+":value")
	if err == nil {
		err = json.Unmarshal([]byte(raw), &st)
	}
	return st, err
}

// writeCounter stores and publishes shared counter key (without the cluster
// key) as another instance does (data/shared_state_variable.go
// updateRemoteState, data/redis.go): value with UpdatedAt at, by writer,
// at "<cluster>/<key>:value" and on channel "counter:<cluster>/<key>".
func (r *redisServer) writeCounter(key string, value int64, at time.Time, writer string) error {
	full := clusterKey + "/" + key
	st, err := json.Marshal(data.CounterInt64State{Value: value, UpdatedAt: at.UnixMilli(), UpdatedBy: writer})
	if err != nil {
		return err
	}
	if _, err := r.do("SET", full+":value", string(st)); err != nil {
		return err
	}
	_, err = r.do("PUBLISH", "counter:"+full, string(st))
	return err
}

func (r *redisServer) stop() {
	if r.cmd == nil {
		return
	}
	_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
	<-r.exited
	r.cmd = nil
}

//
// --- config ---
//

// netSpec is one scenario network as each pod's config declares it.
type netSpec struct {
	project   string
	chainID   int64
	integrity bool
	// minimal: no network entry, and upstreams with only id and endpoint
	// (K2).
	minimal bool
	// evm and extra are merged into the network's evm block and entry
	// (servedTip, shortCircuitFutureBlocks, failover, …).
	evm, extra map[string]interface{}
	// cache: the network's answers are cached, finalized ones in
	// cacheFinal (no expiry), unfinalized ones in cacheRecent.
	cache bool
	ups   []upSpec
	// integrityOff are pods whose config leaves integrity off (K5);
	// checks are integrity checks enabled beyond the level's.
	integrityOff []int
	checks       []string
}

// upSpec is one upstream; every pod names the same endpoint, as pods of
// one deployment do, so they share its shared-state keys.
type upSpec struct {
	id, endpoint, vendor string
	tags                 []string
	// evm is merged into the upstream's evm block (statePollerInterval, …),
	// extra into the upstream entry (failsafe, …).
	evm, extra map[string]interface{}
	// budget is the upstream's rateLimitBudget in requests per second on
	// each pod; 0 none.
	budget uint32
}

// The evmJsonRpcCache connectors of networks with cache: permanent
// (finalized) and recent (unfinalized) entries, told apart by the
// connector label of erpc_cache_set_success_total.
const (
	cacheFinal  = "final"
	cacheRecent = "recent"
)

// clusterKey prefixes every shared-state key of the pods.
const clusterKey = "realworld"

// podConfig is the YAML the pod's own strict loader reads. Without redis
// the pod keeps its shared state in memory.
func podConfig(p *pod, redis *redisServer, nets []*netSpec) []byte {
	type m = map[string]interface{}
	projects := map[string]m{}
	var order []string
	var policies, budgets []m
	for _, n := range nets {
		pr := projects[n.project]
		if pr == nil {
			pr = m{"id": n.project, "networks": []m{}, "upstreams": []m{}}
			projects[n.project] = pr
			order = append(order, n.project)
		}
		if !n.minimal {
			evm := m{"chainId": n.chainID}
			// The old image decodes config strictly and has no chainTracker
			// (§18 rollout: new keys only after the old image is gone).
			if !p.old {
				evm["chainTracker"] = m{"reorgWindow": reorgWindow, "maxBackfill": maxBackfill.String()}
			}
			for k, v := range n.evm {
				evm[k] = v
			}
			nw := m{"architecture": "evm", "evm": evm}
			if n.integrity && !slices.Contains(n.integrityOff, p.idx) {
				integ := m{"level": "authoritative"}
				if len(n.checks) > 0 {
					cs := m{}
					for _, id := range n.checks {
						cs[id] = m{"enabled": true}
					}
					integ["checks"] = cs
				}
				nw["integrity"] = integ
			}
			for k, v := range n.extra {
				nw[k] = v
			}
			pr["networks"] = append(pr["networks"].([]m), nw)
		}
		if n.cache {
			network := fmt.Sprintf("evm:%d", n.chainID)
			// eth_call(latest) is realtime (#560); its key is the number the
			// request ran at, so P6 can hit it by that number.
			policies = append(policies,
				m{"network": network, "method": "*", "finality": "finalized", "connector": cacheFinal},
				m{"network": network, "method": "*", "finality": "unfinalized", "connector": cacheRecent, "ttl": "1m"},
				m{"network": network, "method": "eth_call", "finality": "realtime", "connector": cacheRecent, "ttl": "1m"})
		}
		for _, u := range n.ups {
			up := m{"id": u.id, "endpoint": u.endpoint}
			if !n.minimal {
				up["vendorName"] = u.vendor
				upEvm := m{"chainId": n.chainID}
				for k, v := range u.evm {
					upEvm[k] = v
				}
				up["evm"] = upEvm
				if len(u.tags) > 0 {
					up["tags"] = u.tags
				}
			}
			for k, v := range u.extra {
				up[k] = v
			}
			if u.budget > 0 {
				up["rateLimitBudget"] = u.id
				budgets = append(budgets, m{"id": u.id, "rules": []m{{"method": "*", "maxCount": u.budget, "period": "second"}}})
			}
			pr["upstreams"] = append(pr["upstreams"].([]m), up)
		}
	}
	var prs []m
	for _, id := range order {
		pr := projects[id]
		if len(pr["networks"].([]m)) == 0 {
			delete(pr, "networks")
		}
		prs = append(prs, pr)
	}
	cfg := m{
		"logLevel": "info",
		"server": m{
			"listenV4": true, "httpHostV4": "127.0.0.1", "httpPortV4": p.httpPort,
			"grpcEnabled": true, "grpcHostV4": "127.0.0.1", "grpcPortV4": p.grpcPort,
			"webSocket": webSocketConfig(p.h.webSocket),
		},
		"metrics":  m{"enabled": true, "listenV4": true, "hostV4": "127.0.0.1", "port": p.metricsPort},
		"projects": prs,
	}
	if len(budgets) > 0 {
		cfg["rateLimiters"] = m{"budgets": budgets}
	}
	db := m{}
	if redis != nil {
		db["sharedState"] = m{
			"clusterKey": clusterKey,
			"connector": m{"id": "shared", "driver": "redis", "redis": m{
				"uri": fmt.Sprintf("redis://127.0.0.1:%d/0", redis.port),
			}},
		}
	}
	if len(policies) > 0 {
		mem := m{"maxItems": 100000, "maxTotalSize": "256MB"}
		db["evmJsonRpcCache"] = m{
			"connectors": []m{
				{"id": cacheFinal, "driver": "memory", "memory": mem},
				{"id": cacheRecent, "driver": "memory", "memory": mem},
			},
			"policies": policies,
		}
	}
	if len(db) > 0 {
		cfg["database"] = db
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return out
}

// webSocketConfig is server.webSocket: enabled, plus extra keys.
func webSocketConfig(extra map[string]interface{}) map[string]interface{} {
	ws := map[string]interface{}{"enabled": true}
	for k, v := range extra {
		ws[k] = v
	}
	return ws
}
