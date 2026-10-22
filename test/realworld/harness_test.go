package realworld

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
	// lookups time how long the proxies take to tell pods apart.
	lookups *lookups
	// series ends the per-second record of the pods' vote gauges;
	// seriesDone, set once it runs, closes when it ended.
	series     chan struct{}
	seriesDone chan struct{}

	mu      sync.Mutex
	pids    map[int]*pod
	proxies []*proxy
}

func newHarness(t *testing.T, dir string, withRedis bool) *harness {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, pids: make(map[int]*pod), dumped: make(map[int]bool), start: time.Now(), cpu0: selfCPU(), lookups: &lookups{},
		series: make(chan struct{})}
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

// pod is one eRPC process: the -race binary with its own YAML config.
type pod struct {
	h                               *harness
	name                            string
	idx                             int
	httpPort, grpcPort, metricsPort int
	cfgPath                         string
	logs                            []string
	cpu                             time.Duration // CPU time of every exited process of the pod

	mu     sync.Mutex
	cmd    *exec.Cmd
	exited chan struct{}
}

// startPods writes each pod's config and boots it.
func (h *harness) startPods(n int, nets []*netSpec) {
	for i := range n {
		p := &pod{h: h, idx: i, name: fmt.Sprintf("pod-%d", i+1),
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
// who is live, and the selection policy's order (triage, not a bound).
var seriesNames = []string{"erpc_source_live{", "erpc_chain_tracker_source_heads_total{", "erpc_selection_position{"}

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
	cmd := exec.Command(erpcBin, p.cfgPath)
	cmd.Dir = p.h.dir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	cmd    *exec.Cmd
	exited chan struct{}
}

func startRedis(t *testing.T, dir string) *redisServer {
	r := &redisServer{port: freePort(t)}
	log, err := os.Create(filepath.Join(dir, "redis.log"))
	if err != nil {
		t.Fatal(err)
	}
	r.cmd = exec.Command("redis-server", "--port", strconv.Itoa(r.port), "--bind", "127.0.0.1", "--save", "", "--appendonly", "no", "--dir", dir, "--protected-mode", "no")
	r.cmd.Stdout, r.cmd.Stderr = log, log
	r.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.exited = make(chan struct{})
	go func() {
		_ = r.cmd.Wait()
		_ = log.Close()
		close(r.exited)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !r.ping() {
		if time.Now().After(deadline) {
			r.stop()
			t.Fatalf("redis-server on %d does not answer PING; see %s", r.port, log.Name())
		}
		time.Sleep(50 * time.Millisecond)
	}
	return r
}

func (r *redisServer) ping() bool {
	reply, err := r.do("PING")
	return err == nil && reply == "+PONG"
}

// do sends one command and returns the first line of its reply.
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
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(line, "-") {
		return "", fmt.Errorf("redis %s: %s", args[0], line[1:])
	}
	return line, nil
}

// skewCounter writes shared counter key (without the cluster key) as a
// peer whose clock runs skew ahead would: its state with UpdatedAt now +
// skew, stored and published as data/shared_state_variable.go and
// data/redis.go do ("<cluster>/<key>:value", channel "counter:<cluster>/<key>").
func (r *redisServer) skewCounter(key string, value int64, skew time.Duration) error {
	full := clusterKey + "/" + key
	st, err := json.Marshal(map[string]interface{}{"v": value, "t": time.Now().Add(skew).UnixMilli(), "b": "skewed-peer"})
	if err != nil {
		return err
	}
	if _, err := r.do("SET", full+":value", string(st)); err != nil {
		return err
	}
	_, err = r.do("PUBLISH", "counter:"+full, string(st))
	return err
}

// networkHeadKey is the network's delivered-head counter
// (erpc/networks_registry.go).
func networkHeadKey(project, network string) string {
	return fmt.Sprintf("network/%s/%s/latestBlock", project, network)
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
	ups     []upSpec
}

// upSpec is one upstream; every pod names the same endpoint, as pods of
// one deployment do, so they share its shared-state keys.
type upSpec struct{ id, endpoint, vendor string }

// clusterKey prefixes every shared-state key of the pods.
const clusterKey = "realworld"

// podConfig is the YAML the pod's own strict loader reads. Without redis
// the pod keeps its shared state in memory.
func podConfig(p *pod, redis *redisServer, nets []*netSpec) []byte {
	type m = map[string]interface{}
	projects := map[string]m{}
	var order []string
	for _, n := range nets {
		pr := projects[n.project]
		if pr == nil {
			pr = m{"id": n.project, "networks": []m{}, "upstreams": []m{}}
			projects[n.project] = pr
			order = append(order, n.project)
		}
		if !n.minimal {
			nw := m{"architecture": "evm", "evm": m{
				"chainId":      n.chainID,
				"chainTracker": m{"reorgWindow": reorgWindow, "maxBackfill": maxBackfill.String()},
			}}
			if n.integrity {
				nw["integrity"] = m{"level": "authoritative"}
			}
			pr["networks"] = append(pr["networks"].([]m), nw)
		}
		for _, u := range n.ups {
			up := m{"id": u.id, "endpoint": u.endpoint}
			if !n.minimal {
				up["vendorName"] = u.vendor
				up["evm"] = m{"chainId": n.chainID}
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
	if redis != nil {
		cfg["database"] = m{"sharedState": m{
			"clusterKey": clusterKey,
			"connector": m{"id": "shared", "driver": "redis", "redis": m{
				"uri": fmt.Sprintf("redis://127.0.0.1:%d/0", redis.port),
			}},
		}}
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return out
}
