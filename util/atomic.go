package util

import (
	"sync/atomic"
	"time"
)

func AtomicValue[T any](v T) atomic.Value {
	var av atomic.Value
	av.Store(v)
	return av
}

// Duration is a time.Duration safe for concurrent use: a bound that tests
// shorten while background loops read it.
type Duration struct{ v atomic.Int64 }

func NewDuration(d time.Duration) *Duration {
	out := &Duration{}
	out.v.Store(int64(d))
	return out
}

func (d *Duration) Load() time.Duration { return time.Duration(d.v.Load()) }

func (d *Duration) Store(v time.Duration) { d.v.Store(int64(v)) }
