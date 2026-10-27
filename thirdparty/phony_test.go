package thirdparty

import (
	"net/http"
	"testing"

	archEvm "github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/common"
)

// phonyUpstream is the client.upstream during vendor SupportsNetwork probes, and
// the EVM error extractor reads upstream.Config().Type. The embedded Upstream
// interface is nil, so Config() must be overridden or that read panics
// (nil pointer dereference via the promoted method). Regression guard.
func TestPhonyUpstream_Config_NoPanicInEvmExtractor(t *testing.T) {
	u := &phonyUpstream{id: "temp-erpc-1"}
	if cfg := u.Config(); cfg == nil || cfg.Type != common.UpstreamTypeEvm {
		t.Fatalf("phony Config must be a non-nil EVM config, got %+v", cfg)
	}

	e := archEvm.NewJsonRpcErrorExtractor()
	r := &http.Response{StatusCode: 200, Header: http.Header{}}
	jr := common.MustNewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(-32602, "Invalid params", ""))
	// Must not panic, and (being EVM) must still classify rather than no-op.
	if got := e.Extract(r, nil, jr, u); got == nil {
		t.Fatal("EVM extractor should classify -32602 for a phony (EVM) upstream")
	}
}

// The phony upstream is a probe-only identity: it reports its id as both Id and
// Name, always claims EVM, and carries no vendor or vendor settings.
func TestPhonyUpstream_Identity(t *testing.T) {
	u := &phonyUpstream{id: "temp-goldsky-8453"}
	if u.Id() != "temp-goldsky-8453" || u.Name() != "temp-goldsky-8453" {
		t.Fatalf("Id/Name must echo the probe id, got %q/%q", u.Id(), u.Name())
	}
	if u.Type() != common.UpstreamTypeEvm {
		t.Fatalf("phony upstream must be EVM, got %q", u.Type())
	}
	if u.Config().Id != "temp-goldsky-8453" {
		t.Fatalf("Config().Id must be the probe id, got %q", u.Config().Id)
	}
	if u.Vendor() != nil {
		t.Fatal("phony upstream must not carry a vendor")
	}
	if u.VendorSettings() != nil {
		t.Fatal("phony upstream must not carry vendor settings")
	}
}
