package erpc

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
)

// mockCountedMethod answers one method on one host with a fixed JSON-RPC result.
// Calls are counted by countCalls (a Filter can run more than once per request).
func mockCountedMethod(host, method, resultJSON string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), `"`+method+`"`)
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%s}`, resultJSON)))
}

// countCalls counts the requests gock served, per "host method".
func countCalls(methods ...string) func(host, method string) int64 {
	var mu sync.Mutex
	n := map[string]int64{}
	gock.Observe(func(r *http.Request, _ gock.Mock) {
		if r == nil || r.URL == nil {
			return
		}
		body := util.SafeReadBody(r)
		for _, m := range methods {
			if strings.Contains(body, `"`+m+`"`) {
				mu.Lock()
				n[r.URL.Host+" "+m]++
				mu.Unlock()
			}
		}
	})
	return func(host, method string) int64 {
		mu.Lock()
		defer mu.Unlock()
		return n[host+" "+method]
	}
}
