//go:build !race

package erpc

func isRaceEnabled() bool {
	return false
}
