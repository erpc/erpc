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
	// lookups time how long the proxies take to tell pods apart.
	lookups *lookups
	// series ends the per-second record of the pods' vote gauges;
	// seriesDone, set once it runs, closes when it ended.
	series     chan struct{}
	seriesDone chan struct{}

	mu      sync.Mutex
	pids    map[int]*pod
	proxies []*proxy

	// headers is when each block header first reached each pod, by pod
	// index (headerScan); latests, when it first reached the pod as an
	// eth_getBlockByNumber("latest") answer.
	headersMu sync.Mutex
	headers   map[int]map[hash]time.Time
	latests   map[int]map[hash]time.Time
}

// sawHeader notes that a header of block bh reached pod at at, as a latest
// answer or not; the first sighting stays.
func (h *harness) sawHeader(pod int, bh hash, at time.Time, latest bool) {
	h.headersMu.Lock()
	defer h.headersMu.Unlock()
	first := func(byPod map[int]map[hash]time.Time) {
		m := byPod[pod]
		if m == nil {
			m = make(map[hash]time.Time)
			byPod[pod] = m
		}
		if _, ok := m[bh]; !ok {
			m[bh] = at
		}
	}
	first(h.headers)
	if latest {
		first(h.latests)
	}
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
	h := &harness{t: t, dir: dir, pids: make(map[int]*pod), dumped: make(map[int]bool), start: time.Now(), cpu0: selfCPU(), lookups: &lookups{},
		series: make(chan struct{}), headers: make(map[int]map[hash]time.Time), latests: make(map[int]map[hash]time.Time)}
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
	h.t.Logf("phase used %s CPU in the harness and simulators over %s wall; proxies: %s", (selfCPU() - h.cpu0).Round(time.Second), wall.Round(time.Second), h.lookups.summary())
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

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
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

// pod is one eRPC process: the -race binary with its own YAML config, or
// the old image (X7).
type pod struct {
	h                               *harness
	name                            string
	idx                             int
	old                             bool
	httpPort, grpcPort, metricsPort int
	cfgPath                         string
	logs                            []string
	cpu                             time.Duration // CPU time of every exited process of the pod

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
}

// startPods writes each pod's config and boots it; pods in old run the old
// image.
func (h *harness) startPods(n int, old []int, nets []*netSpec) {
	for i := range n {
		p := &pod{h: h, idx: i, name: fmt.Sprintf("pod-%d", i+1), old: slices.Contains(old, i),
			httpPort: freePort(h.t), grpcPort: freePort(h.t), metricsPort: freePort(h.t)}
		p.cfgPath = filepath.Join(h.dir, p.name+".yaml")
		if err := os.WriteFile(p.cfgPath, podConfig(p, h.redis, nets), 0o644); err != nil {
			h.t.Fatal(err)
		}
		h.pods = append(h.pods, p)
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, p := range h.pods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = p.start()
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
// resets and out-of-range tap ends (triage, not a bound).
var seriesNames = []string{"erpc_upstream_report_age_seconds{", "erpc_chain_tracker_source_heads_total{", "erpc_selection_position{",
	"erpc_chain_tracker_reanchor_total{", "erpc_chain_tracker_reset_total{", "erpc_tap_out_of_range_total{"}

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
			body, err := p.metricsText()
			if err != nil {
				continue
			}
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
// counters such as selection exclusions survive the process (triage).
func (p *pod) saveMetrics() {
	p.mu.Lock()
	if p.cmd == nil {
		p.mu.Unlock()
		return
	}
	path := strings.TrimSuffix(p.logs[len(p.logs)-1], ".stderr.log") + ".metrics.prom"
	p.mu.Unlock()
	if body, err := p.metricsText(); err == nil {
		_ = os.WriteFile(path, body, 0o644)
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

// scrape reads the pod's /metrics.
func (p *pod) scrape() ([]series, error) {
	body, err := p.metricsText()
	if err != nil {
		return nil, err
	}
	var out []series
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		s, ok := parseSeries(line)
		if ok {
			out = append(out, s)
		}
	}
	return out, nil
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
	deadline := time.Now().Add(10 * time.Second)
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
			"webSocket": m{"enabled": true},
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
