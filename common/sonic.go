package common

import (
	"reflect"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/option"
)

var SonicCfg sonic.API

func init() {
	err := sonic.Pretouch(
		reflect.TypeOf(NormalizedResponse{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(JsonRpcResponse{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(JsonRpcRequest{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(NormalizedRequest{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	err = sonic.Pretouch(
		reflect.TypeOf(BaseError{}),
		option.WithCompileMaxInlineDepth(1),
	)
	if err != nil {
		panic(err)
	}
	SonicCfg = sonic.Config{
		// CopyString must be true so that strings produced by Unmarshal
		// own their backing memory. Sonic's default (false) constructs
		// string headers that alias the source byte slice — fast, but
		// any string retained beyond the lifetime of the source buffer
		// keeps that entire buffer alive. The indexer stores parsed
		// block hashes (and similar small strings) in long-lived ring
		// buffers and dedup windows; with aliasing, each retained
		// string pins its multi-KB source payload, turning short-lived
		// notification buffers into a slow accumulating leak that
		// scales with throughput rather than with the data actually
		// kept. Paying the per-string copy is the correct trade.
		CopyString:              true,
		NoNullSliceOrMap:        true,
		NoQuoteTextMarshaler:    true,
		NoValidateJSONMarshaler: true,
		NoValidateJSONSkip:      true,
		EscapeHTML:              false,
		SortMapKeys:             false,
		CompactMarshaler:        true,
		ValidateString:          false,
	}.Froze()
}
