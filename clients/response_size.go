package clients

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/erpc/erpc/common"
)

// jsonrpsee (and so reth) answers a response over its own size cap with this
// code and message. Reusing them makes an over-cap body here indistinguishable
// from that upstream error, for both eRPC's error normalizer and the caller.
const (
	responseTooBigCode    = -32008
	responseTooBigMessage = "Response is too big"
)

var errResponseTooBig = errors.New("upstream response exceeded maxResponseBytes")

// cappedBody stops reading once more than limit bytes have come through, so an
// oversized response is never buffered whole.
type cappedBody struct {
	body      io.ReadCloser
	remaining int64
	exceeded  bool
}

func newCappedBody(body io.ReadCloser, limit int64) *cappedBody {
	return &cappedBody{body: body, remaining: limit}
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.exceeded {
		return 0, errResponseTooBig
	}

	// One byte past the cap is enough to tell "exactly at the cap" from "over it".
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}

	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		b.exceeded = true
		return n, errResponseTooBig
	}

	return n, err
}

func (b *cappedBody) Close() error {
	return b.body.Close()
}

// declaredTooBig reports whether the response announces a body over the cap.
// Only an uncompressed Content-Length says anything about the decoded size.
func declaredTooBig(resp *http.Response, limit int64) bool {
	isGzip := resp.Header.Get("Content-Encoding") == "gzip"

	return !isGzip && resp.ContentLength > limit
}

func newResponseTooBig(id interface{}, limit int64) (*common.JsonRpcResponse, error) {
	return common.NewJsonRpcResponse(id, nil, common.NewErrJsonRpcExceptionExternal(
		responseTooBigCode,
		responseTooBigMessage,
		fmt.Sprintf("Exceeded max limit of %d", limit),
	))
}
