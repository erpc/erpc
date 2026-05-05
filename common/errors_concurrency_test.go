package common

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// TestBaseError_DetailsConcurrentReadWriteUnderRace reproduces the
// `concurrent map iteration and map write` fatal that crashed eRPC when
// consensus.runAnalyzer formatted an error via fmt.Sprintf("%v", err)
// while the caller's failsafe stack was still appending method/upstreamId
// to the same *BaseError's Details map.
//
// Run with `-race`: the detector fires on the first unsynchronized access,
// long before the runtime would otherwise panic. The test passes only when
// every read/write of Details is locked.
func TestBaseError_DetailsConcurrentReadWriteUnderRace(t *testing.T) {
	e := &BaseError{
		Code:    "ErrTest",
		Message: "races against late detail writes",
		Details: map[string]interface{}{"seed": "value"},
	}

	const duration = 100 * time.Millisecond
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Reader: matches consensus.trackAndPunishMisbehavingUpstreams' fmt.Sprintf path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = e.Error()
			}
		}
	}()

	// Reader: matches MarshalJSON path (used by zerolog and HTTP responses).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = json.Marshal(e)
			}
		}
	}()

	// Reader: matches DeepSearch / GetDetail call sites.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = e.DeepSearch("upstreamId")
			}
		}
	}()

	// Writer: matches failsafe.TranslateFailsafeError appending late context.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				e.SetDetail("upstreamId", "u1")
				e.SetDetail("method", "eth_call")
			}
		}
	}()

	// Writer: matches the error normalizer's WithRetryableTowardNetwork classification.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				e.WithRetryableTowardNetwork(i%2 == 0)
			}
		}
	}()

	time.Sleep(duration)
	close(stop)
	wg.Wait()
}
