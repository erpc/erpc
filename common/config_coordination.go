package common

import (
	"fmt"
	"time"
)

// CacheFillConfig coordinates cacheable read misses across replicas through
// the shared-state connector. Default off. It only reduces duplicate upstream
// reads for responses a cache policy actually stores; it is not exactly-once.
type CacheFillConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`
	// LockTtl is how long the filling replica holds the lock. Default 30s.
	LockTtl Duration `yaml:"lockTtl,omitempty" json:"lockTtl,omitempty" tstype:"Duration"`
	// MaxWait bounds how long a replica waits for another replica's fill
	// before calling upstream itself. Default 5s.
	MaxWait Duration `yaml:"maxWait,omitempty" json:"maxWait,omitempty" tstype:"Duration"`
	// PollInterval is how often a waiting replica re-reads the cache. Default 50ms.
	PollInterval Duration `yaml:"pollInterval,omitempty" json:"pollInterval,omitempty" tstype:"Duration"`
	// LockAcquireTimeout bounds lock acquisition. Slower means "shared state
	// unavailable" and the request proceeds upstream immediately. Default 100ms.
	LockAcquireTimeout Duration `yaml:"lockAcquireTimeout,omitempty" json:"lockAcquireTimeout,omitempty" tstype:"Duration"`
}

func (c *CacheFillConfig) SetDefaults() {
	if c == nil {
		return
	}
	if c.LockTtl == 0 {
		c.LockTtl = Duration(30 * time.Second)
	}
	if c.MaxWait == 0 {
		c.MaxWait = Duration(5 * time.Second)
	}
	if c.PollInterval == 0 {
		c.PollInterval = Duration(50 * time.Millisecond)
	}
	if c.LockAcquireTimeout == 0 {
		c.LockAcquireTimeout = Duration(100 * time.Millisecond)
	}
}

func (c *CacheFillConfig) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	if c.LockTtl <= 0 || c.MaxWait <= 0 || c.PollInterval <= 0 || c.LockAcquireTimeout <= 0 {
		return fmt.Errorf("cacheFill durations must be positive")
	}
	if c.PollInterval > c.MaxWait {
		return fmt.Errorf("cacheFill.pollInterval must be <= maxWait")
	}
	if c.MaxWait > c.LockTtl {
		return fmt.Errorf("cacheFill.maxWait must be <= lockTtl")
	}
	return nil
}

const (
	HeadPollingModeAll   = "all"
	HeadPollingModeLease = "lease"
)

// EvmHeadPollingConfig selects who polls upstream latest/finalized block
// numbers. "all" (default) keeps every replica polling. "lease" lets one
// replica per network poll while others reuse the shared per-upstream
// counters, polling themselves whenever that shared state is older than
// StaleAfter.
type EvmHeadPollingConfig struct {
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// LeaseTtl is the lease length, renewed every LeaseTtl/3. Default 10s.
	LeaseTtl Duration `yaml:"leaseTtl,omitempty" json:"leaseTtl,omitempty" tstype:"Duration"`
	// StaleAfter: a non-holder polls anyway when the upstream's shared
	// counter has not been updated for this long. 0 = 3x the upstream's
	// statePollerInterval.
	StaleAfter Duration `yaml:"staleAfter,omitempty" json:"staleAfter,omitempty" tstype:"Duration"`
}

func (c *EvmHeadPollingConfig) SetDefaults() {
	if c == nil {
		return
	}
	if c.Mode == "" {
		c.Mode = HeadPollingModeAll
	}
	if c.LeaseTtl == 0 {
		c.LeaseTtl = Duration(10 * time.Second)
	}
}

func (c *EvmHeadPollingConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.Mode != HeadPollingModeAll && c.Mode != HeadPollingModeLease {
		return fmt.Errorf("evm.headPolling.mode %q is not supported (use %q or %q)", c.Mode, HeadPollingModeAll, HeadPollingModeLease)
	}
	if c.LeaseTtl < Duration(time.Second) {
		return fmt.Errorf("evm.headPolling.leaseTtl must be >= 1s")
	}
	if c.StaleAfter < 0 {
		return fmt.Errorf("evm.headPolling.staleAfter must be >= 0")
	}
	return nil
}

// HeadPollLease tells a state poller whether this replica currently holds
// the network's head polling lease. Nil means "always poll".
type HeadPollLease interface {
	Held() bool
}

// HeadPollLeaseAware is implemented by state pollers that honor a lease.
type HeadPollLeaseAware interface {
	SetHeadPollLease(lease HeadPollLease, staleAfter time.Duration, onSkip func())
}
