//go:build !darwin && !linux

package realworld

import (
	"fmt"
	"os"
	"runtime"
)

// The proxies tell pods apart by the OS socket table; only Linux and macOS
// readers exist.
func init() {
	fmt.Fprintf(os.Stderr, "realworld: no socket-table reader for %s; the suite runs on Linux and macOS\n", runtime.GOOS)
	os.Exit(1)
}

func socketOwner(int, int, []int) int { return 0 }
