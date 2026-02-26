package evm

import "time"

// KnownBlockTimes maps network IDs (e.g. "evm:1") to their known block times.
// Used as a fallback during the seed phase before the dynamic EMA is available.
var KnownBlockTimes = map[string]time.Duration{
	"evm:1":        12_000 * time.Millisecond,
	"evm:10":       2_000 * time.Millisecond,
	"evm:56":       3_000 * time.Millisecond,
	"evm:100":      5_000 * time.Millisecond,
	"evm:137":      2_000 * time.Millisecond,
	"evm:250":      1_000 * time.Millisecond,
	"evm:324":      1_000 * time.Millisecond,
	"evm:1101":     3_000 * time.Millisecond,
	"evm:1329":     500 * time.Millisecond,
	"evm:2020":     3_000 * time.Millisecond,
	"evm:5000":     2_000 * time.Millisecond,
	"evm:8453":     2_000 * time.Millisecond,
	"evm:34443":    2_000 * time.Millisecond,
	"evm:42161":    250 * time.Millisecond,
	"evm:42220":    5_000 * time.Millisecond,
	"evm:43114":    1_500 * time.Millisecond,
	"evm:59144":    2_000 * time.Millisecond,
	"evm:81457":    2_000 * time.Millisecond,
	"evm:534352":   3_000 * time.Millisecond,
	"evm:7777777":  2_000 * time.Millisecond,
	"evm:11155111": 12_000 * time.Millisecond,
}
