package jsonrpc

import (
	"fmt"
	"net/http"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

// JsonRpcErrorExtractor classifies errors from a generic JSON-RPC upstream
// using ONLY the JSON-RPC 2.0 spec and the HTTP status: there is no protocol
// knowledge here, so the taxonomy is deliberately coarse. It no-ops on
// non-jsonrpc upstreams so a mixed project shares a single composite extractor.
type JsonRpcErrorExtractor struct{}

func NewJsonRpcErrorExtractor() *JsonRpcErrorExtractor {
	return &JsonRpcErrorExtractor{}
}

// Standard JSON-RPC 2.0 error codes (https://www.jsonrpc.org/specification).
// These four say "the request itself is malformed" and are the only codes a
// generic extractor can treat as non-retryable: the same request sent to a
// second upstream yields the same verdict. -32603 (internal error) and the
// implementation-defined -32000..-32099 range plus any application code fall
// to the retryable server-side default; whether a second physical call happens
// is decided by the replay guard (see replay.go), never by the code alone.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

func (e *JsonRpcErrorExtractor) Extract(
	resp *http.Response,
	nr *common.NormalizedResponse,
	jr *common.JsonRpcResponse,
	upstream common.Upstream,
) error {
	if upstream == nil || upstream.Config() == nil || upstream.Config().Type != common.UpstreamTypeJsonRpc {
		// Not a jsonrpc upstream — let the composite extractor fall through.
		return nil
	}
	if resp == nil {
		return nil
	}

	details := map[string]interface{}{
		"statusCode": resp.StatusCode,
		"headers":    util.ExtractUsefulHeaders(resp),
	}

	code := 0
	msg := ""
	if jr != nil && jr.Error != nil {
		code = jr.Error.Code
		msg = jr.Error.Message
		// Carry the upstream's "data" member through verbatim; the application
		// packs its actionable detail in there and buildErrorResponseBody
		// re-emits details["data"] as the JSON-RPC error's "data" member.
		if d := jr.Error.Data; d != nil {
			// An empty string is ParseError's "no data" placeholder, not a payload.
			if s, isStr := d.(string); !isStr || s != "" {
				details["data"] = d
			}
		}
	}

	// wireCode is the JSON-RPC "code" the client ends up seeing: a faithful
	// passthrough of the upstream's own code. eRPC's routing verdict lives in
	// the OUTER StandardError class, never in this number. The fallback only
	// applies when the upstream sent an error object with no numeric code,
	// because NewErrJsonRpcExceptionInternal drops a zero normalized code.
	wireCode := func(fallback common.JsonRpcErrorNumber) common.JsonRpcErrorNumber {
		if code == 0 {
			return fallback
		}
		return common.JsonRpcErrorNumber(code)
	}

	// HTTP 401/403 outrank the JSON-RPC body: an auth failure is a verdict
	// about the credential, not about the RPC call.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		authMsg := msg
		if authMsg == "" {
			authMsg = fmt.Sprintf("jsonrpc upstream unauthorized (HTTP %d)", resp.StatusCode)
		}
		return common.NewErrEndpointUnauthorized(
			common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorUnauthorized), authMsg, nil, details),
		)
	}

	// A synthesized -32700 next to a failing status means the body told us
	// nothing (plaintext/HTML from a CDN or proxy), so classify from the
	// status — the only trustworthy signal left. See the svm extractor for
	// the production incident that motivated this.
	unparseableBody := code == codeParseError && resp.StatusCode >= 400
	if jr == nil || jr.Error == nil || unparseableBody {
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return common.NewErrEndpointCapacityExceeded(
				common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorCapacityExceeded,
					fmt.Sprintf("jsonrpc upstream rate limited (HTTP %d)", resp.StatusCode),
					nil, details),
			)
		case resp.StatusCode >= 500 && resp.StatusCode <= 599:
			return common.NewErrEndpointServerSideException(
				common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorServerSideException,
					fmt.Sprintf("jsonrpc upstream http failure %d", resp.StatusCode),
					nil, details),
				details, resp.StatusCode,
			)
		case resp.StatusCode >= 400 && resp.StatusCode <= 499:
			// 4xx without a JSON body is typically an auth/config issue — treat as
			// client-side, do not retry across upstreams (401/403 handled above).
			wrapped := common.NewErrJsonRpcExceptionInternal(0, common.JsonRpcErrorClientSideException,
				fmt.Sprintf("jsonrpc upstream http failure %d", resp.StatusCode),
				nil, details)
			return common.NewErrEndpointClientSideException(wrapped).WithRetryableTowardNetwork(false)
		}
		return nil
	}

	switch code {
	case codeParseError, codeInvalidRequest, codeMethodNotFound, codeInvalidParams:
		// Spec-defined "your request is malformed": the same bytes get the same
		// answer from every upstream, so there is nothing to fail over to.
		return common.NewErrEndpointClientSideException(
			common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), msg, nil, details),
		).WithRetryableTowardNetwork(false)
	}

	// Everything else — -32603, the implementation-defined range and any
	// application code under any status — is a server-side verdict with the
	// code, message and data preserved verbatim. Retryable by class; the replay
	// guard decides whether the method may actually be dispatched again.
	return common.NewErrEndpointServerSideException(
		common.NewErrJsonRpcExceptionInternal(code, wireCode(common.JsonRpcErrorServerSideException), msg, nil, details),
		details, resp.StatusCode,
	)
}
