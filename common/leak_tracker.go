package common

import (
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/rs/zerolog/log"
)

// Lightweight leak tracker for debugging long-lived responses.
// Enabled only when ERPC_DEBUG_LEAKS=1. Does not retain strong refs to targets.

type leakTracker struct {
	enabled bool
	once    sync.Once
	entries sync.Map // key: uintptr (object addr) -> *leakEntry
	cutoff  time.Duration
}

type leakEntry struct {
	createdAt time.Time
	released  atomic.Bool
	kind      string
	stack     []byte
}

var lt = func() *leakTracker {
	enabled := os.Getenv("ERPC_DEBUG_LEAKS") == "1"
	cutoffSec, _ := strconv.Atoi(os.Getenv("ERPC_DEBUG_LEAKS_CUTOFF_SEC"))
	if cutoffSec <= 0 {
		cutoffSec = 600
	}
	return &leakTracker{enabled: enabled, cutoff: time.Duration(cutoffSec) * time.Second}
}()

func (t *leakTracker) start() {
	if !t.enabled {
		return
	}
	t.once.Do(func() {
		go func() {
			ticker := time.NewTicker(2 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				now := time.Now()
				stale := 0
				total := 0
				t.entries.Range(func(key, value any) bool {
					total++
					e := value.(*leakEntry)
					if now.Sub(e.createdAt) > t.cutoff && !e.released.Load() {
						stale++
					}
					return true
				})
				if stale > 0 {
					log.Warn().Int("tracked", total).Int("stale", stale).Dur("cutoff", t.cutoff).Msg("ERPC_DEBUG_LEAKS: potential long-lived responses")
				}
			}
		}()
	})
}

// TrackNR registers a NormalizedResponse for leak tracking.
func TrackNR(nr *NormalizedResponse) {
	if nr == nil || !lt.enabled {
		return
	}
	lt.start()
	key := uintptr(unsafe.Pointer(nr))
	lt.entries.Store(key, &leakEntry{createdAt: time.Now(), kind: "NormalizedResponse", stack: debug.Stack()})
	// Finalizer removes the entry when GC collects nr. Do not capture nr to avoid keeping it alive.
	runtime.SetFinalizer(nr, func(_ *NormalizedResponse) {
		lt.onFinalize(key)
	})
}

// MarkNRReleased marks a response as explicitly released.
func MarkNRReleased(nr *NormalizedResponse) {
	if nr == nil || !lt.enabled {
		return
	}
	key := uintptr(unsafe.Pointer(nr))
	if v, ok := lt.entries.Load(key); ok {
		v.(*leakEntry).released.Store(true)
	}
}

func (t *leakTracker) onFinalize(key uintptr) {
	if !t.enabled {
		return
	}
	if v, ok := t.entries.LoadAndDelete(key); ok {
		e := v.(*leakEntry)
		// Log only if object lived beyond cutoff and was not released.
		if !e.released.Load() {
			log.Error().Dur("age", time.Since(e.createdAt)).Str("kind", e.kind).Str("stack", string(e.stack)).Msg("ERPC_DEBUG_LEAKS: response GC'd after cutoff without Release; stack follows")
		}
	}
}
