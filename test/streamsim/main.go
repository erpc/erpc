// Command streamsim runs eRPC pods from a given binary over a simulated
// chain (test/chainsim), each pod on its own vendors, with a fault proxy
// on each pod's gRPC client link, for tests of the stream libraries. It
// prints one JSON line once ready, then answers one JSON command per
// stdin line with one JSON line on stdout, in order:
//
//	{"op":"wait","blocks":n}          n block times pass
//	{"op":"reorg","depth":d}          the top d canonical blocks are replaced
//	{"op":"halt","on":b}              block production stops or resumes
//	{"op":"kill","pod":i}             SIGKILL
//	{"op":"start","pod":i}            boots it again on its ports
//	{"op":"cut","pod":i,"on":b}       its client link forwards nothing, sockets kept
//	{"op":"pin","pod":i,"on":b}       its vendors stay on the current tip's branch, which grows apart from the canonical one; off: back to canonical
//	{"op":"canon","from":a,"to":b,"filter":k,"view":i}  canonical blocks, or with view those pod i's vendors serve, and their logs' keys (k -1: every log)
//
// stdin closing stops everything.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/erpc/erpc/test/chainsim"
	"gopkg.in/yaml.v2"
)

const project = "rw"

type pod struct {
	idx                int
	httpPort, grpcPort int
	cfg, log           string
	link               *link
	cmd                *exec.Cmd
	exited             chan struct{}
	vendors            []*chainsim.Vendor
	fork               *chainsim.Fork
}

func main() {
	bin := flag.String("erpc", "", "eRPC binary")
	pods := flag.Int("pods", 2, "eRPC pods")
	dir := flag.String("dir", "", "directory for configs and logs")
	chainID := flag.Int64("chain-id", 31337, "chain id")
	blockTime := flag.Duration("block-time", 250*time.Millisecond, "block time")
	reorgWindow := flag.Int("reorg-window", 16, "chainTracker.reorgWindow")
	flag.Parse()
	if *bin == "" || *dir == "" {
		fatal("-erpc and -dir are required")
	}
	chain := chainsim.NewChain(chainsim.Config{ChainID: *chainID, BlockTime: *blockTime, Seed: 1})
	var ps []*pod
	for i := range *pods {
		p := &pod{idx: i, httpPort: freePort(), grpcPort: freePort(),
			cfg: filepath.Join(*dir, fmt.Sprintf("pod-%d.yaml", i+1)), log: filepath.Join(*dir, fmt.Sprintf("pod-%d.log", i+1))}
		var ups []map[string]interface{}
		for _, v := range []struct {
			id, name string
			ws       bool
		}{{"ws-a", "vendor-a", true}, {"ws-b", "vendor-b", true}, {"http-a", "vendor-a", false}, {"http-b", "vendor-b", false}} {
			vd, err := chain.AddVendor(fmt.Sprintf("%s-%d", v.id, i+1), v.name, v.ws)
			if err != nil {
				fatal(err.Error())
			}
			p.vendors = append(p.vendors, vd)
			ups = append(ups, map[string]interface{}{"id": v.id, "endpoint": vd.URL(), "vendorName": v.name,
				"evm": map[string]interface{}{"chainId": *chainID}})
		}
		cfg := map[string]interface{}{
			"logLevel": "warn",
			"server": map[string]interface{}{
				"listenV4": true, "httpHostV4": "127.0.0.1", "httpPortV4": p.httpPort,
				"grpcEnabled": true, "grpcHostV4": "127.0.0.1", "grpcPortV4": p.grpcPort,
			},
			"metrics": map[string]interface{}{"enabled": false},
			"projects": []map[string]interface{}{{
				"id": project,
				"networks": []map[string]interface{}{{"architecture": "evm", "evm": map[string]interface{}{
					"chainId": *chainID, "chainTracker": map[string]interface{}{"reorgWindow": *reorgWindow},
				}}},
				"upstreams": ups,
			}},
		}
		out, err := yaml.Marshal(cfg)
		if err == nil {
			err = os.WriteFile(p.cfg, out, 0o644)
		}
		if err != nil {
			fatal(err.Error())
		}
		ps = append(ps, p)
	}
	chain.Start()
	defer chain.Close()

	var endpoints []string
	for _, p := range ps {
		link, err := newLink(fmt.Sprintf("127.0.0.1:%d", p.grpcPort))
		if err == nil {
			p.link = link
			err = p.start(*bin)
		}
		if err != nil {
			fatal(err.Error())
		}
		defer p.kill()
		endpoints = append(endpoints, fmt.Sprintf("grpc://%s/%s/evm/%d", p.link.addr(), project, *chainID))
	}

	enc := json.NewEncoder(os.Stdout)
	_ = enc.Encode(map[string]interface{}{"ready": true, "chainId": *chainID, "endpoints": endpoints,
		"filters": []map[string]interface{}{chainsim.Filters[0].Params(), chainsim.Filters[1].Params()}})
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var c struct {
			Op                 string
			Blocks, Depth, Pod int
			On                 bool
			From, To           uint64
			Filter             int
			View               *int
		}
		if err := json.Unmarshal(in.Bytes(), &c); err != nil {
			_ = enc.Encode(map[string]interface{}{"error": err.Error()})
			continue
		}
		reply := map[string]interface{}{}
		var p *pod
		if c.Pod >= 0 && c.Pod < len(ps) {
			p = ps[c.Pod]
		}
		switch c.Op {
		case "wait":
			chain.WaitTicks(uint64(c.Blocks))
		case "reorg":
			old, _ := chain.Reorg(c.Depth)
			reply["fork"] = ref(old[0].Parent)
		case "halt":
			chain.Halt(c.On)
		case "kill":
			p.kill()
		case "start":
			if err := p.start(*bin); err != nil {
				reply["error"] = err.Error()
			}
		case "cut":
			p.link.cut(c.On)
		case "pin":
			if c.On && p.fork == nil {
				p.fork = chain.StartFork(0, 0, p.vendors...)
			} else if !c.On && p.fork != nil {
				chain.EndFork(p.fork)
				p.fork = nil
			}
		case "canon":
			at := chain.CanonAt
			if c.View != nil {
				v := ps[*c.View].vendors[0]
				at = func(n uint64) *chainsim.Block { return chain.ServedAt(v, n) }
			}
			var bs []map[string]interface{}
			for n := c.From; n <= c.To; n++ {
				b := at(n)
				if b == nil {
					continue
				}
				keys := []string{}
				for _, l := range b.Logs() {
					if c.Filter < 0 || chainsim.Filters[c.Filter].Matches(l) {
						keys = append(keys, fmt.Sprintf("%s:%d", strings.ToLower(l.BlockHash.Hex()), l.LogIndex))
					}
				}
				r := ref(b)
				r["parentHash"] = strings.ToLower(b.ParentHash().Hex())
				r["keys"] = keys
				bs = append(bs, r)
			}
			reply["blocks"] = bs
		default:
			reply["error"] = "unknown op " + c.Op
		}
		reply["tip"] = ref(chain.Tip())
		_ = enc.Encode(reply)
	}
}

func ref(b *chainsim.Block) map[string]interface{} {
	return map[string]interface{}{"number": b.Number, "hash": strings.ToLower(b.Hash.Hex())}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "streamsim:", msg)
	os.Exit(1)
}

func freePort() int {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		fatal(err.Error())
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// start boots the pod and waits until its HTTP and gRPC ports accept
// connections.
func (p *pod) start(bin string) error {
	log, err := os.OpenFile(p.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, p.cfg)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	p.cmd, p.exited = cmd, make(chan struct{})
	go func(exited chan struct{}) {
		_ = cmd.Wait()
		_ = log.Close()
		close(exited)
	}(p.exited)
	deadline := time.Now().Add(60 * time.Second)
	for _, port := range []int{p.httpPort, p.grpcPort} {
		for {
			c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
			if err == nil {
				_ = c.Close()
				break
			}
			select {
			case <-p.exited:
				return fmt.Errorf("pod-%d exited while booting; see %s", p.idx+1, p.log)
			default:
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("pod-%d: port %d not open; see %s", p.idx+1, port, p.log)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

func (p *pod) kill() {
	if p.cmd == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	<-p.exited
	p.cmd = nil
}

// link forwards client connections to a pod's gRPC port. Cut, it reads
// and drops every byte both ways and keeps the sockets, as a black-holed
// path does; uncut, it closes the connections the cut corrupted.
type link struct {
	ln     net.Listener
	target string

	mu    sync.Mutex
	isCut bool
	conns map[net.Conn]bool
}

func newLink(target string) (*link, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &link{ln: ln, target: target, conns: map[net.Conn]bool{}}
	go l.serve()
	return l, nil
}

func (l *link) addr() string { return l.ln.Addr().String() }

func (l *link) serve() {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go l.forward(c)
	}
}

func (l *link) forward(c net.Conn) {
	l.track(c, true)
	defer l.track(c, false)
	defer c.Close()
	t, err := net.Dial("tcp", l.target)
	if err != nil {
		return
	}
	l.track(t, true)
	defer l.track(t, false)
	defer t.Close()
	done := make(chan struct{}, 2)
	go func() { l.pipe(t, c); done <- struct{}{} }()
	go func() { l.pipe(c, t); done <- struct{}{} }()
	<-done
}

func (l *link) pipe(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !l.cutNow() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (l *link) track(c net.Conn, open bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if open {
		l.conns[c] = true
	} else {
		delete(l.conns, c)
	}
}

func (l *link) cutNow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isCut
}

func (l *link) cut(on bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isCut && !on {
		for c := range l.conns {
			_ = c.Close()
		}
	}
	l.isCut = on
}
