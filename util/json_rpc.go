package util

import (
	"math"
	"math/rand"
)

// RandomID returns a value appropriate for a JSON-RPC ID field (i.e int64 type but with 32 bit range) to avoid overflow during conversions and reading/sending to upstreams
func RandomID() int64 {
	return int64(rand.Intn(math.MaxInt32)) // #nosec G404
}
