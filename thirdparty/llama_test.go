package thirdparty

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func llamaErr(t *testing.T, e *common.ErrJsonRpcExceptionExternal) error {
	t.Helper()
	jrr := &common.JsonRpcResponse{Error: e}
	return CreateLlamaVendor().GetVendorSpecificErrorIfAny(nil, &http.Response{StatusCode: 200}, jrr, map[string]interface{}{})
}

func parseErr(body string) *common.ErrJsonRpcExceptionExternal {
	return common.NewErrJsonRpcExceptionExternal(int(common.JsonRpcErrorParseException),
		"cannot parse json-rpc response: invalid character 'e' looking for beginning of value", body)
}

func TestLlamaVendor_RawCloudflare1015IsCapacityExceeded(t *testing.T) {
	for _, body := range []string{"error code: 1015", "error code: 1015\n", "  error code: 1015 extra"} {
		err := llamaErr(t, parseErr(body))
		require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded), "body %q: %v", body, err)
	}
}

func TestLlamaVendor_OtherNonJsonBodiesUnchanged(t *testing.T) {
	for _, body := range []string{"error code: 1020", "<html>502 Bad Gateway</html>", "", "rate limited error code: 1015"} {
		require.Nil(t, llamaErr(t, parseErr(body)), "body %q must keep generic classification", body)
	}
	// Non-parse error carrying the text as data is not reclassified.
	require.Nil(t, llamaErr(t, common.NewErrJsonRpcExceptionExternal(-32000, "boom", "error code: 1015")))
}
