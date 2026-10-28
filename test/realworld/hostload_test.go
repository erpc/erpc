package realworld

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"time"
)

const (
	// hostSampleEvery: how often the host load is recorded.
	hostSampleEvery = 5 * time.Second
	// schedTick: the ticker whose oversleep is the harness's own
	// scheduling delay.
	schedTick = 10 * time.Millisecond
	// quietLoadPerCPU: a phase starts once the 1-min load average is below
	// this times the CPU count; a latency verdict taken above it is labelled.
	quietLoadPerCPU = 0.75
	// quietWait: how long a phase waits for a quiet host before it fails.
	quietWait = 10 * time.Minute
)

// hostSample is one record: the 1-min load average, and the most a 10 ms
// ticker overslept since the previous record.
type hostSample struct {
	at    time.Time
	load  float64
	delay time.Duration
}

// hostLoad records the host's load every hostSampleEvery for the run.
// loaded (REALWORLD_LOADED_HOST=1): phases start at any load, on a host
// other work keeps busy; latency verdicts above the limit stay labelled.
type hostLoad struct {
	limit  float64
	loaded bool
	done   chan struct{}
	wg     sync.WaitGroup

	mu      sync.Mutex
	samples []hostSample
}

func startHostLoad() *hostLoad {
	hl := &hostLoad{limit: quietLoadPerCPU * float64(runtime.NumCPU()), loaded: os.Getenv("REALWORLD_LOADED_HOST") == "1", done: make(chan struct{})}
	hl.wg.Add(1)
	go hl.run()
	return hl
}

func (hl *hostLoad) stop() {
	close(hl.done)
	hl.wg.Wait()
}

func (hl *hostLoad) run() {
	defer hl.wg.Done()
	tick := time.NewTicker(schedTick)
	defer tick.Stop()
	last, next := time.Now(), time.Now().Add(hostSampleEvery)
	var worst time.Duration
	for {
		select {
		case <-hl.done:
			return
		case now := <-tick.C:
			// The ticker drops ticks it cannot deliver, so the time between
			// two receipts beyond one tick is how late this goroutine ran.
			worst = max(worst, time.Since(last)-schedTick)
			last = time.Now()
			if now.Before(next) {
				continue
			}
			next = next.Add(hostSampleEvery)
			load, err := loadAverage()
			if err != nil {
				load = -1
			}
			hl.mu.Lock()
			hl.samples = append(hl.samples, hostSample{at: time.Now(), load: load, delay: worst})
			hl.mu.Unlock()
			worst = 0
			// The uptime call above is the sampler's own delay, not the host's.
			last = time.Now()
		}
	}
}

var loadRE = regexp.MustCompile(`load averages?: *([0-9]+\.[0-9]+)`)

// loadAverage is the 1-min load average as uptime prints it on Linux
// ("load average: 0.52, …") and macOS ("load averages: 0.52 …").
func loadAverage() (float64, error) {
	cmd := exec.Command("uptime")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("uptime: %w", err)
	}
	m := loadRE.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("uptime printed no load average: %q", out)
	}
	return strconv.ParseFloat(string(m[1]), 64)
}

// waitQuiet waits until the 1-min load average is below the limit, or
// only logs it on a loaded host.
func (hl *hostLoad) waitQuiet(logf func(string, ...interface{})) error {
	start := time.Now()
	for {
		load, err := loadAverage()
		if err != nil {
			return err
		}
		if hl.loaded {
			logf("REALWORLD_LOADED_HOST: starting at load %.1f, limit %.1f", load, hl.limit)
			return nil
		}
		if load < hl.limit {
			if waited := time.Since(start); waited > time.Second {
				logf("host quiet after %s: load %.1f below %.1f", waited.Round(time.Second), load, hl.limit)
			}
			return nil
		}
		if time.Since(start) > quietWait {
			return fmt.Errorf("host overloaded: 1-min load %.1f stayed above %.1f (%.2f × %d CPUs) for %s", load, hl.limit, quietLoadPerCPU, runtime.NumCPU(), quietWait)
		}
		logf("waiting for a quiet host: load %.1f, limit %.1f", load, hl.limit)
		time.Sleep(hostSampleEvery)
	}
}

// window are the samples taken in [from, to], or, for a window shorter
// than a sample interval, the first sample after from.
func (hl *hostLoad) window(from, to time.Time) []hostSample {
	hl.mu.Lock()
	defer hl.mu.Unlock()
	var out []hostSample
	for _, s := range hl.samples {
		if !s.at.Before(from) && !s.at.After(to) {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		for _, s := range hl.samples {
			if s.at.After(from) {
				return []hostSample{s}
			}
		}
	}
	return out
}

// overloaded is the highest load above the limit while [from, to] ran.
func (hl *hostLoad) overloaded(from, to time.Time) (float64, bool) {
	worst := 0.0
	for _, s := range hl.window(from, to) {
		worst = max(worst, s.load)
	}
	return worst, worst >= hl.limit
}

// verdict labels a latency verdict taken while the host was overloaded.
func (hl *hostLoad) verdict(from, to time.Time, msg string) string {
	if load, ok := hl.overloaded(from, to); ok {
		return fmt.Sprintf("host overloaded (load %.1f): %s", load, msg)
	}
	return msg
}

// record is the load record of [from, to] in one line.
func (hl *hostLoad) record(from, to time.Time) string {
	ss := hl.window(from, to)
	if len(ss) == 0 {
		return "no host samples"
	}
	lo, hi := ss[0].load, ss[0].load
	var delay time.Duration
	for _, s := range ss {
		lo, hi, delay = min(lo, s.load), max(hi, s.load), max(delay, s.delay)
	}
	return fmt.Sprintf("load %.1f–%.1f (limit %.1f), scheduling delay max %s, %d samples", lo, hi, hl.limit, delay.Round(time.Millisecond), len(ss))
}
