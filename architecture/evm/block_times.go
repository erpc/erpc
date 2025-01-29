package evm

import "time"

// KnownBlockTimes is a map that holds the block times in milliseconds by chain ids for known networks.
// For other networks the detectFeatures() can fetch last couple of blocks and calculate avg. block time.
var KnownBlockTimes = map[int64]time.Duration{
	1:        12_000 * time.Millisecond,
	10:       2_000 * time.Millisecond,
	56:       3_000 * time.Millisecond,
	100:      5_000 * time.Millisecond,
	137:      2_000 * time.Millisecond,
	250:      1_000 * time.Millisecond,
	324:      1_000 * time.Millisecond,
	1101:     3_000 * time.Millisecond,
	1329:     500 * time.Millisecond,
	2020:     3_000 * time.Millisecond,
	5000:     2_000 * time.Millisecond,
	8453:     2_000 * time.Millisecond,
	34443:    2_000 * time.Millisecond,
	42161:    250 * time.Millisecond,
	42220:    5_000 * time.Millisecond,
	43114:    1_500 * time.Millisecond,
	59144:    2_000 * time.Millisecond,
	81457:    2_000 * time.Millisecond,
	534352:   3_000 * time.Millisecond,
	7777777:  2_000 * time.Millisecond,
	11155111: 12_000 * time.Millisecond,
}
