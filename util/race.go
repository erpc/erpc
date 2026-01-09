//go:build !race

package util

// raceEnabled is set to true when the race detector is enabled.
// This file is compiled when race detector is NOT enabled.
const raceEnabled = false
