package util

import "sync/atomic"

func AtomicValue[T any](v T) atomic.Value {
	var av atomic.Value
	av.Store(v)
	return av
}
