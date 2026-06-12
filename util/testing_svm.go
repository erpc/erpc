package util

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/h2non/gock"
)

// SetupMocksForSvmStatePoller registers the four state-poller calls every SVM
// upstream makes on each tick: getHealth, getSlot(processed), getSlot(finalized),
// getMaxShredInsertSlot. Each filter guards by URL.Host so a test with multiple
// upstreams doesn't accidentally consume another upstream's counters.
//
// Each returned mock is Persist()'d — the poller keeps ticking forever in the
// background, so registering Times(1) would exhaust and then match user mocks.
func SetupMocksForSvmStatePoller(host string, latestSlot, finalizedSlot int64) {
	// getHealth → "ok"
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(filterByHostAndBody(host, "getHealth")).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"ok","_note":"svm state poller expected mock for getHealth"}`))

	// getSlot(processed)
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(filterSvmGetSlot(host, "processed")).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%d,"_note":"svm state poller expected mock for getSlot(processed)"}`, latestSlot)))

	// getSlot(finalized)
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(filterSvmGetSlot(host, "finalized")).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%d,"_note":"svm state poller expected mock for getSlot(finalized)"}`, finalizedSlot)))

	// getMaxShredInsertSlot — return same as latest to zero out lag.
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(filterByHostAndBody(host, "getMaxShredInsertSlot")).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%d,"_note":"svm state poller expected mock for getMaxShredInsertSlot"}`, latestSlot)))
}

func filterByHostAndBody(host, needle string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if r.URL.Host != host {
			return false
		}
		return strings.Contains(SafeReadBody(r), needle)
	}
}

func filterSvmGetSlot(host, commitment string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		if r.URL.Host != host {
			return false
		}
		body := SafeReadBody(r)
		// getSlot for SVM — make sure we match only getSlot not getMaxShredInsertSlot etc.
		if !strings.Contains(body, `"method":"getSlot"`) {
			return false
		}
		return strings.Contains(body, commitment)
	}
}
