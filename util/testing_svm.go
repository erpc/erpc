package util

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/h2non/gock"
)

// SolanaMainnetBetaGenesisHash is the immutable genesis hash for Solana
// mainnet-beta (block 0). Bootstrap validation compares getGenesisHash against
// the hardcoded table for known clusters, so e2e SVM upstreams (all configured
// as mainnet-beta) must answer with this to pass the now fail-closed check.
// Kept here as a literal because util cannot import common (cycle).
const SolanaMainnetBetaGenesisHash = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"

// SetupMocksForSvmStatePoller registers the four state-poller calls every SVM
// upstream makes on each tick: getHealth, getSlot(processed), getSlot(finalized),
// getMaxShredInsertSlot. It also mocks the one-shot bootstrap getGenesisHash with
// the mainnet-beta hash so the upstream passes genesis validation (which is
// fail-closed for known clusters). Each filter guards by URL.Host so a test with
// multiple upstreams doesn't accidentally consume another upstream's counters.
//
// Each returned mock is Persist()'d — the poller keeps ticking forever in the
// background, so registering Times(1) would exhaust and then match user mocks.
//
// A test needing a DIFFERENT genesis hash (e.g. a mismatch test) must register
// its own getGenesisHash mock BEFORE calling this — gock matches in registration
// order, so the earlier mock wins.
func SetupMocksForSvmStatePoller(host string, latestSlot, finalizedSlot int64) {
	// getGenesisHash → mainnet-beta (bootstrap cluster-identity validation)
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(filterByHostAndBody(host, `"method":"getGenesisHash"`)).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + SolanaMainnetBetaGenesisHash + `","_note":"svm bootstrap expected mock for getGenesisHash"}`))

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
