package common

import (
	"reflect"

	"github.com/bytedance/sonic"
	"github.com/bytedance/sonic/option"
)

var SonicCfg sonic.API

func init() {
	sonic.Pretouch(
		reflect.TypeOf(NormalizedResponse{}),
		option.WithCompileMaxInlineDepth(2),
	)
	sonic.Pretouch(
		reflect.TypeOf(JsonRpcResponse{}),
		option.WithCompileMaxInlineDepth(1),
	)
	sonic.Pretouch(
		reflect.TypeOf(JsonRpcRequest{}),
		option.WithCompileMaxInlineDepth(1),
	)
	sonic.Pretouch(
		reflect.TypeOf(NormalizedRequest{}),
		option.WithCompileMaxInlineDepth(2),
	)
	sonic.Pretouch(
		reflect.TypeOf(BaseError{}),
		option.WithCompileMaxInlineDepth(1),
	)
	SonicCfg = sonic.Config{
		CopyString:              false,
		NoQuoteTextMarshaler:    true,
		NoValidateJSONMarshaler: true,
		EscapeHTML:              false,
		SortMapKeys:             false,
		CompactMarshaler:        true,
		ValidateString:          false,
	}.Froze()
}
