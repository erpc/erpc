package data

import (
	"context"
	"sync"
	"time"
)

// SharedVariable represents a generic shared variable with last updated tracking
type SharedVariable interface {
	GetLastUpdated() time.Time
	IsStale(staleness time.Duration) bool
}

// CounterInt64SharedVariable represents an int64 counter that can only increase
type CounterInt64SharedVariable interface {
	SharedVariable
	GetValue() int64
	TryUpdate(ctx context.Context, newValue int64) (int64, bool, error)
}

// SharedStateRegistry manages shared variables across instances
type SharedStateRegistry interface {
	GetCounterInt64(key string) CounterInt64SharedVariable
	Close() error
}

// baseSharedVariable implements the SharedVariable interface
type baseSharedVariable struct {
	lastUpdated time.Time
}

func (v *baseSharedVariable) GetLastUpdated() time.Time {
	return v.lastUpdated
}

func (v *baseSharedVariable) IsStale(staleness time.Duration) bool {
	return time.Since(v.lastUpdated) > staleness
}

// counterInt64 implements CounterInt64SharedVariable
type counterInt64 struct {
	baseSharedVariable
	registry *sharedStateRegistry
	key      string
	value    int64
	mu       sync.RWMutex
}

func (c *counterInt64) GetValue() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.value
}

func (c *counterInt64) TryUpdate(ctx context.Context, newValue int64) (int64, bool, error) {
	return c.registry.updateCounter(ctx, c, newValue)
}
