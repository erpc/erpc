package realworld

import (
	"bufio"
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

// harness is one phase's real components: a redis-server, the pods that
// share it, and the fault proxies between the pods and the vendors.
type harness struct {
	t     *testing.T
	dir   string
	redis *redisServer
	pods  []*pod

	mu      sync.Mutex
	pids    map[int]*pod
	proxies []*proxy
}

func newHarness(t *testing.T, dir string) *harness {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, dir: dir, pids: make(map[int]*pod)}
	h.redis = startRedis(t, dir)
	t.Cleanup(h.stop)
	return h
}

// stop kills every pod and redis, closes the proxies, and fails the phase
// if a pod's output holds a data race or a panic.
func (h *harness) stop() {
	for _, p := range h.pods {
		p.kill()
	}
	h.mu.Lock()
	proxies := h.proxies
	h.mu.Unlock()
	for _, px := range proxies {
		px.close()
	}
	h.redis.stop()
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
		if err := os.WriteFile(p.cfgPath, podConfig(p, h.redis.port, nets), 0o644); err != nil {
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

// kill sends SIGKILL to the pod's process group and waits for it.
func (p *pod) kill() {
	p.mu.Lock()
	cmd, exited := p.cmd, p.exited
	p.cmd = nil
	p.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-exited
	p.h.mu.Lock()
	delete(p.h.pids, cmd.Process.Pid)
	p.h.mu.Unlock()
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
	resp, err := scrapeClient.Get(fmt.Sprintf("http://127.0.0.1:%d/metrics", p.metricsPort))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
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
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", r.port), time.Second)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("PING\r\n")); err != nil {
		return false
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return err == nil && strings.HasPrefix(line, "+PONG")
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

type upSpec struct{ id, endpoint, vendor string }

// podConfig is the YAML the pod's own strict loader reads.
func podConfig(p *pod, redisPort int, nets []*netSpec) []byte {
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
		"metrics": m{"enabled": true, "listenV4": true, "hostV4": "127.0.0.1", "port": p.metricsPort},
		"database": m{"sharedState": m{
			"clusterKey": "realworld",
			"connector": m{"id": "shared", "driver": "redis", "redis": m{
				"uri": fmt.Sprintf("redis://127.0.0.1:%d/0", redisPort),
			}},
		}},
		"projects": prs,
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		panic(err)
	}
	return out
}
