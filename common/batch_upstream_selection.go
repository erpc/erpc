package common

import (
	"context"
	"fmt"
	"sync"
)

type BatchUpstreamSelectionKey struct {
	NetworkID     string
	Method        string
	Finality      DataFinalityState
	UseUpstream   string
	UpstreamGroup string
}

type batchUpstreamSelectionPromise struct {
	done      chan struct{}
	upstreams []Upstream
	err       error
}

type BatchUpstreamSelectionCache struct {
	mu       sync.Mutex
	entries  map[BatchUpstreamSelectionKey][]Upstream
	inflight map[BatchUpstreamSelectionKey]*batchUpstreamSelectionPromise
}

type batchUpstreamSelectionCacheContextKey struct{}

func NewBatchUpstreamSelectionCache() *BatchUpstreamSelectionCache {
	return &BatchUpstreamSelectionCache{
		entries:  make(map[BatchUpstreamSelectionKey][]Upstream),
		inflight: make(map[BatchUpstreamSelectionKey]*batchUpstreamSelectionPromise),
	}
}

func WithBatchUpstreamSelectionCache(ctx context.Context, cache *BatchUpstreamSelectionCache) context.Context {
	if ctx == nil || cache == nil {
		return ctx
	}
	return context.WithValue(ctx, batchUpstreamSelectionCacheContextKey{}, cache)
}

func BatchUpstreamSelectionCacheFromContext(ctx context.Context) *BatchUpstreamSelectionCache {
	if ctx == nil {
		return nil
	}
	if v := ctx.Value(batchUpstreamSelectionCacheContextKey{}); v != nil {
		if c, ok := v.(*BatchUpstreamSelectionCache); ok {
			return c
		}
	}
	return nil
}

func (c *BatchUpstreamSelectionCache) Resolve(
	key BatchUpstreamSelectionKey,
	loader func() ([]Upstream, error),
) ([]Upstream, bool, error) {
	if c == nil {
		ups, err := loader()
		if err != nil {
			return nil, false, err
		}
		return cloneUpstreamSlice(ups), false, nil
	}

	for {
		c.mu.Lock()
		if ups, ok := c.entries[key]; ok {
			c.mu.Unlock()
			return cloneUpstreamSlice(ups), true, nil
		}

		if pending, ok := c.inflight[key]; ok {
			c.mu.Unlock()
			<-pending.done
			if pending.err != nil {
				return nil, false, pending.err
			}
			return cloneUpstreamSlice(pending.upstreams), true, nil
		}

		pending := &batchUpstreamSelectionPromise{done: make(chan struct{})}
		c.inflight[key] = pending
		c.mu.Unlock()

		var (
			cloned []Upstream
			err    error
		)
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("batch upstream selection loader panic: %v", recovered)
				}
			}()

			var ups []Upstream
			ups, err = loader()
			cloned = cloneUpstreamSlice(ups)
		}()

		c.mu.Lock()
		delete(c.inflight, key)
		if err == nil {
			c.entries[key] = cloned
			pending.upstreams = cloned
		} else {
			pending.err = err
		}
		close(pending.done)
		c.mu.Unlock()

		if err != nil {
			return nil, false, err
		}

		return cloneUpstreamSlice(cloned), false, nil
	}
}

func cloneUpstreamSlice(upstreams []Upstream) []Upstream {
	if len(upstreams) == 0 {
		return nil
	}
	cloned := make([]Upstream, len(upstreams))
	copy(cloned, upstreams)
	return cloned
}
