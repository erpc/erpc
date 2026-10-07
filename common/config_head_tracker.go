package common

import (
	"fmt"
	"time"
)

// EvmHeadTrackerConfig configures the fleet-wide head tracker ("stalker").
//
// Exactly one replica per network (the holder of a shared-state lease) loops
// eth_getBlockByNumber("latest") through the network's normal upstream
// selection, timed to the network's measured block time, and publishes the
// observed head through shared state. Every replica then serves that head as
// the network's "latest" (eth_blockNumber, "latest" interpolation, block
// store head) without polling any upstream itself. The per-upstream state
// pollers keep running at their own (slow) interval for health and finality.
type EvmHeadTrackerConfig struct {
	// Enabled turns the tracker on for this network. Default false.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled"`

	// FullBlocks makes the leader poll eth_getBlockByNumber("latest", true)
	// instead of (…, false). The full block is cached under its number and
	// hash, and the hashes-only form is derived from it and cached too, so
	// one upstream call fills both variants. Costs the full-transaction
	// payload once per block per network (not per replica); worth it on
	// networks whose clients read full blocks every head. Default false.
	FullBlocks bool `yaml:"fullBlocks,omitempty" json:"fullBlocks,omitempty"`

	// Interval overrides the wait between polls. Unset derives it from the
	// network's measured (EMA) block time, aligned to the expected next
	// block timestamp plus the observed propagation delay; only on cold start
	// (block time not yet measured) does it use DefaultHeadTrackerColdInterval.
	// Accepts a duration or {blockTimeMultiplier, fallback}.
	Interval *BlockTimeAdaptiveDuration `yaml:"interval,omitempty" json:"interval,omitempty"`

	// LeaseTtl is how long a leader's lease lives without renewal; a follower
	// takes over at most this long after the leader disappears. Default 5s.
	LeaseTtl Duration `yaml:"leaseTtl,omitempty" json:"leaseTtl,omitempty" tstype:"Duration"`
}

const (
	// DefaultHeadTrackerColdInterval is the poll wait used until the network's
	// block time has been measured.
	DefaultHeadTrackerColdInterval = time.Second
	// DefaultHeadTrackerLeaseTtl bounds leader failover time.
	DefaultHeadTrackerLeaseTtl = 5 * time.Second
	// MinHeadTrackerPollWait is the floor between two leader polls.
	MinHeadTrackerPollWait = 500 * time.Millisecond
)

func (c *EvmHeadTrackerConfig) Copy() *EvmHeadTrackerConfig {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Interval = c.Interval.Copy()
	return &cp
}

func (c *EvmHeadTrackerConfig) SetDefaults() {
	if c == nil {
		return
	}
	if c.LeaseTtl == 0 {
		c.LeaseTtl = Duration(DefaultHeadTrackerLeaseTtl)
	}
}

func (c *EvmHeadTrackerConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.LeaseTtl < 0 || (c.LeaseTtl > 0 && c.LeaseTtl.Duration() < time.Second) {
		return fmt.Errorf("network.*.evm.headTracker.leaseTtl must be at least 1s, got %s", c.LeaseTtl.Duration())
	}
	if err := c.Interval.validate("network.*.evm.headTracker.interval"); err != nil {
		return err
	}
	if c.Interval != nil && c.Interval.BlockTimeMultiplier == 0 && c.Interval.Fallback < 0 {
		return fmt.Errorf("network.*.evm.headTracker.interval must not be negative")
	}
	return nil
}

// HeadTrackerEnabled reports whether the head tracker is on for this network.
func (e *EvmNetworkConfig) HeadTrackerEnabled() bool {
	return e != nil && e.HeadTracker != nil && e.HeadTracker.Enabled
}
