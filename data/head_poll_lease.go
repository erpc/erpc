package data

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// extendableLock is implemented by locks that can renew their TTL while still
// owned (e.g. the Redis lock). Locks without it are released and re-acquired.
type extendableLock interface {
	Extend(ctx context.Context) error
}

// HeadPollLease elects one replica per key to poll upstream heads. Held() is
// conservative: it is true only while the last successful acquire/renew,
// timed from BEFORE its network round trip, is younger than the TTL minus a
// safety margin. Any error drops the lease (fail open to normal polling
// through the poller's staleness fallback).
type HeadPollLease struct {
	connector Connector
	key       string
	ttl       time.Duration
	logger    *zerolog.Logger
	onChange  func(held bool)
	nowFn     func() time.Time

	heldUntil atomic.Int64 // unix nanos; 0 = not held
	reported  atomic.Bool

	mu     sync.Mutex // guards lock
	lock   DistributedLock
	cancel context.CancelFunc
	done   chan struct{}
	stop   sync.Once
}

func NewHeadPollLease(connector Connector, key string, ttl time.Duration, logger *zerolog.Logger, onChange func(held bool)) *HeadPollLease {
	if logger == nil {
		l := zerolog.Nop()
		logger = &l
	}
	return &HeadPollLease{
		connector: connector, key: key, ttl: ttl, logger: logger, onChange: onChange,
		nowFn: time.Now, done: make(chan struct{}),
	}
}

// Held reports whether this replica currently owns the lease.
func (l *HeadPollLease) Held() bool {
	if l == nil {
		return false
	}
	until := l.heldUntil.Load()
	return until != 0 && l.nowFn().UnixNano() < until
}

// Start runs acquire/renew every ttl/3 until ctx is done or Stop is called.
func (l *HeadPollLease) Start(ctx context.Context) {
	ctx, l.cancel = context.WithCancel(ctx)
	go func() {
		defer close(l.done)
		defer l.release()
		l.Step(ctx)
		t := time.NewTicker(l.ttl / 3)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				l.Step(ctx)
			}
		}
	}()
}

// Stop releases the lease and waits for the loop to exit.
func (l *HeadPollLease) Stop() {
	l.stop.Do(func() {
		if l.cancel != nil {
			l.cancel()
			<-l.done
		} else {
			l.release()
		}
	})
}

func (l *HeadPollLease) validUntil(start time.Time) int64 {
	// Margin covers clock drift and the renew cadence jitter.
	return start.Add(l.ttl - l.ttl/10).UnixNano()
}

func (l *HeadPollLease) setHeld(until int64) {
	l.heldUntil.Store(until)
	held := until != 0
	if l.reported.Swap(held) != held || !held {
		if l.onChange != nil {
			l.onChange(held)
		}
	}
}

// Step performs one acquire-or-renew attempt (exported for tests).
func (l *HeadPollLease) Step(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	opCtx, cancel := context.WithTimeout(ctx, l.ttl/3)
	defer cancel()

	if l.lock != nil {
		if ext, ok := l.lock.(extendableLock); ok {
			start := l.nowFn()
			if err := ext.Extend(opCtx); err == nil {
				l.setHeld(l.validUntil(start))
				return
			} else {
				l.logger.Debug().Err(err).Str("key", l.key).Msg("head poll lease renew failed")
			}
			// Lost or unknown: stop claiming before anything else.
			l.setHeld(0)
			l.lock = nil
		} else {
			// Non-renewable lock: drop the claim first, then re-acquire below.
			l.setHeld(0)
			_ = l.lock.Unlock(opCtx)
			l.lock = nil
		}
	}

	start := l.nowFn()
	lk, err := l.connector.Lock(opCtx, l.key, l.ttl)
	if err != nil || lk == nil || lk.IsNil() {
		l.setHeld(0)
		return
	}
	if ctx.Err() != nil {
		_ = lk.Unlock(context.Background())
		l.setHeld(0)
		return
	}
	l.lock = lk
	l.setHeld(l.validUntil(start))
}

func (l *HeadPollLease) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setHeld(0)
	if l.lock != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = l.lock.Unlock(ctx)
		cancel()
		l.lock = nil
	}
}
